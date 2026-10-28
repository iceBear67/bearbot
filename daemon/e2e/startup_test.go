package e2e

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"bearbot/daemon/internal/proc"
	"bearbot/daemon/internal/testenv"
)

// TestStartupConsoleAndShutdown starts bearbotd with the console on pipes: the console connects, the agent becomes
// RUNNING, /bb status and /bb here answer on the console, the CLI works, and SIGTERM stops everything.
func TestStartupConsoleAndShutdown(t *testing.T) {
	b := newBed(t, nil)
	d := b.start()
	d.WaitOutputContains(t, 90*time.Second, "控制台已连接")
	b.waitState("running", 90*time.Second)

	info, code := b.agentStatus()
	if code != 200 || info.State != "running" || info.PID == nil || !info.Console || info.SessionID == nil || info.Restarts != 0 {
		t.Fatalf("status = %d %+v", code, info)
	}
	if info.RunningSnapshot == nil || *info.RunningSnapshot != "init" {
		t.Fatalf("runningSnapshot = %v", info.RunningSnapshot)
	}
	st := b.state()
	if st.SessionID != *info.SessionID || !regexp.MustCompile(`^bearbot-[0-9]{14}-[0-9a-f]{8}$`).MatchString(st.SessionID) {
		t.Fatalf("session id in state.json %q, status %q", st.SessionID, *info.SessionID)
	}
	if st.SessionFile == "" || !strings.HasPrefix(st.SessionFile, b.env.Paths.AgentSessionsDir+"/") {
		t.Fatalf("session file %q is not in %s", st.SessionFile, b.env.Paths.AgentSessionsDir)
	}
	// Templates of DESIGN 4.4 in the empty workspace, the default settings.
	if !strings.HasPrefix(b.readFile(b.env.Paths.AgentsMD), "# 记忆") || !strings.HasPrefix(b.readFile(b.env.Paths.ClassifierMD), "# classifier 补充规则") {
		t.Fatal("workspace templates missing")
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(b.readFile(b.env.Paths.AgentSettingsFile)), &settings); err != nil || settings["defaultProjectTrust"] != "never" {
		t.Fatalf("settings.json = %v %v", settings, err)
	}
	// gateway.json: mode 0600, console entry, no platforms.
	gwInfo, err := os.Stat(b.env.Paths.GatewayConfig)
	if err != nil || gwInfo.Mode().Perm() != 0o600 {
		t.Fatalf("gateway.json: %v %v", gwInfo, err)
	}
	var gwFile map[string]any
	if err := json.Unmarshal([]byte(b.readFile(b.env.Paths.GatewayConfig)), &gwFile); err != nil {
		t.Fatal(err)
	}
	if gwFile["console"] == nil || len(gwFile["platforms"].(map[string]any)) != 0 || gwFile["ctlSocket"] != b.env.Paths.GatewayCtlSock {
		t.Fatalf("gateway.json = %v", gwFile)
	}
	for _, s := range []struct {
		path string
		mode os.FileMode
	}{{b.env.Paths.AgentSock, 0o660}, {b.env.Paths.AdminSock, 0o600}} {
		fi, err := os.Stat(s.path)
		if err != nil || fi.Mode().Perm() != s.mode || fi.Mode()&os.ModeSocket == 0 {
			t.Fatalf("%s: %v %v", s.path, fi, err)
		}
	}

	// /bb commands from the console (the console user is the built-in administrator). The gateway answers status
	// with bearbotd's status, its own platforms and bindings; snapshot list and compact are answered by bearbotd.
	lines := len(d.Output())
	b.console("/bb status", "agent：running", 30*time.Second)
	for _, want := range []string{
		"进程：pid " + strconv.Itoa(*info.PID) + "，",
		"会话：" + st.SessionID,
		"快照：当前 init，",
		"  console：line",
		"  console:main（控制台）：trigger=all，reply=final，interrupt=queue",
	} {
		b.outputAfter(lines, want, 10*time.Second)
	}
	b.console("/bb here", "console:main", 30*time.Second)
	b.console("/bb snapshot list", "rollback 不指定快照时的默认目标：没有", 30*time.Second)
	// The option words stay part of the compact instructions; the empty session has nothing to compact.
	b.console("/bb compact 保留 --reply 记忆", "会话内容太少，不需要压缩（Nothing to compact）。", 30*time.Second)

	// The CLI over admin.sock.
	out, stderr, code := b.cli("status")
	if code != 0 || !strings.Contains(out, "gateway：已连接") || !strings.Contains(out, "agent：running") || !strings.Contains(out, "console：") || !strings.Contains(out, "控制台：已启用") {
		t.Fatalf("status: %d\n%s\n%s", code, out, stderr)
	}
	gatewayPID := regexp.MustCompile(`gateway：已连接（PID ([0-9]+)`).FindStringSubmatch(out)
	if gatewayPID == nil {
		t.Fatalf("no gateway pid in %s", out)
	}
	out, stderr, code = b.cli("admin", "token", "--ttl", "5m")
	if code != 0 || !regexp.MustCompile(`(?mi)^bearbot-[a-z2-7]{26}$`).MatchString(out) || !strings.Contains(out, "有效期 5m0s") {
		t.Fatalf("admin token: %d\n%s\n%s", code, out, stderr)
	}
	out, stderr, code = b.cli("admin", "list")
	if code != 0 || !strings.Contains(out, "console:operator") || !strings.Contains(out, "内置") {
		t.Fatalf("admin list: %d\n%s\n%s", code, out, stderr)
	}
	out, stderr, code = b.cli("admin", "revoke", "console:operator")
	if code != 1 || !strings.Contains(stderr, "内置管理员不能撤销") {
		t.Fatalf("admin revoke console:operator: %d\n%s\n%s", code, out, stderr)
	}
	out, stderr, code = b.cli("logs", "daemon", "-n", "5")
	if code != 0 || len(strings.Split(strings.TrimSpace(out), "\n")) != 5 {
		t.Fatalf("logs daemon: %d\n%s\n%s", code, out, stderr)
	}
	out, _, code = b.cli("snapshot", "list")
	if code != 0 || !strings.Contains(out, "init") || !strings.Contains(out, "rollback 不指定快照时的默认目标：没有") {
		t.Fatalf("snapshot list: %d\n%s", code, out)
	}
	// A refused repair prints no progress line.
	out, stderr, code = b.cli("repair")
	if code != 1 || out != "" || !strings.Contains(stderr, "当前状态为 running，不能执行 repair") {
		t.Fatalf("repair in running: %d %q %s", code, out, stderr)
	}

	// agent.sock: logs of the agent are allowed in RUNNING, other sources are not.
	var logs struct {
		Lines []string `json:"lines"`
	}
	if code := b.call("GET", "/v1/logs?source=agent&kind=stderr&n=20", nil, &logs); code != 200 || logs.Lines == nil {
		t.Fatalf("logs agent stderr: %d %v", code, logs)
	}
	var errBody map[string]string
	if code := b.call("GET", "/v1/logs?source=daemon&kind=stderr", nil, &errBody); code != 409 || !strings.Contains(errBody["error"], "当前状态为 running") {
		t.Fatalf("logs daemon from the agent: %d %v", code, errBody)
	}
	if code := b.call("POST", "/v1/recovery/report", map[string]any{"status": "fixed", "summary": "x", "changed_files": []string{}, "lesson": "y"}, &errBody); code != 409 || errBody["error"] != "当前状态为 running，不接受这个请求" {
		t.Fatalf("recovery report in running: %d %v", code, errBody)
	}

	// SIGTERM: the agent and the gateway stop, the daemon exits with 0.
	agentPID := *info.PID
	gwPID, _ := strconv.Atoi(gatewayPID[1])
	d.Stop(t)
	if exit := d.Process().Exit(); !exit.Success() {
		t.Fatalf("bearbotd exit: %s\nstderr: %s", exit, strings.Join(d.Stderr(), "\n"))
	}
	for _, pid := range []int{agentPID, gwPID} {
		if e, err := proc.ReadEntry(pid); err == nil && e.State != 'Z' {
			t.Errorf("process %d still runs after the shutdown", pid)
		}
	}
	exits := b.logLines(b.env.Paths.AgentExitsLog)
	if len(exits) != 1 || !strings.Contains(exits[0], `"expected":true`) {
		t.Fatalf("exits.jsonl = %q", exits)
	}
	if fileExists(b.env.Paths.AgentSock) || fileExists(b.env.Paths.AdminSock) {
		t.Fatal("sockets not removed at shutdown")
	}
	if st := b.state(); st.SessionID != *info.SessionID {
		t.Fatalf("session id changed: %s", st.SessionID)
	}
}

// TestConsoleDisabled: with [platforms.console] enabled = false and stdin a pipe, every input line gets the notice
// that the console is unavailable; the agent runs without the console.
func TestConsoleDisabled(t *testing.T) {
	b := newBed(t, func(_ *testenv.Env, c *testenv.Config) { c.ConsoleEnabled = false })
	d := b.start()
	b.waitState("running", 90*time.Second)
	info, _ := b.agentStatus()
	if info.Console {
		t.Fatal("console reported as enabled")
	}
	d.Send(t, "hello")
	d.WaitOutputContains(t, 10*time.Second, "控制台不可用：gateway 没有启用 console 适配器（[platforms.console] enabled = false）")
	d.Send(t, "/bb status")
	b.waitFor("two notices", 10*time.Second, func() bool {
		n := 0
		for _, l := range d.Output() {
			if strings.HasPrefix(l, "控制台不可用") {
				n++
			}
		}
		return n == 2
	})
	for _, l := range d.Output() {
		if strings.Contains(l, "控制台已连接") {
			t.Fatal("console connected although disabled")
		}
	}
	var gwFile map[string]any
	if err := json.Unmarshal([]byte(b.readFile(b.env.Paths.GatewayConfig)), &gwFile); err != nil {
		t.Fatal(err)
	}
	if _, ok := gwFile["console"]; ok {
		t.Fatalf("gateway.json has console: %v", gwFile)
	}
	out, _, code := b.cli("admin", "list")
	if code != 0 || strings.Contains(out, "console:operator") {
		t.Fatalf("admin list without console: %d\n%s", code, out)
	}
}
