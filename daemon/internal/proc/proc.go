// Package proc starts and stops the daemon's child processes (contracts section 2, DESIGN 5.3 and 5.13), runs short
// helper commands, and writes logs.
//
// Every child gets an explicit environment, a working directory, stdin from a pipe or /dev/null, and its own session
// (Setsid), so it has no controlling terminal. Children of the daemon are waited for individually through os/exec;
// the daemon never calls wait(-1).
package proc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Spec describes a long running child process.
type Spec struct {
	// Path is the absolute path of the executable; Args are argv[1:].
	Path string
	Args []string
	// Env is the complete environment of the child; nothing is inherited from the daemon.
	Env []string
	Dir string
	// Credential is nil when the child runs as the daemon's own user (rootless mode).
	Credential *syscall.Credential
	// ExtraFiles become file descriptors 3, 4, ... in the child.
	ExtraFiles []*os.File
	// StdinPipe gives the child a pipe as stdin (Process.Stdin). Otherwise stdin is /dev/null.
	StdinPipe bool
	// Stdout receives the child's stdout. When nil, the caller reads Process.Stdout.
	Stdout io.Writer
	// Stderr receives the child's stderr. When nil and StderrToStdout is false, the caller reads Process.Stderr.
	Stderr io.Writer
	// StderrToStdout sends stderr into the same pipe as stdout, keeping the order of both streams.
	StderrToStdout bool
}

// ExitStatus is how a process ended: Code is set when it exited, Signal when a signal killed it.
type ExitStatus struct {
	Code   *int    `json:"code"`
	Signal *string `json:"signal"`
}

// String describes the status in Chinese for messages shown to users.
func (s ExitStatus) String() string {
	switch {
	case s.Code != nil:
		return fmt.Sprintf("退出码 %d", *s.Code)
	case s.Signal != nil:
		return "信号 " + *s.Signal
	default:
		return "未知的退出状态"
	}
}

// Success reports whether the process exited with code 0.
func (s ExitStatus) Success() bool {
	return s.Code != nil && *s.Code == 0
}

func exitStatusOf(state *os.ProcessState) ExitStatus {
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok {
		code := state.ExitCode()
		return ExitStatus{Code: &code}
	}
	if ws.Signaled() {
		name := unix.SignalName(ws.Signal())
		if name == "" {
			name = fmt.Sprintf("signal %d", int(ws.Signal()))
		}
		return ExitStatus{Signal: &name}
	}
	code := ws.ExitStatus()
	return ExitStatus{Code: &code}
}

// Process is a started child process.
type Process struct {
	cmd *exec.Cmd

	// Stdin is the write end of the child's stdin pipe; nil unless Spec.StdinPipe.
	Stdin *os.File
	// Stdout is the read end of the child's stdout pipe when Spec.Stdout was nil.
	Stdout *os.File
	// Stderr is the read end of the child's stderr pipe when Spec.Stderr was nil and StderrToStdout was false.
	Stderr *os.File

	// StartedAt is the time the child was started.
	StartedAt time.Time

	done      chan struct{}
	exit      ExitStatus
	waitErr   error
	stdinOnce sync.Once
	stdinErr  error

	copyWG     sync.WaitGroup
	outputDone chan struct{}
	copyMu     sync.Mutex
	copyErr    error
	copyFrom   []*os.File
}

// Start starts the child described by spec.
func Start(spec Spec) (*Process, error) {
	if spec.Path == "" {
		return nil, errors.New("proc: empty Path")
	}
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Env = append([]string{}, spec.Env...)
	cmd.Dir = spec.Dir
	cmd.ExtraFiles = spec.ExtraFiles
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Credential: spec.Credential}

	p := &Process{cmd: cmd, done: make(chan struct{}), outputDone: make(chan struct{})}
	var childEnds, parentEnds []*os.File
	closeAll := func() {
		for _, f := range childEnds {
			f.Close()
		}
		for _, f := range parentEnds {
			f.Close()
		}
	}
	newPipe := func() (r, w *os.File, err error) {
		r, w, err = os.Pipe()
		if err != nil {
			return nil, nil, err
		}
		return r, w, nil
	}

	if spec.StdinPipe {
		r, w, err := newPipe()
		if err != nil {
			return nil, err
		}
		childEnds = append(childEnds, r)
		parentEnds = append(parentEnds, w)
		cmd.Stdin = r
		p.Stdin = w
	}

	outR, outW, err := newPipe()
	if err != nil {
		closeAll()
		return nil, err
	}
	childEnds = append(childEnds, outW)
	parentEnds = append(parentEnds, outR)
	cmd.Stdout = outW
	var copies []func()
	if spec.Stdout == nil {
		p.Stdout = outR
	} else {
		w := spec.Stdout
		copies = append(copies, func() { p.copyOutput(w, outR) })
	}

	if spec.StderrToStdout {
		cmd.Stderr = outW
	} else {
		errR, errW, err := newPipe()
		if err != nil {
			closeAll()
			return nil, err
		}
		childEnds = append(childEnds, errW)
		parentEnds = append(parentEnds, errR)
		cmd.Stderr = errW
		if spec.Stderr == nil {
			p.Stderr = errR
		} else {
			w := spec.Stderr
			copies = append(copies, func() { p.copyOutput(w, errR) })
		}
	}

	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, fmt.Errorf("启动 %s 失败：%w", spec.Path, err)
	}
	p.StartedAt = time.Now()
	for _, f := range childEnds {
		f.Close()
	}
	p.copyWG.Add(len(copies))
	for _, c := range copies {
		go func() {
			defer p.copyWG.Done()
			c()
		}()
	}
	go func() {
		p.copyWG.Wait()
		close(p.outputDone)
	}()
	go p.wait()
	return p, nil
}

func (p *Process) copyOutput(w io.Writer, r *os.File) {
	p.copyMu.Lock()
	p.copyFrom = append(p.copyFrom, r)
	p.copyMu.Unlock()
	defer r.Close()
	buf := make([]byte, 64*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				p.copyMu.Lock()
				if p.copyErr == nil {
					p.copyErr = werr
				}
				p.copyMu.Unlock()
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				p.copyMu.Lock()
				if p.copyErr == nil {
					p.copyErr = err
				}
				p.copyMu.Unlock()
			}
			return
		}
	}
}

// closeOutputs closes the read ends used by the copy goroutines, which makes them return.
func (p *Process) closeOutputs() {
	p.copyMu.Lock()
	defer p.copyMu.Unlock()
	for _, f := range p.copyFrom {
		f.Close()
	}
}

func (p *Process) wait() {
	err := p.cmd.Wait()
	p.exit = exitStatusOf(p.cmd.ProcessState)
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		p.waitErr = err
	}
	if p.Stdin != nil {
		p.CloseStdin()
	}
	close(p.done)
}

// PID returns the child's process id.
func (p *Process) PID() int { return p.cmd.Process.Pid }

// Done is closed when the child has exited and was reaped.
func (p *Process) Done() <-chan struct{} { return p.done }

// Exited reports whether the child has exited.
func (p *Process) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// Exit returns the exit status. It must only be called after Done is closed.
func (p *Process) Exit() ExitStatus {
	<-p.done
	return p.exit
}

// WaitErr returns an error of the wait system call itself (not a non-zero exit). Valid after Done.
func (p *Process) WaitErr() error {
	<-p.done
	return p.waitErr
}

// OutputDone is closed when the Spec.Stdout and Spec.Stderr writers received everything (end of the pipes). A
// descendant that keeps the pipes open delays it beyond the child's exit.
func (p *Process) OutputDone() <-chan struct{} { return p.outputDone }

// OutputErr returns the first error of writing to Spec.Stdout/Spec.Stderr or reading the pipes.
func (p *Process) OutputErr() error {
	p.copyMu.Lock()
	defer p.copyMu.Unlock()
	return p.copyErr
}

// CloseStdin closes the write end of the stdin pipe. Calling it more than once is allowed.
func (p *Process) CloseStdin() error {
	if p.Stdin == nil {
		return nil
	}
	p.stdinOnce.Do(func() {
		if err := p.Stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			p.stdinErr = err
		}
	})
	return p.stdinErr
}

// Signal sends sig to the child itself. A child that already exited is not an error.
func (p *Process) Signal(sig syscall.Signal) error {
	if err := p.cmd.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func (p *Process) waitFor(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-p.done:
		return true
	case <-t.C:
		return false
	}
}

// Stop runs the stop procedure of DESIGN 5.3: close stdin, wait grace; then collect the whole process tree from
// /proc, send SIGTERM to the root, wait grace again, and send SIGKILL to every collected process that is still
// alive. The tree is also collected before stdin is closed, so descendants that the child leaves behind when it
// exits by itself are killed as well. Stop returns after the child was reaped.
func (p *Process) Stop(grace time.Duration) error {
	initial, err := CollectTree(p.PID())
	if err != nil {
		return err
	}
	if err := p.CloseStdin(); err != nil {
		return err
	}
	if p.waitFor(grace) {
		return KillAll(initial)
	}
	return p.terminate(grace, initial)
}

// Terminate collects the process tree, sends SIGTERM to the root, waits grace, then sends SIGKILL to every
// collected process that is still alive. It returns after the child was reaped.
func (p *Process) Terminate(grace time.Duration) error {
	initial, err := CollectTree(p.PID())
	if err != nil {
		return err
	}
	return p.terminate(grace, initial)
}

func (p *Process) terminate(grace time.Duration, earlier []Entry) error {
	tree, err := CollectTree(p.PID())
	if err != nil {
		return err
	}
	all := mergeEntries(earlier, tree)
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	p.waitFor(grace)
	if err := KillAll(all); err != nil {
		return err
	}
	if err := p.Signal(syscall.SIGKILL); err != nil {
		return err
	}
	<-p.done
	return nil
}

// KillTree sends SIGKILL to the child and all of its descendants at once (used when the child stopped responding)
// and returns after the child was reaped.
func (p *Process) KillTree() error {
	tree, err := CollectTree(p.PID())
	if err != nil {
		return err
	}
	if err := KillAll(tree); err != nil {
		return err
	}
	if err := p.Signal(syscall.SIGKILL); err != nil {
		return err
	}
	<-p.done
	return nil
}
