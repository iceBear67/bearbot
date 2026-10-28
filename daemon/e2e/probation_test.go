package e2e

import (
	"strings"
	"syscall"
	"testing"
	"time"

	"bearbot/daemon/internal/testenv"
)

// probationUntil returns the probation end of state.json; the test fails when there is none.
func (b *bed) probationUntil() time.Time {
	b.t.Helper()
	st := b.state()
	if st.ProbationUntil == nil || !st.LastGoodPending {
		b.t.Fatalf("state.json has no probation: probationUntil %v, lastGoodPending %v", st.ProbationUntil, st.LastGoodPending)
	}
	return *st.ProbationUntil
}

// requireProbation checks that state.json and GET /v1/status show the probation end.
func (b *bed) requireProbation(end time.Time) {
	b.t.Helper()
	if got := b.probationUntil(); !got.Equal(end) {
		b.t.Fatalf("state.json probationUntil %s, want %s", got, end)
	}
	info, code := b.agentStatus()
	if code != 200 || info.ProbationUntil == nil {
		b.t.Fatalf("status: %d %+v", code, info)
	}
	got, err := time.Parse(time.RFC3339Nano, *info.ProbationUntil)
	if err != nil || !got.Equal(end.Truncate(time.Millisecond)) {
		b.t.Fatalf("status probationUntil %s (%v), want %s", *info.ProbationUntil, err, end)
	}
}

// requireNoProbation checks that state.json and GET /v1/status show no probation.
func (b *bed) requireNoProbation() {
	b.t.Helper()
	if st := b.state(); st.ProbationUntil != nil || st.LastGoodPending {
		b.t.Fatalf("state.json: probationUntil %v, lastGoodPending %v", st.ProbationUntil, st.LastGoodPending)
	}
	if info, code := b.agentStatus(); code != 200 || info.ProbationUntil != nil {
		b.t.Fatalf("status: %d %+v", code, info)
	}
}

func (b *bed) lastGoodIs(ref string) bool {
	b.t.Helper()
	info, code := b.agentStatus()
	return code == 200 && info.LastGood != nil && *info.LastGood == ref
}

// lastGoodMovesAt checks that last-good is not ref one second before end and is ref within two seconds after end.
func (b *bed) lastGoodMovesAt(ref string, end time.Time) {
	b.t.Helper()
	if time.Until(end) < 2*time.Second {
		b.t.Fatalf("less than 2 s are left before %s; the steps before took too long", end)
	}
	time.Sleep(time.Until(end.Add(-time.Second)))
	if b.lastGoodIs(ref) {
		b.t.Fatalf("last-good is %s before %s", ref, end)
	}
	b.waitFor("last-good at "+ref, time.Until(end.Add(2*time.Second)), func() bool { return b.lastGoodIs(ref) })
	if b.gitRev("last-good") != b.gitRev(ref) {
		b.t.Fatalf("the last-good tag does not point at %s", ref)
	}
}

// TestProbationWindowAcrossRestarts (DESIGN 5.4 观察期, 5.2 step 9, 5.9 runningSnapshot): the probation after an apply,
// a resume or a rollback ends at a fixed time, the ready time of that start plus [agent] probation, stored in
// state.json.
//   - `bearbotd restart` inside the probation keeps the end, and last-good moves at it;
//   - `/bb restart` keeps it too, and a SIGKILL of the restarted process before the end opens an incident
//     (观察期内退出);
//   - bearbotd stopped with SIGTERM and started again before the end keeps it;
//   - bearbotd started again after the end does not run in probation: a SIGKILL restarts the agent, and last-good
//     moves after a continuous run of [agent] probation.
func TestProbationWindowAcrossRestarts(t *testing.T) {
	const probation = 25 * time.Second
	b := newBed(t, func(_ *testenv.Env, c *testenv.Config) { c.Probation = probation.String() })
	d := b.start()
	d.WaitOutputContains(t, 90*time.Second, "控制台已连接")
	b.waitState("running", 90*time.Second)

	// The end of the probation of apply/1 is the ready time of the apply restart plus probation.
	b.ext("good.ts", goodExtension)
	applying := time.Now()
	b.apply("第一次应用", "apply/1")
	applied := b.waitRunningWith(0, "apply/1", 60*time.Second)
	end := b.probationUntil()
	if end.Before(applying.Add(probation)) || end.After(time.Now().Add(probation)) {
		t.Fatalf("probationUntil %s is not the ready time of the apply restart plus %s", end, probation)
	}
	b.requireProbation(end)

	// bearbotd restart inside the probation keeps the end: a probation that began again at the restart would end at
	// least 5 s later.
	time.Sleep(5 * time.Second)
	if out, stderr, code := b.cli("restart"); code != 0 || !strings.Contains(out, "agent 已重启") {
		t.Fatalf("restart: %d %s %s", code, out, stderr)
	}
	restarted, _ := b.agentStatus()
	if *restarted.PID == *applied.PID {
		t.Fatal("restart kept the process")
	}
	b.requireProbation(end)
	b.lastGoodMovesAt("apply/1", end)
	b.requireNoProbation()

	// /bb restart inside the probation of apply/2 keeps its end; a SIGKILL before the end opens an incident.
	b.ext("good2.ts", commandExtension("e2e-good-2"))
	b.apply("第二次应用", "apply/2")
	b.waitRunningWith(*restarted.PID, "apply/2", 60*time.Second)
	end = b.probationUntil()
	b.console("/bb restart", "agent 已重启（PID", 60*time.Second)
	restarted, _ = b.agentStatus()
	b.requireProbation(end)
	if time.Until(end) < 2*time.Second {
		t.Fatalf("less than 2 s are left before %s; the restart took too long", end)
	}
	if err := syscall.Kill(*restarted.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	id := b.waitMaintenance(90 * time.Second)
	if rec := b.incident(id); rec.Trigger != "观察期内退出" {
		t.Fatalf("incident = %+v", rec)
	}
	if st := b.state(); st.ProbationUntil != nil {
		t.Fatalf("probationUntil %s left after the incident", st.ProbationUntil)
	}
	if out, _, _ := b.cli("status"); strings.Contains(out, "观察期：到") {
		t.Fatalf("status in maintenance shows a probation:\n%s", out)
	}
	if b.gitRev("last-good") != b.gitRev("apply/1") {
		t.Fatal("last-good moved although the probation of apply/2 ended with an incident")
	}

	// resume begins a probation; bearbotd stopped with SIGTERM and started again before its end keeps it, and the
	// incident closes at the end.
	if out, stderr, code := b.cli("resume"); code != 0 || !strings.Contains(out, "agent 已启动，进入观察期") {
		t.Fatalf("resume: %d %s %s", code, out, stderr)
	}
	b.waitState("running", 30*time.Second)
	end = b.probationUntil()
	d.Stop(t)
	if got := b.probationUntil(); !got.Equal(end) {
		t.Fatalf("probationUntil %s after the shutdown, want %s", got, end)
	}
	d = b.start()
	b.waitState("running", 90*time.Second)
	b.requireProbation(end)
	b.lastGoodMovesAt("resume/1", end)
	b.requireNoProbation()
	b.waitFor("incident closed", 10*time.Second, func() bool { return b.state().OpenIncident == nil })
	if rec := b.incident(id); rec.Status != "closed" {
		t.Fatalf("incident after the probation of resume/1: %+v", rec)
	}

	// The end of the probation of apply/3 passes while bearbotd is stopped: the next start is not in probation.
	b.ext("good3.ts", commandExtension("e2e-good-3"))
	b.apply("第三次应用", "apply/3")
	b.waitRunningWith(0, "apply/3", 60*time.Second)
	end = b.probationUntil()
	d.Stop(t)
	time.Sleep(time.Until(end.Add(time.Second)))
	d = b.start()
	b.waitState("running", 90*time.Second)
	b.requireNoProbation()
	if !b.lastGoodIs("resume/1") {
		t.Fatal("last-good moved at the start after the probation ended")
	}
	info, _ := b.agentStatus()
	if err := syscall.Kill(*info.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	info = b.waitRunningWith(*info.PID, "apply/3", 60*time.Second)
	ready := time.Now()
	if info.CrashesInWindow != 1 || info.Incident != nil {
		t.Fatalf("status after the SIGKILL: %+v", info)
	}
	// last-good moves after the first continuous run of probation since bearbotd started.
	time.Sleep(time.Until(ready.Add(probation - 2*time.Second)))
	if b.lastGoodIs("apply/3") {
		t.Fatal("last-good moved before a continuous run of probation")
	}
	b.waitFor("last-good at apply/3", 10*time.Second, func() bool { return b.lastGoodIs("apply/3") })
}
