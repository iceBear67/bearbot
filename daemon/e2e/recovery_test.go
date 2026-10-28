package e2e

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"bearbot/daemon/internal/testenv"
)

// unreachableModel has an API key, so `pi auth check` accepts the credentials, but its endpoint refuses connections.
var unreachableModel = testenv.Model{BaseURL: "http://127.0.0.1:9/v1", APIKey: "invalid-key", ID: "unreachable"}

// TestRecoveryTurnError: recovery has credentials but the model service refuses the connection. The real recovery
// process (bearbot-recovery extension) gets the incident prompt, its turn ends with an error, so "recovery 无法运行"
// and the maintenance state follow. `/bb repair <说明>` runs recovery again in the same recovery session with the
// note.
func TestRecoveryTurnError(t *testing.T) {
	// The turn ends with an error only when the model service refuses the connection (retries are off).
	if c, err := net.DialTimeout("tcp", "127.0.0.1:9", time.Second); err == nil {
		c.Close()
		t.Fatal("127.0.0.1:9 accepts connections; TestRecoveryTurnError needs a port that refuses them")
	} else if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("connect 127.0.0.1:9: %v", err)
	}
	b := newBed(t, func(e *testenv.Env, c *testenv.Config) {
		dir := e.Paths.RecoveryOverrideDir
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "models.json"), testenv.ModelsJSON(unreachableModel), 0o600); err != nil {
			t.Fatal(err)
		}
		settings := `{"quietStartup": true, "defaultProjectTrust": "never", "defaultProvider": "testprovider", "defaultModel": "unreachable", "retry": {"enabled": false}}`
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	d := b.start()
	d.WaitOutputContains(t, 90*time.Second, "控制台已连接")
	b.waitState("running", 90*time.Second)
	b.ext("boom.ts", factoryThrowExtension)
	if out, stderr, code := b.cli("restart"); code != 0 || !strings.Contains(out, "agent 重启失败") {
		t.Fatalf("restart: %d %s %s", code, out, stderr)
	}
	id := b.waitMaintenance(120 * time.Second)
	rec := b.incident(id)
	if !strings.Contains(rec.MaintenanceReason, "recovery 无法运行：recovery 的回合以错误结束") {
		t.Fatalf("maintenance reason = %q", rec.MaintenanceReason)
	}
	log := b.readFile(b.env.Paths.RecoveryLog(id))
	for _, want := range []string{"故障 " + id, "触发：启动失败", "factory boom e2e", "快照仓库：git --git-dir=" + b.env.Paths.SnapshotsGit, `"type":"agent_settled"`} {
		if !strings.Contains(log, want) {
			t.Fatalf("recovery log lacks %q:\n%s", want, log)
		}
	}
	out, stderr, code := b.cli("logs", "recovery:"+id, "-n", "500")
	if code != 0 || !strings.Contains(out, "触发：启动失败") {
		t.Fatalf("logs recovery:%s: %d\n%s\n%s", id, code, out, stderr)
	}
	if _, stderr, code := b.cli("logs", "recovery:inc-1"); code != 1 || !strings.Contains(stderr, "无效的故障 id") {
		t.Fatalf("logs recovery:inc-1: %d %s", code, stderr)
	}
	sessions, err := filepath.Glob(filepath.Join(b.env.Paths.RecoverySessionsDir, "*_"+id+".jsonl"))
	if err != nil || len(sessions) != 1 {
		t.Fatalf("recovery session files %v %v", sessions, err)
	}
	// The recovery profile with the recovery credentials exists only while a repair runs (DESIGN 15).
	if fileExists(b.env.Paths.RecoveryAgentDir) {
		t.Fatalf("%s still exists in maintenance", b.env.Paths.RecoveryAgentDir)
	}

	b.console("/bb repair 请先检查 boom.ts", "没有修复，bot 仍处于维护状态", 120*time.Second)
	rec = b.incident(id)
	if len(rec.Repairs) != 2 {
		t.Fatalf("repairs = %+v", rec)
	}
	log = b.readFile(b.env.Paths.RecoveryLog(id))
	if !strings.Contains(log, "管理员说明：请先检查 boom.ts") {
		t.Fatalf("recovery log lacks the note:\n%s", log)
	}
	if sessions, _ := filepath.Glob(filepath.Join(b.env.Paths.RecoverySessionsDir, "*.jsonl")); len(sessions) != 1 {
		t.Fatalf("the second recovery process did not continue the session: %v", sessions)
	}
	if st := b.state(); st.Maintenance == nil || *st.Maintenance.IncidentID != id {
		t.Fatalf("state after repair: %+v", st.Maintenance)
	}
}

// TestRecoveryReportFixed: the test does the part of the recovery model (fix the files, call the recovery API of
// agent.sock in REPAIRING) while the real recovery process keeps its turn open: pi retries the refused model request
// with long delays. The daemon aborts the turn, commits incident/<id>/attempt-1, checks, starts the agent in
// probation and closes the incident when probation ends.
func TestRecoveryReportFixed(t *testing.T) {
	b := newBed(t, func(e *testenv.Env, c *testenv.Config) {
		dir := e.Paths.RecoveryOverrideDir
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "models.json"), testenv.ModelsJSON(unreachableModel), 0o600); err != nil {
			t.Fatal(err)
		}
		settings := `{"quietStartup": true, "defaultProjectTrust": "never", "defaultProvider": "testprovider", "defaultModel": "unreachable", "retry": {"enabled": true, "maxRetries": 5, "baseDelayMs": 4000}}`
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
		// A clear margin over the calls below, which include a full check before the report.
		c.RecoveryTimeout = "30s"
	})
	d := b.start()
	d.WaitOutputContains(t, 90*time.Second, "控制台已连接")
	b.waitState("running", 90*time.Second)
	boom := b.ext("boom.ts", factoryThrowExtension)
	if out, stderr, code := b.cli("restart"); code != 0 || !strings.Contains(out, "agent 重启失败") {
		t.Fatalf("restart: %d %s %s", code, out, stderr)
	}
	b.waitState("repairing", 60*time.Second)
	id := b.state().OpenIncident.ID
	b.waitFor("prompt sent to recovery", 60*time.Second, func() bool {
		return fileExists(b.env.Paths.RecoveryLog(id)) && strings.Contains(b.readFile(b.env.Paths.RecoveryLog(id)), "=== 提示结束 ===")
	})

	// The recovery endpoints answer in REPAIRING; the agent endpoints do not.
	info, code := b.agentStatus()
	if code != 200 || info.State != "repairing" || info.Incident == nil || info.Incident.ID != id {
		t.Fatalf("status in repairing: %d %+v", code, info)
	}
	var logs struct {
		Lines []string `json:"lines"`
	}
	if code := b.call("GET", "/v1/logs?source=daemon&kind=stderr&n=5", nil, &logs); code != 200 || len(logs.Lines) != 5 {
		t.Fatalf("daemon logs in repairing: %d %v", code, logs)
	}
	// For source gateway and daemon the kind does not matter (recovery_logs always sends one).
	logs.Lines = nil
	if code := b.call("GET", "/v1/logs?source=gateway&kind=rpc&n=5", nil, &logs); code != 200 || len(logs.Lines) != 5 {
		t.Fatalf("gateway logs in repairing: %d %v", code, logs)
	}
	// The recovery profile exists while the repair runs and has the override files.
	if got := b.readFile(filepath.Join(b.env.Paths.RecoveryAgentDir, "settings.json")); !strings.Contains(got, `"unreachable"`) {
		t.Fatalf("recovery settings.json = %s", got)
	}
	if code := b.call("POST", "/v1/bridge/ready", map[string]any{"tools": []string{}, "commands": []string{}}, nil); code != 409 {
		t.Fatalf("bridge/ready in repairing: %d", code)
	}
	if err := os.Remove(boom); err != nil {
		t.Fatal(err)
	}
	var rep checkReport
	if code := b.call("POST", "/v1/check", map[string]any{}, &rep); code != 200 || !rep.Passed {
		t.Fatalf("check in repairing: %d %+v", code, rep)
	}
	report := map[string]any{"status": "fixed", "summary": "boom.ts 的工厂函数抛出异常，删除了它", "changed_files": []string{boom}, "lesson": "工厂函数中不要抛出异常"}
	var accepted map[string]any
	if code := b.call("POST", "/v1/recovery/report", report, &accepted); code != 200 || accepted["accepted"] != true {
		t.Fatalf("report: %d %v", code, accepted)
	}
	var errBody map[string]string
	if code := b.call("POST", "/v1/recovery/report", report, &errBody); code != 409 {
		t.Fatalf("second report: %d %v", code, errBody)
	}

	b.waitState("running", 90*time.Second)
	st := b.state()
	if st.RunningSnapshot != b.gitRev("incident/"+id+"/attempt-1") || st.OpenIncident == nil || st.OpenIncident.State != "probation" || st.OpenIncident.Attempts != 1 {
		t.Fatalf("state after the fix: %+v %+v", st, st.OpenIncident)
	}
	// The agent process that runs now cannot read the recovery credentials.
	if fileExists(b.env.Paths.RecoveryAgentDir) {
		t.Fatalf("%s still exists while the agent runs", b.env.Paths.RecoveryAgentDir)
	}
	if !fileExists(filepath.Join(b.env.Paths.IncidentDir(id), "attempts", "1.md")) {
		t.Fatal("attempts/1.md missing")
	}
	d.WaitOutputContains(t, 30*time.Second, "故障 "+id+" 已由 recovery 修复（第 1 次修复）")
	b.waitFor("incident closed", 30*time.Second, func() bool { return b.state().OpenIncident == nil })
	rec := b.incident(id)
	if rec.Status != "closed" || rec.Resolution == nil || rec.Resolution.Kind != "recovery" {
		t.Fatalf("incident = %+v", rec)
	}
	d.WaitOutputContains(t, 30*time.Second, "故障 "+id+"（启动失败）已关闭")
	b.waitFor("last-good at the attempt", 10*time.Second, func() bool {
		info, _ := b.agentStatus()
		return info.LastGood != nil && *info.LastGood == "incident/"+id+"/attempt-1"
	})
	if !strings.Contains(b.readFile(b.env.Paths.DaemonLog), "incident notice pushed on bridge events") {
		t.Fatal("the incident notice was not pushed to bearbot-im")
	}
	if b.state().Deliver.Incident != nil {
		t.Fatal("the pushed notice is still kept for bridge/ready")
	}
}

// TestRecoveryAttemptsExhausted: a `fixed` report whose snapshot does not pass the daemon's check is sent back to the
// same recovery process as the next prompt; when [recovery] attempts are used up, the bot enters maintenance.
func TestRecoveryAttemptsExhausted(t *testing.T) {
	b := newBed(t, func(e *testenv.Env, c *testenv.Config) {
		dir := e.Paths.RecoveryOverrideDir
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "models.json"), testenv.ModelsJSON(unreachableModel), 0o600); err != nil {
			t.Fatal(err)
		}
		settings := `{"quietStartup": true, "defaultProjectTrust": "never", "defaultProvider": "testprovider", "defaultModel": "unreachable", "retry": {"enabled": true, "maxRetries": 5, "baseDelayMs": 4000}}`
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
		c.RecoveryTimeout = "8s"
		c.RecoveryAttempts = 2
	})
	d := b.start()
	d.WaitOutputContains(t, 90*time.Second, "控制台已连接")
	b.waitState("running", 90*time.Second)
	b.ext("boom.ts", factoryThrowExtension)
	if out, stderr, code := b.cli("restart"); code != 0 {
		t.Fatalf("restart: %d %s %s", code, out, stderr)
	}
	b.waitState("repairing", 60*time.Second)
	id := b.state().OpenIncident.ID
	report := map[string]any{"status": "fixed", "summary": "以为修好了", "changed_files": []string{}, "lesson": "先运行 recovery_check"}
	for n, marker := range []string{"快照仓库：git --git-dir=", "第 1 次修复（快照 incident/" + id + "/attempt-1）没有通过 daemon 的检查"} {
		b.waitFor("prompt "+marker, 60*time.Second, func() bool {
			return fileExists(b.env.Paths.RecoveryLog(id)) && strings.Contains(b.readFile(b.env.Paths.RecoveryLog(id)), marker)
		})
		var accepted map[string]any
		if code := b.call("POST", "/v1/recovery/report", report, &accepted); code != 200 {
			t.Fatalf("report %d: %d %v", n+1, code, accepted)
		}
	}
	b.waitMaintenance(90 * time.Second)
	rec := b.incident(id)
	if !strings.Contains(rec.MaintenanceReason, "第 2 次修复没有通过检查，修复次数（2 次）已经用完") {
		t.Fatalf("maintenance reason = %q", rec.MaintenanceReason)
	}
	tags := b.gitTags()
	for _, tag := range []string{"incident/" + id + "/attempt-1", "incident/" + id + "/attempt-2"} {
		if !contains(tags, tag) {
			t.Fatalf("tag %s missing in %v", tag, tags)
		}
	}
	if !strings.Contains(b.readFile(filepath.Join(b.env.Paths.IncidentDir(id), "attempts", "2.md")), "检查：未通过") {
		t.Fatal("attempts/2.md does not record the failed check")
	}
	// One recovery process handled both prompts.
	if n := strings.Count(b.readFile(b.env.Paths.RecoveryLog(id)), "=== recovery 进程启动于"); n != 1 {
		t.Fatalf("%d recovery processes started", n)
	}
}

const throwOutsideTrialExtension = `import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

export default function (_pi: ExtensionAPI) {
  if (process.env.BEARBOT_TRIAL !== "1") throw new Error("outside the trial");
}
`

// TestRecoveryStartFailureNewProcess (contracts 9.3): the fix passes the check (the extension only throws outside the
// trial) but the agent start fails; a new recovery process with the same session id gets the failure as its prompt;
// the second fix works and the agent runs.
func TestRecoveryStartFailureNewProcess(t *testing.T) {
	b := newBed(t, func(e *testenv.Env, c *testenv.Config) {
		dir := e.Paths.RecoveryOverrideDir
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "models.json"), testenv.ModelsJSON(unreachableModel), 0o600); err != nil {
			t.Fatal(err)
		}
		settings := `{"quietStartup": true, "defaultProjectTrust": "never", "defaultProvider": "testprovider", "defaultModel": "unreachable", "retry": {"enabled": true, "maxRetries": 5, "baseDelayMs": 4000}}`
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
		c.RecoveryTimeout = "8s"
		c.RecoveryAttempts = 2
	})
	d := b.start()
	d.WaitOutputContains(t, 90*time.Second, "控制台已连接")
	b.waitState("running", 90*time.Second)
	ext := b.ext("outside.ts", throwOutsideTrialExtension)
	if out, stderr, code := b.cli("restart"); code != 0 || !strings.Contains(out, "outside the trial") {
		t.Fatalf("restart: %d %s %s", code, out, stderr)
	}
	b.waitState("repairing", 60*time.Second)
	id := b.state().OpenIncident.ID
	report := map[string]any{"status": "fixed", "summary": "检查通过了", "changed_files": []string{}, "lesson": "试运行与正式启动不同"}
	waitPrompt := func(marker string) {
		b.waitFor("prompt "+marker, 90*time.Second, func() bool {
			return fileExists(b.env.Paths.RecoveryLog(id)) && strings.Contains(b.readFile(b.env.Paths.RecoveryLog(id)), marker)
		})
	}
	waitPrompt("快照仓库：git --git-dir=")
	if code := b.call("POST", "/v1/recovery/report", report, nil); code != 200 {
		t.Fatalf("first report: %d", code)
	}
	waitPrompt("通过了检查，但 agent 进程启动失败")
	if err := os.Remove(ext); err != nil {
		t.Fatal(err)
	}
	report["summary"] = "删除了 outside.ts"
	if code := b.call("POST", "/v1/recovery/report", report, nil); code != 200 {
		t.Fatalf("second report: %d", code)
	}
	b.waitState("running", 90*time.Second)
	log := b.readFile(b.env.Paths.RecoveryLog(id))
	if n := strings.Count(log, "=== recovery 进程启动于"); n != 2 {
		t.Fatalf("%d recovery processes started:\n%s", n, log)
	}
	if !strings.Contains(log, "outside the trial") {
		t.Fatal("the start failure did not reach recovery")
	}
	sessions, _ := filepath.Glob(filepath.Join(b.env.Paths.RecoverySessionsDir, "*.jsonl"))
	if len(sessions) != 1 || !strings.HasSuffix(sessions[0], "_"+id+".jsonl") {
		t.Fatalf("recovery sessions %v", sessions)
	}
	rec := b.incident(id)
	if len(rec.Attempts) != 2 || rec.Attempts[0].Start == nil || rec.Attempts[0].Start.OK || rec.Attempts[1].Start == nil || !rec.Attempts[1].Start.OK {
		t.Fatalf("attempts = %+v", rec.Attempts)
	}
	b.waitFor("incident closed", 30*time.Second, func() bool { return b.state().OpenIncident == nil })
}

// TestRecoveryTimeoutAndCannotFix: without a report within [recovery] timeout the turn is aborted and the bot enters
// maintenance; after `/bb repair`, a cannot_fix report enters maintenance with recovery's summary.
func TestRecoveryTimeoutAndCannotFix(t *testing.T) {
	b := newBed(t, func(e *testenv.Env, c *testenv.Config) {
		dir := e.Paths.RecoveryOverrideDir
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "models.json"), testenv.ModelsJSON(unreachableModel), 0o600); err != nil {
			t.Fatal(err)
		}
		settings := `{"quietStartup": true, "defaultProjectTrust": "never", "defaultProvider": "testprovider", "defaultModel": "unreachable", "retry": {"enabled": true, "maxRetries": 5, "baseDelayMs": 4000}}`
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
		c.RecoveryTimeout = "5s"
	})
	d := b.start()
	d.WaitOutputContains(t, 90*time.Second, "控制台已连接")
	b.waitState("running", 90*time.Second)
	b.ext("boom.ts", factoryThrowExtension)
	if _, _, code := b.cli("restart"); code != 0 {
		t.Fatal("restart failed")
	}
	id := b.waitMaintenance(60 * time.Second)
	if rec := b.incident(id); rec.MaintenanceReason != "recovery 在 5s 内没有提交修复报告" {
		t.Fatalf("maintenance reason = %q", rec.MaintenanceReason)
	}

	skip := len(d.Output())
	d.Send(t, "/bb repair 第二次")
	b.waitState("repairing", 30*time.Second)
	b.waitFor("second prompt", 60*time.Second, func() bool {
		return strings.Contains(b.readFile(b.env.Paths.RecoveryLog(id)), "管理员说明：第二次")
	})
	report := map[string]any{"status": "cannot_fix", "summary": "boom.ts 需要管理员决定", "changed_files": []string{}, "lesson": "无"}
	if code := b.call("POST", "/v1/recovery/report", report, nil); code != 200 {
		t.Fatalf("report: %d", code)
	}
	b.outputAfter(skip, "故障 "+id+" 没有修复，bot 仍处于维护状态", 60*time.Second)
	b.waitMaintenance(10 * time.Second)
	if rec := b.incident(id); rec.MaintenanceReason != "recovery 报告无法修复：boom.ts 需要管理员决定" || len(rec.Attempts) != 0 {
		t.Fatalf("incident = %+v", rec)
	}
}

// TestRepairContinuesAfterRestart: SIGTERM during REPAIRING stops recovery and the daemon; the next start of
// bearbotd runs recovery again for the open incident.
func TestRepairContinuesAfterRestart(t *testing.T) {
	b := newBed(t, func(e *testenv.Env, c *testenv.Config) {
		dir := e.Paths.RecoveryOverrideDir
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "models.json"), testenv.ModelsJSON(unreachableModel), 0o600); err != nil {
			t.Fatal(err)
		}
		settings := `{"quietStartup": true, "defaultProjectTrust": "never", "defaultProvider": "testprovider", "defaultModel": "unreachable", "retry": {"enabled": true, "maxRetries": 5, "baseDelayMs": 4000}}`
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
		c.RecoveryTimeout = "12s"
	})
	d := b.start()
	b.waitState("running", 90*time.Second)
	b.ext("boom.ts", factoryThrowExtension)
	if _, _, code := b.cli("restart"); code != 0 {
		t.Fatal("restart failed")
	}
	b.waitState("repairing", 60*time.Second)
	id := b.state().OpenIncident.ID
	b.waitFor("first prompt", 60*time.Second, func() bool {
		return fileExists(b.env.Paths.RecoveryLog(id)) && strings.Contains(b.readFile(b.env.Paths.RecoveryLog(id)), "=== 提示结束 ===")
	})
	start := time.Now()
	d.Stop(t)
	if exit := d.Process().Exit(); !exit.Success() || time.Since(start) > 30*time.Second {
		t.Fatalf("bearbotd exit %s after %v", exit, time.Since(start))
	}
	if st := b.state(); st.OpenIncident == nil || st.OpenIncident.State != "repairing" || st.Maintenance != nil {
		t.Fatalf("state after the stop: %+v", st.OpenIncident)
	}

	d = b.start()
	d.WaitOutputContains(t, 90*time.Second, "bearbotd 重新启动时故障 "+id+"（启动失败）还在修复中，重新运行 recovery")
	b.waitState("repairing", 30*time.Second)
	b.waitFor("second recovery process", 60*time.Second, func() bool {
		return strings.Count(b.readFile(b.env.Paths.RecoveryLog(id)), "=== 提示结束 ===") == 2
	})
	report := map[string]any{"status": "cannot_fix", "summary": "交给管理员", "changed_files": []string{}, "lesson": "无"}
	if code := b.call("POST", "/v1/recovery/report", report, nil); code != 200 {
		t.Fatalf("report: %d", code)
	}
	b.waitMaintenance(60 * time.Second)
	if rec := b.incident(id); len(rec.Repairs) != 2 {
		t.Fatalf("repairs = %+v", rec.Repairs)
	}

	// An administrator's repair that the stop of bearbotd interrupts continues with the same note.
	d.Send(t, "/bb repair 说明X")
	b.waitState("repairing", 30*time.Second)
	b.waitFor("prompt with the note", 60*time.Second, func() bool {
		return strings.Count(b.readFile(b.env.Paths.RecoveryLog(id)), "=== 提示结束 ===") == 3
	})
	d.Stop(t)
	d = b.start()
	b.waitState("repairing", 90*time.Second)
	b.waitFor("prompt after the restart", 60*time.Second, func() bool {
		return strings.Count(b.readFile(b.env.Paths.RecoveryLog(id)), "=== 提示结束 ===") == 4
	})
	if prompt := lastPrompt(b.readFile(b.env.Paths.RecoveryLog(id))); !strings.Contains(prompt, "管理员说明：说明X") {
		t.Fatalf("the continued run lost the note:\n%s", prompt)
	}
	if rec := b.incident(id); len(rec.Repairs) != 4 || rec.Repairs[3].Note != "说明X" {
		t.Fatalf("repairs = %+v", rec.Repairs)
	}
	if code := b.call("POST", "/v1/recovery/report", report, nil); code != 200 {
		t.Fatalf("report: %d", code)
	}
	b.waitMaintenance(60 * time.Second)
}

// lastPrompt returns the last prompt the daemon sent to recovery in a recovery log.
func lastPrompt(log string) string {
	start := strings.LastIndex(log, "=== daemon 发送提示")
	end := strings.LastIndex(log, "=== 提示结束 ===")
	if start < 0 || end < start {
		return ""
	}
	return log[start:end]
}

// recoveryOverride writes the recovery profile overrides with the unreachable test model; retry tells whether pi
// retries the refused model request (which keeps the recovery turn open).
func recoveryOverride(t *testing.T, e *testenv.Env, retry bool) {
	t.Helper()
	dir := e.Paths.RecoveryOverrideDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "models.json"), testenv.ModelsJSON(unreachableModel), 0o600); err != nil {
		t.Fatal(err)
	}
	retrySettings := `{"enabled": false}`
	if retry {
		retrySettings = `{"enabled": true, "maxRetries": 5, "baseDelayMs": 4000}`
	}
	settings := `{"quietStartup": true, "defaultProjectTrust": "never", "defaultProvider": "testprovider", "defaultModel": "unreachable", "retry": ` + retrySettings + `}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAppliedNotConfirmedAfterRepair (contracts 6 `applied` once after the apply restart, DESIGN 9.4): the restart of
// an apply fails and recovery repairs it. The repaired process gets no applied: the apply never took effect.
func TestAppliedNotConfirmedAfterRepair(t *testing.T) {
	b := newBed(t, func(e *testenv.Env, c *testenv.Config) {
		recoveryOverride(t, e, true)
		c.RecoveryTimeout = "8s"
	})
	b.start()
	b.waitState("running", 90*time.Second)
	ext := b.ext("outside.ts", throwOutsideTrialExtension)
	b.apply("启动时抛出异常的扩展", "apply/1")
	b.waitState("repairing", 90*time.Second)
	if st := b.state(); st.Deliver.Applied != nil {
		t.Fatalf("deliver.applied during the repair: %+v", st.Deliver.Applied)
	}
	id := b.state().OpenIncident.ID
	b.waitFor("prompt sent to recovery", 60*time.Second, func() bool {
		return fileExists(b.env.Paths.RecoveryLog(id)) && strings.Contains(b.readFile(b.env.Paths.RecoveryLog(id)), "=== 提示结束 ===")
	})
	if err := os.Remove(ext); err != nil {
		t.Fatal(err)
	}
	report := map[string]any{"status": "fixed", "summary": "删除了 outside.ts", "changed_files": []string{ext}, "lesson": "试运行与正式启动不同"}
	if code := b.call("POST", "/v1/recovery/report", report, nil); code != 200 {
		t.Fatalf("report: %d", code)
	}
	b.waitState("running", 90*time.Second)
	if ready := b.lastLine(b.env.Paths.DaemonLog, "msg=bridge/ready"); !strings.Contains(ready, "applied=false") {
		t.Fatalf("bridge/ready of the repaired process: %s", ready)
	}
}

// TestRollbackFailureWithOpenIncident (DESIGN 5.8, 10.4): a rollback that fails while an incident is open (after the
// incident's repair ended in maintenance) moves that incident to maintenance with the rollback's reason; the notice
// names it, and the next repair continues it with the reason in the prompt.
func TestRollbackFailureWithOpenIncident(t *testing.T) {
	b := newBed(t, func(e *testenv.Env, c *testenv.Config) { recoveryOverride(t, e, false) })
	d := b.start()
	d.WaitOutputContains(t, 90*time.Second, "控制台已连接")
	b.waitState("running", 90*time.Second)
	dep := b.env.Paths.WorkspaceDir + "/.pi/extensions/dep"
	b.env.WriteFile(t, "/bearbot/workspace/.pi/extensions/dep/index.ts", goodExtension)
	b.env.WriteFile(t, "/bearbot/workspace/.pi/extensions/dep/package.json", `{"name": "dep", "version": "1.0.0", "private": true, "dependencies": {"bearbot-e2e-no-such-package-7f3a": "1.0.0"}}`+"\n")
	b.apply("依赖不存在的包", "apply/1")
	b.waitRunningWith(0, "apply/1", 60*time.Second)
	if err := os.RemoveAll(dep); err != nil {
		t.Fatal(err)
	}
	b.ext("boom.ts", factoryThrowExtension)
	if out, stderr, code := b.cli("restart"); code != 0 || !strings.Contains(out, "agent 重启失败") {
		t.Fatalf("restart: %d %s %s", code, out, stderr)
	}
	id := b.waitMaintenance(120 * time.Second)

	lines := len(d.Output())
	out, stderr, code := b.cli("rollback", "apply/1")
	if code != 0 || !strings.Contains(out, "回滚到 apply/1 时恢复文件失败") || !strings.Contains(out, "bot 已进入维护状态") {
		t.Fatalf("rollback: %d\n%s\n%s", code, out, stderr)
	}
	b.outputAfter(lines, "未关闭的故障："+id+"（启动失败），执行 repair 时继续修复这个故障", 30*time.Second)
	st := b.state()
	if st.Maintenance == nil || st.Maintenance.IncidentID != nil || st.OpenIncident == nil || st.OpenIncident.ID != id || st.OpenIncident.State != "maintenance" {
		t.Fatalf("state after the failed rollback: %+v %+v", st.Maintenance, st.OpenIncident)
	}
	rec := b.incident(id)
	if rec.Status != "maintenance" || !strings.Contains(rec.MaintenanceReason, "回滚到 apply/1 时恢复文件失败") || rec.Resolution != nil {
		t.Fatalf("incident after the failed rollback: %+v", rec)
	}
	if out, _, _ := b.cli("incident", "show", id); !strings.Contains(out, "进入维护状态的原因：回滚到 apply/1 时恢复文件失败") {
		t.Fatalf("incident show:\n%s", out)
	}

	out, stderr, code = b.cli("repair", "说明Y")
	if code != 0 || !strings.Contains(out, "故障 "+id+" 没有修复") {
		t.Fatalf("repair: %d\n%s\n%s", code, out, stderr)
	}
	prompt := lastPrompt(b.readFile(b.env.Paths.RecoveryLog(id)))
	if !strings.Contains(prompt, "进入维护状态的原因：回滚到 apply/1 时恢复文件失败") || !strings.Contains(prompt, "管理员说明：说明Y") {
		t.Fatalf("prompt of the repair:\n%s", prompt)
	}
	if rec := b.incident(id); !strings.Contains(rec.Repairs[len(rec.Repairs)-1].MaintenanceReason, "回滚到 apply/1") {
		t.Fatalf("repairs = %+v", rec.Repairs)
	}
}

// TestRecoveryWithModel needs a model service: recovery repairs an extension that throws in its factory, the agent
// starts in probation, the incident closes and its notice reaches bearbot-im.
func TestRecoveryWithModel(t *testing.T) {
	m := testenv.RequireModel(t)
	b := newBed(t, func(e *testenv.Env, c *testenv.Config) {
		e.InstallModel(t, m)
		c.ClassifierModel = m.Ref()
		c.RecoveryTimeout = "10m"
		c.Probation = "5s"
	})
	b.start()
	b.waitState("running", 90*time.Second)
	b.ext("boom.ts", factoryThrowExtension)
	if out, stderr, code := b.cli("restart"); code != 0 || !strings.Contains(out, "agent 重启失败") {
		t.Fatalf("restart: %d %s %s", code, out, stderr)
	}
	b.waitFor("incident closed", 20*time.Minute, func() bool {
		st := b.state()
		return st.OpenIncident == nil && st.Maintenance == nil && b.fixedIncidentClosed()
	})
	if !strings.Contains(b.readFile(b.env.Paths.DaemonLog), "incident notice") {
		t.Fatal("the incident notice was not delivered")
	}
}

// fixedIncidentClosed reports whether at least one incident record is closed after a fix by recovery.
func (b *bed) fixedIncidentClosed() bool {
	dirs, err := filepath.Glob(filepath.Join(b.env.Paths.IncidentsDir, "inc-*"))
	if err != nil {
		b.t.Fatal(err)
	}
	for _, dir := range dirs {
		rec := b.incident(filepath.Base(dir))
		if rec.Status == "closed" && rec.Resolution != nil && rec.Resolution.Kind == "recovery" {
			return true
		}
	}
	return false
}
