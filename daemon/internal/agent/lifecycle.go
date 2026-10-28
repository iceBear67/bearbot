package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"bearbot/daemon/internal/incident"
	"bearbot/daemon/internal/pirpc"
	"bearbot/daemon/internal/rollback"
	"bearbot/daemon/internal/state"
	"bearbot/daemon/internal/wire"
)

// drainQuiet is how long no agent_start may follow agent_settled before a draining agent is restarted (DESIGN 5.4).
const drainQuiet = time.Second

// opResult is the reply of a lifecycle command.
type opResult struct {
	text string
	err  error
}

// command is a request handed to the lifecycle goroutine.
type command interface {
	op() string
	reject(err error)
}

type restartCmd struct{ reply chan opResult }
type newSessionCmd struct{ reply chan opResult }
type applyDrainCmd struct{ snapshot string }
type repairCmd struct {
	note  string
	reply chan opResult
}
type resumeCmd struct{ reply chan opResult }

func (restartCmd) op() string    { return "restart" }
func (newSessionCmd) op() string { return "session new" }
func (applyDrainCmd) op() string { return "apply" }
func (repairCmd) op() string     { return "repair" }
func (resumeCmd) op() string     { return "resume" }

func (c restartCmd) reject(err error)    { c.reply <- opResult{err: err} }
func (c newSessionCmd) reject(err error) { c.reply <- opResult{err: err} }
func (applyDrainCmd) reject(error)       {}
func (c repairCmd) reject(err error)     { c.reply <- opResult{err: err} }
func (c resumeCmd) reject(err error)     { c.reply <- opResult{err: err} }

// rollbackCmd is a rollback whose requester holds the change lock.
type rollbackCmd struct {
	req    rollback.Request
	target string
	reply  chan opResult
}

// pendingRollback is a rollback request that waits for the change lock or for the lifecycle goroutine.
type pendingRollback struct{ cancel context.CancelFunc }

// Steps of the lifecycle loop.
type step interface{}

type startReplyKind int

const (
	replyRestart startReplyKind = iota
	replyNewSession
)

type startReply struct {
	kind startReplyKind
	ch   chan opResult
}

// startStep starts the agent process; with probation the start begins a new probation, otherwise it keeps the
// stored one (DESIGN 5.4 观察期).
type startStep struct {
	probation bool
	replies   []startReply
}
type monitorStep struct{}
type incidentStep struct{ failure *incident.Failure }
type continueStep struct{}
type maintenanceStep struct{}
type stopStep struct{}

// Run is the lifecycle goroutine. It returns nil after ctx was cancelled and the agent was stopped (the shutdown
// of DESIGN 5.2: wait for the turn up to shutdown_grace, then the stop procedure), or the error that makes the
// daemon exit.
func (s *Supervisor) Run(ctx context.Context) error {
	next := s.initialStep()
	for {
		var err error
		switch n := next.(type) {
		case startStep:
			next, err = s.runStart(ctx, n)
		case monitorStep:
			next, err = s.monitor(ctx)
		case incidentStep:
			next, err = flowStep(s.incidents.Open(ctx, n.failure))
		case continueStep:
			next, err = flowStep(s.incidents.Continue(ctx))
		case maintenanceStep:
			next, err = s.maintenance(ctx)
		case stopStep:
			err := s.stopLeftover()
			s.setWire(wire.Stopped, "")
			return err
		default:
			panic(fmt.Sprintf("unknown lifecycle step %T", next))
		}
		if err != nil {
			if ctx.Err() != nil && errors.Is(err, context.Canceled) {
				next = stopStep{}
				continue
			}
			s.setListening(false)
			s.setWire(wire.Stopped, "bearbotd 出错")
			return errors.Join(err, s.stopLeftover())
		}
	}
}

// stopLeftover stops an agent process that is still running when the lifecycle ends: the daemon exits and the
// process must not stay behind.
func (s *Supervisor) stopLeftover() error {
	s.mu.Lock()
	ap := s.cur
	s.mu.Unlock()
	if ap == nil {
		return nil
	}
	s.log.Warn("stopping the agent process left running at the end of the lifecycle", "pid", ap.p.PID())
	return s.stopAgent(ap, true)
}

func (s *Supervisor) initialStep() step {
	st := s.stateCopy()
	switch {
	case st.Maintenance != nil:
		s.setWire(wire.Maintenance, st.Maintenance.Reason)
		s.notify(fmt.Sprintf("bearbotd 已启动，bot 仍处于维护状态。\n原因：%s\n可用的命令：/bb repair [说明]、/bb rollback [快照]、/bb resume；在命令行中为 bearbotd repair、bearbotd rollback、bearbotd resume。", st.Maintenance.Reason))
		return maintenanceStep{}
	case st.OpenIncident != nil && st.OpenIncident.State == state.IncidentRepairing:
		return continueStep{}
	default:
		// A start that began a probation and was not ready when bearbotd stopped begins it with this start; a stored
		// probation end is kept or has passed (probationAtReady).
		return startStep{probation: st.LastGoodPending && st.ProbationUntil == nil}
	}
}

// flowStep converts the outcome of an incident flow into the next step.
func flowStep(o incident.Outcome, err error) (step, error) {
	if err != nil {
		return nil, err
	}
	if o == incident.Running {
		return monitorStep{}, nil
	}
	return maintenanceStep{}, nil
}

func (s *Supervisor) runStart(ctx context.Context, n startStep) (step, error) {
	err := s.startAgent(ctx, n.probation)
	s.replyStart(n.replies, err)
	if err == nil {
		return monitorStep{}, nil
	}
	var f *incident.Failure
	if errors.As(err, &f) {
		return incidentStep{failure: f}, nil
	}
	return nil, err
}

func (s *Supervisor) replyStart(replies []startReply, err error) {
	if len(replies) == 0 {
		return
	}
	var text string
	var f *incident.Failure
	switch {
	case err == nil:
		s.mu.Lock()
		pid, session := 0, s.st.SessionID
		if s.cur != nil {
			pid = s.cur.p.PID()
		}
		s.mu.Unlock()
		text = fmt.Sprintf("agent 已重启（PID %d，会话 %s）。", pid, session)
	case errors.As(err, &f):
		text = fmt.Sprintf("agent 重启失败：%s\n将建立故障，由 recovery 修复。", f.Brief())
	}
	restart := false
	for _, r := range replies {
		restart = restart || r.kind == replyRestart
		switch {
		case text == "":
			r.ch <- opResult{err: fmt.Errorf("重启没有完成：%w", err)}
		case r.kind == replyNewSession && err == nil:
			r.ch <- opResult{text: fmt.Sprintf("已开启新会话 %s，%s", s.stateCopy().SessionID, text)}
		default:
			r.ch <- opResult{text: text}
		}
	}
	// The result of a restart also goes to every administrator (DESIGN 3).
	if text != "" && restart {
		s.notify(text)
	}
}

func rejectReplies(replies []startReply, err error) {
	for _, r := range replies {
		r.ch <- opResult{err: err}
	}
}

// setListening marks whether the lifecycle goroutine takes commands. When it stops listening, waiting commands are
// rejected and pending rollbacks are cancelled.
func (s *Supervisor) setListening(on bool) {
	s.mu.Lock()
	s.listening = on
	var cancels []context.CancelFunc
	if !on {
		for pr := range s.pendingRollbacks {
			cancels = append(cancels, pr.cancel)
		}
	}
	s.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	if on {
		return
	}
	for {
		select {
		case c := <-s.cmds:
			c.reject(&StateError{State: s.CurrentState(), Op: c.op()})
		default:
			return
		}
	}
}

// drain is the DRAINING state (DESIGN 5.4).
type drain struct {
	quiet      *time.Timer
	quietArmed bool
	deadline   *time.Timer
	replies    []startReply
}

func (s *Supervisor) beginDrain(ap *agentProc, reason string) *drain {
	s.setWire(wire.Draining, reason)
	d := &drain{quiet: time.NewTimer(time.Hour), deadline: time.NewTimer(s.cfg.Agent.DrainTimeout)}
	d.quiet.Stop()
	d.activity(ap.isBusy())
	return d
}

// activity re-arms the quiet window when the agent is idle and disarms it while a run is active.
func (d *drain) activity(busy bool) {
	if busy {
		d.quiet.Stop()
		d.quietArmed = false
		return
	}
	d.quiet.Reset(drainQuiet)
	d.quietArmed = true
}

func (d *drain) channels() (quiet, deadline <-chan time.Time) {
	if d == nil {
		return nil, nil
	}
	if d.quietArmed {
		quiet = d.quiet.C
	}
	return quiet, d.deadline.C
}

func (d *drain) stop() {
	if d == nil {
		return
	}
	d.quiet.Stop()
	d.deadline.Stop()
}

func (d *drain) takeReplies() []startReply {
	if d == nil {
		return nil
	}
	r := d.replies
	d.replies = nil
	return r
}

type heartbeat struct {
	st  *pirpc.State
	err error
}

// monitor watches the running agent process (RUNNING and DRAINING): heartbeat, probation, drain, commands, exits.
func (s *Supervisor) monitor(ctx context.Context) (step, error) {
	s.mu.Lock()
	ap := s.cur
	needLastGood := s.needLastGood
	probationUntil := s.st.ProbationUntil
	s.mu.Unlock()
	if ap == nil {
		return nil, errors.New("monitor: no agent process")
	}
	s.setListening(true)
	defer s.setListening(false)

	beat := time.NewTicker(s.cfg.Agent.HeartbeatInterval)
	defer beat.Stop()
	beats := make(chan heartbeat, 1)
	beating := false
	var lastGood <-chan time.Time
	if needLastGood {
		// In probation last-good moves at the stored end of the probation, otherwise after a continuous run of
		// [agent] probation (DESIGN 5.2 step 9).
		end := ap.readyAt.Add(s.cfg.Agent.Probation)
		if ap.probation {
			if probationUntil == nil {
				return nil, errors.New("monitor: the agent process runs in probation, but state.json has no probationUntil")
			}
			end = *probationUntil
		}
		t := time.NewTimer(time.Until(end))
		defer t.Stop()
		lastGood = t.C
	}
	var dr *drain
	defer func() { dr.stop() }()

	for {
		quietC, deadlineC := dr.channels()
		select {
		case <-ap.p.Done():
			return s.unexpectedExit(ap, "", dr.takeReplies())

		case <-ap.rpc.Done():
			// stdout ended or pi wrote a record the client cannot read: no get_state can be answered any more
			// (DESIGN 5.3 heartbeat). A crash closes stdout just before the exit, so the exit gets term_grace.
			grace := time.NewTimer(s.cfg.Daemon.TermGrace)
			select {
			case <-ap.p.Done():
				grace.Stop()
				return s.unexpectedExit(ap, "", dr.takeReplies())
			case <-grace.C:
			}
			s.log.Error("pi stdout can no longer be read while the process runs, killing the agent process tree", "pid", ap.p.PID(), "err", ap.rpc.Err())
			if err := ap.p.KillTree(); err != nil {
				return nil, err
			}
			return s.unexpectedExit(ap, fmt.Sprintf("；daemon 无法继续读取 stdout（%v），终止了整个进程树", ap.rpc.Err()), dr.takeReplies())

		case <-beat.C:
			if !beating {
				beating = true
				go func() {
					c, cancel := s.rpcCtx(context.Background())
					st, err := ap.rpc.GetState(c, "")
					cancel()
					beats <- heartbeat{st, err}
				}()
			}

		case b := <-beats:
			beating = false
			if b.err == nil {
				if err := s.recordSession(b.st); err != nil {
					return nil, err
				}
				continue
			}
			if !errors.Is(b.err, context.DeadlineExceeded) {
				// The stream ended: the exit of the process follows.
				continue
			}
			s.log.Error("heartbeat timeout, killing the agent process tree", "pid", ap.p.PID(), "rpcTimeout", s.cfg.Agent.RPCTimeout)
			if err := ap.p.KillTree(); err != nil {
				return nil, err
			}
			return s.unexpectedExit(ap, fmt.Sprintf("；心跳超时：rpc_timeout（%s）内没有响应 get_state（Node 事件循环可能被同步代码占住），daemon 终止了整个进程树", s.cfg.Agent.RPCTimeout), dr.takeReplies())

		case <-lastGood:
			lastGood = nil
			if ap.p.Exited() {
				// The process exited before its run reached the end; the exit is handled next.
				continue
			}
			if err := s.probationEnded(ctx, ap); err != nil {
				return nil, err
			}

		case <-ap.activity:
			if dr != nil {
				dr.activity(ap.isBusy())
			}

		case <-quietC:
			return s.restartAfterDrain(ap, dr)

		case <-deadlineC:
			s.log.Warn("drain_timeout reached, aborting the turn", "pid", ap.p.PID())
			c, cancel := s.rpcCtx(context.Background())
			if err := ap.rpc.Abort(c); err != nil {
				s.log.Error("abort after drain_timeout failed", "err", err)
			}
			cancel()
			return s.restartAfterDrain(ap, dr)

		case c := <-s.cmds:
			switch c := c.(type) {
			case applyDrainCmd:
				if dr == nil {
					dr = s.beginDrain(ap, "应用 "+c.snapshot+"，本轮结束后重启")
				}
			case restartCmd:
				if dr == nil {
					dr = s.beginDrain(ap, "重启，等待当前回合结束")
				}
				dr.replies = append(dr.replies, startReply{kind: replyRestart, ch: c.reply})
			case newSessionCmd:
				// contracts 4.2: only in RUNNING; a command queued before the drain began is refused.
				if dr != nil {
					c.reject(&StateError{State: wire.Draining, Op: c.op()})
				} else {
					return s.newSession(ap, c)
				}
			default:
				c.reject(&StateError{State: s.CurrentState(), Op: c.op()})
			}

		case rb := <-s.rollbackCh:
			replies := dr.takeReplies()
			dr.stop()
			rejectReplies(replies, errors.New("管理员回滚取代了这次重启"))
			return s.runRollback(ctx, rb, false)

		case <-ctx.Done():
			return s.shutdownAgent(ap, dr)
		}
	}
}

func (s *Supervisor) restartAfterDrain(ap *agentProc, dr *drain) (step, error) {
	replies := dr.takeReplies()
	dr.stop()
	s.setWire(wire.Restarting, "")
	s.setListening(false)
	if err := s.stopAgent(ap, true); err != nil {
		rejectReplies(replies, err)
		return nil, err
	}
	// The restart keeps the stored probation; a pending apply makes it the apply restart, which begins a new one.
	return startStep{replies: replies}, nil
}

// newSession handles `/bb new` (contracts 4.2): abort, a new session id in state.json, the stop procedure and a
// start with the new id.
func (s *Supervisor) newSession(ap *agentProc, c newSessionCmd) (step, error) {
	s.setWire(wire.Restarting, "开启新会话")
	s.setListening(false)
	ctx, cancel := s.rpcCtx(context.Background())
	if err := ap.rpc.Abort(ctx); err != nil {
		s.log.Warn("abort before the new session failed", "err", err)
	}
	cancel()
	id := state.NewSessionID(time.Now())
	if err := s.update(func(x *state.State) {
		x.SessionID = id
		x.SessionFile = ""
	}); err != nil {
		c.reply <- opResult{err: err}
		return nil, err
	}
	s.log.Info("new session id", "session", id)
	if err := s.stopAgent(ap, false); err != nil {
		c.reply <- opResult{err: err}
		return nil, err
	}
	return startStep{replies: []startReply{{kind: replyNewSession, ch: c.reply}}}, nil
}

// unexpectedExit handles an exit the daemon did not ask for (DESIGN 5.4): lastExit, then an incident when the
// process was in probation or crash_limit is reached, otherwise a restart.
func (s *Supervisor) unexpectedExit(ap *agentProc, extra string, replies []startReply) (step, error) {
	s.setListening(false)
	if err := s.afterExit(ap); err != nil {
		rejectReplies(replies, err)
		return nil, err
	}
	exit := ap.p.Exit()
	detail := fmt.Sprintf("agent 进程意外退出（%s）%s", exit, extra)
	rejectReplies(replies, errors.New(detail+"，按意外退出处理"))
	f := &incident.Failure{Detail: detail, Exit: &exit, Stderr: ap.stderr.Lines(incidentStderrLines)}
	count, err := s.recordUnexpectedExit(ap)
	if err != nil {
		return nil, err
	}
	s.log.Warn("unexpected agent exit", "pid", ap.p.PID(), "exit", exit.String(), "probation", ap.probation, "crashesInWindow", count)
	if ap.probation {
		f.Trigger = incident.TriggerProbationExit
		return incidentStep{failure: f}, nil
	}
	if count >= s.cfg.Agent.CrashLimit {
		f.Trigger = incident.TriggerRepeatedExit
		f.Detail += fmt.Sprintf("；crash_window（%s）内意外退出 %d 次，达到 crash_limit", s.cfg.Agent.CrashWindow, count)
		return incidentStep{failure: f}, nil
	}
	s.setWire(wire.Restarting, detail)
	return startStep{}, nil
}

// recordUnexpectedExit stores the "上次退出" record of an unexpected exit for the next bridge/ready and, outside
// probation, counts the exit in crash_window (DESIGN 5.4). It returns the number of exits in the window.
func (s *Supervisor) recordUnexpectedExit(ap *agentProc) (int, error) {
	exit := ap.p.Exit()
	now := time.Now()
	count := 0
	err := s.update(func(x *state.State) {
		x.Deliver.LastExit = &state.LastExit{Code: exit.Code, Signal: exit.Signal, StderrTail: strings.Join(ap.stderr.Lines(lastExitStderrLines), "\n"), At: now}
		if !ap.probation {
			x.Crashes = append(recentCrashes(x.Crashes, s.cfg.Agent.CrashWindow, now), now)
			count = len(x.Crashes)
		}
	})
	return count, err
}

// probationEnded moves last-good to runningSnapshot at the end of the probation, or after the process ran for
// probation (DESIGN 5.4, 5.2 step 9), and closes an incident in probation, pushing its notice to bearbot-im
// (contracts 6).
func (s *Supervisor) probationEnded(ctx context.Context, ap *agentProc) error {
	// The git commands are short; they finish even when the shutdown begins meanwhile.
	ctx = context.WithoutCancel(ctx)
	st := s.stateCopy()
	if st.RunningSnapshot == "" {
		return errors.New("state.json 中没有 runningSnapshot，无法移动 last-good")
	}
	if err := s.repo.MoveLastGood(ctx, st.RunningSnapshot); err != nil {
		return err
	}
	if err := s.update(func(x *state.State) {
		x.LastGoodPending = false
		x.ProbationUntil = nil
	}); err != nil {
		return err
	}
	s.mu.Lock()
	s.needLastGood = false
	ap.probation = false
	s.mu.Unlock()
	name, err := s.repo.NameOf(ctx, st.RunningSnapshot)
	if err != nil {
		return err
	}
	s.log.Info("probation ended, last-good moved", "lastGood", name)
	notice, err := s.incidents.ProbationEnded()
	if err != nil {
		return err
	}
	if notice == nil {
		return nil
	}
	if s.hub.pushIncident(ap.gen, wire.BridgeEvent{Type: wire.EventIncident, Incident: notice}) {
		s.log.Info("incident notice pushed on bridge events", "incident", notice.ID)
		return nil
	}
	s.log.Info("no events connection, incident notice kept for the next bridge/ready", "incident", notice.ID)
	return s.update(func(x *state.State) { x.Deliver.Incident = notice })
}

// shutdownAgent stops the agent for the daemon's shutdown: wait for the running turn up to shutdown_grace, then
// the stop procedure.
func (s *Supervisor) shutdownAgent(ap *agentProc, dr *drain) (step, error) {
	rejectReplies(dr.takeReplies(), errors.New("bearbotd 正在关闭"))
	dr.stop()
	s.setWire(wire.Draining, "bearbotd 正在关闭")
	s.setListening(false)
	grace := time.NewTimer(s.cfg.Daemon.ShutdownGrace)
	defer grace.Stop()
wait:
	for ap.isBusy() {
		select {
		case <-ap.activity:
		case <-ap.p.Done():
			break wait
		case <-grace.C:
			s.log.Warn("shutdown_grace reached while a turn runs", "pid", ap.p.PID())
			break wait
		}
	}
	if ap.p.Exited() {
		// The process exited by itself before the stop procedure began: an unexpected exit (DESIGN 5.4). Its record
		// is delivered with the first bridge/ready after the daemon starts again; the daemon is shutting down, so
		// there is no restart and no incident.
		if err := s.afterExit(ap); err != nil {
			return nil, err
		}
		count, err := s.recordUnexpectedExit(ap)
		if err != nil {
			return nil, err
		}
		s.log.Warn("unexpected agent exit during the shutdown wait", "pid", ap.p.PID(), "exit", ap.p.Exit().String(), "probation", ap.probation, "crashesInWindow", count)
		return stopStep{}, nil
	}
	if err := s.stopAgent(ap, true); err != nil {
		return nil, err
	}
	return stopStep{}, nil
}

// maintenance waits in MAINTENANCE for repair, resume or rollback (DESIGN 5.8 step 5).
func (s *Supervisor) maintenance(ctx context.Context) (step, error) {
	s.setListening(true)
	defer s.setListening(false)
	for {
		select {
		case c := <-s.cmds:
			switch c := c.(type) {
			case repairCmd:
				s.setListening(false)
				outcome, text, err := s.incidents.Repair(ctx, c.note)
				if err != nil {
					c.reply <- opResult{err: err}
					return nil, err
				}
				c.reply <- opResult{text: text}
				return flowStep(outcome, nil)
			case resumeCmd:
				s.setListening(false)
				text, err := s.incidents.Resume(ctx)
				var f *incident.Failure
				switch {
				case err == nil:
					c.reply <- opResult{text: text}
					s.notify(text)
					return monitorStep{}, nil
				case errors.As(err, &f):
					c.reply <- opResult{text: text}
					s.notify(text)
					return incidentStep{failure: f}, nil
				default:
					c.reply <- opResult{err: err}
					return nil, err
				}
			default:
				c.reject(&StateError{State: s.CurrentState(), Op: c.op()})
			}
		case rb := <-s.rollbackCh:
			return s.runRollback(ctx, rb, true)
		case <-ctx.Done():
			return stopStep{}, nil
		}
	}
}

func (s *Supervisor) runRollback(ctx context.Context, rb *rollbackCmd, fromMaintenance bool) (step, error) {
	s.setListening(false)
	s.mu.Lock()
	s.rollbacks++
	s.mu.Unlock()
	res, err := rollback.Run(ctx, &host{s}, s.incidents, rb.req, rb.target, fromMaintenance)
	if err != nil {
		rb.reply <- opResult{err: err}
		return nil, err
	}
	rb.reply <- opResult{text: res.Text}
	switch {
	case res.Maintenance:
		return maintenanceStep{}, nil
	case res.Failure != nil:
		return incidentStep{failure: res.Failure}, nil
	default:
		return monitorStep{}, nil
	}
}
