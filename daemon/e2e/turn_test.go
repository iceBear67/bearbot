package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"bearbot/daemon/internal/testenv"
)

// turnModelProfile gives the agent profile the unreachable test model with retries: a console message starts a turn
// that stays open while pi retries the refused model request (4 s, 8 s, 16 s, ...).
func turnModelProfile(t *testing.T, e *testenv.Env) {
	t.Helper()
	if err := os.MkdirAll(e.Paths.AgentDir, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.Paths.AgentModelsFile, testenv.ModelsJSON(unreachableModel), 0o664); err != nil {
		t.Fatal(err)
	}
	settings := `{"quietStartup": true, "defaultProjectTrust": "never", "defaultProvider": "testprovider", "defaultModel": "unreachable", "retry": {"enabled": true, "maxRetries": 5, "baseDelayMs": 4000}}` + "\n"
	if err := os.WriteFile(filepath.Join(e.Paths.AgentDir, "settings.json"), []byte(settings), 0o664); err != nil {
		t.Fatal(err)
	}
}

// queueMarkerExtension is an inbound middleware that writes the marker file .queued into the workspace after it passed
// on a message whose text starts with "排队": bearbot-im has queued the message (or steered it) by then, because next
// routes the message synchronously when no later middleware is registered.
func queueMarkerExtension(sdk string) string {
	return `import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { useBearbot } from ` + strconv.Quote(sdk) + `;

export default function (pi: ExtensionAPI) {
  const bearbot = useBearbot(pi, { name: "e2e-queue-marker" });
  bearbot.use((msg, next) => {
    next(msg);
    if (msg.text.startsWith("排队")) writeFileSync(join(bearbot.workspace.dir, ".queued"), msg.text);
  }, { order: 100 });
}
`
}

// TestTurnInProgress (DESIGN 16 管理员回滚, 5.4 DRAINING, 5.2 shutdown, 6.7 /bb stop): with a turn running,
//   - /bb stop aborts the turn, naming who started it, also when bearbot-im starts a queued turn right after the
//     aborted one (pi answers abort only after that turn; contracts 4.2), and the queued turn still runs;
//   - /bb snapshot list shows the default target;
//   - an apply waits in DRAINING, the turn is aborted at drain_timeout and the agent restarts with the apply;
//   - /bb rollback aborts the turn before the stop;
//   - an exit of the agent process while bearbotd waits for the turn at its shutdown is an unexpected exit, delivered
//     as lastExit after bearbotd started again.
func TestTurnInProgress(t *testing.T) {
	b := newBed(t, func(e *testenv.Env, c *testenv.Config) {
		turnModelProfile(t, e)
		c.DrainTimeout = "5s"
		c.ShutdownGrace = "60s"
	})
	d := b.start()
	d.WaitOutputContains(t, 90*time.Second, "控制台已连接")
	b.waitState("running", 90*time.Second)
	rpcLog := b.env.Paths.AgentRPCEventsLog
	starts := func() int { return b.countLines(rpcLog, `"type":"agent_start"`) }
	// waitStart waits for a run that started after n runs had started.
	waitStart := func(n int) {
		t.Helper()
		b.waitFor("agent_start of a run", 60*time.Second, func() bool { return starts() > n })
	}
	turn := func(text string) {
		t.Helper()
		n := starts()
		d.Send(t, text)
		waitStart(n)
	}

	b.ext("good.ts", goodExtension)
	b.ext("exit-later.ts", exitLaterExtension)
	b.ext("queue-marker.ts", queueMarkerExtension(b.env.Paths.SDKFile))
	n := starts()
	b.apply("三个扩展", "apply/1")
	b.waitRunningWith(0, "apply/1", 60*time.Second)
	// The restart delivers applied, and bearbot-im's confirmation turn stays open. A console line typed now waits in
	// bearbot-im's queue (the console binding has interrupt=queue). bearbot-im dispatches it from its agent_settled
	// handler of the aborted confirmation turn, before pi answers abort.
	waitStart(n)
	n = starts()
	d.Send(t, "排队的回合")
	b.waitFor("the console line queued in bearbot-im", 30*time.Second, func() bool {
		return fileExists(filepath.Join(b.env.Paths.WorkspaceDir, ".queued"))
	})
	if starts() != n {
		t.Fatal("the queued console line started a run while the confirmation turn runs")
	}
	lines := len(d.Output())
	b.console("/bb stop", "已中止正在进行的回合。", 30*time.Second)
	b.outputAfter(lines, "该回合由 console:main（控制台） 发起（角色 system，开始于", 10*time.Second)
	waitStart(n)
	lines = len(d.Output())
	b.console("/bb stop", "已中止正在进行的回合。", 30*time.Second)
	b.outputAfter(lines, "该回合由 console:main（控制台） 发起（角色 admin，开始于", 10*time.Second)

	turn("第一个回合")
	b.console("/bb snapshot list", "rollback 不指定快照时的默认目标：apply/1^", 30*time.Second)
	lines = len(d.Output())
	b.console("/bb stop", "已中止正在进行的回合。", 30*time.Second)
	b.outputAfter(lines, "该回合由 console:main（控制台） 发起（角色 admin，开始于", 10*time.Second)

	turn("第二个回合")
	before, _ := b.agentStatus()
	n = starts()
	lines = len(d.Output())
	b.ext("good2.ts", commandExtension("e2e-good-2"))
	b.apply("回合进行中的应用", "apply/2")
	b.outputAfter(lines, "agent：running → draining", 30*time.Second)
	b.waitRunningWith(*before.PID, "apply/2", 60*time.Second)
	if !strings.Contains(b.readFile(b.env.Paths.DaemonLog), "drain_timeout reached, aborting the turn") {
		t.Fatal("the drain did not end at drain_timeout")
	}

	// The confirmation turn of apply/2 runs; /bb rollback aborts it before the stop.
	waitStart(n)
	aborted := b.countLines(rpcLog, `"aborted":true`)
	n = starts()
	b.console("/bb rollback", "把 bot 回滚到了快照 apply/2^", 90*time.Second)
	b.waitState("running", 30*time.Second)
	if b.countLines(rpcLog, `"aborted":true`) <= aborted {
		t.Fatal("the rollback did not abort the running turn")
	}
	if tags := b.gitTags(); !contains(tags, "rollback/1/before") || !contains(tags, "rollback/1/after") {
		t.Fatalf("tags %v", tags)
	}

	// bearbot-im's rollback turn runs while bearbotd shuts down.
	waitStart(n)
	info, _ := b.agentStatus()
	if err := d.Process().Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	b.waitFor("shutdown wait for the turn", 30*time.Second, func() bool {
		return strings.Contains(b.readFile(b.env.Paths.DaemonLog), `to=draining reason="bearbotd 正在关闭"`)
	})
	b.marker(".go-exit")
	select {
	case <-d.Process().Done():
	case <-time.After(60 * time.Second):
		t.Fatal("bearbotd did not stop after the agent process exited")
	}
	if exit := d.Process().Exit(); !exit.Success() {
		t.Fatalf("bearbotd exit %s", exit)
	}
	record := b.lastLine(b.env.Paths.AgentExitsLog, `"pid":`+strconv.Itoa(*info.PID)+`,`)
	if !strings.Contains(record, `"code":7`) || !strings.Contains(record, `"expected":false`) {
		t.Fatalf("exit record: %s", record)
	}
	if st := b.state(); st.Deliver.LastExit == nil || st.Deliver.LastExit.Code == nil || *st.Deliver.LastExit.Code != 7 {
		t.Fatalf("lastExit after the shutdown: %+v", st.Deliver.LastExit)
	}
	b.start()
	b.waitState("running", 90*time.Second)
	if ready := b.lastLine(b.env.Paths.DaemonLog, "msg=bridge/ready"); !strings.Contains(ready, "lastExit=true") {
		t.Fatalf("bridge/ready after the start: %s", ready)
	}
}
