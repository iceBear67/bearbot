package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"bearbot/daemon/internal/proc"
	"bearbot/daemon/internal/testenv"
)

// otherPython returns a CPython whose version differs from /usr/bin/python3.14: a uv managed one, or CPython 3.13
// installed by uv into a temporary directory.
func otherPython(t *testing.T, e *testenv.Env) string {
	t.Helper()
	ctx := context.Background()
	env := append(os.Environ(), "UV_PYTHON_DOWNLOADS=never")
	for _, v := range []string{"3.13", "3.12", "3.11", "3.15"} {
		res, err := proc.Run(ctx, proc.RunSpec{Path: e.Paths.Uv, Args: []string{"python", "find", "--managed-python", v}, Env: env, Dir: "/", Timeout: 30 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if res.OK() {
			return strings.TrimSpace(string(res.Stdout))
		}
	}
	dir := t.TempDir()
	if _, err := proc.RunChecked(ctx, proc.RunSpec{Path: e.Paths.Uv, Args: []string{"python", "install", "--install-dir", dir, "3.13"}, Env: os.Environ(), Dir: "/", Timeout: 10 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	res, err := proc.RunChecked(ctx, proc.RunSpec{Path: e.Paths.Uv, Args: []string{"python", "find", "3.13"}, Env: append(os.Environ(), "UV_PYTHON_INSTALL_DIR="+dir), Dir: "/", Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(res.Stdout))
}

// TestPythonVersionChange (DESIGN 4.7, 16): after the image's interpreter changed, `bearbotd run` exits with a
// non-zero status and the rebuild command; `bearbotd python rebuild` reinstalls the packages of the old environment
// and the daemon starts again.
func TestPythonVersionChange(t *testing.T) {
	b := newBed(t, nil)
	d := b.start()
	b.waitState("running", 90*time.Second)
	ctx := context.Background()
	// The agent installed a package into the persistent environment.
	if _, err := proc.RunChecked(ctx, proc.RunSpec{
		Path:    b.env.Paths.Uv,
		Args:    []string{"pip", "install", "six==1.17.0"},
		Env:     append(b.env.Paths.ToolEnv(), "VIRTUAL_ENV="+b.env.Paths.PythonVenv, "UV_LINK_MODE=copy"),
		Dir:     b.env.Paths.BearbotHome,
		Timeout: 10 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	d.Stop(t)

	other := otherPython(t, b.env)
	if err := os.Remove(b.env.Paths.Python); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, b.env.Paths.Python); err != nil {
		t.Fatal(err)
	}
	d = b.start()
	select {
	case <-d.Process().Done():
	case <-time.After(2 * time.Minute):
		t.Fatal("bearbotd did not exit after the Python version changed")
	}
	stderr := strings.Join(d.Stderr(), "\n")
	if exit := d.Process().Exit(); exit.Code == nil || *exit.Code != 1 || !strings.Contains(stderr, "docker run --rm <挂载参数> bearbot python rebuild") {
		t.Fatalf("exit %s, stderr:\n%s", exit, stderr)
	}

	out, errOut, code := b.cli("python", "rebuild")
	if code != 0 || !strings.Contains(out, "six==1.17.0") || !strings.Contains(out, "Python 虚拟环境重建完成") {
		t.Fatalf("python rebuild: %d\n%s\n%s", code, out, errOut)
	}
	backups, _ := filepath.Glob(filepath.Join(b.env.Paths.ProfileDir, "python.*.old"))
	if len(backups) != 1 {
		t.Fatalf("old environments %v", backups)
	}
	b.start()
	b.waitState("running", 90*time.Second)
	res, err := proc.RunChecked(ctx, proc.RunSpec{Path: b.env.Paths.PythonBin, Args: []string{"-c", "import six, sys; print(six.__version__, sys.version_info[:2])"}, Env: b.env.Paths.ToolEnv(), Dir: "/", Timeout: time.Minute})
	if err != nil || !strings.HasPrefix(string(res.Stdout), "1.17.0") {
		t.Fatalf("six in the new environment: %s %v", res.Stdout, err)
	}
}

// ptyReader collects the output of a pseudo-terminal.
type ptyReader struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[PX^_][^\x1b]*\x1b\\`)

func (r *ptyReader) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return ansi.ReplaceAllString(r.buf.String(), "")
}

func (r *ptyReader) wait(t *testing.T, timeout time.Duration, want string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !strings.Contains(r.text(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("terminal does not show %q within %v:\n%s", want, timeout, r.text())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestLogin runs `bearbotd login` in a pseudo-terminal: interactive pi -ne with the agent profile, /login opens the
// authentication dialog, and the interactive start writes into the profile's settings.json.
func TestLogin(t *testing.T) {
	b := newBed(t, nil)
	if err := os.MkdirAll(b.env.Paths.AgentDir, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.env.Paths.AgentSettingsFile, []byte(`{"quietStartup": true, "defaultProjectTrust": "never"}`), 0o664); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(testenv.DaemonBinary(t), "--root", b.env.Root, "--rootless", "login")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	defer tty.Close()
	r := &ptyReader{}
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, err := tty.Read(buf)
			r.mu.Lock()
			r.buf.Write(buf[:n])
			r.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	// Each key goes in after the screen shows the state it is meant for.
	r.wait(t, 30*time.Second, "Use /login")
	if _, err := tty.WriteString("/login"); err != nil {
		t.Fatal(err)
	}
	r.wait(t, 10*time.Second, "Configure provider authentication")
	if _, err := tty.WriteString("\r"); err != nil {
		t.Fatal(err)
	}
	r.wait(t, 10*time.Second, "Select authentication method")
	if _, err := tty.WriteString("\r"); err != nil {
		t.Fatal(err)
	}
	r.wait(t, 20*time.Second, "Select provider to configure")
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	// Close both dialogs, then quit pi on the empty editor.
	keys := []string{"\x1b", "\x1b", "\x1b", "\x04", "\x03", "\x03", "\x04"}
	var waitErr error
	ended := false
	for _, key := range keys {
		tty.WriteString(key)
		select {
		case waitErr = <-done:
			ended = true
		case <-time.After(time.Second):
		}
		if ended {
			break
		}
	}
	if !ended {
		select {
		case waitErr = <-done:
		case <-time.After(20 * time.Second):
			cmd.Process.Kill()
			t.Fatalf("bearbotd login did not end:\n%s", r.text())
		}
	}
	if waitErr != nil {
		t.Fatalf("bearbotd login: %v\n%s", waitErr, r.text())
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(b.readFile(b.env.Paths.AgentSettingsFile)), &settings); err != nil {
		t.Fatal(err)
	}
	if settings["lastChangelogVersion"] == nil || settings["defaultProjectTrust"] != "never" {
		t.Fatalf("settings.json after login = %v", settings)
	}
	if !fileExists(filepath.Join(b.env.Paths.AgentDir, "auth.json")) {
		t.Fatal("pi did not use the agent profile")
	}
}
