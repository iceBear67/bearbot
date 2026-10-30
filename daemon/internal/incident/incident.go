// Package incident keeps the incident records and runs the repair flow with recovery (DESIGN 5.7), the maintenance
// state with its hooks (DESIGN 5.8), `repair`, `resume`, and the closing of an incident when the probation after a
// repair ended. The agent supervisor runs these flows on its lifecycle goroutine and provides Host.
package incident

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"bearbot/daemon/internal/check"
	"bearbot/daemon/internal/config"
	"bearbot/daemon/internal/layout"
	"bearbot/daemon/internal/proc"
	"bearbot/daemon/internal/snapshot"
	"bearbot/daemon/internal/state"
	"bearbot/daemon/internal/wire"
)

// Triggers of DESIGN 10.4.
const (
	TriggerStartFailure  = "启动失败"
	TriggerProbationExit = "观察期内退出"
	TriggerRepeatedExit  = "反复退出"
	TriggerAdmin         = "管理员要求"
)

// Failure describes a failed agent process; the supervisor opens an incident with it. It is also the error
// Host.StartAgent returns for a failed start.
type Failure struct {
	Trigger string
	// Detail explains the failure in Chinese.
	Detail string
	// Exit is the exit status when the process exited.
	Exit *proc.ExitStatus
	// Stderr holds the last stderr lines of the process (at most 200).
	Stderr []string
}

func (f *Failure) Error() string { return f.Trigger + "：" + f.Detail }

// Brief describes the failure with at most the last 10 stderr lines, for replies in IM.
func (f *Failure) Brief() string {
	if len(f.Stderr) == 0 {
		return f.Detail
	}
	tail := f.Stderr
	if len(tail) > 10 {
		tail = tail[len(tail)-10:]
	}
	return f.Detail + "\nstderr 最后的内容：\n" + strings.Join(tail, "\n")
}

// Text describes the failure with its exit status and the end of stderr, for prompts and attempt records.
func (f *Failure) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n退出码 / 信号：%s", f.Detail, exitText(f.Exit))
	if len(f.Stderr) > 0 {
		tail := f.Stderr
		if len(tail) > 60 {
			tail = tail[len(tail)-60:]
		}
		fmt.Fprintf(&b, "\nstderr 最后 %d 行：\n%s", len(tail), strings.Join(tail, "\n"))
	}
	return b.String()
}

// Host is what the flows need from the agent supervisor.
type Host interface {
	Paths() layout.Paths
	Users() *layout.Users
	Config() *config.Config
	Repo() *snapshot.Repo
	Logger() *slog.Logger
	// State returns a copy of state.json; Update changes it and saves it.
	State() *state.State
	Update(func(*state.State)) error
	// SetWire changes the agent state reported to the gateway and bearbot-im.
	SetWire(s wire.AgentState, reason string)
	// Notify sends a notice to every administrator (queued while the gateway is not connected).
	Notify(text string)
	// Node is node found in the daemon's PATH; pi runs as `<Node> <PiBin>`.
	Node() string
	// Check runs the full check of DESIGN 5.6 holding the change lock.
	Check(ctx context.Context) (*check.Report, error)
	// LockChanges takes the change lock of DESIGN 5.4 (released by the returned function), so the snapshot commits of
	// the flows do not run together with an apply, a restore or a check of agent.sock.
	LockChanges(ctx context.Context) (unlock func(), err error)
	// StartAgent starts the agent process with the files as they are and waits until it is ready (RUNNING). With
	// probation the start begins a probation period. A failed start returns *Failure.
	StartAgent(ctx context.Context, probation bool) error
}

// Outcome is how a flow left the agent.
type Outcome int

// Outcomes.
const (
	// Running: the agent process runs (in probation).
	Running Outcome = iota + 1
	// InMaintenance: the bot is in the maintenance state.
	InMaintenance
)

// Manager runs the flows. Its methods other than SubmitReport and RecoveryLog are called from the supervisor's
// lifecycle goroutine only.
type Manager struct {
	h Host

	mu   sync.Mutex
	box  chan wire.RecoveryReport
	logs map[string]*proc.LogWriter
}

// New returns a Manager.
func New(h Host) *Manager {
	return &Manager{h: h, logs: map[string]*proc.LogWriter{}}
}

// SubmitReport hands a POST /v1/recovery/report to the waiting repair flow.
func (m *Manager) SubmitReport(r wire.RecoveryReport) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.box == nil {
		return errors.New("当前没有等待中的修复报告")
	}
	select {
	case m.box <- r:
		return nil
	default:
		return errors.New("这一轮已经收到修复报告")
	}
}

func (m *Manager) openBox() chan wire.RecoveryReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.box = make(chan wire.RecoveryReport, 1)
	return m.box
}

// takeBox closes the box and returns a report that arrived.
func (m *Manager) takeBox() (wire.RecoveryReport, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	box := m.box
	m.box = nil
	if box == nil {
		return wire.RecoveryReport{}, false
	}
	select {
	case r := <-box:
		return r, true
	default:
		return wire.RecoveryReport{}, false
	}
}

// RecoveryLog returns the log writer of the repair running for incident id, or nil.
func (m *Manager) RecoveryLog(id string) *proc.LogWriter {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.logs[id]
}

func (m *Manager) openLog(id string) *proc.LogWriter {
	w := proc.NewLogWriter(m.h.Paths().RecoveryLog(id), 500)
	m.mu.Lock()
	m.logs[id] = w
	m.mu.Unlock()
	return w
}

func (m *Manager) closeLog(id string) {
	m.mu.Lock()
	w := m.logs[id]
	delete(m.logs, id)
	m.mu.Unlock()
	if w != nil {
		if err := w.Close(); err != nil {
			m.h.Logger().Error("close recovery log", "incident", id, "err", err)
		}
	}
}

// newID allocates inc-<UTC yyyymmddhhmmss> and creates its directory; a second incident within the same second
// waits for the next one (the snapshot names allow exactly 14 digits).
func newID(p layout.Paths) (string, error) {
	for range 5 {
		now := time.Now().UTC()
		id := "inc-" + now.Format("20060102150405")
		err := os.Mkdir(p.IncidentDir(id), 0o755)
		if err == nil {
			if err := os.Chmod(p.IncidentDir(id), 0o755); err != nil {
				return "", err
			}
			return id, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("创建故障目录失败：%w", err)
		}
		time.Sleep(now.Truncate(time.Second).Add(time.Second).Sub(time.Now()) + 10*time.Millisecond)
	}
	return "", errors.New("无法分配故障 id")
}

// lastApply returns the newest apply snapshot with its summary and origin, or nil when there is none.
func lastApply(ctx context.Context, repo *snapshot.Repo) (*state.Applied, error) {
	next, err := repo.NextApply(ctx)
	if err != nil {
		return nil, err
	}
	if next == 1 {
		return nil, nil
	}
	tag := snapshot.ApplyTag(next - 1)
	e, err := repo.Describe(ctx, tag)
	if err != nil {
		return nil, err
	}
	a := &state.Applied{Snapshot: tag, Summary: e.Summary}
	if e.Thread != "" {
		a.Origin.Thread = state.Ptr(e.Thread)
	}
	if e.Requester != "" {
		a.Origin.Requester = state.Ptr(e.Requester)
	}
	return a, nil
}

// create records a new incident: incident.json, the snapshot incident/<id>/before, the open incident in
// state.json, and the agent state "incident". An open incident that was in probation is closed as superseded. The
// state changes before the change lock is taken: an apply that passed its state check before commits first and its
// files are part of incident/<id>/before; a later one sees the new state. The id and its directory are allocated
// under the lock, and the records run to the end even when the daemon is asked to stop meanwhile, so a stop during
// the wait for the lock leaves no incident directory without incident.json.
func (m *Manager) create(ctx context.Context, trigger, detail string, exit *proc.ExitStatus, stderr []string) (*Record, error) {
	h := m.h
	p := h.Paths()
	repo := h.Repo()
	h.SetWire(wire.Incident, trigger)
	unlock, err := h.LockChanges(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	ctx = context.WithoutCancel(ctx)
	id, err := newID(p)
	if err != nil {
		return nil, err
	}
	h.SetWire(wire.Incident, id+"："+trigger)
	st := h.State()
	if old := st.OpenIncident; old != nil {
		prev, err := Load(p, old.ID)
		if err != nil {
			return nil, err
		}
		now := time.Now()
		prev.Status, prev.ClosedAt = StatusClosed, &now
		prev.Resolution = &Resolution{Kind: ResolvedBySuperseded, Detail: "又出现了新的故障 " + id}
		if err := prev.Save(p); err != nil {
			return nil, err
		}
		h.Logger().Info("incident superseded", "incident", old.ID, "by", id)
	}
	rpcTail, err := proc.TailFile(p.AgentRPCEventsLog, 50)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败：%w", p.AgentRPCEventsLog, err)
	}
	applied, err := lastApply(ctx, repo)
	if err != nil {
		return nil, err
	}
	var origin state.Origin
	if applied != nil && st.RunningSnapshot != "" {
		commit, err := repo.Resolve(ctx, applied.Snapshot)
		if err != nil {
			return nil, err
		}
		if commit == st.RunningSnapshot {
			origin = applied.Origin
		}
	}
	before := snapshot.IncidentBeforeTag(id)
	if _, err := repo.Commit(ctx, before, snapshot.Message{Summary: fmt.Sprintf("故障 %s：%s", id, trigger)}); err != nil {
		return nil, err
	}
	base := snapshot.InitTag
	var lastGoodName *string
	if commit, ok, err := repo.LastGood(ctx); err != nil {
		return nil, err
	} else if ok {
		name, err := repo.NameOf(ctx, commit)
		if err != nil {
			return nil, err
		}
		lastGoodName = &name
		base = commit
	}
	diff, err := repo.DiffStatPaths(ctx, base, before, nil)
	if err != nil {
		return nil, err
	}
	if len(stderr) > 200 {
		stderr = stderr[len(stderr)-200:]
	}
	rec := &Record{
		ID:         id,
		CreatedAt:  time.Now(),
		Trigger:    trigger,
		Detail:     detail,
		Exit:       exit,
		StderrTail: append([]string{}, stderr...),
		RPCEvents:  rpcTail,
		LastApply:  applied,
		Origin:     origin,
		LastGood:   lastGoodName,
		DiffStat:   diff,
		Before:     before,
		Status:     StatusRepairing,
	}
	if err := rec.Save(p); err != nil {
		return nil, err
	}
	if err := h.Update(func(s *state.State) {
		s.OpenIncident = &state.OpenIncident{ID: id, Trigger: trigger, State: state.IncidentRepairing, LastApply: applied}
		s.Crashes = nil
		// A running probation ends with the incident; the start after a repair, resume or rollback begins a new one.
		s.ProbationUntil = nil
		// An apply that waited for its restart, or whose restart failed before bridge/ready, is part of the files of
		// incident/<id>/before now. It is not confirmed as applied: the incident flow handles it (DESIGN 9.4), and
		// its data stays in OpenIncident.LastApply and incident.json.
		s.PendingApply = nil
		s.Deliver.Applied = nil
	}); err != nil {
		return nil, err
	}
	h.Logger().Info("incident opened", "incident", id, "trigger", trigger, "detail", detail)
	return rec, nil
}

// Open records an incident for the failure f, notifies the administrators and runs the repair flow (DESIGN 5.7).
func (m *Manager) Open(ctx context.Context, f *Failure) (Outcome, error) {
	rec, err := m.create(ctx, f.Trigger, f.Detail, f.Exit, f.Stderr)
	if err != nil {
		return 0, err
	}
	m.h.Notify(fmt.Sprintf("bearbot 出现故障 %s（%s）：%s\n正在由 recovery 修复，修复期间触发消息会收到维护提示。", rec.ID, rec.Trigger, f.Brief()))
	return m.repair(ctx, rec, "", "")
}

// Continue runs the repair flow again for the open incident in state repairing (the daemon stopped during a repair).
// The run continues the interrupted one: it gets the same administrator note and maintenance reason.
func (m *Manager) Continue(ctx context.Context) (Outcome, error) {
	st := m.h.State()
	if st.OpenIncident == nil {
		return 0, errors.New("没有未关闭的故障")
	}
	rec, err := Load(m.h.Paths(), st.OpenIncident.ID)
	if err != nil {
		return 0, err
	}
	note, reason := "", ""
	if n := len(rec.Repairs); n > 0 {
		note, reason = rec.Repairs[n-1].Note, rec.Repairs[n-1].MaintenanceReason
	}
	m.h.Notify(fmt.Sprintf("bearbotd 重新启动时故障 %s（%s）还在修复中，重新运行 recovery。", rec.ID, rec.Trigger))
	return m.repair(ctx, rec, note, reason)
}

// Repair handles `repair [说明]` in the maintenance state: recovery runs again with the note and a new count of
// attempts. An open incident is repaired again, and the prompt also names the reason of the maintenance state (for
// example a rollback that failed after the incident). Without an open incident (the maintenance state came from a
// failed rollback) a new incident with the trigger "管理员要求" is opened. It returns the reply text.
func (m *Manager) Repair(ctx context.Context, note string) (Outcome, string, error) {
	h := m.h
	st := h.State()
	if st.Maintenance == nil {
		return 0, "", errors.New("不在维护状态")
	}
	reason := st.Maintenance.Reason
	if err := m.ExitMaintenance(ctx, "管理员执行 repair"); err != nil {
		return 0, "", err
	}
	var rec *Record
	var err error
	maintenanceReason := ""
	if st.OpenIncident != nil {
		if rec, err = Load(h.Paths(), st.OpenIncident.ID); err != nil {
			return 0, "", err
		}
		maintenanceReason = reason
	} else if rec, err = m.create(ctx, TriggerAdmin, "管理员在维护状态下执行 repair，维护原因："+reason, nil, nil); err != nil {
		return 0, "", err
	}
	outcome, err := m.repair(ctx, rec, note, maintenanceReason)
	if err != nil {
		return 0, "", err
	}
	return m.repairReply(rec.ID, outcome)
}

// ForceRepair handles `repair --force` after the supervisor stopped the running agent process (DESIGN 5.8): it opens
// an incident with the trigger "管理员要求" and the stderr of the stopped process, notifies the administrators and
// runs recovery with the note. It returns the reply text.
func (m *Manager) ForceRepair(ctx context.Context, note string, stderr []string) (Outcome, string, error) {
	rec, err := m.create(ctx, TriggerAdmin, "管理员执行 repair --force，停止了运行中的 agent 进程", nil, stderr)
	if err != nil {
		return 0, "", err
	}
	m.h.Notify(fmt.Sprintf("管理员要求修复：已停止 agent 进程，建立故障 %s，正在由 recovery 修复，修复期间触发消息会收到维护提示。", rec.ID))
	outcome, err := m.repair(ctx, rec, note, "")
	if err != nil {
		return 0, "", err
	}
	return m.repairReply(rec.ID, outcome)
}

// repairReply is the reply to `repair` after the repair flow of incident id ended with outcome.
func (m *Manager) repairReply(id string, outcome Outcome) (Outcome, string, error) {
	rec, err := Load(m.h.Paths(), id)
	if err != nil {
		return 0, "", err
	}
	if outcome == Running {
		a := rec.attempt(rec.Resolution.Attempt)
		return outcome, fmt.Sprintf("故障 %s 已由 recovery 修复（第 %d 次修复）：%s\nagent 已启动，进入观察期（%s）。", rec.ID, a.N, strings.TrimSpace(a.Report.Summary), m.h.Config().Agent.Probation), nil
	}
	return outcome, fmt.Sprintf("故障 %s 没有修复，bot 仍处于维护状态。\n原因：%s", rec.ID, rec.MaintenanceReason), nil
}

// Resume handles `resume` in the maintenance state: commit resume/<n>, make it runningSnapshot, leave the
// maintenance state and start the agent in probation. A failed start returns *Failure as the error together with
// the reply text.
func (m *Manager) Resume(ctx context.Context) (string, error) {
	h := m.h
	p := h.Paths()
	tag, commit, err := m.commitResume(ctx)
	if err != nil {
		return "", err
	}
	st := h.State()
	if st.OpenIncident != nil {
		rec, err := Load(p, st.OpenIncident.ID)
		if err != nil {
			return "", err
		}
		rec.Status = StatusProbation
		rec.Resolution = &Resolution{Kind: ResolvedByResume, Detail: tag}
		if err := rec.Save(p); err != nil {
			return "", err
		}
	}
	if err := h.Update(func(s *state.State) {
		s.RunningSnapshot = commit
		s.PendingApply = nil
		// The next start runs in probation, also when bearbotd stops before it starts the agent.
		s.LastGoodPending = true
		s.ProbationUntil = nil
		if s.OpenIncident != nil {
			s.OpenIncident.State = state.IncidentProbation
		}
	}); err != nil {
		return "", err
	}
	if err := m.ExitMaintenance(ctx, "管理员执行 resume"); err != nil {
		return "", err
	}
	if err := h.StartAgent(ctx, true); err != nil {
		var f *Failure
		if errors.As(err, &f) {
			return fmt.Sprintf("已提交快照 %s，但 agent 启动失败：%s\n将建立新的故障，由 recovery 修复。", tag, f.Brief()), err
		}
		return "", err
	}
	return fmt.Sprintf("已提交快照 %s，agent 已启动，进入观察期（%s）。", tag, h.Config().Agent.Probation), nil
}

// commitResume commits resume/<n> holding the change lock.
func (m *Manager) commitResume(ctx context.Context) (string, string, error) {
	repo := m.h.Repo()
	unlock, err := m.h.LockChanges(ctx)
	if err != nil {
		return "", "", err
	}
	defer unlock()
	n, err := repo.NextResume(ctx)
	if err != nil {
		return "", "", err
	}
	tag := snapshot.ResumeTag(n)
	commit, err := repo.Commit(ctx, tag, snapshot.Message{Summary: "管理员在维护状态下恢复运行"})
	if err != nil {
		return "", "", err
	}
	return tag, commit, nil
}

// Resolve marks the open incident as resolved by kind (a rollback); it waits for the probation that follows.
func (m *Manager) Resolve(kind, detail string) error {
	h := m.h
	st := h.State()
	if st.OpenIncident == nil {
		return nil
	}
	rec, err := Load(h.Paths(), st.OpenIncident.ID)
	if err != nil {
		return err
	}
	rec.Status = StatusProbation
	rec.Resolution = &Resolution{Kind: kind, Detail: detail}
	if err := rec.Save(h.Paths()); err != nil {
		return err
	}
	return h.Update(func(s *state.State) { s.OpenIncident.State = state.IncidentProbation })
}

// ProbationEnded closes the open incident when its probation ended (DESIGN 5.7 step 6): report.md, a notice to the
// administrators, and the IncidentNotice to give to bearbot-im when recovery fixed it (nil otherwise).
func (m *Manager) ProbationEnded() (*state.IncidentNotice, error) {
	h := m.h
	st := h.State()
	if st.OpenIncident == nil || st.OpenIncident.State != state.IncidentProbation {
		return nil, nil
	}
	rec, err := Load(h.Paths(), st.OpenIncident.ID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	rec.Status, rec.ClosedAt = StatusClosed, &now
	if err := rec.Save(h.Paths()); err != nil {
		return nil, err
	}
	if err := h.Update(func(s *state.State) { s.OpenIncident = nil }); err != nil {
		return nil, err
	}
	h.Logger().Info("incident closed", "incident", rec.ID, "resolution", resolutionText(rec.Resolution))
	h.Notify(fmt.Sprintf("故障 %s（%s）已关闭：%s，之后的版本度过了观察期。报告：%s", rec.ID, rec.Trigger, resolutionText(rec.Resolution), h.Paths().IncidentDir(rec.ID)+"/report.md"))
	if rec.Resolution == nil || rec.Resolution.Kind != ResolvedByRecovery {
		return nil, nil
	}
	a := rec.attempt(rec.Resolution.Attempt)
	if a == nil {
		return nil, fmt.Errorf("故障 %s 的记录中没有第 %d 次修复", rec.ID, rec.Resolution.Attempt)
	}
	return &state.IncidentNotice{
		ID:           rec.ID,
		Trigger:      rec.Trigger,
		Summary:      a.Report.Summary,
		ChangedFiles: append([]string{}, a.Report.ChangedFiles...),
		Lesson:       a.Report.Lesson,
		Origin:       rec.Origin,
	}, nil
}

// maintenanceNotice is the notice sent to the administrators when the maintenance state begins (DESIGN 5.8 step 3).
// open is the open incident when the maintenance state does not come from an incident (a failed rollback) but an
// incident is still open; repair continues that incident.
func maintenanceNotice(reason string, incidentID *string, trigger string, open *state.OpenIncident) string {
	var b strings.Builder
	b.WriteString("bearbot 进入维护状态，暂时不会回复消息。\n")
	switch {
	case incidentID != nil:
		fmt.Fprintf(&b, "故障：%s（%s）\n", *incidentID, trigger)
	case open != nil:
		fmt.Fprintf(&b, "未关闭的故障：%s（%s），执行 repair 时继续修复这个故障\n", open.ID, open.Trigger)
	default:
		b.WriteString("故障：无\n")
	}
	fmt.Fprintf(&b, "原因：%s\n", reason)
	b.WriteString("可用的命令：/bb repair [说明]（带着说明重新运行 recovery）、/bb rollback [快照]（回滚，默认回到最近一次应用之前的状态）、/bb resume（不做改动，恢复运行）；在命令行中为 bearbotd repair、bearbotd rollback、bearbotd resume。")
	return b.String()
}

// EnterMaintenance enters the maintenance state (DESIGN 5.8): the mark in state.json, the agent state, the on_enter
// hooks, and the notice to the administrators. incidentID is nil when the maintenance state does not come from an
// incident (a failed rollback); an incident that is still open then goes to the maintenance state with this reason,
// so incident.json, state.json and the notice agree, and repair continues it.
func (m *Manager) EnterMaintenance(ctx context.Context, reason string, incidentID *string) error {
	h := m.h
	var open *state.OpenIncident
	if st := h.State(); incidentID == nil && st.OpenIncident != nil {
		open = st.OpenIncident
		rec, err := Load(h.Paths(), open.ID)
		if err != nil {
			return err
		}
		rec.Status, rec.MaintenanceReason, rec.Resolution = StatusMaintenance, reason, nil
		if err := rec.Save(h.Paths()); err != nil {
			return err
		}
	}
	if err := h.Update(func(s *state.State) {
		s.Maintenance = &state.Maintenance{Reason: reason, IncidentID: incidentID, Since: time.Now()}
		if s.OpenIncident != nil {
			s.OpenIncident.State = state.IncidentMaintenance
		}
		// No probation runs in the maintenance state; the start that leaves it begins a new one.
		s.ProbationUntil = nil
	}); err != nil {
		return err
	}
	h.SetWire(wire.Maintenance, reason)
	trigger := ""
	if st := h.State(); st.OpenIncident != nil {
		trigger = st.OpenIncident.Trigger
	}
	text := maintenanceNotice(reason, incidentID, trigger, open)
	ev := HookEvent{Event: EventMaintenanceEnter, Reason: reason, Report: text}
	if incidentID != nil {
		ev.IncidentID = *incidentID
	}
	h.Logger().Warn("maintenance state entered", "reason", reason, "incident", ev.IncidentID)
	cfg := h.Config()
	RunHooks(ctx, h.Paths(), cfg.Maintenance.OnEnter, cfg.Maintenance.HookTimeout, ev, h.Logger())
	h.Notify(text)
	return nil
}

// ExitMaintenance leaves the maintenance state: the mark is removed and the on_exit hooks run. how tells why.
func (m *Manager) ExitMaintenance(ctx context.Context, how string) error {
	h := m.h
	st := h.State()
	if st.Maintenance == nil {
		return nil
	}
	if err := h.Update(func(s *state.State) { s.Maintenance = nil }); err != nil {
		return err
	}
	ev := HookEvent{Event: EventMaintenanceExit, Reason: st.Maintenance.Reason, Report: how}
	if st.Maintenance.IncidentID != nil {
		ev.IncidentID = *st.Maintenance.IncidentID
	}
	h.Logger().Info("maintenance state left", "how", how)
	cfg := h.Config()
	RunHooks(ctx, h.Paths(), cfg.Maintenance.OnExit, cfg.Maintenance.HookTimeout, ev, h.Logger())
	return nil
}
