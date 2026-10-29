package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"bearbot/daemon/internal/check"
	"bearbot/daemon/internal/classifierturn"
	"bearbot/daemon/internal/incident"
	"bearbot/daemon/internal/pirpc"
	"bearbot/daemon/internal/proc"
	"bearbot/daemon/internal/rollback"
	"bearbot/daemon/internal/snapshot"
	"bearbot/daemon/internal/state"
	"bearbot/daemon/internal/wire"
)

// ConflictError means a request conflicts with the current records (HTTP 409).
type ConflictError struct{ Msg string }

func (e *ConflictError) Error() string { return e.Msg }

// AgentStates are the states in which the agent process exists (contracts 6).
var AgentStates = []wire.AgentState{wire.Starting, wire.Running, wire.Draining, wire.Restarting}

// Prepare finishes the startup steps that need the state (DESIGN 5.2 steps 3-4): a session id for the first start,
// runningSnapshot for a new repository, and the comparison of an unfinished member turn.
func (s *Supervisor) Prepare(ctx context.Context, repoCreated bool) error {
	st := s.stateCopy()
	running := st.RunningSnapshot
	if running == "" {
		var err error
		if repoCreated {
			running, err = s.repo.Resolve(ctx, snapshot.InitTag)
		} else {
			running, err = s.repo.Head(ctx)
		}
		if err != nil {
			return err
		}
		s.log.Info("runningSnapshot initialized", "commit", running, "newRepository", repoCreated)
	}
	if err := s.update(func(x *state.State) {
		if x.SessionID == "" {
			x.SessionID = state.NewSessionID(time.Now())
		}
		x.RunningSnapshot = running
	}); err != nil {
		return err
	}
	return s.compareClassifierAfterExit()
}

// submit hands c to the lifecycle goroutine when it waits for commands in one of the allowed states.
func (s *Supervisor) submit(c command, allowed ...wire.AgentState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.listening || !slices.Contains(allowed, s.wire) {
		return &StateError{State: s.wire, Op: c.op()}
	}
	select {
	case s.cmds <- c:
		return nil
	default:
		return errors.New("daemon 正在处理其他请求，请稍后再试")
	}
}

func (s *Supervisor) run(c command, reply chan opResult, allowed ...wire.AgentState) (string, error) {
	if err := s.submit(c, allowed...); err != nil {
		return "", err
	}
	res := <-reply
	return res.text, res.err
}

// Restart restarts the agent after the running turn (`/bb restart`, `bearbotd restart`); only in RUNNING. It
// returns after the new process is ready or the start failed.
func (s *Supervisor) Restart() (string, error) {
	reply := make(chan opResult, 1)
	return s.run(restartCmd{reply: reply}, reply, wire.Running)
}

// Repair runs recovery again in MAINTENANCE (DESIGN 5.8); it returns after the repair ended.
func (s *Supervisor) Repair(note string) (string, error) {
	reply := make(chan opResult, 1)
	return s.run(repairCmd{note: note, reply: reply}, reply, wire.Maintenance)
}

// Resume leaves MAINTENANCE without changes (DESIGN 5.8); it returns after the agent start.
func (s *Supervisor) Resume() (string, error) {
	reply := make(chan opResult, 1)
	return s.run(resumeCmd{reply: reply}, reply, wire.Maintenance)
}

// Rollback runs the administrator rollback (DESIGN 5.12) in RUNNING, DRAINING or MAINTENANCE. The request is
// registered as pending first, so an apply whose check ends from then on is cancelled; the target is checked, then
// the request waits for the change lock and for the lifecycle goroutine. The target is computed again under the
// change lock, so the default is the parent of the newest apply at the time the rollback runs. It returns after the
// rollback ended.
func (s *Supervisor) Rollback(ctx context.Context, req rollback.Request) (string, error) {
	allowed := []wire.AgentState{wire.Running, wire.Draining, wire.Maintenance}
	if err := s.Require("rollback", allowed...); err != nil {
		return "", err
	}
	if req.Snapshot != "" {
		if err := snapshot.ValidateRef(req.Snapshot); err != nil {
			return "", &UserError{err}
		}
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr := &pendingRollback{cancel: cancel}
	s.mu.Lock()
	if !s.listening || !slices.Contains(allowed, s.wire) {
		st := s.wire
		s.mu.Unlock()
		return "", &StateError{State: st, Op: "rollback"}
	}
	s.pendingRollbacks[pr] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pendingRollbacks, pr)
		s.mu.Unlock()
	}()
	rejected := func() error {
		if ctx.Err() != nil {
			return errors.New("bearbotd 正在关闭，回滚没有执行")
		}
		return &StateError{State: s.CurrentState(), Op: "rollback"}
	}
	// A bad target is rejected before the wait.
	if _, err := rollback.Target(ctx, s.repo, req.Snapshot); err != nil {
		return "", &UserError{err}
	}
	s.log.Info("rollback waits for the change lock", "snapshot", req.Snapshot)
	if err := s.change.lock(waitCtx); err != nil {
		return "", rejected()
	}
	defer s.change.unlock()
	// No apply can commit while the change lock is held.
	target, err := rollback.Target(ctx, s.repo, req.Snapshot)
	if err != nil {
		return "", &UserError{err}
	}
	reply := make(chan opResult, 1)
	select {
	case s.rollbackCh <- &rollbackCmd{req: req, target: target, reply: reply}:
	case <-waitCtx.Done():
		return "", rejected()
	}
	res := <-reply
	return res.text, res.err
}

// Session handles gateway-ctl `session` (contracts 4.2).
func (s *Supervisor) Session(ctx context.Context, action, instructions string) (string, error) {
	switch action {
	case "new":
		reply := make(chan opResult, 1)
		return s.run(newSessionCmd{reply: reply}, reply, wire.Running)
	case "abort":
		ap, err := s.runningProcess("session abort", wire.Running, wire.Draining)
		if err != nil {
			return "", err
		}
		busy := ap.isBusy()
		c, cancel := s.rpcCtx(ctx)
		defer cancel()
		if err := ap.rpc.Abort(c); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return "", fmt.Errorf("abort 在 rpc_timeout（%s）内没有完成，回合可能停在扩展的处理函数中无法结束；可以用 /bb restart 或 /bb rollback 处理", s.cfg.Agent.RPCTimeout)
			}
			return "", err
		}
		if busy {
			return "已中止正在进行的回合。", nil
		}
		return "当前没有正在进行的回合。", nil
	case "compact":
		ap, err := s.runningProcess("session compact", wire.Running)
		if err != nil {
			return "", err
		}
		c, cancel := s.rpcCtx(ctx)
		defer cancel()
		res, err := ap.rpc.Compact(c, instructions)
		var ce *pirpc.CommandError
		switch {
		case err == nil:
			after := ""
			if res.EstimatedTokensAfter != nil {
				after = fmt.Sprintf("，压缩后约 %d tokens", *res.EstimatedTokensAfter)
			}
			return fmt.Sprintf("已压缩会话：摘要 %d 字符，压缩前约 %d tokens%s。", len([]rune(res.Summary)), res.TokensBefore, after), nil
		case errors.As(err, &ce):
			if strings.Contains(ce.Message, "Nothing to compact") {
				return "会话内容太少，不需要压缩（Nothing to compact）。", nil
			}
			return "压缩失败：" + ce.Message, nil
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Sprintf("压缩没有在 rpc_timeout（%s）内完成，pi 仍在压缩，完成后生效。", s.cfg.Agent.RPCTimeout), nil
		default:
			return "", err
		}
	default:
		return "", userErr("未知的 session action %q（可选 new、abort、compact）", action)
	}
}

func (s *Supervisor) runningProcess(op string, allowed ...wire.AgentState) (*agentProc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(allowed, s.wire) || s.cur == nil {
		return nil, &StateError{State: s.wire, Op: op}
	}
	return s.cur, nil
}

// Check runs the full check holding the change lock (POST /v1/check, and the check of the repair flow).
func (s *Supervisor) Check(ctx context.Context) (*check.Report, error) {
	if err := s.change.lock(ctx); err != nil {
		return nil, err
	}
	defer s.change.unlock()
	return s.checker.Run(ctx)
}

// ApplyResult is the answer of POST /v1/apply.
type ApplyResult struct {
	Accepted bool          `json:"accepted"`
	Snapshot string        `json:"snapshot,omitempty"`
	Report   *check.Report `json:"report,omitempty"`
}

// Apply runs the apply of DESIGN 5.6: under the change lock, the state must be RUNNING with the agent process that
// sent the request, the full check must pass, then apply/<n> is committed, the pending apply is recorded and the
// agent drains; the restart after the turn makes it take effect. A rollback that waits for the change lock, or one
// that ran while this request waited for it, cancels the apply before anything is committed.
func (s *Supervisor) Apply(ctx context.Context, summary string, origin state.Origin) (*ApplyResult, error) {
	if strings.TrimSpace(summary) == "" {
		return nil, userErr("summary 不能为空")
	}
	if err := snapshot.CheckSummary(summary); err != nil {
		return nil, &UserError{err}
	}
	for _, v := range []*string{origin.Requester, origin.Thread} {
		if v != nil && strings.ContainsAny(*v, "\r\n\x00") {
			return nil, userErr("origin 的 requester 与 thread 不能包含换行或 NUL 字符")
		}
	}
	c, err := s.agentCaller("apply")
	if err != nil {
		return nil, err
	}
	if err := s.change.lock(ctx); err != nil {
		return nil, err
	}
	defer s.change.unlock()
	if err := s.requireApplyCaller(c); err != nil {
		return nil, err
	}
	rep, err := s.checker.Run(ctx)
	if err != nil {
		return nil, err
	}
	if !rep.Passed {
		s.log.Info("apply rejected by the check", "check", rep.ID)
		return &ApplyResult{Accepted: false, Report: rep}, nil
	}
	s.mu.Lock()
	rollbackWaiting := len(s.pendingRollbacks) > 0
	s.mu.Unlock()
	if rollbackWaiting {
		return nil, &ConflictError{"管理员正在回滚，这次应用已取消，没有提交快照"}
	}
	if err := s.requireApplyCaller(c); err != nil {
		return nil, err
	}
	n, err := s.repo.NextApply(ctx)
	if err != nil {
		return nil, err
	}
	tag := snapshot.ApplyTag(n)
	msg := snapshot.Message{Summary: summary}
	if origin.Requester != nil {
		msg.Requester = *origin.Requester
	}
	if origin.Thread != nil {
		msg.Thread = *origin.Thread
	}
	if _, err := s.repo.Commit(ctx, tag, msg); err != nil {
		return nil, err
	}
	if err := s.update(func(x *state.State) {
		x.PendingApply = &state.PendingApply{CheckID: rep.ID, Snapshot: tag, Summary: summary, Origin: origin}
	}); err != nil {
		return nil, err
	}
	if err := s.submit(applyDrainCmd{snapshot: tag}, wire.Running); err != nil {
		s.log.Warn("apply recorded, it takes effect with the next start", "snapshot", tag, "err", err)
	}
	s.log.Info("apply accepted", "snapshot", tag, "check", rep.ID)
	return &ApplyResult{Accepted: true, Snapshot: tag}, nil
}

// SnapshotList is the text of `snapshot list` (DESIGN 5.11).
func (s *Supervisor) SnapshotList(ctx context.Context) (string, error) {
	l, err := s.repo.List(ctx, s.stateCopy().RunningSnapshot)
	if err != nil {
		return "", err
	}
	return l.Text(), nil
}

// SnapshotDiff is the text of `snapshot diff <ref> [ref]`.
func (s *Supervisor) SnapshotDiff(ctx context.Context, ref, ref2 string) (string, error) {
	if ref == "" {
		return "", userErr("需要快照引用")
	}
	for _, r := range []string{ref, ref2} {
		if r == "" {
			continue
		}
		if err := snapshot.ValidateRef(r); err != nil {
			return "", &UserError{err}
		}
		if _, err := s.repo.Resolve(ctx, r); err != nil {
			return "", &UserError{err}
		}
	}
	return s.repo.DiffStat(ctx, ref, ref2)
}

// RestoreResult is the answer of POST /v1/snapshot with action restore.
type RestoreResult struct {
	Restored []string `json:"restored"`
	Text     string   `json:"text"`
}

// caller identifies who sent an agent.sock request that waits for the change lock: the agent process (by its
// generation) in the agent states, or the recovery of the open incident in REPAIRING. rollbacks is the rollback
// count when the request arrived.
type caller struct {
	state     wire.AgentState
	gen       int
	incident  string
	rollbacks uint64
}

// agentCaller records the agent process that sends the current request; without one the request is refused.
func (s *Supervisor) agentCaller(op string) (caller, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.wire.AgentProcessExists() || s.cur == nil {
		return caller{}, &StateError{State: s.wire, Op: op}
	}
	return caller{state: s.wire, gen: s.cur.gen, rollbacks: s.rollbacks}, nil
}

// requireApplyCaller checks after the wait for the change lock that no rollback ran meanwhile and that the agent
// process that sent the apply still runs in RUNNING (DESIGN 5.6 apply step 1).
func (s *Supervisor) requireApplyCaller(c caller) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rollbacks != c.rollbacks {
		return &ConflictError{"管理员在这次应用等待变更锁期间执行了回滚，这次应用已取消，没有提交快照"}
	}
	if s.wire != wire.Running || s.cur == nil || s.cur.gen != c.gen {
		return &ConflictError{fmt.Sprintf("发起这次应用的 agent 进程已经不在运行（例如等待变更锁期间管理员已经回滚），当前状态为 %s，这次应用已取消，没有提交快照", s.wire)}
	}
	return nil
}

// restoreCaller records who sends a restore: the agent process in the agent states, recovery in REPAIRING.
func (s *Supervisor) restoreCaller() (caller, error) {
	s.mu.Lock()
	st, cur, oi, rollbacks := s.wire, s.cur, s.st.OpenIncident, s.rollbacks
	s.mu.Unlock()
	switch {
	case st == wire.Repairing && oi != nil:
		return caller{state: st, incident: oi.ID, rollbacks: rollbacks}, nil
	case st.AgentProcessExists() && cur != nil:
		return caller{state: st, gen: cur.gen, rollbacks: rollbacks}, nil
	default:
		return caller{}, &StateError{State: st, Op: "snapshot restore"}
	}
}

// requireRestoreCaller checks after the wait for the change lock that no rollback ran meanwhile and that the sender
// still exists: the same agent process, or the repair of the same incident.
func (s *Supervisor) requireRestoreCaller(c caller) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rollbacks != c.rollbacks {
		return &ConflictError{"管理员在这次恢复等待变更锁期间执行了回滚，这次恢复已取消，没有改动文件"}
	}
	if c.state == wire.Repairing {
		if s.wire != wire.Repairing || s.st.OpenIncident == nil || s.st.OpenIncident.ID != c.incident {
			return &ConflictError{fmt.Sprintf("发起这次恢复的修复（故障 %s）已经结束，当前状态为 %s，这次恢复已取消，没有改动文件", c.incident, s.wire)}
		}
		return nil
	}
	if !s.wire.AgentProcessExists() || s.cur == nil || s.cur.gen != c.gen {
		return &ConflictError{fmt.Sprintf("发起这次恢复的 agent 进程已经不在运行，当前状态为 %s，这次恢复已取消，没有改动文件", s.wire)}
	}
	return nil
}

// SnapshotRestore restores restart effective paths to ref under the change lock (DESIGN 5.9). The agent process
// keeps running with the old files; an apply makes the restored files take effect.
func (s *Supervisor) SnapshotRestore(ctx context.Context, ref string, paths []string) (*RestoreResult, error) {
	if ref == "" {
		return nil, userErr("restore 需要快照引用 ref")
	}
	if err := snapshot.ValidateRef(ref); err != nil {
		return nil, &UserError{err}
	}
	if _, err := s.repo.Resolve(ctx, ref); err != nil {
		return nil, &UserError{err}
	}
	for _, p := range paths {
		if _, err := s.repo.RestorePath(p); err != nil {
			return nil, &UserError{err}
		}
	}
	c, err := s.restoreCaller()
	if err != nil {
		return nil, err
	}
	if err := s.change.lock(ctx); err != nil {
		return nil, err
	}
	defer s.change.unlock()
	if err := s.requireRestoreCaller(c); err != nil {
		return nil, err
	}
	res, err := s.repo.Restore(ctx, ref, paths)
	if err != nil {
		return nil, err
	}
	restored := res.Restored
	if restored == nil {
		restored = []string{}
	}
	s.log.Info("snapshot restored", "ref", ref, "files", len(restored))
	return &RestoreResult{Restored: restored, Text: res.Text}, nil
}

// Ready answers POST /v1/bridge/ready with the deliveries of contracts 6 and marks the starting process ready.
func (s *Supervisor) Ready(tools, commands []string) (*wire.ReadyResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.wire.AgentProcessExists() {
		return nil, &StateError{State: s.wire}
	}
	res := &wire.ReadyResult{}
	d := &s.st.Deliver
	res.LastExit, d.LastExit = d.LastExit, nil
	res.Applied, d.Applied = d.Applied, nil
	res.Incident, d.Incident = d.Incident, nil
	res.ClassifierRestore, d.ClassifierRestore = d.ClassifierRestore, nil
	if r := s.st.Rollback; r != nil {
		c := *r
		c.Reverted = slices.Clone(r.Reverted)
		c.Requesters = slices.Clone(r.Requesters)
		res.Rollback = &c
		r.Notified = true
	}
	if err := state.Save(s.paths.StateFile, s.st); err != nil {
		return nil, err
	}
	pid := 0
	if s.cur != nil {
		pid = s.cur.p.PID()
		s.cur.markReady()
	}
	s.log.Info("bridge/ready", "pid", pid, "tools", tools, "commands", commands,
		"lastExit", res.LastExit != nil, "applied", res.Applied != nil, "incident", res.Incident != nil,
		"rollback", res.Rollback != nil, "classifierRestore", res.ClassifierRestore != nil)
	return res, nil
}

// ClassifierTurnStart records CLASSIFIER.md at the start of a member turn (DESIGN 7.10).
func (s *Supervisor) ClassifierTurnStart(turnID string, senders []string) (*classifierturn.StartResult, error) {
	if turnID == "" {
		return nil, userErr("turnId 不能为空")
	}
	if senders == nil {
		senders = []string{}
	}
	s.classifierMu.Lock()
	defer s.classifierMu.Unlock()
	pending, err := s.recorder.Pending()
	if err != nil {
		return nil, err
	}
	if pending != nil {
		return nil, &ConflictError{fmt.Sprintf("回合 %s 的 CLASSIFIER.md 记录还没有结束", pending.TurnID)}
	}
	res, rec, err := s.recorder.Start(turnID, senders, s.cfg.Classifier.RulesMaxBytes)
	if err != nil {
		return nil, err
	}
	if err := s.update(func(x *state.State) {
		x.ClassifierTurn = &state.ClassifierTurn{TurnID: turnID, StartedAt: rec.StartedAt, Senders: senders}
	}); err != nil {
		return nil, err
	}
	return res, nil
}

// TurnEndResult is the answer of the end phase.
type TurnEndResult struct {
	Restored bool   `json:"restored"`
	Detail   string `json:"detail,omitempty"`
}

// ClassifierTurnEnd compares CLASSIFIER.md at the end of a member turn and restores it (DESIGN 7.10). Without an
// open record (the daemon compared already) nothing changed.
func (s *Supervisor) ClassifierTurnEnd(turnID string) (*TurnEndResult, error) {
	if turnID == "" {
		return nil, userErr("turnId 不能为空")
	}
	s.classifierMu.Lock()
	defer s.classifierMu.Unlock()
	pending, err := s.recorder.Pending()
	if err != nil {
		return nil, err
	}
	if pending == nil {
		return &TurnEndResult{}, nil
	}
	if pending.TurnID != turnID {
		return nil, &ConflictError{fmt.Sprintf("未结束的回合是 %s，不是 %s", pending.TurnID, turnID)}
	}
	res, err := s.recorder.End(turnID)
	if err != nil {
		return nil, err
	}
	if err := s.finishClassifierTurn(res, false); err != nil {
		return nil, err
	}
	if !res.Changed {
		return &TurnEndResult{}, nil
	}
	return &TurnEndResult{Restored: res.Restored, Detail: res.Detail}, nil
}

// RecordDecision appends a classifier decision to logs/agent/classifier.jsonl.
func (s *Supervisor) RecordDecision(d wire.ClassifierDecision) error {
	if err := d.Validate(); err != nil {
		return &UserError{err}
	}
	return s.classifierLog.WriteRecord(wire.DecisionRecord{Type: "decision", ClassifierDecision: d})
}

// RollbackAck deletes the rollback record id (DESIGN 9.6 step 3).
func (s *Supervisor) RollbackAck(id int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.st.Rollback
	if r == nil {
		return &ConflictError{"没有尚未确认的回滚记录"}
	}
	if r.ID != id {
		return &ConflictError{fmt.Sprintf("回滚记录已经更新为 %d（合并了新的回滚），不能确认 %d", r.ID, id)}
	}
	s.st.Rollback = nil
	if err := state.Save(s.paths.StateFile, s.st); err != nil {
		return err
	}
	s.log.Info("rollback record acknowledged", "id", id)
	return nil
}

// RecoveryReport hands a recovery report to the repair flow (POST /v1/recovery/report).
func (s *Supervisor) RecoveryReport(r wire.RecoveryReport) error {
	if err := r.Validate(); err != nil {
		return &UserError{err}
	}
	if err := s.incidents.SubmitReport(r); err != nil {
		return &ConflictError{err.Error()}
	}
	s.log.Info("recovery report received", "status", r.Status)
	return nil
}

// IncidentList is the text of `incident list`.
func (s *Supervisor) IncidentList() (string, error) { return incident.ListText(s.paths) }

// IncidentShow is the text of `incident show <id>`.
func (s *Supervisor) IncidentShow(id string) (string, error) {
	if !incident.ValidID(id) {
		return "", userErr("无效的故障 id %q（格式为 inc-<14 位数字>）", id)
	}
	text, err := incident.ShowText(s.paths, id)
	if err != nil {
		return "", &UserError{err}
	}
	return text, nil
}

// LogsDefault is the number of lines when n is not given; LogsMax bounds n.
const (
	LogsDefault = 100
	LogsMax     = 10000
)

// LogPath returns the log file of GET /v1/logs source and kind.
func (s *Supervisor) LogPath(source, kind string) (string, error) {
	switch source {
	case "agent":
		switch kind {
		case "stderr":
			return s.paths.AgentStderrLog, nil
		case "rpc":
			return s.paths.AgentRPCEventsLog, nil
		case "exits":
			return s.paths.AgentExitsLog, nil
		default:
			return "", userErr("kind 必须是 stderr、rpc 或 exits，收到 %q", kind)
		}
	case "gateway":
		return s.paths.GatewayLog, nil
	case "daemon":
		return s.paths.DaemonLog, nil
	default:
		return "", userErr("source 必须是 agent、gateway 或 daemon，收到 %q", source)
	}
}

// Logs returns the last n lines of a log (GET /v1/logs).
func (s *Supervisor) Logs(source, kind string, n int) ([]string, error) {
	path, err := s.LogPath(source, kind)
	if err != nil {
		return nil, err
	}
	lines, err := proc.TailFile(path, n)
	if err != nil {
		return nil, err
	}
	if lines == nil {
		lines = []string{}
	}
	return lines, nil
}
