package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"bearbot/daemon/internal/testenv"
)

// TestConsoleInputClosed (DESIGN 16 控制台): closing the input pipe ends the console input; bearbotd, the agent and
// the gateway keep running, and the console reports that its input ended.
func TestConsoleInputClosed(t *testing.T) {
	b := newBed(t, nil)
	d := b.start()
	d.WaitOutputContains(t, 90*time.Second, "控制台已连接")
	b.waitState("running", 90*time.Second)
	agent, _ := b.agentStatus()
	gateway := b.gatewayPID()
	d.CloseInput(t)
	b.waitFor("console input ended", 30*time.Second, func() bool {
		out, _, code := b.cli("status")
		return code == 0 && strings.Contains(out, "console：input-ended")
	})
	time.Sleep(2 * time.Second)
	if d.Process().Exited() {
		t.Fatalf("bearbotd exited after its input was closed: %s", d.Process().Exit())
	}
	if info, _ := b.agentStatus(); info.State != "running" || *info.PID != *agent.PID {
		t.Fatalf("agent after the end of the input: %+v", info)
	}
	if pid := b.gatewayPID(); pid != gateway {
		t.Fatalf("gateway %d replaced by %d after the end of the input", gateway, pid)
	}
}

// TestConsoleWithoutInput (DESIGN 5.13, 16): with /dev/null as stdin (`docker run -d`) the console is not enabled:
// status reports it, gateway.json has no console entry, and console:operator is not an administrator.
func TestConsoleWithoutInput(t *testing.T) {
	b := newBed(t, nil)
	b.d = b.env.StartDaemonNoInput(t)
	b.waitState("running", 90*time.Second)
	if info, _ := b.agentStatus(); info.Console {
		t.Fatal("console reported as enabled")
	}
	if out, _, code := b.cli("status"); code != 0 || !strings.Contains(out, "控制台：没有启用") {
		t.Fatalf("status: %d\n%s", code, out)
	}
	var gwFile map[string]any
	if err := json.Unmarshal([]byte(b.readFile(b.env.Paths.GatewayConfig)), &gwFile); err != nil {
		t.Fatal(err)
	}
	if _, ok := gwFile["console"]; ok {
		t.Fatalf("gateway.json has console: %v", gwFile)
	}
	if out, _, code := b.cli("admin", "list"); code != 0 || strings.Contains(out, "console:operator") {
		t.Fatalf("admin list: %d\n%s", code, out)
	}
	if !strings.Contains(b.readFile(b.env.Paths.DaemonLog), `msg="console decision" enabled=false configEnabled=true`) {
		t.Fatal("daemon.log does not record the console decision")
	}
}

// TestConsoleDisabledOnTerminal (DESIGN 5.13, 16): [platforms.console] enabled = false with a terminal as stdin
// (`docker run -it`): every line typed gets the notice that the console is unavailable.
func TestConsoleDisabledOnTerminal(t *testing.T) {
	b := newBed(t, func(_ *testenv.Env, c *testenv.Config) { c.ConsoleEnabled = false })
	d := b.env.StartDaemonOnTerminal(t)
	b.waitState("running", 90*time.Second)
	d.Type(t, "hello\r")
	n := d.WaitText(t, 20*time.Second, 0, "控制台不可用")
	d.Type(t, "/bb status\r")
	d.WaitText(t, 20*time.Second, n, "控制台不可用")
}

// TestConsoleTerminal (DESIGN 5.13, 16): bearbotd in the foreground of a terminal (`docker run -it`). Ctrl-C clears
// the line being typed and shows the hint; bearbotd keeps running. The agent process has no controlling terminal and
// runs in a session of its own.
func TestConsoleTerminal(t *testing.T) {
	b := newBed(t, nil)
	d := b.env.StartDaemonOnTerminal(t)
	n := d.WaitText(t, 90*time.Second, 0, "控制台已连接")
	b.waitState("running", 90*time.Second)
	agent, _ := b.agentStatus()
	d.Type(t, "这一行不会发送")
	d.Type(t, "\x03")
	n = d.WaitText(t, 10*time.Second, n, "docker attach 中按 Ctrl-P Ctrl-Q 断开")
	time.Sleep(2 * time.Second)
	select {
	case <-d.Done():
		t.Fatal("bearbotd exited after Ctrl-C")
	default:
	}
	if info, _ := b.agentStatus(); info.State != "running" || *info.PID != *agent.PID {
		t.Fatalf("agent after Ctrl-C: %+v", info)
	}
	d.Type(t, "/bb status\r")
	d.WaitText(t, 30*time.Second, n, "agent：running")
	// A line sent as a message would start an administrator turn, which fails without a model.
	if strings.Contains(d.Text()[n:], "No API key found") {
		t.Fatalf("the line cleared with Ctrl-C was sent:\n%s", d.Text()[n:])
	}
	session, tty := procStat(t, *agent.PID)
	if tty != 0 || session != *agent.PID {
		t.Fatalf("agent process %d: session %d, tty_nr %d", *agent.PID, session, tty)
	}
}
