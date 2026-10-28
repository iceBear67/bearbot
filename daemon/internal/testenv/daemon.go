package testenv

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bearbot/daemon/internal/proc"
)

var daemonBinary string

// RunWithDaemon builds ./cmd/bearbotd once into a temporary directory, runs the tests, and removes the directory.
// Use it in TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(testenv.RunWithDaemon(m)) }
func RunWithDaemon(m *testing.M) int {
	dir, err := os.MkdirTemp("", "bearbotd-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	path, err := BuildDaemon(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	daemonBinary = path
	return m.Run()
}

// BuildDaemon runs `go build -o <dir>/bearbotd ./cmd/bearbotd` in the daemon module and returns the binary path.
func BuildDaemon(dir string) (string, error) {
	repo, err := RepoRoot()
	if err != nil {
		return "", err
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("构建 bearbotd 需要 go 命令：%w", err)
	}
	out := filepath.Join(dir, "bearbotd")
	if _, err := proc.RunChecked(context.Background(), proc.RunSpec{
		Path:    goTool,
		Args:    []string{"build", "-o", out, "./cmd/bearbotd"},
		Env:     os.Environ(),
		Dir:     filepath.Join(repo, "daemon"),
		Timeout: 10 * time.Minute,
	}); err != nil {
		return "", fmt.Errorf("构建 bearbotd 失败：%w", err)
	}
	return out, nil
}

// DaemonBinary returns the binary built by RunWithDaemon; the test fails when TestMain did not build it.
func DaemonBinary(t testing.TB) string {
	t.Helper()
	if daemonBinary == "" {
		t.Fatal("bearbotd 没有构建：在 TestMain 中使用 testenv.RunWithDaemon")
	}
	return daemonBinary
}

// Daemon is a running `bearbotd --root R --rootless run` with pipes as stdin and stdout (console line mode).
type Daemon struct {
	env    *Env
	proc   *proc.Process
	stderr *proc.Ring

	mu     sync.Mutex
	lines  []string
	notify chan struct{}
	eof    bool
}

// StartDaemon starts bearbotd with a pipe as stdin. The daemon gets the test's environment plus extraEnv
// ("NAME=value"). It is stopped when the test ends.
func (e *Env) StartDaemon(t testing.TB, extraEnv ...string) *Daemon {
	t.Helper()
	return e.startDaemon(t, true, extraEnv)
}

// StartDaemonNoInput starts bearbotd with /dev/null as stdin, as `docker run -d` does; stdout is a pipe. Send and
// CloseInput cannot be used.
func (e *Env) StartDaemonNoInput(t testing.TB, extraEnv ...string) *Daemon {
	t.Helper()
	return e.startDaemon(t, false, extraEnv)
}

func (e *Env) startDaemon(t testing.TB, stdinPipe bool, extraEnv []string) *Daemon {
	t.Helper()
	e.RequireGateway(t)
	bin := DaemonBinary(t)
	stderr := proc.NewRing(2000)
	p, err := proc.Start(proc.Spec{
		Path:      bin,
		Args:      []string{"--root", e.Root, "--rootless", "run"},
		Env:       append(os.Environ(), extraEnv...),
		Dir:       e.Root,
		StdinPipe: stdinPipe,
		Stderr:    stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{env: e, proc: p, stderr: stderr, notify: make(chan struct{})}
	go d.read()
	t.Cleanup(func() { d.Stop(t) })
	return d
}

func (d *Daemon) read() {
	r := bufio.NewReaderSize(d.proc.Stdout, 64*1024)
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			d.mu.Lock()
			d.lines = append(d.lines, strings.TrimSuffix(line, "\n"))
			close(d.notify)
			d.notify = make(chan struct{})
			d.mu.Unlock()
		}
		if err != nil {
			d.mu.Lock()
			d.eof = true
			close(d.notify)
			d.notify = make(chan struct{})
			d.mu.Unlock()
			return
		}
	}
}

// Process returns the daemon process.
func (d *Daemon) Process() *proc.Process { return d.proc }

// Send writes one console input line.
func (d *Daemon) Send(t testing.TB, line string) {
	t.Helper()
	if _, err := io.WriteString(d.proc.Stdin, line+"\n"); err != nil {
		t.Fatalf("write to bearbotd stdin: %v", err)
	}
}

// CloseInput closes the daemon's stdin (end of console input).
func (d *Daemon) CloseInput(t testing.TB) {
	t.Helper()
	if err := d.proc.CloseStdin(); err != nil {
		t.Fatal(err)
	}
}

// Output returns every stdout line read so far.
func (d *Daemon) Output() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string{}, d.lines...)
}

// Stderr returns the last lines the daemon wrote to stderr.
func (d *Daemon) Stderr() []string { return d.stderr.Lines(0) }

// WaitOutput waits until a stdout line (including lines read before the call) satisfies match and returns it. The
// test fails after timeout or when stdout ends.
func (d *Daemon) WaitOutput(t testing.TB, timeout time.Duration, match func(line string) bool) string {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	seen := 0
	for {
		d.mu.Lock()
		for ; seen < len(d.lines); seen++ {
			if match(d.lines[seen]) {
				line := d.lines[seen]
				d.mu.Unlock()
				return line
			}
		}
		eof, notify := d.eof, d.notify
		d.mu.Unlock()
		if eof {
			t.Fatalf("bearbotd stdout ended without the expected line\nstdout:\n%s\nstderr:\n%s", strings.Join(d.Output(), "\n"), strings.Join(d.Stderr(), "\n"))
		}
		select {
		case <-notify:
		case <-deadline.C:
			t.Fatalf("no expected bearbotd output within %v\nstdout:\n%s\nstderr:\n%s", timeout, strings.Join(d.Output(), "\n"), strings.Join(d.Stderr(), "\n"))
		}
	}
}

// WaitOutputContains waits for a stdout line that contains substr.
func (d *Daemon) WaitOutputContains(t testing.TB, timeout time.Duration, substr string) string {
	t.Helper()
	return d.WaitOutput(t, timeout, func(line string) bool { return strings.Contains(line, substr) })
}

// StopGrace bounds how long Stop waits for bearbotd to shut down after SIGTERM before everything is killed.
const StopGrace = 60 * time.Second

// Stop sends SIGTERM to bearbotd, waits up to StopGrace, then kills every process of the daemon's tree that is still
// alive. Calling it again does nothing.
func (d *Daemon) Stop(t testing.TB) {
	t.Helper()
	if d.proc.Exited() {
		return
	}
	if err := d.proc.Terminate(StopGrace); err != nil {
		t.Errorf("stop bearbotd: %v", err)
	}
}

// UnixClient returns an HTTP client whose connections go to the unix socket at path.
func UnixClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
}

// WaitForSocket waits until a unix socket at path accepts connections.
func WaitForSocket(t testing.TB, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		c, err := net.Dial("unix", path)
		if err == nil {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket %s does not accept connections within %v: %v", path, timeout, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func jsonRequest(t testing.TB, client *http.Client, method, path string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, "http://bearbotd"+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// CallAgent sends a JSON request to agent.sock and decodes the JSON response body into out (when out is not nil).
// It returns the HTTP status code.
func (e *Env) CallAgent(t testing.TB, method, path string, body any, out any) int {
	t.Helper()
	resp := jsonRequest(t, UnixClient(e.Paths.AgentSock), method, path, body)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, path, data, err)
		}
	}
	return resp.StatusCode
}

// AdminResult is the decoded NDJSON answer of an admin.sock endpoint (contracts 7).
type AdminResult struct {
	Lines []string
	OK    bool
	Error string
}

// CallAdmin calls an admin.sock endpoint and reads the whole NDJSON answer.
func (e *Env) CallAdmin(t testing.TB, method, path string, body any) AdminResult {
	t.Helper()
	resp := jsonRequest(t, UnixClient(e.Paths.AdminSock), method, path, body)
	defer resp.Body.Close()
	var res AdminResult
	dec := json.NewDecoder(resp.Body)
	for {
		var rec struct {
			Line  *string `json:"line"`
			Done  bool    `json:"done"`
			OK    bool    `json:"ok"`
			Error string  `json:"error"`
		}
		if err := dec.Decode(&rec); err != nil {
			if errors.Is(err, io.EOF) {
				t.Fatalf("%s %s: answer ended without done record (lines %q)", method, path, res.Lines)
			}
			t.Fatalf("%s %s: %v", method, path, err)
		}
		if rec.Done {
			res.OK, res.Error = rec.OK, rec.Error
			return res
		}
		if rec.Line == nil {
			t.Fatalf("%s %s: record without line or done", method, path)
		}
		res.Lines = append(res.Lines, *rec.Line)
	}
}
