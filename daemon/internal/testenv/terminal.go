package testenv

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"bearbot/daemon/internal/proc"
)

// TerminalDaemon is `bearbotd --root R --rootless run` on a pseudo-terminal that is its stdin, stdout and
// controlling terminal, as with `docker run -it` (DESIGN 5.13).
type TerminalDaemon struct {
	cmd    *exec.Cmd
	pty    *os.File
	stderr *proc.Ring
	done   chan struct{}

	mu  sync.Mutex
	out bytes.Buffer
}

// StartDaemonOnTerminal starts bearbotd on a new pseudo-terminal (40 rows, 160 columns) in its own session with the
// terminal as controlling terminal. The daemon gets the test's environment plus extraEnv. It is stopped when the
// test ends.
func (e *Env) StartDaemonOnTerminal(t testing.TB, extraEnv ...string) *TerminalDaemon {
	t.Helper()
	e.RequireGateway(t)
	bin := DaemonBinary(t)
	master, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(master, &pty.Winsize{Rows: 40, Cols: 160}); err != nil {
		t.Fatal(err)
	}
	d := &TerminalDaemon{pty: master, stderr: proc.NewRing(2000), done: make(chan struct{})}
	cmd := exec.Command(bin, "--root", e.Root, "--rootless", "run")
	cmd.Env = append(append(os.Environ(), "TERM=xterm-256color"), extraEnv...)
	cmd.Dir = e.Root
	cmd.Stdin, cmd.Stdout = tty, tty
	cmd.Stderr = d.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		tty.Close()
		master.Close()
		t.Fatal(err)
	}
	if err := tty.Close(); err != nil {
		t.Fatal(err)
	}
	d.cmd = cmd
	go d.read()
	go func() {
		cmd.Wait()
		close(d.done)
	}()
	t.Cleanup(func() {
		d.Stop(t)
		master.Close()
	})
	return d
}

func (d *TerminalDaemon) read() {
	buf := make([]byte, 64*1024)
	for {
		n, err := d.pty.Read(buf)
		d.mu.Lock()
		d.out.Write(buf[:n])
		d.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// PID returns the process id of bearbotd.
func (d *TerminalDaemon) PID() int { return d.cmd.Process.Pid }

// Done is closed when bearbotd exited.
func (d *TerminalDaemon) Done() <-chan struct{} { return d.done }

// Type writes keys to the terminal.
func (d *TerminalDaemon) Type(t testing.TB, keys string) {
	t.Helper()
	if _, err := io.WriteString(d.pty, keys); err != nil {
		t.Fatalf("write to the terminal: %v", err)
	}
}

var terminalControl = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[PX^_][^\x1b]*\x1b\\|\x1b[=>]`)

// Text returns what the terminal showed so far, without control sequences.
func (d *TerminalDaemon) Text() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return terminalControl.ReplaceAllString(d.out.String(), "")
}

// WaitText waits until the terminal shows want after the first skip bytes of Text and returns the length of Text
// at that moment.
func (d *TerminalDaemon) WaitText(t testing.TB, timeout time.Duration, skip int, want string) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		text := d.Text()
		if len(text) >= skip && strings.Contains(text[skip:], want) {
			return len(text)
		}
		select {
		case <-d.done:
			t.Fatalf("bearbotd exited; the terminal does not show %q:\n%s\nstderr:\n%s", want, text, strings.Join(d.stderr.Lines(0), "\n"))
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the terminal does not show %q within %v:\n%s\nstderr:\n%s", want, timeout, text, strings.Join(d.stderr.Lines(0), "\n"))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Stop sends SIGTERM to bearbotd, waits up to StopGrace, then kills every process of the daemon's tree that is still
// alive. Calling it again does nothing.
func (d *TerminalDaemon) Stop(t testing.TB) {
	t.Helper()
	select {
	case <-d.done:
		return
	default:
	}
	if err := syscall.Kill(d.PID(), syscall.SIGTERM); err != nil {
		t.Errorf("SIGTERM bearbotd: %v", err)
	}
	select {
	case <-d.done:
	case <-time.After(StopGrace):
		entries, err := proc.CollectTree(d.PID())
		if err == nil {
			err = proc.KillAll(entries)
		}
		<-d.done
		t.Errorf("bearbotd did not stop within %v after SIGTERM (%v)", StopGrace, err)
	}
}
