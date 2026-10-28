package e2e

import (
	"bufio"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"bearbot/daemon/internal/proc"
	"bearbot/daemon/internal/testenv"
)

// TestUnexpectedExitNewSessionRestart: SIGKILL of the agent is an unexpected exit: restart with the same session,
// lastExit given to the next bridge/ready, crash counted. /bb new restarts with a new session id; `bearbotd restart`
// keeps it.
func TestUnexpectedExitNewSessionRestart(t *testing.T) {
	b := newBed(t, nil)
	d := b.start()
	d.WaitOutputContains(t, 90*time.Second, "控制台已连接")
	b.waitState("running", 90*time.Second)
	first, _ := b.agentStatus()
	session := *first.SessionID

	if err := syscall.Kill(*first.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	b.waitFor("restart after SIGKILL", 60*time.Second, func() bool {
		info, code := b.agentStatus()
		return code == 200 && info.State == "running" && *info.PID != *first.PID
	})
	info, _ := b.agentStatus()
	if info.Restarts != 1 || info.CrashesInWindow != 1 || *info.SessionID != session || info.Incident != nil {
		t.Fatalf("status after SIGKILL: %+v", info)
	}
	st := b.state()
	if len(st.Crashes) != 1 || st.Deliver.LastExit != nil || st.SessionID != session {
		t.Fatalf("state after SIGKILL: %+v", st)
	}
	exits := b.logLines(b.env.Paths.AgentExitsLog)
	if len(exits) != 1 || !strings.Contains(exits[0], `"signal":"SIGKILL"`) || !strings.Contains(exits[0], `"expected":false`) {
		t.Fatalf("exits.jsonl = %q", exits)
	}
	if !strings.Contains(b.readFile(b.env.Paths.DaemonLog), "lastExit=true") {
		t.Fatal("lastExit was not delivered with bridge/ready")
	}

	// /bb new: a new session id, the process restarts.
	lines := len(d.Output())
	b.console("/bb new", "已开启新会话 bearbot-", 60*time.Second)
	newSession := b.state().SessionID
	if newSession == session || !regexp.MustCompile(`^bearbot-[0-9]{14}-[0-9a-f]{8}$`).MatchString(newSession) {
		t.Fatalf("session after /bb new: %s (before %s)", newSession, session)
	}
	b.outputAfter(lines, "agent：restarting → starting", 10*time.Second)
	b.waitState("running", 30*time.Second)
	info, _ = b.agentStatus()
	if *info.SessionID != newSession {
		t.Fatalf("status session %s, state.json %s", *info.SessionID, newSession)
	}
	st = b.state()
	if !strings.Contains(st.SessionFile, newSession) {
		t.Fatalf("session file %q does not belong to %s", st.SessionFile, newSession)
	}

	// bearbotd restart keeps the session.
	before := *info.PID
	out, stderr, code := b.cli("restart")
	if code != 0 || !strings.Contains(out, "agent 已重启") {
		t.Fatalf("restart: %d %s %s", code, out, stderr)
	}
	info, _ = b.agentStatus()
	if *info.PID == before || *info.SessionID != newSession || info.State != "running" {
		t.Fatalf("after restart: %+v", info)
	}
	// /bb restart from the console replies after the new process is ready.
	b.console("/bb restart", "agent 已重启（PID", 60*time.Second)
	// abort and compact with an idle agent.
	b.console("/bb stop", "当前没有正在进行的回合", 30*time.Second)
	b.console("/bb compact", "会话内容太少，不需要压缩", 30*time.Second)
}

// busyLoopExtension blocks the event loop once the test created the marker file .go-busy in the workspace (outside
// the trial), so the loop starts after the agent became ready.
const busyLoopExtension = `import { existsSync, unlinkSync } from "node:fs";
import { join } from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

export default function (pi: ExtensionAPI) {
  pi.on("session_start", () => {
    if (process.env.BEARBOT_TRIAL === "1") return;
    const marker = join(process.env.BEARBOT_WORKSPACE_DIR ?? "", ".go-busy");
    setInterval(() => {
      if (existsSync(marker)) {
        unlinkSync(marker);
        for (;;) {}
      }
    }, 100);
  });
}
`

// TestHeartbeatTimeoutCrashLimit: an extension that blocks the event loop makes get_state time out; the daemon kills
// the process tree and restarts it; at crash_limit the unexpected exits open an incident.
func TestHeartbeatTimeoutCrashLimit(t *testing.T) {
	b := newBed(t, func(_ *testenv.Env, c *testenv.Config) {
		c.CrashLimit = 2
		c.HeartbeatInterval = "1s"
		c.RPCTimeout = "2s"
	})
	b.start()
	b.waitState("running", 90*time.Second)
	b.ext("busy-loop.ts", busyLoopExtension)
	if out, stderr, code := b.cli("restart"); code != 0 || !strings.Contains(out, "agent 已重启") {
		t.Fatalf("restart: %d %s %s", code, out, stderr)
	}
	first, _ := b.agentStatus()
	b.marker(".go-busy")
	b.waitRunningWith(*first.PID, "", 60*time.Second)
	b.marker(".go-busy")
	id := b.waitMaintenance(90 * time.Second)
	rec := b.incident(id)
	if rec.Trigger != "反复退出" || !strings.Contains(rec.Detail, "心跳超时") || !strings.Contains(rec.Detail, "crash_limit") {
		t.Fatalf("incident = %+v", rec)
	}
	var killed int
	for _, l := range b.logLines(b.env.Paths.AgentExitsLog) {
		if strings.Contains(l, `"signal":"SIGKILL"`) && strings.Contains(l, `"expected":false`) {
			killed++
		}
	}
	if killed != 2 {
		t.Fatalf("exits.jsonl has %d killed processes:\n%s", killed, strings.Join(b.logLines(b.env.Paths.AgentExitsLog), "\n"))
	}
	if st := b.state(); len(st.Crashes) != 0 || st.Deliver.LastExit == nil {
		t.Fatalf("state: crashes %v, lastExit %v", st.Crashes, st.Deliver.LastExit)
	}
}

// TestSignalDuringStartup (DESIGN 5.2): SIGTERM while bearbotd is still starting, before the gateway is ready, stops
// bearbotd; no gateway process stays behind.
func TestSignalDuringStartup(t *testing.T) {
	b := newBed(t, nil)
	d := b.start()
	time.Sleep(150 * time.Millisecond)
	if err := d.Process().Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.Process().Done():
	case <-time.After(30 * time.Second):
		t.Fatal("bearbotd did not exit within 30 s after SIGTERM during its startup")
	}
	if exit := d.Process().Exit(); !exit.Success() {
		t.Fatalf("bearbotd exit %s\nstderr: %s", exit, strings.Join(d.Stderr(), "\n"))
	}
	if fileExists(b.env.Paths.DaemonLog) {
		for _, m := range regexp.MustCompile(`msg="gateway started" pid=([0-9]+)`).FindAllStringSubmatch(b.readFile(b.env.Paths.DaemonLog), -1) {
			pid, _ := strconv.Atoi(m[1])
			if !gone(pid) {
				t.Errorf("gateway %d still runs after bearbotd exited", pid)
			}
		}
	}
}

// garbageExtension writes a line to stdout that is not an RPC record once the test created the marker file
// .go-garbage (fd 1 bypasses pi's output guard).
const garbageExtension = `import { existsSync, unlinkSync, writeSync } from "node:fs";
import { join } from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

export default function (pi: ExtensionAPI) {
  pi.on("session_start", () => {
    if (process.env.BEARBOT_TRIAL === "1") return;
    const marker = join(process.env.BEARBOT_WORKSPACE_DIR ?? "", ".go-garbage");
    setInterval(() => {
      if (existsSync(marker)) {
        unlinkSync(marker);
        writeSync(1, "not a json record\n");
      }
    }, 100);
  });
}
`

// TestStdoutProtocolViolation (DESIGN 5.3): the agent process writes a line to stdout that is not an RPC record and
// keeps running. The daemon can no longer read its stdout, kills the process tree and handles it as an unexpected
// exit: a restart, a counted crash and lastExit for the next bridge/ready.
func TestStdoutProtocolViolation(t *testing.T) {
	b := newBed(t, nil)
	b.start()
	b.waitState("running", 90*time.Second)
	b.ext("garbage.ts", garbageExtension)
	if out, stderr, code := b.cli("restart"); code != 0 || !strings.Contains(out, "agent 已重启") {
		t.Fatalf("restart: %d %s %s", code, out, stderr)
	}
	before, _ := b.agentStatus()
	b.marker(".go-garbage")
	info := b.waitRunningWith(*before.PID, "", 60*time.Second)
	if info.CrashesInWindow != 1 || info.Incident != nil {
		t.Fatalf("status after the restart: %+v", info)
	}
	if !strings.Contains(b.readFile(b.env.Paths.DaemonLog), "pi stdout can no longer be read while the process runs") {
		t.Fatal("daemon.log does not record the unreadable stdout")
	}
	killed := b.lastLine(b.env.Paths.AgentExitsLog, `"pid":`+strconv.Itoa(*before.PID)+`,`)
	if !strings.Contains(killed, `"signal":"SIGKILL"`) || !strings.Contains(killed, `"expected":false`) {
		t.Fatalf("exit record: %s", killed)
	}
	if ready := b.lastLine(b.env.Paths.DaemonLog, "msg=bridge/ready"); !strings.Contains(ready, "lastExit=true") {
		t.Fatalf("bridge/ready after the restart: %s", ready)
	}
}

// mcpServer waits for the MCP stdio server (sleep 300) among the descendants of the running agent process and
// checks that it runs in a session of its own; it returns its PID.
func (b *bed) mcpServer(notPID int) int {
	b.t.Helper()
	server, agent := 0, 0
	b.waitFor("MCP server of the agent process", 60*time.Second, func() bool {
		info, code := b.agentStatus()
		if code != 200 || info.State != "running" || info.PID == nil || *info.PID == notPID {
			return false
		}
		agent = *info.PID
		tree, err := proc.CollectTree(agent)
		if err != nil {
			b.t.Fatal(err)
		}
		for _, e := range tree {
			cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(e.PID) + "/cmdline")
			if err == nil && strings.Contains(string(cmdline), "sleep\x00300\x00") {
				server = e.PID
				return true
			}
		}
		return false
	})
	if s, _ := procStat(b.t, server); s == agent || s != server {
		b.t.Fatalf("MCP server %d is in session %d (agent %d)", server, s, agent)
	}
	return server
}

// TestMCPServerProcessTree (DESIGN 5.3, 16): pi runs MCP stdio servers in sessions of their own. The daemon stops
// the whole process tree, so the server ends with the agent process after `bearbotd restart`, after the heartbeat
// kill and when bearbotd stops.
func TestMCPServerProcessTree(t *testing.T) {
	b := newBed(t, func(_ *testenv.Env, c *testenv.Config) {
		c.HeartbeatInterval = "1s"
		c.RPCTimeout = "2s"
	})
	d := b.start()
	b.waitState("running", 90*time.Second)
	b.env.WriteFile(t, "/bearbot/workspace/.pi/mcp.json", `{"mcpServers": {"slow": {"command": "/usr/bin/sleep", "args": ["300"], "timeout": 600}}}`+"\n")
	b.ext("busy-loop.ts", busyLoopExtension)
	if out, stderr, code := b.cli("restart"); code != 0 || !strings.Contains(out, "agent 已重启") {
		t.Fatalf("restart: %d %s %s", code, out, stderr)
	}
	server := b.mcpServer(0)
	agent, _ := b.agentStatus()

	if out, stderr, code := b.cli("restart"); code != 0 || !strings.Contains(out, "agent 已重启") {
		t.Fatalf("second restart: %d %s %s", code, out, stderr)
	}
	if !gone(server) {
		t.Fatalf("MCP server %d still runs after bearbotd restart", server)
	}
	server = b.mcpServer(*agent.PID)
	agent, _ = b.agentStatus()

	b.marker(".go-busy")
	b.waitRunningWith(*agent.PID, "", 60*time.Second)
	if !gone(server) {
		t.Fatalf("MCP server %d still runs after the heartbeat kill", server)
	}
	server = b.mcpServer(*agent.PID)

	d.Stop(t)
	if !gone(server) {
		t.Fatalf("MCP server %d still runs after bearbotd stopped", server)
	}
}

const syntaxErrorExtension = "export default function (pi) {\n  const x = { a: };\n}\n"

const duplicateToolExtension = `import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
export default function (pi: ExtensionAPI) {
  pi.registerTool({
    name: "dup_tool",
    label: "Dup",
    description: "dup",
    parameters: Type.Object({}),
    async execute() {
      return { content: [{ type: "text", text: "x" }], details: {} };
    },
  });
}
`

// TestStartFailureVariants (DESIGN 16 启动失败与 recovery, DESIGN 5.3): an extension with a syntax error, two
// extensions that register the same tool, and an extension in the profile's extensions directory each make the agent
// start fail; every case opens an incident (启动失败) with its snapshot and ends in maintenance without recovery
// credentials. resume after removing the files starts the agent again.
func TestStartFailureVariants(t *testing.T) {
	b := newBed(t, nil)
	b.start()
	b.waitState("running", 90*time.Second)
	variants := []struct {
		name   string
		files  map[string]string
		stderr []string
		detail string
	}{
		{"syntax error", map[string]string{"/bearbot/workspace/.pi/extensions/broken.js": syntaxErrorExtension}, []string{"Failed to load extension", "broken.js"}, ""},
		{"duplicate tool", map[string]string{"/bearbot/workspace/.pi/extensions/tool-a.ts": duplicateToolExtension, "/bearbot/workspace/.pi/extensions/tool-b.ts": duplicateToolExtension}, []string{"dup_tool", "conflicts"}, ""},
		{"profile extension", map[string]string{"/bearbot/profile/agent/extensions/x.ts": goodExtension}, nil, "启动前的位置检查没有通过"},
	}
	for _, v := range variants {
		var written []string
		for rel, content := range v.files {
			written = append(written, b.env.WriteFile(t, rel, content))
		}
		out, stderr, code := b.cli("restart")
		if code != 0 || !strings.Contains(out, "agent 重启失败") {
			t.Fatalf("%s: restart: %d\n%s\n%s", v.name, code, out, stderr)
		}
		id := b.waitMaintenance(60 * time.Second)
		rec := b.incident(id)
		tail := strings.Join(rec.StderrTail, "\n")
		if rec.Trigger != "启动失败" || !strings.Contains(rec.Detail, v.detail) {
			t.Fatalf("%s: incident = %+v", v.name, rec)
		}
		for _, want := range v.stderr {
			if !strings.Contains(tail, want) {
				t.Fatalf("%s: stderr of the incident lacks %q:\n%s", v.name, want, tail)
			}
		}
		if !contains(b.gitTags(), "incident/"+id+"/before") {
			t.Fatalf("%s: tags %v", v.name, b.gitTags())
		}
		for _, p := range written {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		}
		if out, stderr, code := b.cli("resume"); code != 0 || !strings.Contains(out, "agent 已启动") {
			t.Fatalf("%s: resume: %d\n%s\n%s", v.name, code, out, stderr)
		}
		b.waitState("running", 30*time.Second)
	}
}

// hangingStartExtension never finishes its session_start handler outside the trial: the boot get_state waits for it.
const hangingStartExtension = `import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

export default function (pi: ExtensionAPI) {
  pi.on("session_start", async () => {
    if (process.env.BEARBOT_TRIAL === "1") return;
    await new Promise<void>(() => {});
  });
}
`

// gatewayHello connects to gateway.sock and completes hello, like bearbot-im does; the gateway accepts one such
// connection at a time.
func (b *bed) gatewayHello() net.Conn {
	b.t.Helper()
	conn, err := net.Dial("unix", b.env.Paths.GatewaySock)
	if err != nil {
		b.t.Fatal(err)
	}
	if _, err := io.WriteString(conn, `{"jsonrpc":"2.0","id":1,"method":"hello","params":{}}`+"\n"); err != nil {
		b.t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		b.t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.Contains(line, `"result"`) {
		b.t.Fatalf("hello on gateway.sock: %q %v", line, err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		b.t.Fatal(err)
	}
	return conn
}

// TestReadinessTimeouts (DESIGN 5.3 就绪): a session_start handler that never ends keeps the boot get_state from
// being answered, so the start fails at start_timeout; while another connection holds gateway.sock, bearbot-im cannot
// call /v1/bridge/ready, so the start fails at ready_timeout.
func TestReadinessTimeouts(t *testing.T) {
	b := newBed(t, func(_ *testenv.Env, c *testenv.Config) {
		c.StartTimeout = "10s"
		c.ReadyTimeout = "10s"
	})
	b.start()
	b.waitState("running", 90*time.Second)
	hang := b.ext("hang.ts", hangingStartExtension)
	out, stderr, code := b.cli("restart")
	if code != 0 || !strings.Contains(out, "agent 重启失败") || !strings.Contains(out, "start_timeout（10s）") {
		t.Fatalf("restart: %d\n%s\n%s", code, out, stderr)
	}
	id := b.waitMaintenance(60 * time.Second)
	if rec := b.incident(id); rec.Trigger != "启动失败" || !strings.Contains(rec.Detail, "没有在 start_timeout（10s）内响应 get_state") {
		t.Fatalf("incident = %+v", rec)
	}
	if err := os.Remove(hang); err != nil {
		t.Fatal(err)
	}

	conn := b.gatewayHello()
	out, stderr, code = b.cli("resume")
	conn.Close()
	if code != 0 || !strings.Contains(out, "agent 启动失败") || !strings.Contains(out, "ready_timeout（10s）") {
		t.Fatalf("resume while gateway.sock is taken: %d\n%s\n%s", code, out, stderr)
	}
	id2 := b.waitMaintenance(60 * time.Second)
	if rec := b.incident(id2); rec.Trigger != "启动失败" || !strings.Contains(rec.Detail, "没有在 ready_timeout（10s）内调用 /v1/bridge/ready") {
		t.Fatalf("incident = %+v", rec)
	}
	if out, stderr, code := b.cli("resume"); code != 0 || !strings.Contains(out, "agent 已启动") {
		t.Fatalf("resume: %d\n%s\n%s", code, out, stderr)
	}
	b.waitState("running", 30*time.Second)
}
