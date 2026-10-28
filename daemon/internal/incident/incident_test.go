package incident

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bearbot/daemon/internal/check"
	"bearbot/daemon/internal/layout"
	"bearbot/daemon/internal/proc"
	"bearbot/daemon/internal/state"
	"bearbot/daemon/internal/testenv"
	"bearbot/daemon/internal/wire"
)

func prepare(t *testing.T) *testenv.Env {
	t.Helper()
	e := testenv.New(t)
	u := e.Users(t)
	for _, d := range e.Paths.Dirs() {
		if d.Path == e.Paths.RunDir {
			continue
		}
		if _, err := layout.EnsureDir(d, u); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func TestRunHooks(t *testing.T) {
	e := prepare(t)
	out := filepath.Join(e.Root, "hook.out")
	script := filepath.Join(e.Paths.ConfigDir, "hook.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\npwd > "+out+"\nenv | grep '^BEARBOT_' | sort >> "+out+"\necho \"PATH=$PATH\" >> "+out+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	ev := HookEvent{Event: EventMaintenanceEnter, IncidentID: "inc-20261027083000", Reason: "原因\n第二行", Report: "报告"}
	start := time.Now()
	RunHooks(context.Background(), e.Paths, []string{"exit 3", "sleep 30", script}, time.Second, ev, log)
	if time.Since(start) > 10*time.Second {
		t.Fatal("the timeout did not stop the sleeping hook")
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the hook after the failing ones did not run: %v\n%s", err, logs.String())
	}
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if lines[0] != e.Paths.ConfigDir {
		t.Fatalf("cwd %q", lines[0])
	}
	text := string(got)
	for _, want := range []string{"BEARBOT_EVENT=maintenance_enter", "BEARBOT_INCIDENT_ID=inc-20261027083000", "BEARBOT_REASON=原因", "BEARBOT_REPORT=报告", "PATH=" + layout.RootPATH} {
		if !strings.Contains(text, want) {
			t.Fatalf("hook output lacks %q:\n%s", want, text)
		}
	}
	for _, want := range []string{"maintenance hook failed", "退出码 3", "超过"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log lacks %q:\n%s", want, logs.String())
		}
	}
}

func TestRecordFiles(t *testing.T) {
	e := prepare(t)
	p := e.Paths
	if text, err := ListText(p); err != nil || text != "还没有故障记录\n" {
		t.Fatalf("empty list = %q %v", text, err)
	}
	id1, err := newID(p)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := newID(p)
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 || !ValidID(id1) || !ValidID(id2) {
		t.Fatalf("ids %s %s", id1, id2)
	}
	if err := os.Remove(p.IncidentDir(id2)); err != nil {
		t.Fatal(err)
	}
	code := 1
	thread, requester := "telegram:-100", "telegram:1"
	rec := &Record{
		ID: id1, CreatedAt: time.Now(), Trigger: TriggerStartFailure, Detail: "进程退出",
		Exit:       &proc.ExitStatus{Code: &code},
		StderrTail: []string{"Error: Failed to load extension"},
		LastApply:  &state.Applied{Snapshot: "apply/3", Summary: "加了一个扩展", Origin: state.Origin{Thread: &thread, Requester: &requester}},
		Origin:     state.Origin{Thread: &thread, Requester: &requester},
		DiffStat:   " workspace/.pi/extensions/a.ts | 3 +++",
		Before:     "incident/" + id1 + "/before",
		Status:     StatusMaintenance, MaintenanceReason: "recovery 无法运行",
		Repairs: []Repair{{StartedAt: time.Now(), Note: "看看 a.ts", Result: "进入维护状态"}},
	}
	a := Attempt{N: 1, Snapshot: "incident/" + id1 + "/attempt-1", Commit: strings.Repeat("a", 40), ReportedAt: time.Now(),
		Report: wire.RecoveryReport{Status: wire.ReportFixed, Summary: "把 a.ts 移到 .disabled", ChangedFiles: []string{"/a.ts"}, Lesson: "工厂函数不要抛出异常"},
		Check:  &CheckSummary{ID: "chk-1", Passed: false, ReportPath: "/r.json", Problems: "试运行未通过"}}
	if err := rec.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := rec.SaveAttempt(p, a); err != nil {
		t.Fatal(err)
	}
	attempt, err := os.ReadFile(filepath.Join(p.IncidentDir(id1), "attempts", "1.md"))
	if err != nil || !strings.Contains(string(attempt), "工厂函数不要抛出异常") || !strings.Contains(string(attempt), "检查：未通过（chk-1") {
		t.Fatalf("attempts/1.md = %s %v", attempt, err)
	}
	loaded, err := Load(p, id1)
	if err != nil || loaded.Trigger != TriggerStartFailure || *loaded.Exit.Code != 1 || loaded.LastApply.Snapshot != "apply/3" {
		t.Fatalf("loaded = %+v %v", loaded, err)
	}
	report, err := os.ReadFile(filepath.Join(p.IncidentDir(id1), "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# 故障 " + id1, "状态：维护状态", "触发：启动失败", "退出码 1", "快照 apply/3，摘要 加了一个扩展，发起者 telegram:1，会话串 telegram:-100", "管理员说明：看看 a.ts", "recovery 无法运行"} {
		if !strings.Contains(string(report), want) {
			t.Errorf("report.md lacks %q:\n%s", want, report)
		}
	}
	list, err := ListText(p)
	if err != nil || !strings.Contains(list, id1) || !strings.Contains(list, "维护状态") {
		t.Fatalf("list = %q %v", list, err)
	}
	show, err := ShowText(p, id1)
	if err != nil || !strings.Contains(show, "Error: Failed to load extension") || !strings.Contains(show, p.RecoveryLog(id1)) {
		t.Fatalf("show = %q %v", show, err)
	}
	if _, err := ShowText(p, "inc-20000101000000"); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("missing incident: %v", err)
	}
	if _, err := Load(p, "../etc"); err == nil {
		t.Fatal("invalid id accepted")
	}
}

func TestCheckProblems(t *testing.T) {
	file, line, col := "/w/.pi/extensions/a.ts", 3, 9
	exit := 1
	rep := &check.Report{
		Location:  check.Location{Passed: true},
		Typecheck: check.Typecheck{Ran: true, Passed: false, Errors: []check.TypeError{{File: &file, Line: &line, Column: &col, Code: "TS2322", Message: "不能赋值"}}},
	}
	if got := checkProblems(rep); !strings.Contains(got, "/w/.pi/extensions/a.ts(3,9): TS2322 不能赋值") {
		t.Fatalf("typecheck problems = %q", got)
	}
	rep = &check.Report{Location: check.Location{Passed: true}, Typecheck: check.Typecheck{Ran: true, Passed: true},
		Trial: check.Trial{Ran: true, Passed: false, ExitCode: &exit, Stderr: "a\nError: boom\n"}}
	if got := checkProblems(rep); !strings.Contains(got, "试运行未通过（退出码 1）") || !strings.Contains(got, "Error: boom") {
		t.Fatalf("trial problems = %q", got)
	}
	f := &Failure{Trigger: TriggerStartFailure, Detail: "退出", Stderr: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12"}}
	if b := f.Brief(); strings.Contains(b, "\n1\n") || !strings.HasSuffix(b, "\n12") || strings.Count(b, "\n") != 11 {
		t.Fatalf("brief = %q", b)
	}
}
