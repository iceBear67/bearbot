package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"bearbot/daemon/internal/testenv"
)

const factoryThrowExtension = `import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

export default function (_pi: ExtensionAPI) {
  throw new Error("factory boom e2e");
}
`

// hookScript writes the BEARBOT_* variables of a maintenance hook into out.
func hookScript(out string) string {
	return "#!/bin/sh\nenv | grep '^BEARBOT_' | sort > " + out + "\necho hook done\n"
}

type incidentRecord struct {
	ID         string   `json:"id"`
	Trigger    string   `json:"trigger"`
	Detail     string   `json:"detail"`
	StderrTail []string `json:"stderrTail"`
	Before     string   `json:"before"`
	Status     string   `json:"status"`
	Exit       *struct {
		Code *int `json:"code"`
	} `json:"exit"`
	MaintenanceReason string `json:"maintenanceReason"`
	Resolution        *struct {
		Kind string `json:"kind"`
	} `json:"resolution"`
	Repairs []struct {
		Note              string `json:"note"`
		MaintenanceReason string `json:"maintenanceReason"`
		Result            string `json:"result"`
	} `json:"repairs"`
	Attempts []struct {
		N     int `json:"n"`
		Start *struct {
			OK bool `json:"ok"`
		} `json:"start"`
	} `json:"attempts"`
}

func (b *bed) incident(id string) incidentRecord {
	b.t.Helper()
	var rec incidentRecord
	if err := json.Unmarshal([]byte(b.readFile(filepath.Join(b.env.Paths.IncidentDir(id), "incident.json"))), &rec); err != nil {
		b.t.Fatal(err)
	}
	return rec
}

func (b *bed) waitMaintenance(timeout time.Duration) string {
	b.t.Helper()
	b.waitFor("maintenance", timeout, func() bool {
		st := b.state()
		return st.Maintenance != nil && st.Maintenance.IncidentID != nil && st.OpenIncident != nil && st.OpenIncident.State == "maintenance"
	})
	b.waitState("maintenance", 10*time.Second)
	return *b.state().Maintenance.IncidentID
}

// TestStartFailureMaintenanceResumeRollback: an extension that throws in its factory makes the restart fail; the
// incident's recovery cannot run without credentials, so the bot enters the maintenance state (the on_enter hook
// runs, the administrators are notified). resume fails again; rollback without an apply needs a snapshot; rollback
// to init removes the extension, the agent runs in probation and the incident is closed afterwards.
func TestStartFailureMaintenanceResumeRollback(t *testing.T) {
	var enterOut, exitOut string
	b := newBed(t, func(e *testenv.Env, c *testenv.Config) {
		enterOut = filepath.Join(e.Root, "hook-enter.txt")
		exitOut = filepath.Join(e.Root, "hook-exit.txt")
		enter := filepath.Join(e.Paths.HooksDir, "enter.sh")
		exit := filepath.Join(e.Paths.HooksDir, "exit.sh")
		writeExecutable(t, enter, hookScript(enterOut))
		writeExecutable(t, exit, hookScript(exitOut))
		c.OnEnter = []string{enter}
		c.OnExit = []string{exit}
	})
	d := b.start()
	d.WaitOutputContains(t, 90*time.Second, "控制台已连接")
	b.waitState("running", 90*time.Second)

	b.ext("boom.ts", factoryThrowExtension)
	out, stderr, code := b.cli("restart")
	if code != 0 || !strings.Contains(out, "agent 重启失败") || !strings.Contains(out, "factory boom e2e") {
		t.Fatalf("restart: %d\n%s\n%s", code, out, stderr)
	}
	id := b.waitMaintenance(60 * time.Second)
	rec := b.incident(id)
	if rec.Trigger != "启动失败" || rec.Status != "maintenance" || !strings.Contains(strings.Join(rec.StderrTail, "\n"), "factory boom e2e") || rec.Exit == nil || rec.Exit.Code == nil || *rec.Exit.Code != 1 {
		t.Fatalf("incident.json = %+v", rec)
	}
	if !strings.Contains(rec.MaintenanceReason, "recovery 无法运行") || !strings.Contains(rec.MaintenanceReason, "anthropic") {
		t.Fatalf("maintenance reason = %q", rec.MaintenanceReason)
	}
	if !contains(b.gitTags(), "incident/"+id+"/before") {
		t.Fatalf("tags %v", b.gitTags())
	}
	if !fileExists(filepath.Join(b.env.Paths.IncidentDir(id), "report.md")) {
		t.Fatal("report.md missing")
	}
	b.waitFor("on_enter hook", 20*time.Second, func() bool { return fileExists(enterOut) })
	hook := b.readFile(enterOut)
	for _, want := range []string{"BEARBOT_EVENT=maintenance_enter", "BEARBOT_INCIDENT_ID=" + id, "BEARBOT_REASON=recovery 无法运行", "BEARBOT_REPORT=bearbot 进入维护状态"} {
		if !strings.Contains(hook, want) {
			t.Fatalf("hook environment lacks %q:\n%s", want, hook)
		}
	}
	d.WaitOutputContains(t, 20*time.Second, "bearbot 出现故障 "+id)
	d.WaitOutputContains(t, 20*time.Second, "bearbot 进入维护状态")
	d.WaitOutputContains(t, 20*time.Second, "agent：repairing → maintenance")

	// Trigger messages in maintenance get the maintenance notice, once per channel during the maintenance state.
	const notice = "bearbot 正在修复，暂时无法回复。"
	skip := len(d.Output())
	d.Send(t, "维护期间的第一条消息")
	b.outputAfter(skip, notice, 30*time.Second)
	d.Send(t, "维护期间的第二条消息")
	time.Sleep(3 * time.Second)
	if n := strings.Count(strings.Join(d.Output()[skip:], "\n"), notice); n != 1 {
		t.Fatalf("%d maintenance notices for two messages:\n%s", n, strings.Join(d.Output()[skip:], "\n"))
	}

	// agent.sock refuses everything in maintenance.
	var errBody map[string]string
	if code := b.call("POST", "/v1/check", map[string]any{}, &errBody); code != 409 || errBody["error"] != "当前状态为 maintenance，不接受这个请求" {
		t.Fatalf("check in maintenance: %d %v", code, errBody)
	}
	out, _, code = b.cli("incident", "list")
	if code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "维护状态") {
		t.Fatalf("incident list: %s", out)
	}
	out, _, code = b.cli("incident", "show", id)
	if code != 0 || !strings.Contains(out, "触发：启动失败") || !strings.Contains(out, "factory boom e2e") {
		t.Fatalf("incident show: %s", out)
	}
	// A refused restart prints no progress line.
	out, stderr, code = b.cli("restart")
	if code != 1 || out != "" || !strings.Contains(stderr, "当前状态为 maintenance，不能执行 restart") {
		t.Fatalf("restart in maintenance: %d %q %s", code, out, stderr)
	}

	// resume: the extension is still there, so the start fails and a new incident opens.
	lines := len(d.Output())
	b.console("/bb resume", "已提交快照 resume/1，但 agent 启动失败", 60*time.Second)
	b.waitFor("on_exit hook", 20*time.Second, func() bool { return fileExists(exitOut) })
	if hook := b.readFile(exitOut); !strings.Contains(hook, "BEARBOT_EVENT=maintenance_exit") || !strings.Contains(hook, "BEARBOT_REPORT=管理员执行 resume") {
		t.Fatalf("exit hook: %s", hook)
	}
	b.outputAfter(lines, "bearbot 进入维护状态", 60*time.Second)
	id2 := b.waitMaintenance(30 * time.Second)
	if id2 == id {
		t.Fatal("resume did not open a new incident")
	}
	if prev := b.incident(id); prev.Status != "closed" || prev.Resolution == nil || prev.Resolution.Kind != "superseded" {
		t.Fatalf("first incident after the second = %+v", prev)
	}
	if !contains(b.gitTags(), "resume/1") {
		t.Fatal("resume/1 missing")
	}

	// rollback without a snapshot needs an apply.
	b.console("/bb rollback", "还没有任何应用记录，请指定要回滚到的快照", 30*time.Second)
	// Invalid references are rejected everywhere.
	if code := b.call("POST", "/v1/snapshot", map[string]any{"action": "diff", "ref": "-x"}, &errBody); code != 409 {
		t.Fatalf("snapshot in maintenance: %d", code)
	}
	_, stderr, code = b.cli("rollback", "--", "-x")
	if code != 1 || !strings.Contains(stderr, `无效的快照引用 "-x"`) {
		t.Fatalf("rollback -x: %d %s", code, stderr)
	}
	_, stderr, code = b.cli("snapshot", "diff", "--", "-x")
	if code != 1 || !strings.Contains(stderr, `无效的快照引用 "-x"`) {
		t.Fatalf("snapshot diff -x: %d %s", code, stderr)
	}
	b.console("/bb snapshot diff apply/1;rm", "无效的快照引用", 30*time.Second)
	// The console passes references that start with "-" on as references.
	b.console("/bb rollback -x", `无效的快照引用 "-x"`, 30*time.Second)
	b.console("/bb snapshot diff -x", `无效的快照引用 "-x"`, 30*time.Second)
	output := filepath.Join(b.env.Root, "diff-output")
	b.console("/bb snapshot diff --output="+output, "无效的快照引用", 30*time.Second)
	if fileExists(output) {
		t.Fatalf("%s was written", output)
	}
	for _, tag := range b.gitTags() {
		if strings.HasPrefix(tag, "rollback/") {
			t.Fatalf("a rejected rollback created %s", tag)
		}
	}

	// rollback to init from the console.
	lines = len(d.Output())
	b.console("/bb rollback init", "把 bot 回滚到了快照 init", 90*time.Second)
	b.outputAfter(lines, "agent 已重新启动，进入观察期", 30*time.Second)
	b.waitState("running", 30*time.Second)
	if fileExists(filepath.Join(b.env.Paths.WorkspaceDir, ".pi/extensions/boom.ts")) {
		t.Fatal("boom.ts still there after the rollback")
	}
	tags := b.gitTags()
	if !contains(tags, "rollback/1/before") || !contains(tags, "rollback/1/after") {
		t.Fatalf("tags %v", tags)
	}
	st := b.state()
	if st.Maintenance != nil || st.RunningSnapshot != b.gitRev("rollback/1/after") || st.Rollback == nil {
		t.Fatalf("state after the rollback: %+v", st)
	}
	r := st.Rollback
	if r.ID != 1 || r.Target != "init" || r.Saved != "rollback/1/before" || len(r.Requesters) != 1 || r.Requesters[0] != "console:operator" || r.Thread == nil || *r.Thread != "console:main" || !r.Notified {
		t.Fatalf("rollback record = %+v", r)
	}
	if hook := b.readFile(exitOut); !strings.Contains(hook, "BEARBOT_REPORT=回滚到 init（管理员 console:operator）") {
		t.Fatalf("exit hook after the rollback: %s", hook)
	}
	// Probation ends: last-good moves to rollback/1/after, the incident is closed.
	b.waitFor("incident closed", 30*time.Second, func() bool { return b.state().OpenIncident == nil })
	if rec := b.incident(id2); rec.Status != "closed" || rec.Resolution == nil || rec.Resolution.Kind != "rollback" {
		t.Fatalf("second incident = %+v", rec)
	}
	b.waitFor("last-good at rollback/1/after", 10*time.Second, func() bool {
		info, _ := b.agentStatus()
		return info.LastGood != nil && *info.LastGood == "rollback/1/after"
	})
	d.WaitOutputContains(t, 10*time.Second, "故障 "+id2)

	// The rollback record is acknowledged by bearbot-im after an administrator turn; here through agent.sock.
	if code := b.call("POST", "/v1/rollback/ack", map[string]any{"id": 7}, &errBody); code != 409 || !strings.Contains(errBody["error"], "不能确认 7") {
		t.Fatalf("ack of a wrong id: %d %v", code, errBody)
	}
	if code := b.call("POST", "/v1/rollback/ack", map[string]any{"id": 1}, nil); code != 200 {
		t.Fatalf("ack: %d", code)
	}
	if b.state().Rollback != nil {
		t.Fatal("rollback record still there after the ack")
	}
	if !regexp.MustCompile(`^inc-[0-9]{14}$`).MatchString(id) {
		t.Fatalf("incident id %s", id)
	}
}

// TestForceRepair (DESIGN 5.8 repair --force): while a turn runs, another message waits in bearbot-im's queue and a
// restart waits in DRAINING, `bearbotd repair --force <说明>` replaces the restart, stops the agent process as an
// expected exit and opens an incident with the trigger 管理员要求; recovery cannot run without credentials, so the bot
// enters the maintenance state. In maintenance --force changes nothing. After resume, `/bb repair --force` from the
// console in RUNNING opens a new incident with its note.
func TestForceRepair(t *testing.T) {
	b := newBed(t, func(e *testenv.Env, c *testenv.Config) {
		turnModelProfile(t, e)
		c.DrainTimeout = "120s"
	})
	d := b.start()
	d.WaitOutputContains(t, 90*time.Second, "控制台已连接")
	b.waitState("running", 90*time.Second)
	rpcLog := b.env.Paths.AgentRPCEventsLog
	starts := func() int { return b.countLines(rpcLog, `"type":"agent_start"`) }
	marker := filepath.Join(b.env.Paths.WorkspaceDir, ".queued")

	// Without --force repair is refused outside the maintenance state.
	if out, stderr, code := b.cli("repair"); code != 1 || out != "" || !strings.Contains(stderr, "当前状态为 running，不能执行 repair") {
		t.Fatalf("repair in running: %d %q %s", code, out, stderr)
	}

	b.ext("queue-marker.ts", queueMarkerExtension(b.env.Paths.SDKFile))
	n := starts()
	b.apply("队列标记", "apply/1")
	info := b.waitRunningWith(0, "apply/1", 60*time.Second)
	// The confirmation turn after the apply restart stays open while pi retries the model request; it is stopped so
	// that the console's own turn runs.
	b.waitFor("agent_start of the confirmation turn", 60*time.Second, func() bool { return starts() > n })
	b.console("/bb stop", "已中止正在进行的回合。", 30*time.Second)
	n = starts()
	d.Send(t, "第一个回合")
	b.waitFor("agent_start of the console turn", 60*time.Second, func() bool { return starts() > n })
	// The second line waits in bearbot-im's queue (the console binding has interrupt=queue).
	d.Send(t, "排队的消息")
	b.waitFor("the console line queued in bearbot-im", 30*time.Second, func() bool { return fileExists(marker) })

	// A restart waits in DRAINING for the turn; the forced repair replaces it.
	restart := b.cliAsync("restart")
	b.waitState("draining", 30*time.Second)
	lines := len(d.Output())
	// --force counts only before the note, as in /bb repair.
	out, stderr, code := b.cli("repair", "--force", "先看", "--force")
	if code != 0 || !strings.Contains(out, "正在运行 recovery 修复，可能需要较长时间……") || !strings.Contains(out, "没有修复，bot 仍处于维护状态") {
		t.Fatalf("repair --force: %d\n%s\n%s", code, out, stderr)
	}
	r := <-restart
	if r.err != nil || r.code != 1 || !strings.Contains(r.errOut, "管理员强制修复取代了这次重启") {
		t.Fatalf("restart replaced by repair --force: %+v", r)
	}
	id := b.waitMaintenance(30 * time.Second)
	rec := b.incident(id)
	if rec.Trigger != "管理员要求" || !strings.Contains(rec.Detail, "repair --force") || rec.Exit != nil || len(rec.Repairs) != 1 || rec.Repairs[0].Note != "先看 --force" || rec.Repairs[0].MaintenanceReason != "" {
		t.Fatalf("incident.json = %+v", rec)
	}
	if !strings.Contains(rec.MaintenanceReason, "recovery 无法运行") {
		t.Fatalf("maintenance reason = %q", rec.MaintenanceReason)
	}
	if !contains(b.gitTags(), "incident/"+id+"/before") {
		t.Fatalf("tags %v", b.gitTags())
	}
	b.outputAfter(lines, "agent：draining → incident（管理员强制修复，正在停止 agent 进程）", 10*time.Second)
	b.outputAfter(lines, "管理员要求修复：已停止 agent 进程，建立故障 "+id, 10*time.Second)
	b.outputAfter(lines, "agent：repairing → maintenance", 10*time.Second)
	stopped := b.lastLine(b.env.Paths.AgentExitsLog, `"pid":`+strconv.Itoa(*info.PID)+`,`)
	if !strings.Contains(stopped, `"expected":true`) {
		t.Fatalf("exit record of the stopped process: %s", stopped)
	}
	if st := b.state(); st.Deliver.LastExit != nil || st.Maintenance == nil || *st.Maintenance.IncidentID != id {
		t.Fatalf("state after repair --force: %+v", st)
	}
	if _, stderr, code := b.cli("repair", "--force"); code != 0 {
		t.Fatalf("repair --force in maintenance: %d %s", code, stderr)
	}
	if rec := b.incident(id); len(rec.Repairs) != 2 || rec.Repairs[1].MaintenanceReason == "" {
		t.Fatalf("repair --force in maintenance did not continue the incident: %+v", rec)
	}

	b.console("/bb resume", "agent 已启动，进入观察期", 60*time.Second)
	b.console("/bb repair --force 第二次", "没有修复，bot 仍处于维护状态", 60*time.Second)
	id2 := b.waitMaintenance(30 * time.Second)
	if id2 == id {
		t.Fatal("/bb repair --force did not open a new incident")
	}
	if prev := b.incident(id); prev.Status != "closed" {
		t.Fatalf("first incident after the second = %+v", prev)
	}
	if rec := b.incident(id2); rec.Trigger != "管理员要求" || len(rec.Repairs) != 1 || rec.Repairs[0].Note != "第二次" {
		t.Fatalf("second incident = %+v", rec)
	}
}

// slowFactoryExtension keeps the trial run of a check busy for 10 seconds, so the check holds the change lock.
const slowFactoryExtension = `import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

export default function (_pi: ExtensionAPI) {
  const end = Date.now() + 10_000;
  while (Date.now() < end) {}
}
`

// TestIncidentStopWhileWaitingForLock (DESIGN 5.7 建立故障): `repair --force` stops the agent while a check holds the
// change lock, and bearbotd is stopped while the new incident waits for the lock. The incident id is allocated only
// under the lock, so no incident directory is left without incident.json and no incident is open.
func TestIncidentStopWhileWaitingForLock(t *testing.T) {
	b := newBed(t, nil)
	d := b.start()
	d.WaitOutputContains(t, 90*time.Second, "控制台已连接")
	b.waitState("running", 90*time.Second)
	b.ext("slow.ts", slowFactoryExtension)
	checks := b.checkDirs()
	b.callAsync("POST", "/v1/check", map[string]any{})
	b.waitCheckRunning(checks)
	b.cliAsync("repair", "--force")
	d.WaitOutputContains(t, 30*time.Second, "→ incident（管理员要求）")
	time.Sleep(500 * time.Millisecond)
	d.Stop(t)
	if !strings.Contains(b.readFile(b.env.Paths.DaemonLog), "forced repair, stopping the agent process") {
		t.Fatal("the forced repair did not run")
	}
	entries, err := os.ReadDir(b.env.Paths.IncidentsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("incident directories after the stop during the wait for the change lock: %v", entries)
	}
	if st := b.state(); st.OpenIncident != nil {
		t.Fatalf("open incident after the stop: %+v", st.OpenIncident)
	}
}
