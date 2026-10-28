package proc

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var testEnv = []string{"PATH=/usr/bin:/bin"}

// startScript starts sh -c script and reads the PIDs the script prints (one per line) until it printed want of
// them.
func startScript(t *testing.T, script string, want int) (*Process, []int) {
	t.Helper()
	p, err := Start(Spec{Path: "/bin/sh", Args: []string{"-c", script}, Env: testEnv, Dir: "/", StdinPipe: true, Stderr: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !p.Exited() {
			p.KillTree()
		}
	})
	sc := bufio.NewScanner(p.Stdout)
	var pids []int
	for len(pids) < want && sc.Scan() {
		pid, err := strconv.Atoi(strings.TrimSpace(sc.Text()))
		if err != nil {
			t.Fatalf("unexpected output %q", sc.Text())
		}
		pids = append(pids, pid)
	}
	if len(pids) < want {
		t.Fatalf("script printed %d pids, want %d (%v)", len(pids), want, sc.Err())
	}
	// Keep draining stdout so that the script never blocks on it.
	go func() {
		for sc.Scan() {
		}
	}()
	return p, pids
}

func entriesOf(t *testing.T, pids []int) []Entry {
	t.Helper()
	var out []Entry
	for _, pid := range pids {
		e, err := ReadEntry(pid)
		if err != nil {
			t.Fatalf("read %d: %v", pid, err)
		}
		out = append(out, e)
	}
	return out
}

func requireDead(t *testing.T, entries []Entry) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for _, e := range entries {
		for {
			alive, err := e.Alive()
			if err != nil {
				t.Fatal(err)
			}
			if !alive {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("process %d is still alive", e.PID)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func requireInTree(t *testing.T, root int, pids []int) {
	t.Helper()
	tree, err := CollectTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) == 0 || tree[0].PID != root {
		t.Fatalf("tree of %d = %+v", root, tree)
	}
	for _, pid := range pids {
		found := false
		for _, e := range tree {
			if e.PID == pid {
				found = true
			}
		}
		if !found {
			t.Fatalf("pid %d not in tree %+v", pid, tree)
		}
	}
}

// The child prints the PIDs of a background sleep, of a sleep in its own session started by a nested shell (a
// grandchild of the root), and of that nested shell.
const treeScript = `
sleep 300 &
echo $!
sh -c 'setsid sleep 300 & echo $!; wait' &
nested=$!
`

func TestStopAfterStdinClose(t *testing.T) {
	// The root exits as soon as stdin is closed and leaves its descendants behind.
	p, pids := startScript(t, treeScript+"echo $nested\nread line\nexit 0\n", 3)
	requireInTree(t, p.PID(), pids)
	entries := entriesOf(t, pids)
	start := time.Now()
	if err := p.Stop(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("Stop waited %v although the root exited at once", time.Since(start))
	}
	if !p.Exit().Success() {
		t.Fatalf("exit = %v", p.Exit())
	}
	requireDead(t, entries)
}

func TestStopSigtermRoot(t *testing.T) {
	// The root ignores stdin and is ended by SIGTERM.
	p, pids := startScript(t, treeScript+"echo $nested\nwhile :; do sleep 0.05; done\n", 3)
	entries := entriesOf(t, pids)
	start := time.Now()
	if err := p.Stop(500 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed < 500*time.Millisecond {
		t.Fatalf("Stop returned after %v, before the grace period", elapsed)
	}
	exit := p.Exit()
	if exit.Signal == nil || *exit.Signal != "SIGTERM" {
		t.Fatalf("exit = %v, want SIGTERM", exit)
	}
	requireDead(t, entries)
}

func TestStopSigkillSurvivors(t *testing.T) {
	// Root and nested shell ignore SIGTERM; everything must end with SIGKILL after two grace periods.
	script := `
trap '' TERM
sleep 300 &
echo $!
sh -c 'trap "" TERM; setsid sh -c "trap \"\" TERM; echo \$\$; while :; do sleep 0.05; done" & wait' &
echo $!
while :; do sleep 0.05; done
`
	p, pids := startScript(t, script, 3)
	requireInTree(t, p.PID(), pids)
	entries := entriesOf(t, pids)
	start := time.Now()
	if err := p.Stop(300 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 600*time.Millisecond {
		t.Fatalf("Stop returned after %v, before two grace periods", elapsed)
	}
	exit := p.Exit()
	if exit.Signal == nil || *exit.Signal != "SIGKILL" {
		t.Fatalf("exit = %v, want SIGKILL", exit)
	}
	requireDead(t, entries)
}

func TestKillTree(t *testing.T) {
	p, pids := startScript(t, treeScript+"echo $nested\nwhile :; do sleep 0.05; done\n", 3)
	entries := entriesOf(t, pids)
	if err := p.KillTree(); err != nil {
		t.Fatal(err)
	}
	if s := p.Exit().Signal; s == nil || *s != "SIGKILL" {
		t.Fatalf("exit = %v", p.Exit())
	}
	requireDead(t, entries)
}

func TestTerminate(t *testing.T) {
	p, pids := startScript(t, treeScript+"echo $nested\nwhile :; do sleep 0.05; done\n", 3)
	entries := entriesOf(t, pids)
	if err := p.Terminate(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	if s := p.Exit().Signal; s == nil || *s != "SIGTERM" {
		t.Fatalf("exit = %v", p.Exit())
	}
	requireDead(t, entries)
}

func TestSignalSkipsReusedPID(t *testing.T) {
	p, err := Start(Spec{Path: "/bin/sleep", Args: []string{"300"}, Env: testEnv, Dir: "/"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := ReadEntry(p.PID())
	if err != nil {
		t.Fatal(err)
	}
	stale := e
	stale.StartTime++
	if err := stale.Signal(9); err != nil {
		t.Fatal(err)
	}
	if alive, _ := e.Alive(); !alive {
		t.Fatal("a stale entry signalled the running process")
	}
	if err := p.KillTree(); err != nil {
		t.Fatal(err)
	}
	if alive, _ := e.Alive(); alive {
		t.Fatal("process still alive")
	}
}

func TestOutputWritersAndExitCode(t *testing.T) {
	var ring = NewRing(10)
	p, err := Start(Spec{
		Path:           "/bin/sh",
		Args:           []string{"-c", "echo out1; echo err1 >&2; printf 'last'; exit 7"},
		Env:            testEnv,
		Dir:            "/",
		Stdout:         ring,
		StderrToStdout: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	<-p.Done()
	<-p.OutputDone()
	if code := p.Exit().Code; code == nil || *code != 7 {
		t.Fatalf("exit = %v", p.Exit())
	}
	got := ring.Lines(0)
	if strings.Join(got, "|") != "out1|err1|last" {
		t.Fatalf("lines = %q", got)
	}
	if p.Exit().String() != "退出码 7" {
		t.Fatalf("String = %q", p.Exit().String())
	}
}

func TestStartFailure(t *testing.T) {
	_, err := Start(Spec{Path: "/nonexistent/binary", Env: testEnv, Dir: "/"})
	if err == nil || !strings.Contains(err.Error(), "/nonexistent/binary") {
		t.Fatalf("err = %v", err)
	}
}

func TestStdinIsDevNullWithoutPipe(t *testing.T) {
	ring := NewRing(4)
	p, err := Start(Spec{Path: "/bin/sh", Args: []string{"-c", "readlink /proc/self/fd/0; cat; echo done"}, Env: testEnv, Dir: "/", Stdout: ring, Stderr: ring})
	if err != nil {
		t.Fatal(err)
	}
	<-p.Done()
	<-p.OutputDone()
	if got := ring.Lines(0); len(got) != 2 || got[0] != "/dev/null" || got[1] != "done" {
		t.Fatalf("lines = %q", got)
	}
}

func TestNewSession(t *testing.T) {
	ring := NewRing(4)
	p, err := Start(Spec{Path: "/bin/sh", Args: []string{"-c", "cat /proc/self/stat"}, Env: testEnv, Dir: "/", Stdout: ring, Stderr: ring})
	if err != nil {
		t.Fatal(err)
	}
	<-p.Done()
	<-p.OutputDone()
	lines := ring.Lines(0)
	if len(lines) != 1 {
		t.Fatalf("lines = %q", lines)
	}
	stat := lines[0]
	fields := strings.Fields(stat[strings.LastIndexByte(stat, ')')+2:])
	// fields[3] is the session id (field 6), fields[4] tty_nr (field 7).
	if fields[3] != strconv.Itoa(p.PID()) {
		t.Fatalf("session = %s, want %d", fields[3], p.PID())
	}
	if fields[4] != "0" {
		t.Fatalf("tty_nr = %s, want 0", fields[4])
	}
}

func TestRun(t *testing.T) {
	ctx := context.Background()
	// stdout and stderr are separate pipes: the pause lets the daemon read "a" before "b" is written.
	res, err := Run(ctx, RunSpec{Path: "/bin/sh", Args: []string{"-c", "echo a; sleep 0.5; echo b >&2; exit 3"}, Env: testEnv, Dir: "/", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != "a\n" || string(res.Stderr) != "b\n" || string(res.Output) != "a\nb\n" {
		t.Fatalf("output = %q %q %q", res.Stdout, res.Stderr, res.Output)
	}
	if res.OK() || res.Exit.Code == nil || *res.Exit.Code != 3 {
		t.Fatalf("exit = %v", res.Exit)
	}
	if msg := res.Err().Error(); !strings.Contains(msg, "退出码 3") || !strings.Contains(msg, "a\nb") || !strings.Contains(msg, `"echo a; sleep 0.5; echo b >&2; exit 3"`) {
		t.Fatalf("Err = %q", msg)
	}
	if _, err := RunChecked(ctx, RunSpec{Path: "/bin/true", Env: testEnv, Dir: "/", Timeout: 10 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if _, err := RunChecked(ctx, RunSpec{Path: "/bin/false", Env: testEnv, Dir: "/", Timeout: 10 * time.Second}); err == nil {
		t.Fatal("RunChecked(false) succeeded")
	}
	if _, err := Run(ctx, RunSpec{Path: "/bin/true", Env: testEnv, Dir: "/"}); err == nil {
		t.Fatal("Run without timeout succeeded")
	}
}

// TestRunStdinAndStdout: Stdin reaches the command and is closed after the last byte, Stdout receives stdout instead
// of the result, and a command that succeeds without reading its stdin is an error.
func TestRunStdinAndStdout(t *testing.T) {
	ctx := context.Background()
	// More than a pipe buffer, so the copy has to wait for the reader.
	input := strings.Repeat("0123456789abcdef", 64*1024)
	res, err := RunChecked(ctx, RunSpec{Path: "/bin/sh", Args: []string{"-c", "wc -c; echo done >&2"}, Env: testEnv, Dir: "/", Stdin: strings.NewReader(input), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(res.Stdout)) != strconv.Itoa(len(input)) || string(res.Stderr) != "done\n" {
		t.Fatalf("output = %q %q", res.Stdout, res.Stderr)
	}
	out, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	res, err = RunChecked(ctx, RunSpec{Path: "/bin/sh", Args: []string{"-c", "cat; echo err >&2"}, Env: testEnv, Dir: "/", Stdin: strings.NewReader(input), Stdout: out, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != 0 || string(res.Output) != "err\n" {
		t.Fatalf("result with Stdout = %q %q", res.Stdout, res.Output)
	}
	data, err := os.ReadFile(out.Name())
	if err != nil || string(data) != input {
		t.Fatalf("Stdout file has %d bytes, want %d (%v)", len(data), len(input), err)
	}
	if _, err := RunChecked(ctx, RunSpec{Path: "/bin/true", Env: testEnv, Dir: "/", Stdin: strings.NewReader(input), Timeout: 10 * time.Second}); err == nil || !strings.Contains(err.Error(), "没有读完标准输入") {
		t.Fatalf("unread stdin: %v", err)
	}
	// A failing command reports its own exit, not the unread input.
	if _, err := RunChecked(ctx, RunSpec{Path: "/bin/false", Env: testEnv, Dir: "/", Stdin: strings.NewReader(input), Timeout: 10 * time.Second}); err == nil || !strings.Contains(err.Error(), "退出码 1") {
		t.Fatalf("failing command with stdin: %v", err)
	}
}

func TestRunEnvIsExplicit(t *testing.T) {
	t.Setenv("BEARBOT_PROC_TEST_LEAK", "1")
	res, err := RunChecked(context.Background(), RunSpec{Path: "/usr/bin/env", Env: []string{"ONLY=1"}, Dir: "/", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(res.Stdout)) != "ONLY=1" {
		t.Fatalf("env = %q", res.Stdout)
	}
}

func TestRunTimeoutKillsTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	res, err := Run(context.Background(), RunSpec{
		Path:    "/bin/sh",
		Args:    []string{"-c", "setsid sleep 300 & echo $! > " + pidFile + "; echo started; wait"},
		Env:     testEnv,
		Dir:     "/",
		Timeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.OK() {
		t.Fatalf("result = %+v", res)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("Run took %v", time.Since(start))
	}
	if !strings.Contains(res.Err().Error(), "没有结束") {
		t.Fatalf("Err = %v", res.Err())
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	e, err := ReadEntry(pid)
	if err == nil && e.State != 'Z' {
		t.Fatalf("grandchild %d survived the timeout: %+v", pid, e)
	}
}

func TestRunContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	res, err := Run(ctx, RunSpec{Path: "/bin/sleep", Args: []string{"300"}, Env: testEnv, Dir: "/", Timeout: time.Minute})
	if err != context.Canceled {
		t.Fatalf("err = %v", err)
	}
	if res == nil || res.Exit.Signal == nil {
		t.Fatalf("res = %+v", res)
	}
}

func TestRunLingeringPipeHolder(t *testing.T) {
	// A background process keeps stdout open after the command exited; Run must still return.
	start := time.Now()
	res, err := Run(context.Background(), RunSpec{Path: "/bin/sh", Args: []string{"-c", "sleep 5 & echo hi"}, Env: testEnv, Dir: "/", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() || string(res.Stdout) != "hi\n" {
		t.Fatalf("res = %+v", res)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("Run waited %v for the lingering process", time.Since(start))
	}
}
