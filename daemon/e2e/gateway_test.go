package e2e

import (
	"bufio"
	"context"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"bearbot/daemon/internal/testenv"
)

func (b *bed) gatewayPID() int {
	b.t.Helper()
	out, _, _ := b.cli("status")
	m := regexp.MustCompile(`gateway：已连接（PID ([0-9]+)`).FindStringSubmatch(out)
	if m == nil {
		b.t.Fatalf("no connected gateway in status:\n%s", out)
	}
	pid, _ := strconv.Atoi(m[1])
	return pid
}

// TestGatewayCrashLimitHook: when the gateway exits more often than [gateway] crash_limit without a stable run, the
// on_enter hooks run with BEARBOT_EVENT=gateway_crash; the gateway keeps being restarted and the agent keeps running.
// `bearbotd logs -f` follows the agent's stderr.
func TestGatewayCrashLimitHook(t *testing.T) {
	var hookOut string
	b := newBed(t, func(e *testenv.Env, c *testenv.Config) {
		hookOut = filepath.Join(e.Root, "hook.txt")
		hook := filepath.Join(e.Paths.HooksDir, "enter.sh")
		writeExecutable(t, hook, hookScript(hookOut))
		c.OnEnter = []string{hook}
		c.GatewayCrashLimit = 1
	})
	d := b.start()
	d.WaitOutputContains(t, 90*time.Second, "控制台已连接")
	b.waitState("running", 90*time.Second)
	agent, _ := b.agentStatus()

	// The first crash is restarted after 1 s.
	pid := b.gatewayPID()
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	b.waitFor("gateway restarted", 30*time.Second, func() bool {
		out, _, _ := b.cli("status")
		return strings.Contains(out, "gateway：已连接") && !strings.Contains(out, "PID "+strconv.Itoa(pid)+"，")
	})
	if fileExists(hookOut) {
		t.Fatal("hook ran within crash_limit")
	}
	// The second one exceeds crash_limit: the hook runs and restarts happen every 60 s.
	seen := len(d.Output())
	if err := syscall.Kill(b.gatewayPID(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	b.waitFor("on_enter hook", 20*time.Second, func() bool { return fileExists(hookOut) })
	hook := b.readFile(hookOut)
	if !strings.Contains(hook, "BEARBOT_EVENT=gateway_crash") || !strings.Contains(hook, "超过 [gateway] crash_limit（1）") {
		t.Fatalf("hook environment:\n%s", hook)
	}
	time.Sleep(5 * time.Second)
	if out, _, _ := b.cli("status"); !strings.Contains(out, "gateway：没有连接") || !strings.Contains(out, "连续意外退出 2 次") {
		t.Fatalf("status 5 s after the second crash:\n%s", out)
	}
	b.outputAfter(seen, "控制台已连接", 90*time.Second)
	b.outputAfter(seen, "gateway 连续意外退出 2 次", 30*time.Second)
	info, _ := b.agentStatus()
	if info.State != "running" || *info.PID != *agent.PID {
		t.Fatalf("agent after the gateway crashes: %+v", info)
	}
	if b.state().Maintenance != nil {
		t.Fatal("gateway crashes put the bot into maintenance")
	}

	// logs -f: the agent's stderr after a restart arrives on the stream.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, testenv.DaemonBinary(t), "--root", b.env.Root, "--rootless", "logs", "agent", "-n", "1", "-f")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	select {
	case <-lines:
	case <-time.After(10 * time.Second):
		t.Fatal("logs -f printed nothing")
	}
	if out, _, code := b.cli("restart"); code != 0 {
		t.Fatalf("restart: %s", out)
	}
	deadline := time.After(30 * time.Second)
	for found := false; !found; {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("logs -f ended")
			}
			found = strings.Contains(l, "No project session found")
		case <-deadline:
			t.Fatal("logs -f did not show the new process's stderr")
		}
	}
	cancel()
	cmd.Wait()
}
