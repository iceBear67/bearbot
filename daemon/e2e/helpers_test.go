// Package e2e runs bearbotd end to end in a rootless test root (contracts 10): the real gateway, the real pi with
// bearbot-im, real git, uv and node. Scenarios without a model service need no credentials.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"bearbot/daemon/internal/proc"
	"bearbot/daemon/internal/state"
	"bearbot/daemon/internal/testenv"
)

func TestMain(m *testing.M) { os.Exit(testenv.RunWithDaemon(m)) }

// bed is one daemon under test.
type bed struct {
	t   *testing.T
	env *testenv.Env
	d   *testenv.Daemon
}

// requireComponents fails when another component of the repository is missing.
func requireComponents(t *testing.T, e *testenv.Env) {
	t.Helper()
	e.RequireGateway(t)
	for _, rel := range []string{"extensions/bearbot-im/index.ts", "extensions/bearbot-im/package.json", "extensions/bearbot-recovery/index.ts", "sdk/index.ts", "recovery-profile/settings.json"} {
		e.RequireRepoFile(t, rel)
	}
}

// fastConfig is the test configuration: short timeouts and probation.
func fastConfig() testenv.Config {
	c := testenv.DefaultConfig()
	c.TermGrace = "2s"
	c.ShutdownGrace = "5s"
	c.StartTimeout = "60s"
	c.ReadyTimeout = "40s"
	c.HeartbeatInterval = "2s"
	c.RPCTimeout = "5s"
	c.Probation = "4s"
	c.DrainTimeout = "30s"
	c.RecoveryTimeout = "1m"
	c.HookTimeout = "10s"
	return c
}

func newBed(t *testing.T, configure func(*testenv.Env, *testenv.Config)) *bed {
	t.Helper()
	e := testenv.New(t)
	requireComponents(t, e)
	cfg := fastConfig()
	if configure != nil {
		configure(e, &cfg)
	}
	e.WriteConfig(t, cfg)
	b := &bed{t: t, env: e}
	t.Cleanup(func() {
		// The daemons were stopped already (their cleanups were registered later). With GOFLAGS=-race the binary
		// reports data races on stderr.
		if b.d != nil && strings.Contains(strings.Join(b.d.Stderr(), "\n"), "WARNING: DATA RACE") {
			t.Error("bearbotd reported a data race")
		}
		if t.Failed() {
			b.dumpLogs()
		}
	})
	return b
}

func (b *bed) start(extraEnv ...string) *testenv.Daemon {
	b.t.Helper()
	b.d = b.env.StartDaemon(b.t, extraEnv...)
	return b.d
}

// dumpLogs writes the daemon log and the end of the other logs into the test output.
func (b *bed) dumpLogs() {
	p := b.env.Paths
	for _, f := range []struct {
		path string
		n    int
	}{{p.DaemonLog, 400}, {p.GatewayLog, 150}, {p.AgentStderrLog, 150}, {p.AgentExitsLog, 20}} {
		lines, err := proc.TailFile(f.path, f.n)
		if err != nil {
			b.t.Logf("read %s: %v", f.path, err)
			continue
		}
		b.t.Logf("==== %s (last %d lines)\n%s", f.path, f.n, strings.Join(lines, "\n"))
	}
	if b.d != nil {
		b.t.Logf("==== bearbotd stdout\n%s\n==== bearbotd stderr\n%s", strings.Join(b.d.Output(), "\n"), strings.Join(b.d.Stderr(), "\n"))
	}
}

// state reads state.json.
func (b *bed) state() *state.State {
	b.t.Helper()
	st, exists, err := state.Load(b.env.Paths.StateFile)
	if err != nil {
		b.t.Fatal(err)
	}
	if !exists {
		b.t.Fatal("state.json does not exist")
	}
	return st
}

// statusInfo is the part of StatusInfo the tests look at.
type statusInfo struct {
	State           string  `json:"state"`
	Reason          *string `json:"reason"`
	PID             *int    `json:"pid"`
	Restarts        int     `json:"restarts"`
	CrashesInWindow int     `json:"crashesInWindow"`
	ProbationUntil  *string `json:"probationUntil"`
	SessionID       *string `json:"sessionId"`
	RunningSnapshot *string `json:"runningSnapshot"`
	LastGood        *string `json:"lastGood"`
	PendingApply    *struct {
		Snapshot string `json:"snapshot"`
	} `json:"pendingApply"`
	Incident *struct {
		ID       string `json:"id"`
		Trigger  string `json:"trigger"`
		Attempts int    `json:"attempts"`
	} `json:"incident"`
	Maintenance *struct {
		Reason     string  `json:"reason"`
		IncidentID *string `json:"incidentId"`
	} `json:"maintenance"`
	Rollback *struct {
		ID int `json:"id"`
	} `json:"rollback"`
	Console bool `json:"console"`
}

// agentStatus calls GET /v1/status; ok is false when the state does not accept it.
func (b *bed) agentStatus() (statusInfo, int) {
	b.t.Helper()
	var info statusInfo
	code := b.call(http.MethodGet, "/v1/status", nil, &info)
	return info, code
}

// call sends a request to agent.sock and decodes the answer into out (also for error answers).
func (b *bed) call(method, path string, body, out any) int {
	b.t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			b.t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, "http://bearbotd"+path, reader)
	if err != nil {
		b.t.Fatal(err)
	}
	resp, err := testenv.UnixClient(b.env.Paths.AgentSock).Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		b.t.Fatal(err)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			b.t.Fatalf("%s %s: decode %q: %v", method, path, data, err)
		}
	}
	return resp.StatusCode
}

// waitState waits until GET /v1/status or the daemon log shows the state. States in which /v1/status is not
// accepted are recognized from the 409 answer.
func (b *bed) waitState(want string, timeout time.Duration) {
	b.t.Helper()
	deadline := time.Now().Add(timeout)
	testenv.WaitForSocket(b.t, b.env.Paths.AgentSock, timeout)
	last := ""
	for {
		var body map[string]any
		code := b.call(http.MethodGet, "/v1/status", nil, &body)
		switch code {
		case http.StatusOK:
			last, _ = body["state"].(string)
		case http.StatusConflict:
			msg, _ := body["error"].(string)
			last = strings.TrimSuffix(strings.TrimPrefix(msg, "当前状态为 "), "，不接受这个请求")
		default:
			b.t.Fatalf("GET /v1/status: %d %v", code, body)
		}
		if last == want {
			return
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("state %q not reached within %v; last state %q", want, timeout, last)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitFor polls cond until it returns true.
func (b *bed) waitFor(what string, timeout time.Duration, cond func() bool) {
	b.t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			b.t.Fatalf("%s: not reached within %v", what, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// cli runs a management subcommand of the built binary and returns stdout, stderr and the exit code.
func (b *bed) cli(args ...string) (string, string, int) {
	b.t.Helper()
	out, errOut, code, err := runCLI(testenv.DaemonBinary(b.t), b.env.Root, args...)
	if err != nil {
		b.t.Fatalf("bearbotd %v: %v", args, err)
	}
	return out, errOut, code
}

// cliResult is the outcome of a management subcommand run in a goroutine.
type cliResult struct {
	out, errOut string
	code        int
	err         error
}

// cliAsync runs a management subcommand in a goroutine (without the testing.T) and delivers its outcome.
func (b *bed) cliAsync(args ...string) <-chan cliResult {
	b.t.Helper()
	bin := testenv.DaemonBinary(b.t)
	ch := make(chan cliResult, 1)
	go func() {
		out, errOut, code, err := runCLI(bin, b.env.Root, args...)
		ch <- cliResult{out, errOut, code, err}
	}()
	return ch
}

// runCLI runs `bearbotd --root root --rootless <args>`; err is set only when the command could not run.
func runCLI(bin, root string, args ...string) (string, string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, append([]string{"--root", root, "--rootless"}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		return stdout.String(), stderr.String(), exitErr.ExitCode(), nil
	case err != nil:
		return "", "", 0, err
	}
	return stdout.String(), stderr.String(), 0, nil
}

// answer is the outcome of an agent.sock request sent from a goroutine.
type answer struct {
	code int
	data []byte
	err  error
}

// callAsync sends a request to agent.sock in a goroutine (without the testing.T) and delivers the answer.
func (b *bed) callAsync(method, path string, body any) <-chan answer {
	socket := b.env.Paths.AgentSock
	ch := make(chan answer, 1)
	go func() {
		code, data, err := agentRequest(socket, method, path, body)
		ch <- answer{code, data, err}
	}()
	return ch
}

func agentRequest(socket, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, "http://bearbotd"+path, reader)
	if err != nil {
		return 0, nil, err
	}
	resp, err := testenv.UnixClient(socket).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

// errorText returns the error text of an answer with the given status; the test fails for any other answer.
func (b *bed) errorText(what string, a answer, status int) string {
	b.t.Helper()
	if a.err != nil {
		b.t.Fatalf("%s: %v", what, a.err)
	}
	var body map[string]any
	if err := json.Unmarshal(a.data, &body); err != nil {
		b.t.Fatalf("%s: decode %q: %v", what, a.data, err)
	}
	msg, ok := body["error"].(string)
	if a.code != status || !ok {
		b.t.Fatalf("%s: %d %s, want %d with an error", what, a.code, a.data, status)
	}
	return msg
}

// checkDirs returns the names of the check directories.
func (b *bed) checkDirs() map[string]bool {
	b.t.Helper()
	entries, err := os.ReadDir(b.env.Paths.ChecksDir)
	if err != nil {
		b.t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	return names
}

// waitCheckRunning waits until a check directory that is not in before has its tsconfig.json: the check runs then
// (Checker.Typecheck writes it under the change lock), and its trial keeps it running for at least 3 seconds.
func (b *bed) waitCheckRunning(before map[string]bool) {
	b.t.Helper()
	b.waitFor("a check started", 30*time.Second, func() bool {
		for name := range b.checkDirs() {
			if !before[name] && fileExists(filepath.Join(b.env.Paths.ChecksDir, name, "tsconfig.json")) {
				return true
			}
		}
		return false
	})
}

// countLines counts the lines of the log at path that contain s.
func (b *bed) countLines(path, s string) int {
	b.t.Helper()
	n := 0
	for _, l := range b.logLines(path) {
		if strings.Contains(l, s) {
			n++
		}
	}
	return n
}

// lastLine returns the last line of the log at path that contains s; the test fails when there is none.
func (b *bed) lastLine(path, s string) string {
	b.t.Helper()
	lines := b.logLines(path)
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], s) {
			return lines[i]
		}
	}
	b.t.Fatalf("%s has no line with %q", path, s)
	return ""
}

// waitRunningWith waits until GET /v1/status shows RUNNING with a process other than notPID and, when snapshot is
// not empty, that runningSnapshot.
func (b *bed) waitRunningWith(notPID int, snapshot string, timeout time.Duration) statusInfo {
	b.t.Helper()
	var info statusInfo
	b.waitFor("running with a new process", timeout, func() bool {
		var code int
		info, code = b.agentStatus()
		return code == 200 && info.State == "running" && info.PID != nil && *info.PID != notPID &&
			(snapshot == "" || info.RunningSnapshot != nil && *info.RunningSnapshot == snapshot)
	})
	return info
}

// apply posts /v1/apply and requires that it was accepted with the snapshot want.
func (b *bed) apply(summary, want string) {
	b.t.Helper()
	var applied map[string]any
	if code := b.call("POST", "/v1/apply", map[string]any{"summary": summary, "origin": map[string]any{"thread": "console:main", "requester": "console:operator"}}, &applied); code != 200 || applied["accepted"] != true || applied["snapshot"] != want {
		b.t.Fatalf("apply %q: %d %v", summary, code, applied)
	}
}

// procStat returns the session id (field 6) and tty_nr (field 7) of /proc/<pid>/stat.
func procStat(t *testing.T, pid int) (session, tty int) {
	t.Helper()
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	end := bytes.LastIndexByte(data, ')')
	fields := strings.Fields(string(data[end+2:]))
	session, err = strconv.Atoi(fields[3])
	if err != nil {
		t.Fatal(err)
	}
	tty, err = strconv.Atoi(fields[4])
	if err != nil {
		t.Fatal(err)
	}
	return session, tty
}

// gone reports whether the process pid no longer exists or is a zombie.
func gone(pid int) bool {
	e, err := proc.ReadEntry(pid)
	return err != nil || e.State == 'Z'
}

// ext writes a workspace extension.
func (b *bed) ext(name, content string) string {
	b.t.Helper()
	return b.env.WriteFile(b.t, "/bearbot/workspace/.pi/extensions/"+name, content)
}

// console sends a console line and waits for an output line written after it that contains want.
func (b *bed) console(line, want string, timeout time.Duration) string {
	b.t.Helper()
	skip := len(b.d.Output())
	b.d.Send(b.t, line)
	return b.outputAfter(skip, want, timeout)
}

// outputAfter waits for a line containing want that was written after the first `skip` lines.
func (b *bed) outputAfter(skip int, want string, timeout time.Duration) string {
	b.t.Helper()
	n := 0
	return b.d.WaitOutput(b.t, timeout, func(line string) bool {
		n++
		return n > skip && strings.Contains(line, want)
	})
}

func (b *bed) readFile(path string) string {
	b.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		b.t.Fatal(err)
	}
	return string(data)
}

func (b *bed) logLines(path string) []string {
	b.t.Helper()
	lines, err := proc.TailFile(path, 0)
	if err != nil {
		b.t.Fatal(err)
	}
	return lines
}

// gitTags lists the tags of the snapshot repository.
func (b *bed) gitTags() []string {
	b.t.Helper()
	out, err := exec.Command("git", "--git-dir="+b.env.Paths.SnapshotsGit, "tag", "--list").Output()
	if err != nil {
		b.t.Fatal(err)
	}
	return strings.Fields(string(out))
}

// gitFiles lists the files of a snapshot.
func (b *bed) gitFiles(ref string) []string {
	b.t.Helper()
	out, err := exec.Command("git", "--git-dir="+b.env.Paths.SnapshotsGit, "ls-tree", "-r", "--name-only", ref).Output()
	if err != nil {
		b.t.Fatalf("ls-tree %s: %v", ref, err)
	}
	return strings.Fields(string(out))
}

func (b *bed) gitRev(ref string) string {
	b.t.Helper()
	out, err := exec.Command("git", "--git-dir="+b.env.Paths.SnapshotsGit, "rev-parse", "--verify", ref+"^{commit}").Output()
	if err != nil {
		b.t.Fatalf("rev-parse %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}

func contains(list []string, s string) bool { return slices.Contains(list, s) }

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
