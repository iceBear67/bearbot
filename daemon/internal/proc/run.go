package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// RunSpec describes a short helper command (git, npm, uv, tsc, maintenance hooks). Stdin is /dev/null unless Stdin
// is set, and the command runs in its own session like every other child.
type RunSpec struct {
	Path       string
	Args       []string
	Env        []string
	Dir        string
	Credential *syscall.Credential
	// Timeout bounds the run; when it expires the whole process tree is killed. It must be positive.
	Timeout time.Duration
	// Stdin, when set, is copied into the command's stdin, which is closed after the last byte. It must not block
	// (bytes, a string, a regular file). A command that exits with code 0 without reading all of it is an error.
	Stdin io.Reader
	// Stdout, when set, receives the command's stdout; Result.Stdout then stays empty and Result.Output holds only
	// stderr.
	Stdout io.Writer
}

// Result is the outcome of a finished helper command.
type Result struct {
	// Command is the command line, for messages.
	Command string
	Stdout  []byte
	Stderr  []byte
	// Output holds stdout and stderr interleaved in the order the daemon read them. They are separate pipes, so
	// parts written to both within a moment of each other may appear in either order.
	Output   []byte
	Exit     ExitStatus
	TimedOut bool
	Duration time.Duration
}

// OK reports whether the command exited with code 0 within the timeout.
func (r *Result) OK() bool { return !r.TimedOut && r.Exit.Success() }

// Err returns nil when OK, otherwise an error that names the command, the reason and the output.
func (r *Result) Err() error {
	if r.OK() {
		return nil
	}
	reason := r.Exit.String()
	if r.TimedOut {
		reason = fmt.Sprintf("超过 %s 没有结束，已终止", r.Duration.Round(time.Millisecond))
	}
	out := strings.TrimSpace(string(r.Output))
	if out == "" {
		return fmt.Errorf("命令 %s 失败（%s），没有输出", r.Command, reason)
	}
	return fmt.Errorf("命令 %s 失败（%s），输出：\n%s", r.Command, reason, out)
}

type lockedBuffer struct {
	mu  *sync.Mutex
	own *bytes.Buffer
	all *bytes.Buffer
}

func (b lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.own.Write(p)
	b.all.Write(p)
	return len(p), nil
}

// outputLinger is how long Run keeps reading the output pipes after the command exited, for descendants that
// still hold them open.
const outputLinger = 2 * time.Second

// Run runs the command and waits for it. The returned error is only about starting the command or ctx ending;
// a non-zero exit or a timeout is reported in the Result (see Result.Err).
func Run(ctx context.Context, spec RunSpec) (*Result, error) {
	if spec.Timeout <= 0 {
		return nil, errors.New("proc.Run: Timeout must be positive")
	}
	var mu sync.Mutex
	var stdout, stderr, all bytes.Buffer
	var out io.Writer = lockedBuffer{mu: &mu, own: &stdout, all: &all}
	if spec.Stdout != nil {
		out = spec.Stdout
	}
	start := time.Now()
	p, err := Start(Spec{
		Path:       spec.Path,
		Args:       spec.Args,
		Env:        spec.Env,
		Dir:        spec.Dir,
		Credential: spec.Credential,
		StdinPipe:  spec.Stdin != nil,
		Stdout:     out,
		Stderr:     lockedBuffer{mu: &mu, own: &stderr, all: &all},
	})
	if err != nil {
		return nil, err
	}
	var stdinDone chan error
	if spec.Stdin != nil {
		stdinDone = make(chan error, 1)
		go func() {
			_, err := io.Copy(p.Stdin, spec.Stdin)
			if cerr := p.CloseStdin(); err == nil {
				err = cerr
			}
			stdinDone <- err
		}()
	}
	timer := time.NewTimer(spec.Timeout)
	defer timer.Stop()
	timedOut := false
	var ctxErr error
	select {
	case <-p.Done():
	case <-timer.C:
		timedOut = true
		if err := p.KillTree(); err != nil {
			return nil, err
		}
	case <-ctx.Done():
		ctxErr = ctx.Err()
		if err := p.KillTree(); err != nil {
			return nil, err
		}
	}
	linger := time.NewTimer(outputLinger)
	select {
	case <-p.OutputDone():
	case <-linger.C:
		p.closeOutputs()
		<-p.OutputDone()
	}
	linger.Stop()
	if err := p.WaitErr(); err != nil {
		return nil, err
	}
	if err := p.OutputErr(); err != nil {
		return nil, err
	}
	// The process was reaped, so its stdin is closed and the copy has ended.
	var stdinErr error
	if stdinDone != nil {
		stdinErr = <-stdinDone
	}
	mu.Lock()
	res := &Result{
		Command:  commandLine(spec.Path, spec.Args),
		Stdout:   bytes.Clone(stdout.Bytes()),
		Stderr:   bytes.Clone(stderr.Bytes()),
		Output:   bytes.Clone(all.Bytes()),
		Exit:     p.Exit(),
		TimedOut: timedOut,
		Duration: time.Since(start),
	}
	mu.Unlock()
	if ctxErr != nil {
		return res, ctxErr
	}
	if stdinErr != nil && res.OK() {
		return res, fmt.Errorf("命令 %s 没有读完标准输入：%w", res.Command, stdinErr)
	}
	return res, nil
}

// RunChecked is Run followed by Result.Err: any failure, including a non-zero exit, is returned as an error.
func RunChecked(ctx context.Context, spec RunSpec) (*Result, error) {
	res, err := Run(ctx, spec)
	if err != nil {
		return res, err
	}
	return res, res.Err()
}

// AttachedSpec describes an interactive command that uses the caller's terminal.
type AttachedSpec struct {
	Path       string
	Args       []string
	Env        []string
	Dir        string
	Credential *syscall.Credential
}

// RunAttached runs an interactive command with the caller's stdin, stdout and stderr and without a new session, so
// it keeps the caller's controlling terminal. Only `bearbotd login` uses it (contracts 2.5).
func RunAttached(spec AttachedSpec) (ExitStatus, error) {
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Env = append([]string{}, spec.Env...)
	cmd.Dir = spec.Dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: spec.Credential}
	if err := cmd.Start(); err != nil {
		return ExitStatus{}, fmt.Errorf("启动 %s 失败：%w", spec.Path, err)
	}
	err := cmd.Wait()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return ExitStatus{}, err
	}
	return exitStatusOf(cmd.ProcessState), nil
}

func commandLine(path string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	for _, a := range append([]string{path}, args...) {
		if a == "" || strings.ContainsAny(a, " \t\n\"'\\$") {
			parts = append(parts, fmt.Sprintf("%q", a))
		} else {
			parts = append(parts, a)
		}
	}
	return strings.Join(parts, " ")
}
