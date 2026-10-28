package e2e

import (
	"os"
	"strings"
	"testing"
	"time"
)

// exitLaterExtension exits with code 7 once the test created the marker file .go-exit in the workspace (outside the
// trial), so the exit comes after the agent became ready.
const exitLaterExtension = `import { existsSync, unlinkSync } from "node:fs";
import { join } from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

export default function (pi: ExtensionAPI) {
  pi.on("session_start", () => {
    if (process.env.BEARBOT_TRIAL === "1") return;
    const marker = join(process.env.BEARBOT_WORKSPACE_DIR ?? "", ".go-exit");
    setInterval(() => {
      if (existsSync(marker)) {
        unlinkSync(marker);
        process.exit(7);
      }
    }, 100);
  });
}
`

// marker creates a marker file in the workspace that a test extension waits for.
func (b *bed) marker(name string) {
	b.t.Helper()
	b.env.WriteFile(b.t, "/bearbot/workspace/"+name, "")
}

// TestProbationExitAndDefaultRollback: an applied extension that exits soon after the start makes the process exit
// in probation: incident, maintenance (no recovery credentials). The default rollback target apply/1^ equals
// last-good; rolling back to it restores the files. A second rollback before the acknowledgement is merged into the
// record.
func TestProbationExitAndDefaultRollback(t *testing.T) {
	b := newBed(t, nil)
	d := b.start()
	d.WaitOutputContains(t, 90*time.Second, "控制台已连接")
	b.waitState("running", 90*time.Second)
	b.waitFor("last-good at init", 30*time.Second, func() bool {
		info, _ := b.agentStatus()
		return info.LastGood != nil && *info.LastGood == "init"
	})

	ext := b.ext("exit-later.ts", exitLaterExtension)
	var applied struct {
		Accepted bool   `json:"accepted"`
		Snapshot string `json:"snapshot"`
	}
	if code := b.call("POST", "/v1/apply", map[string]any{"summary": "会退出的扩展", "origin": map[string]any{"thread": "console:main", "requester": "console:operator"}}, &applied); code != 200 || !applied.Accepted {
		t.Fatalf("apply: %d %+v", code, applied)
	}
	// The apply restart is ready (probation runs for 4 s); then the extension exits.
	b.waitRunningWith(0, "apply/1", 60*time.Second)
	b.marker(".go-exit")
	id := b.waitMaintenance(90 * time.Second)
	rec := b.incident(id)
	if rec.Trigger != "观察期内退出" || rec.Exit == nil || rec.Exit.Code == nil || *rec.Exit.Code != 7 {
		t.Fatalf("incident = %+v", rec)
	}
	if b.gitRev("apply/1^") != b.gitRev("last-good") {
		t.Fatal("the default target apply/1^ is not last-good")
	}
	out, _, _ := b.cli("snapshot", "list")
	if !strings.Contains(out, "rollback 不指定快照时的默认目标：apply/1^") {
		t.Fatalf("snapshot list:\n%s", out)
	}

	b.console("/bb rollback", "把 bot 回滚到了快照 apply/1^", 90*time.Second)
	b.waitState("running", 30*time.Second)
	if fileExists(ext) {
		t.Fatal("exit-later.ts still there")
	}
	r := b.state().Rollback
	if r == nil || r.ID != 1 || r.Target != "apply/1^" || len(r.Reverted) != 1 || r.Reverted[0].Ref != "apply/1" || r.Reverted[0].Summary != "会退出的扩展" ||
		len(r.Requesters) != 1 || r.Requesters[0] != "console:operator" || r.Thread == nil || *r.Thread != "console:main" {
		t.Fatalf("rollback record = %+v", r)
	}
	if b.state().Maintenance != nil {
		t.Fatal("still in maintenance")
	}
	// The applied extension is still in the snapshot history.
	b.console("/bb snapshot diff init apply/1", "workspace/.pi/extensions/exit-later.ts", 30*time.Second)

	// A second rollback from the command line before bearbot-im acknowledged the first one.
	out, stderr, code := b.cli("rollback", "init")
	if code != 0 || !strings.Contains(out, "管理员通过命令行把 bot 回滚到了快照 init") || !strings.Contains(out, "被撤销的应用：apply/1：会退出的扩展") {
		t.Fatalf("rollback init: %d\n%s\n%s", code, out, stderr)
	}
	r = b.state().Rollback
	if r == nil || r.ID != 2 || r.Target != "init" || r.Saved != "rollback/2/before" || r.Thread != nil || len(r.Requesters) != 1 || r.Requesters[0] != "console:operator" || len(r.Reverted) != 1 || !r.Notified {
		t.Fatalf("merged rollback record = %+v", r)
	}
	tags := b.gitTags()
	for _, tag := range []string{"apply/1", "rollback/1/before", "rollback/1/after", "rollback/2/before", "rollback/2/after"} {
		if !contains(tags, tag) {
			t.Fatalf("tag %s missing in %v", tag, tags)
		}
	}
	// rollback/1/before keeps the reverted extension.
	files := b.gitFiles("rollback/1/before")
	if !contains(files, "workspace/.pi/extensions/exit-later.ts") {
		t.Fatalf("rollback/1/before files %v", files)
	}
	b.waitFor("last-good at rollback/2/after", 30*time.Second, func() bool {
		info, _ := b.agentStatus()
		return info.LastGood != nil && *info.LastGood == "rollback/2/after"
	})
	if rec := b.incident(id); rec.Status != "closed" {
		t.Fatalf("incident after the rollbacks: %+v", rec)
	}
	if b.state().OpenIncident != nil {
		t.Fatal("incident still open")
	}
}

// TestRollbackNpmFailure (DESIGN 16): the restored package.json references a package that does not exist, npm
// install fails, and the rollback enters the maintenance state with the failing step and npm's output.
func TestRollbackNpmFailure(t *testing.T) {
	b := newBed(t, nil)
	b.start()
	b.waitState("running", 90*time.Second)
	dir := b.env.Paths.WorkspaceDir + "/.pi/extensions/dep"
	b.env.WriteFile(t, "/bearbot/workspace/.pi/extensions/dep/index.ts", goodExtension)
	b.env.WriteFile(t, "/bearbot/workspace/.pi/extensions/dep/package.json", `{"name": "dep", "version": "1.0.0", "private": true, "dependencies": {"bearbot-e2e-no-such-package-7f3a": "1.0.0"}}`+"\n")
	var applied map[string]any
	if code := b.call("POST", "/v1/apply", map[string]any{"summary": "依赖不存在的包", "origin": map[string]any{"thread": nil, "requester": nil}}, &applied); code != 200 || applied["accepted"] != true {
		t.Fatalf("apply: %d %v", code, applied)
	}
	b.waitFor("apply restart", 60*time.Second, func() bool {
		info, code := b.agentStatus()
		return code == 200 && info.State == "running" && info.RunningSnapshot != nil && *info.RunningSnapshot == "apply/1"
	})
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	out, stderr, code := b.cli("rollback", "apply/1")
	if code != 0 || !strings.Contains(out, "回滚到 apply/1 时恢复文件失败") || !strings.Contains(out, "npm install") || !strings.Contains(out, "bot 已进入维护状态") {
		t.Fatalf("rollback: %d\n%s\n%s", code, out, stderr)
	}
	b.waitState("maintenance", 30*time.Second)
	st := b.state()
	if st.Maintenance == nil || st.Maintenance.IncidentID != nil || !strings.Contains(st.Maintenance.Reason, "npm install") {
		t.Fatalf("maintenance = %+v", st.Maintenance)
	}
	if !contains(b.gitTags(), "rollback/1/before") || contains(b.gitTags(), "rollback/1/after") {
		t.Fatalf("tags %v", b.gitTags())
	}
	// repair without an incident opens one with the trigger 管理员要求; recovery has no credentials.
	out, _, code = b.cli("repair")
	if code != 0 || !strings.Contains(out, "没有修复，bot 仍处于维护状态") {
		t.Fatalf("repair: %d %s", code, out)
	}
	st = b.state()
	if st.OpenIncident == nil || st.OpenIncident.Trigger != "管理员要求" {
		t.Fatalf("incident after repair: %+v", st.OpenIncident)
	}
}

// TestRollbackDuringApplyCheck (DESIGN 16): a rollback that arrives while an apply runs its check waits for the
// change lock, then runs; the apply returns an error and commits no snapshot. The open member turn is compared when
// the rollback stops the agent.
func TestRollbackDuringApplyCheck(t *testing.T) {
	b := newBed(t, nil)
	b.start()
	b.waitState("running", 90*time.Second)
	original := b.readFile(b.env.Paths.ClassifierMD)
	if code := b.call("POST", "/v1/classifier/turn", map[string]any{"phase": "start", "turnId": "member-turn", "senders": []string{"telegram:9"}}, nil); code != 200 {
		t.Fatalf("turn start: %d", code)
	}
	if err := os.WriteFile(b.env.Paths.ClassifierMD, []byte("成员改的\n"), 0o664); err != nil {
		t.Fatal(err)
	}
	b.ext("good.ts", goodExtension)

	checks := b.checkDirs()
	applyDone := b.callAsync("POST", "/v1/apply", map[string]any{"summary": "检查期间被回滚", "origin": map[string]any{"thread": nil, "requester": nil}})
	// The rollback arrives while the check of this apply runs.
	b.waitCheckRunning(checks)
	out, stderr, code := b.cli("rollback", "init")
	if code != 0 || !strings.Contains(out, "回滚到了快照 init") {
		t.Fatalf("rollback: %d\n%s\n%s", code, out, stderr)
	}
	if msg := b.errorText("apply", <-applyDone, 409); !strings.Contains(msg, "管理员正在回滚，这次应用已取消") {
		t.Fatalf("apply answer: %s", msg)
	}
	for _, tag := range b.gitTags() {
		if strings.HasPrefix(tag, "apply/") {
			t.Fatalf("apply snapshot %s committed", tag)
		}
	}
	if got := b.readFile(b.env.Paths.ClassifierMD); got != original {
		t.Fatalf("CLASSIFIER.md not restored when the rollback stopped the agent: %q", got)
	}
	if !strings.Contains(out, "AGENTS.md 与 CLASSIFIER.md 保持回滚之前的内容，与目标快照相同") {
		t.Fatalf("rollback text:\n%s", out)
	}
	b.waitState("running", 30*time.Second)
	if !strings.Contains(b.readFile(b.env.Paths.DaemonLog), "classifierRestore=true") {
		t.Fatal("classifierRestore not delivered after the rollback")
	}
}

// commandExtension is goodExtension with another command name.
func commandExtension(name string) string { return strings.ReplaceAll(goodExtension, "e2e-good", name) }

// TestRequestsQueuedBehindRollback (DESIGN 5.6 apply step 1, 5.12): an apply and a restore of the agent process wait
// for the change lock behind a rollback. After the rollback stopped that process and started a new one, both are
// refused: no apply/ snapshot is committed and the restored files stay as the rollback left them.
func TestRequestsQueuedBehindRollback(t *testing.T) {
	b := newBed(t, nil)
	b.start()
	b.waitState("running", 90*time.Second)
	good := b.ext("good.ts", goodExtension)
	b.apply("添加 e2e-good 命令", "apply/1")
	b.waitRunningWith(0, "apply/1", 60*time.Second)
	good2 := b.ext("good2.ts", commandExtension("e2e-good-2"))

	// A check holds the change lock; the rollback waits for it, the apply and the restore wait behind the rollback.
	checks := b.checkDirs()
	checkDone := b.callAsync("POST", "/v1/check", map[string]any{})
	b.waitCheckRunning(checks)
	rollbackDone := b.cliAsync("rollback", "init")
	b.waitFor("rollback waits for the change lock", 30*time.Second, func() bool {
		return b.countLines(b.env.Paths.DaemonLog, "rollback waits for the change lock") == 1
	})
	applyDone := b.callAsync("POST", "/v1/apply", map[string]any{"summary": "排在回滚之后的应用", "origin": map[string]any{"thread": nil, "requester": nil}})
	restoreDone := b.callAsync("POST", "/v1/snapshot", map[string]any{"action": "restore", "ref": "apply/1", "paths": []string{"workspace/.pi/extensions/good.ts"}})

	if a := <-checkDone; a.err != nil || a.code != 200 {
		t.Fatalf("check: %d %s %v", a.code, a.data, a.err)
	}
	rb := <-rollbackDone
	if rb.err != nil || rb.code != 0 || !strings.Contains(rb.out, "回滚到了快照 init") {
		t.Fatalf("rollback: %d %v\n%s\n%s", rb.code, rb.err, rb.out, rb.errOut)
	}
	if msg := b.errorText("apply", <-applyDone, 409); !strings.Contains(msg, "回滚") || !strings.Contains(msg, "没有提交快照") {
		t.Fatalf("apply after the rollback: %s", msg)
	}
	if msg := b.errorText("restore", <-restoreDone, 409); !strings.Contains(msg, "回滚") || !strings.Contains(msg, "没有改动文件") {
		t.Fatalf("restore after the rollback: %s", msg)
	}
	if contains(b.gitTags(), "apply/2") {
		t.Fatalf("apply/2 committed after the rollback: %v", b.gitTags())
	}
	if fileExists(good) || fileExists(good2) {
		t.Fatal("the files after the rollback were changed by a request that waited behind it")
	}
	b.waitState("running", 30*time.Second)
	if st := b.state(); st.RunningSnapshot != b.gitRev("rollback/1/after") || st.PendingApply != nil {
		t.Fatalf("state after the rollback: running %s, pending %+v", st.RunningSnapshot, st.PendingApply)
	}
}

// TestNoAppliedAfterFailedApplyStart (contracts 6 `applied` once after the apply restart): the restart of an apply
// fails, the incident ends in maintenance (no recovery credentials) and a rollback reverts the apply. The apply is
// never confirmed as applied: state.json keeps no deliver.applied, and the bridge/ready after the rollback has no
// applied.
func TestNoAppliedAfterFailedApplyStart(t *testing.T) {
	b := newBed(t, nil)
	b.start()
	b.waitState("running", 90*time.Second)
	b.ext("outside.ts", throwOutsideTrialExtension)
	b.apply("启动时抛出异常的扩展", "apply/1")
	b.waitMaintenance(90 * time.Second)
	if st := b.state(); st.Deliver.Applied != nil || st.PendingApply != nil {
		t.Fatalf("maintenance: deliver.applied %+v, pendingApply %+v", st.Deliver.Applied, st.PendingApply)
	}
	out, stderr, code := b.cli("rollback")
	if code != 0 || !strings.Contains(out, "把 bot 回滚到了快照 apply/1^") || !strings.Contains(out, "被撤销的应用：apply/1：启动时抛出异常的扩展") {
		t.Fatalf("rollback: %d\n%s\n%s", code, out, stderr)
	}
	b.waitState("running", 30*time.Second)
	ready := b.lastLine(b.env.Paths.DaemonLog, "msg=bridge/ready")
	if !strings.Contains(ready, "applied=false") || !strings.Contains(ready, "rollback=true") {
		t.Fatalf("bridge/ready after the rollback: %s", ready)
	}
}

// TestRollbackDocumentsAndSecondApply (DESIGN 16 管理员回滚): AGENTS.md changed after the apply stays as it is and the
// reply lists its difference from the target; the console shows rolling_back. After a second apply, a rollback
// without a snapshot goes to the state before the second apply, which is the first rollback's after.
func TestRollbackDocumentsAndSecondApply(t *testing.T) {
	b := newBed(t, nil)
	d := b.start()
	d.WaitOutputContains(t, 90*time.Second, "控制台已连接")
	b.waitState("running", 90*time.Second)
	first := b.ext("first.ts", commandExtension("e2e-first"))
	b.apply("第一次应用", "apply/1")
	b.waitRunningWith(0, "apply/1", 60*time.Second)
	memory := b.readFile(b.env.Paths.AgentsMD) + "\n- 回滚之前写入的记忆\n"
	if err := os.WriteFile(b.env.Paths.AgentsMD, []byte(memory), 0o664); err != nil {
		t.Fatal(err)
	}

	lines := len(d.Output())
	b.console("/bb rollback", "把 bot 回滚到了快照 apply/1^", 90*time.Second)
	b.outputAfter(lines, "它们与目标快照之间的差别", 30*time.Second)
	b.outputAfter(lines, "workspace/AGENTS.md", 30*time.Second)
	b.outputAfter(lines, "agent：running → rolling_back", 30*time.Second)
	b.waitState("running", 30*time.Second)
	if b.readFile(b.env.Paths.AgentsMD) != memory {
		t.Fatal("the rollback changed AGENTS.md")
	}
	if fileExists(first) {
		t.Fatal("first.ts still there after the rollback")
	}

	b.ext("second.ts", commandExtension("e2e-second"))
	b.apply("第二次应用", "apply/2")
	b.waitRunningWith(0, "apply/2", 60*time.Second)
	out, stderr, code := b.cli("rollback")
	if code != 0 || !strings.Contains(out, "把 bot 回滚到了快照 apply/2^") || !strings.Contains(out, "被撤销的应用：apply/2：第二次应用") {
		t.Fatalf("default rollback after the second apply: %d\n%s\n%s", code, out, stderr)
	}
	if b.gitRev("apply/2^") != b.gitRev("rollback/1/after") {
		t.Fatal("apply/2^ is not rollback/1/after")
	}
	if r := b.state().Rollback; r == nil || r.Target != "apply/2^" || r.ID != 2 {
		t.Fatalf("rollback record = %+v", r)
	}
	var restored, want []string
	for _, f := range b.gitFiles("rollback/2/after") {
		if strings.HasPrefix(f, "workspace/.pi/") {
			restored = append(restored, f)
		}
	}
	for _, f := range b.gitFiles("rollback/1/after") {
		if strings.HasPrefix(f, "workspace/.pi/") {
			want = append(want, f)
		}
	}
	if strings.Join(restored, " ") != strings.Join(want, " ") || contains(restored, "workspace/.pi/extensions/second.ts") {
		t.Fatalf("files after the second rollback %v, want those of rollback/1/after %v", restored, want)
	}
	b.waitState("running", 30*time.Second)
}
