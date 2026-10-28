// Package rollback runs the administrator rollback of DESIGN 5.12: stop the agent, save rollback/<n>/before,
// restore the restart effective paths to the target, save rollback/<n>/after, write or merge the rollback record,
// and start the agent in probation. A failure in steps 3-6 enters the maintenance state.
package rollback

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"bearbot/daemon/internal/incident"
	"bearbot/daemon/internal/snapshot"
	"bearbot/daemon/internal/state"
	"bearbot/daemon/internal/wire"
)

// Host extends the incident host with stopping the agent.
type Host interface {
	incident.Host
	// StopAgent sends RPC abort without waiting for the turn to end, records the session id and stops the agent
	// process with the stop procedure of DESIGN 5.3 (including the CLASSIFIER.md comparison of an open member turn).
	// It does nothing when no agent process exists.
	StopAgent(ctx context.Context) error
}

// Request is one rollback request. Requester and Thread are nil when it came from the command line.
type Request struct {
	// Snapshot is the target given by the administrator; empty for the default target.
	Snapshot  string
	Requester *string
	Thread    *string
}

// Result is the outcome of Run.
type Result struct {
	// Text is the reply for the requester (and the notice to the administrators).
	Text string
	// Started is true when the agent runs in probation afterwards.
	Started bool
	// Failure is set when the agent did not start after the rollback; the supervisor opens an incident with it.
	Failure *incident.Failure
	// Maintenance is true when the rollback failed and the bot is in the maintenance state.
	Maintenance bool
}

// Target validates the requested snapshot, or computes the default target apply/<n>^ (DESIGN 5.12), and checks that
// it exists. It changes nothing, so a bad target is rejected before the rollback begins.
func Target(ctx context.Context, repo *snapshot.Repo, requested string) (string, error) {
	target := requested
	if target == "" {
		var err error
		if target, err = repo.DefaultRollbackTarget(ctx); err != nil {
			return "", err
		}
	}
	if _, err := repo.Resolve(ctx, target); err != nil {
		return "", err
	}
	return target, nil
}

// Merge returns the rollback record after rollback n: a new record, or the unacknowledged one merged with it
// (target, saved and thread from the newest, reverted as the union, requesters appended, notified false).
func Merge(old *state.Rollback, n int, target, saved string, reverted []state.Reverted, requester, thread *string) *state.Rollback {
	rec := &state.Rollback{ID: n, Target: target, Saved: saved, Thread: thread, Notified: false}
	if old != nil {
		rec.Reverted = append(rec.Reverted, old.Reverted...)
		rec.Requesters = append(rec.Requesters, old.Requesters...)
	}
	for _, r := range reverted {
		if !slices.ContainsFunc(rec.Reverted, func(o state.Reverted) bool { return o.Ref == r.Ref }) {
			rec.Reverted = append(rec.Reverted, r)
		}
	}
	if requester != nil && !slices.Contains(rec.Requesters, *requester) {
		rec.Requesters = append(rec.Requesters, *requester)
	}
	if rec.Reverted == nil {
		rec.Reverted = []state.Reverted{}
	}
	return rec
}

func revertedText(reverted []state.Reverted) string {
	if len(reverted) == 0 {
		return "无"
	}
	parts := make([]string, len(reverted))
	for i, r := range reverted {
		parts[i] = r.Ref + "：" + r.Summary
	}
	return strings.Join(parts, "；")
}

// Run performs DESIGN 5.12 steps 2-9 for target (already checked with Target). The caller holds the change lock;
// fromMaintenance tells whether the state before was MAINTENANCE.
func Run(ctx context.Context, h Host, m *incident.Manager, req Request, target string, fromMaintenance bool) (Result, error) {
	repo := h.Repo()
	log := h.Logger()
	h.SetWire(wire.RollingBack, "回滚到 "+target)
	log.Info("rollback started", "target", target, "requester", req.Requester)

	// actor names who started the rollback in texts; source is the short form for commit messages.
	actor, source := "管理员通过命令行", "命令行"
	if req.Requester != nil {
		actor, source = "管理员 "+*req.Requester+" ", "管理员 "+*req.Requester
	}
	fail := func(step string, err error) (Result, error) {
		reason := fmt.Sprintf("回滚到 %s 时%s失败：%v", target, step, err)
		log.Error("rollback failed", "step", step, "err", err)
		if err := m.EnterMaintenance(ctx, reason, nil); err != nil {
			return Result{}, err
		}
		return Result{Text: reason + "\nbot 已进入维护状态。", Maintenance: true}, nil
	}

	// Step 3.
	if err := h.StopAgent(ctx); err != nil {
		return fail("停止 agent 进程", err)
	}
	// The pending apply is discarded; an apply the rollback reverts is never reported as applied. The probation of
	// the stopped process ends; the start after the rollback begins a new one.
	if err := h.Update(func(s *state.State) {
		s.PendingApply = nil
		s.Deliver.Applied = nil
		s.ProbationUntil = nil
	}); err != nil {
		return Result{}, err
	}
	// The file operations run to the end even when the daemon is asked to stop meanwhile.
	fileCtx := context.WithoutCancel(ctx)
	reverted, err := repo.AppliesAfter(fileCtx, target)
	if err != nil {
		return fail("查找被撤销的应用", err)
	}
	// Step 4.
	n, err := repo.NextRollback(fileCtx)
	if err != nil {
		return fail("分配回滚序号", err)
	}
	before := snapshot.RollbackBeforeTag(n)
	msg := snapshot.Message{Summary: fmt.Sprintf("回滚到 %s 之前的状态（%s）", target, source)}
	if req.Requester != nil {
		msg.Requester = *req.Requester
	}
	if req.Thread != nil {
		msg.Thread = *req.Thread
	}
	if _, err := repo.Commit(fileCtx, before, msg); err != nil {
		return fail("提交快照 "+before, err)
	}
	// Step 5.
	restored, err := repo.Restore(fileCtx, target, nil)
	if err != nil {
		return fail("恢复文件", err)
	}
	// Step 6.
	after := snapshot.RollbackAfterTag(n)
	msg.Summary = fmt.Sprintf("回滚到 %s（%s）", target, source)
	afterCommit, err := repo.Commit(fileCtx, after, msg)
	if err != nil {
		return fail("提交快照 "+after, err)
	}
	docs, err := repo.DiffStatPaths(fileCtx, target, after, snapshot.DocumentPaths)
	if err != nil {
		return fail("比较 AGENTS.md 与 CLASSIFIER.md", err)
	}
	// Step 7.
	var record *state.Rollback
	if err := h.Update(func(s *state.State) {
		s.RunningSnapshot = afterCommit
		// The next start runs in probation, also when bearbotd stops before step 8 starts the agent.
		s.LastGoodPending = true
		s.ProbationUntil = nil
		s.Rollback = Merge(s.Rollback, n, target, before, reverted, req.Requester, req.Thread)
		record = s.Rollback
	}); err != nil {
		return Result{}, err
	}
	log.Info("rollback files restored", "target", target, "before", before, "after", after, "record", record.ID)

	var b strings.Builder
	fmt.Fprintf(&b, "%s把 bot 回滚到了快照 %s（%s）。\n", actor, target, shortCommit(restored.Commit))
	fmt.Fprintf(&b, "回滚之前的状态保存在 %s，回滚之后的状态为 %s。\n", before, after)
	fmt.Fprintf(&b, "被撤销的应用：%s\n", revertedText(reverted))
	b.WriteString(restored.Text)
	if docs == "" {
		b.WriteString("AGENTS.md 与 CLASSIFIER.md 保持回滚之前的内容，与目标快照相同。\n")
	} else {
		fmt.Fprintf(&b, "AGENTS.md 与 CLASSIFIER.md 保持回滚之前的内容，它们与目标快照之间的差别：\n%s\n", docs)
	}

	// Step 8.
	if fromMaintenance {
		if err := m.ExitMaintenance(ctx, fmt.Sprintf("回滚到 %s（%s）", target, source)); err != nil {
			return Result{}, err
		}
	}
	if err := m.Resolve(incident.ResolvedByRollback, fmt.Sprintf("回滚到 %s（%s）", target, after)); err != nil {
		return Result{}, err
	}
	res := Result{}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	startErr := h.StartAgent(ctx, true)
	switch {
	case startErr == nil:
		res.Started = true
		fmt.Fprintf(&b, "agent 已重新启动，进入观察期（%s）。", h.Config().Agent.Probation)
	case errors.As(startErr, &res.Failure):
		fmt.Fprintf(&b, "agent 启动失败：%s\n将建立故障，由 recovery 修复。", res.Failure.Brief())
	default:
		return Result{}, startErr
	}
	// Step 9.
	res.Text = b.String()
	h.Notify(res.Text)
	return res, nil
}

func shortCommit(c string) string { return c[:min(7, len(c))] }
