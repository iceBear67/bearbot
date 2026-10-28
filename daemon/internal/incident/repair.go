package incident

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"bearbot/daemon/internal/check"
	"bearbot/daemon/internal/recovery"
	"bearbot/daemon/internal/snapshot"
	"bearbot/daemon/internal/state"
	"bearbot/daemon/internal/wire"
)

// cannotRunError means "recovery 无法运行" (DESIGN 5.7): the flow goes to the maintenance state with this reason.
type cannotRunError struct{ err error }

func (e *cannotRunError) Error() string { return e.err.Error() }

// noReportError means recovery gave no report; it is handled as cannot_fix.
type noReportError struct{ reason string }

func (e *noReportError) Error() string { return e.reason }

const reportReminder = "这一轮结束了，但 daemon 没有收到修复报告。请调用 recovery_report 提交报告：已经修复并且 recovery_check 通过时 status 为 fixed，无法修复时为 cannot_fix。"

// repair runs the repair flow for rec (DESIGN 5.7 steps 1-5 and 7, contracts 9.3). note is the administrator's note
// and maintenanceReason the reason of the maintenance state the run starts from; both are empty for the automatic
// run. The recovery profile with the recovery credentials exists only while a recovery process runs (DESIGN 15): it
// is prepared before each recovery process starts and removed when the process is stopped and when the flow ends.
func (m *Manager) repair(ctx context.Context, rec *Record, note, maintenanceReason string) (outcome Outcome, err error) {
	h := m.h
	p := h.Paths()
	cfg := h.Config()
	h.SetWire(wire.Repairing, "故障 "+rec.ID)
	rec.Status, rec.Resolution, rec.MaintenanceReason = StatusRepairing, nil, ""
	rec.Repairs = append(rec.Repairs, Repair{StartedAt: time.Now(), Note: note, MaintenanceReason: maintenanceReason})
	if err := rec.Save(p); err != nil {
		return 0, err
	}
	if err := h.Update(func(s *state.State) {
		s.OpenIncident.State = state.IncidentRepairing
		s.OpenIncident.Attempts = 0
	}); err != nil {
		return 0, err
	}
	m.openLog(rec.ID)
	defer m.closeLog(rec.ID)

	defer func() {
		if rmErr := recovery.RemoveProfile(p); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
	}()
	if err := recovery.PrepareProfile(p, h.Users()); err != nil {
		return m.toMaintenance(ctx, rec, "recovery 无法运行：准备 recovery profile 失败："+err.Error())
	}
	if err := recovery.CheckCredentials(ctx, p, h.Users(), cfg, h.Node(), rec.ID); err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return m.toMaintenance(ctx, rec, "recovery 无法运行："+err.Error())
	}
	rp, err := m.startRecovery(ctx, rec.ID)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return m.toMaintenance(ctx, rec, "recovery 无法运行："+err.Error())
	}
	prompt := m.initialPrompt(rec, note, maintenanceReason)
	attempts := 0
	for {
		report, err := m.waitReport(ctx, rp, prompt)
		if err != nil {
			stopErr := m.stopRecovery(rp)
			if ctx.Err() != nil {
				return 0, errors.Join(ctx.Err(), stopErr)
			}
			if stopErr != nil {
				return 0, stopErr
			}
			var cr *cannotRunError
			if errors.As(err, &cr) {
				return m.toMaintenance(ctx, rec, "recovery 无法运行："+cr.Error())
			}
			var nr *noReportError
			if errors.As(err, &nr) {
				return m.toMaintenance(ctx, rec, nr.reason)
			}
			return 0, err
		}
		if report.Status == wire.ReportCannotFix {
			if err := m.stopRecovery(rp); err != nil {
				return 0, err
			}
			return m.toMaintenance(ctx, rec, "recovery 报告无法修复："+strings.TrimSpace(report.Summary))
		}

		attempts++
		a, err := m.recordFixed(ctx, rec, *report, attempts)
		if err != nil {
			return 0, errors.Join(err, m.stopRecovery(rp))
		}
		rep, err := h.Check(ctx)
		if err != nil {
			return 0, errors.Join(err, m.stopRecovery(rp))
		}
		a.Check = &CheckSummary{ID: rep.ID, Passed: rep.Passed, ReportPath: rep.ReportPath, Problems: checkProblems(rep)}
		if err := m.saveAttempt(rec, *a); err != nil {
			return 0, errors.Join(err, m.stopRecovery(rp))
		}
		if !rep.Passed {
			if attempts < cfg.Recovery.Attempts {
				if rp, err = m.ensureRecovery(ctx, rp, rec.ID); err != nil {
					return m.recoveryLost(ctx, rec, err)
				}
				prompt = checkFailedPrompt(*a, rep, cfg.Recovery.Attempts-attempts)
				continue
			}
			if err := m.stopRecovery(rp); err != nil {
				return 0, err
			}
			return m.toMaintenance(ctx, rec, fmt.Sprintf("第 %d 次修复没有通过检查，修复次数（%d 次）已经用完：\n%s", a.N, cfg.Recovery.Attempts, a.Check.Problems))
		}

		// The check passed: recovery and the agent never run together.
		if err := m.stopRecovery(rp); err != nil {
			return 0, err
		}
		rec.Status = StatusProbation
		rec.Resolution = &Resolution{Kind: ResolvedByRecovery, Attempt: a.N}
		if err := rec.Save(p); err != nil {
			return 0, err
		}
		if err := h.Update(func(s *state.State) {
			s.RunningSnapshot = a.Commit
			s.PendingApply = nil
			// The next start runs in probation, also when bearbotd stops before it starts the agent.
			s.LastGoodPending = true
			s.ProbationUntil = nil
			s.OpenIncident.State = state.IncidentProbation
		}); err != nil {
			return 0, err
		}
		startErr := h.StartAgent(ctx, true)
		if startErr == nil {
			a.Start = &StartSummary{OK: true}
			rec.Repairs[len(rec.Repairs)-1].Result = fmt.Sprintf("第 %d 次修复通过检查，agent 已启动，进入观察期", a.N)
			if err := m.saveAttempt(rec, *a); err != nil {
				return 0, err
			}
			h.Notify(fmt.Sprintf("故障 %s 已由 recovery 修复（第 %d 次修复）：%s\nagent 已启动，进入观察期（%s），观察期结束后关闭故障。", rec.ID, a.N, strings.TrimSpace(a.Report.Summary), cfg.Agent.Probation))
			return Running, nil
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		var f *Failure
		if !errors.As(startErr, &f) {
			return 0, startErr
		}
		a.Start = &StartSummary{OK: false, Error: f.Text()}
		rec.Status, rec.Resolution = StatusRepairing, nil
		if err := m.saveAttempt(rec, *a); err != nil {
			return 0, err
		}
		if err := h.Update(func(s *state.State) { s.OpenIncident.State = state.IncidentRepairing }); err != nil {
			return 0, err
		}
		h.SetWire(wire.Repairing, "故障 "+rec.ID)
		if attempts >= cfg.Recovery.Attempts {
			return m.toMaintenance(ctx, rec, fmt.Sprintf("第 %d 次修复通过了检查，但 agent 启动失败，修复次数（%d 次）已经用完：%s", a.N, cfg.Recovery.Attempts, f.Detail))
		}
		// contracts 9.3: a new recovery process with the same session id continues the conversation; the profile was
		// removed when the previous process was stopped.
		if err := recovery.PrepareProfile(p, h.Users()); err != nil {
			return m.toMaintenance(ctx, rec, "recovery 无法运行：准备 recovery profile 失败："+err.Error())
		}
		if rp, err = m.startRecovery(ctx, rec.ID); err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			return m.toMaintenance(ctx, rec, "recovery 无法运行："+err.Error())
		}
		prompt = startFailedPrompt(*a, f, cfg.Recovery.Attempts-attempts)
	}
}

// recoveryLost ends the flow when a recovery process that has to continue cannot be started again.
func (m *Manager) recoveryLost(ctx context.Context, rec *Record, err error) (Outcome, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	return m.toMaintenance(ctx, rec, "recovery 无法运行："+err.Error())
}

func (m *Manager) startRecovery(ctx context.Context, id string) (*recovery.Process, error) {
	h := m.h
	rp, err := recovery.Start(ctx, recovery.Options{
		Paths:      h.Paths(),
		Users:      h.Users(),
		Config:     h.Config(),
		Node:       h.Node(),
		IncidentID: id,
		Log:        m.RecoveryLog(id),
		Logger:     h.Logger(),
	})
	if err != nil {
		return nil, err
	}
	h.Logger().Info("recovery process started", "incident", id, "pid", rp.PID())
	return rp, nil
}

// ensureRecovery returns rp when it still runs, otherwise a new recovery process for the same incident (with the
// profile prepared again).
func (m *Manager) ensureRecovery(ctx context.Context, rp *recovery.Process, id string) (*recovery.Process, error) {
	select {
	case <-rp.Done():
		m.h.Logger().Warn("recovery process exited after its report; starting a new one", "incident", id, "exit", rp.Exit().String())
		if err := m.stopRecovery(rp); err != nil {
			return nil, err
		}
		if err := recovery.PrepareProfile(m.h.Paths(), m.h.Users()); err != nil {
			return nil, fmt.Errorf("准备 recovery profile 失败：%w", err)
		}
		return m.startRecovery(ctx, id)
	default:
		return rp, nil
	}
}

// stopRecovery stops the recovery process and removes the recovery profile with its credentials.
func (m *Manager) stopRecovery(rp *recovery.Process) error {
	if err := rp.Close(); err != nil {
		return fmt.Errorf("停止 recovery 进程失败：%w", err)
	}
	return recovery.RemoveProfile(m.h.Paths())
}

// recordFixed commits incident/<id>/attempt-<n> for a fixed report and records the attempt.
func (m *Manager) recordFixed(ctx context.Context, rec *Record, report wire.RecoveryReport, attempts int) (*Attempt, error) {
	h := m.h
	n, tag, commit, err := m.commitAttempt(ctx, rec.ID, report)
	if err != nil {
		return nil, err
	}
	a := &Attempt{N: n, Snapshot: tag, Commit: commit, ReportedAt: time.Now(), Report: report}
	if err := m.saveAttempt(rec, *a); err != nil {
		return nil, err
	}
	if err := h.Update(func(s *state.State) { s.OpenIncident.Attempts = attempts }); err != nil {
		return nil, err
	}
	h.Logger().Info("recovery reported a fix", "incident", rec.ID, "attempt", n, "snapshot", tag)
	return a, nil
}

// commitAttempt commits incident/<id>/attempt-<n> holding the change lock.
func (m *Manager) commitAttempt(ctx context.Context, id string, report wire.RecoveryReport) (int, string, string, error) {
	repo := m.h.Repo()
	unlock, err := m.h.LockChanges(ctx)
	if err != nil {
		return 0, "", "", err
	}
	defer unlock()
	n, err := repo.NextAttempt(ctx, id)
	if err != nil {
		return 0, "", "", err
	}
	tag := snapshot.IncidentAttemptTag(id, n)
	commit, err := repo.Commit(ctx, tag, snapshot.Message{Summary: fmt.Sprintf("故障 %s 第 %d 次修复：%s", id, n, firstLine(report.Summary))})
	if err != nil {
		return 0, "", "", err
	}
	return n, tag, commit, nil
}

func (m *Manager) saveAttempt(rec *Record, a Attempt) error {
	rec.setAttempt(a)
	p := m.h.Paths()
	if err := rec.SaveAttempt(p, a); err != nil {
		return err
	}
	return rec.Save(p)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}

// toMaintenance ends a repair run in the maintenance state.
func (m *Manager) toMaintenance(ctx context.Context, rec *Record, reason string) (Outcome, error) {
	rec.Status, rec.MaintenanceReason = StatusMaintenance, reason
	if len(rec.Repairs) > 0 {
		rec.Repairs[len(rec.Repairs)-1].Result = "进入维护状态：" + reason
	}
	if err := rec.Save(m.h.Paths()); err != nil {
		return 0, err
	}
	id := rec.ID
	if err := m.EnterMaintenance(ctx, reason, &id); err != nil {
		return 0, err
	}
	return InMaintenance, nil
}

func drainTurns(rp *recovery.Process) {
	for {
		select {
		case <-rp.Turns():
		default:
			return
		}
	}
}

// waitReport sends prompt and waits for the report (DESIGN 5.7 steps 2-3). A run that ends without a report gets
// one reminder; when the second run ends without a report too, the result is a noReportError (handled as
// cannot_fix). A run that ends with an error, a failed prompt or an exit of the process is a cannotRunError.
func (m *Manager) waitReport(ctx context.Context, rp *recovery.Process, prompt string) (*wire.RecoveryReport, error) {
	cfg := m.h.Config()
	reminded := false
	for {
		drainTurns(rp)
		box := m.openBox()
		if err := rp.Prompt(ctx, prompt); err != nil {
			m.takeBox()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &cannotRunError{fmt.Errorf("向 recovery 发送提示失败：%w", err)}
		}
		timer := time.NewTimer(cfg.Recovery.Timeout)
		select {
		case rep := <-box:
			m.takeBox()
			err := m.settle(ctx, rp, timer)
			timer.Stop()
			if err != nil {
				return nil, err
			}
			return &rep, nil
		case end := <-rp.Turns():
			timer.Stop()
			if rep, ok := m.takeBox(); ok {
				return &rep, nil
			}
			if end.Failed() {
				return nil, &cannotRunError{fmt.Errorf("recovery 的回合以错误结束：%s", end.ErrorMessage)}
			}
			if reminded {
				return nil, &noReportError{"提醒之后 recovery 仍然没有提交修复报告，按无法修复处理"}
			}
			reminded = true
			prompt = reportReminder
		case <-rp.Done():
			timer.Stop()
			if rep, ok := m.takeBox(); ok {
				return &rep, nil
			}
			reason := fmt.Sprintf("recovery 进程退出（%s）", rp.Exit())
			if tail := strings.Join(rp.StderrTail(30), "\n"); tail != "" {
				reason += "，stderr 最后的内容：\n" + tail
			}
			return nil, &cannotRunError{errors.New(reason)}
		case <-timer.C:
			if rep, ok := m.takeBox(); ok {
				return &rep, nil
			}
			if err := rp.Abort(ctx); err != nil {
				return nil, &cannotRunError{fmt.Errorf("recovery 超过 %s 没有提交报告，中止它的回合也失败了：%w", cfg.Recovery.Timeout, err)}
			}
			return nil, &noReportError{fmt.Sprintf("recovery 在 %s 内没有提交修复报告", cfg.Recovery.Timeout)}
		case <-ctx.Done():
			timer.Stop()
			m.takeBox()
			return nil, ctx.Err()
		}
	}
}

// settle waits until the run that delivered the report ends, so the process can take the next prompt; it aborts
// the run when the remaining time of [recovery] timeout is used up.
func (m *Manager) settle(ctx context.Context, rp *recovery.Process, timer *time.Timer) error {
	select {
	case <-rp.Turns():
		return nil
	case <-rp.Done():
		return nil
	case <-timer.C:
		if err := rp.Abort(ctx); err != nil {
			return &cannotRunError{fmt.Errorf("recovery 提交报告之后回合一直没有结束，中止它的回合失败：%w", err)}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// initialPrompt is the incident report of DESIGN 10.4. maintenanceReason is the reason of the maintenance state the
// run started from when it continues an open incident; it is written before the administrator's note.
func (m *Manager) initialPrompt(rec *Record, note, maintenanceReason string) string {
	p := m.h.Paths()
	var b strings.Builder
	fmt.Fprintf(&b, "故障 %s\n", rec.ID)
	fmt.Fprintf(&b, "触发：%s\n", rec.Trigger)
	fmt.Fprintf(&b, "详情：%s\n", orNone(rec.Detail))
	fmt.Fprintf(&b, "退出码 / 信号：%s\n", exitText(rec.Exit))
	fmt.Fprintf(&b, "stderr 最后 200 行：\n%s\n", orNone(strings.Join(rec.StderrTail, "\n")))
	fmt.Fprintf(&b, "rpc 事件最后 50 条：\n%s\n", orNone(strings.Join(rec.RPCEvents, "\n")))
	fmt.Fprintf(&b, "最近一次应用：%s\n", lastApplyText(rec.LastApply))
	fmt.Fprintf(&b, "相对 last-good 的改动：\n%s\n", orNone(rec.DiffStat))
	b.WriteString("此前的修复尝试：")
	if len(rec.Attempts) == 0 {
		b.WriteString("无\n")
	} else {
		b.WriteString("\n")
		for _, a := range rec.Attempts {
			fmt.Fprintf(&b, "第 %d 次（快照 %s）\n报告：%s\n%s", a.N, a.Snapshot, strings.TrimSpace(a.Report.Summary), a.verificationText())
		}
	}
	if maintenanceReason != "" {
		fmt.Fprintf(&b, "进入维护状态的原因：%s\n", maintenanceReason)
	}
	fmt.Fprintf(&b, "管理员说明：%s\n", orNone(note))
	fmt.Fprintf(&b, "快照仓库：git --git-dir=%s --work-tree=%s …（故障时的状态是快照 %s）\n", p.SnapshotsGit, p.SnapshotWorkTree, rec.Before)
	return b.String()
}

func checkFailedPrompt(a Attempt, rep *check.Report, remaining int) string {
	return fmt.Sprintf("第 %d 次修复（快照 %s）没有通过 daemon 的检查（%s，完整报告 %s）：\n%s\n还可以再尝试 %d 次。请继续修复，用 recovery_check 确认通过后再调用 recovery_report。",
		a.N, a.Snapshot, rep.ID, rep.ReportPath, checkProblems(rep), remaining)
}

func startFailedPrompt(a Attempt, f *Failure, remaining int) string {
	return fmt.Sprintf("第 %d 次修复（快照 %s）通过了检查，但 agent 进程启动失败：\n%s\n还可以再尝试 %d 次。请继续修复，用 recovery_check 确认通过后再调用 recovery_report。",
		a.N, a.Snapshot, f.Text(), remaining)
}
