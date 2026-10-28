package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"bearbot/daemon/internal/check"
	"bearbot/daemon/internal/classifierturn"
	"bearbot/daemon/internal/incident"
	"bearbot/daemon/internal/pirpc"
	"bearbot/daemon/internal/proc"
	"bearbot/daemon/internal/state"
	"bearbot/daemon/internal/wire"
)

// Lines of the agent's stderr kept in memory per process (DESIGN 5.3), given with an unexpected exit (DESIGN 5.4)
// and with an incident (DESIGN 5.7).
const (
	stderrRingLines     = 500
	lastExitStderrLines = 50
	incidentStderrLines = 200
)

// agentProc is one agent process.
type agentProc struct {
	s         *Supervisor
	gen       int
	sessionID string
	p         *proc.Process
	rpc       *pirpc.Client
	stderr    *proc.Ring

	readyOnce sync.Once
	readyCh   chan struct{}
	// readyAt and probation are written by the lifecycle goroutine under the supervisor mutex.
	readyAt   time.Time
	probation bool

	expected atomic.Bool

	finishOnce sync.Once
	finishErr  error

	mu       sync.Mutex
	busy     bool
	activity chan struct{}
}

func (ap *agentProc) isBusy() bool {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	return ap.busy
}

func (ap *agentProc) setBusy(b bool) {
	ap.mu.Lock()
	ap.busy = b
	ap.mu.Unlock()
	select {
	case ap.activity <- struct{}{}:
	default:
	}
}

func (ap *agentProc) markReady() { ap.readyOnce.Do(func() { close(ap.readyCh) }) }

func (ap *agentProc) onEvent(ev pirpc.Event) {
	s := ap.s
	if ev.RPCLogged() {
		if _, err := s.rpcLog.Write(append(ev.LogRecord(), '\n')); err != nil {
			s.log.Error("write rpc-events.jsonl", "err", err)
		}
	}
	switch ev.Type {
	case "agent_start":
		ap.setBusy(true)
	case "agent_settled":
		ap.setBusy(false)
	case "extension_error":
		x, ok := ev.ExtensionError()
		if !ok {
			s.log.Error("invalid extension_error record", "record", string(ev.Raw))
			return
		}
		s.log.Warn("extension_error", "pid", ap.p.PID(), "extensionPath", x.ExtensionPath, "event", x.Event, "error", x.Error)
		s.hub.extensionError(ap.gen, wire.BridgeEvent{Type: wire.EventExtensionError, ExtensionError: x})
	case "extension_ui_request":
		r, _ := ev.UIRequest()
		if ev.AutoCancelled {
			s.log.Info("dialog request cancelled", "pid", ap.p.PID(), "method", r.Method, "title", r.Title)
		} else {
			s.log.Info("extension UI request", "pid", ap.p.PID(), "method", r.Method, "message", r.Message, "notifyType", r.NotifyType, "statusKey", r.StatusKey, "statusText", r.StatusText, "title", r.Title)
		}
	case "response":
		s.log.Debug("late RPC response", "pid", ap.p.PID(), "record", string(ev.Raw))
	}
}

// spawn starts an agent process with the session id (contracts 2.1) as `<node> <PiBin> <args>`.
func (s *Supervisor) spawn(sessionID string) (*agentProc, error) {
	ring := proc.NewRing(stderrRingLines)
	p, err := proc.Start(proc.Spec{
		Path:       s.node,
		Args:       append([]string{s.paths.PiBin}, s.paths.AgentArgs(sessionID)...),
		Env:        s.paths.AgentEnv(s.cfg),
		Dir:        s.paths.WorkspaceDir,
		Credential: s.users.Credential(s.users.Bearbot),
		StdinPipe:  true,
		Stderr:     io.MultiWriter(s.stderrLog, ring),
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.gen++
	gen := s.gen
	s.mu.Unlock()
	s.hub.setGen(gen)
	ap := &agentProc{
		s:         s,
		gen:       gen,
		sessionID: sessionID,
		p:         p,
		stderr:    ring,
		readyCh:   make(chan struct{}),
		activity:  make(chan struct{}, 1),
	}
	ap.rpc = pirpc.New(p.Stdin, p.Stdout, pirpc.Options{OnEvent: ap.onEvent, IDPrefix: "d-"})
	go func() {
		<-p.Done()
		// A descendant may keep stdout open, so the stream may never end by itself.
		ap.rpc.Close()
	}()
	s.log.Info("agent process started", "pid", p.PID(), "session", sessionID, "generation", gen)
	return ap, nil
}

// recordSession stores the session id and file of a get_state answer (DESIGN 5.3 "会话 id").
func (s *Supervisor) recordSession(st *pirpc.State) error {
	if st.SessionID == "" {
		return nil
	}
	s.mu.Lock()
	same := s.st.SessionID == st.SessionID && s.st.SessionFile == st.SessionFile
	s.mu.Unlock()
	if same {
		return nil
	}
	s.log.Info("session recorded", "session", st.SessionID, "file", st.SessionFile)
	return s.update(func(x *state.State) {
		x.SessionID = st.SessionID
		x.SessionFile = st.SessionFile
	})
}

func (s *Supervisor) rpcCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.cfg.Agent.RPCTimeout)
}

// startAgent runs the start of DESIGN 5.3: location check, a pending apply becomes the apply restart, the process
// is started with the recorded session id, get_state "boot" within start_timeout, then bridge/ready within
// ready_timeout. With probation (and for the apply restart) the start begins a new probation; any other start keeps
// the stored one (probationAtReady). On success the state is RUNNING and s.cur is the process. A failed start
// returns *incident.Failure; the process is gone then.
func (s *Supervisor) startAgent(ctx context.Context, probation bool) error {
	s.setWire(wire.Starting, "")
	if err := s.gw.WaitConnected(ctx); err != nil {
		return err
	}
	loc, err := check.CheckLocation(s.paths)
	if err != nil {
		return err
	}
	if !loc.Passed {
		return &incident.Failure{Trigger: incident.TriggerStartFailure, Detail: "启动前的位置检查没有通过：" + loc.Message}
	}
	st := s.stateCopy()
	if pa := st.PendingApply; pa != nil {
		commit, err := s.repo.Resolve(ctx, pa.Snapshot)
		if err != nil {
			return err
		}
		if err := s.update(func(x *state.State) {
			x.RunningSnapshot = commit
			x.Deliver.Applied = &state.Applied{Snapshot: pa.Snapshot, Summary: pa.Summary, Origin: pa.Origin}
			x.PendingApply = nil
			x.LastGoodPending = true
			x.ProbationUntil = nil
		}); err != nil {
			return err
		}
		s.log.Info("apply takes effect with this start", "snapshot", pa.Snapshot)
		probation = true
	} else if probation {
		// The end of the new probation is stored when this start is ready; until then a restart of bearbotd starts
		// it again (initialStep).
		if err := s.update(func(x *state.State) {
			x.LastGoodPending = true
			x.ProbationUntil = nil
		}); err != nil {
			return err
		}
	}
	ap, err := s.spawn(st.SessionID)
	if err != nil {
		return &incident.Failure{Trigger: incident.TriggerStartFailure, Detail: "无法启动 pi：" + err.Error()}
	}
	s.mu.Lock()
	s.cur = ap
	s.starts++
	if probation {
		s.needLastGood = true
	}
	s.mu.Unlock()

	bootCtx, cancel := context.WithTimeout(ctx, s.cfg.Agent.StartTimeout)
	booted, err := ap.rpc.GetState(bootCtx, "boot")
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return errors.Join(ctx.Err(), s.killAgent(ap))
		}
		if ap.p.Exited() || errors.Is(err, pirpc.ErrStreamEnded) || errors.Is(err, pirpc.ErrClosed) {
			select {
			case <-ap.p.Done():
			case <-time.After(s.cfg.Daemon.TermGrace):
				// stdout ended but the process still runs.
				if err := s.killAgent(ap); err != nil {
					return err
				}
			}
			return s.failStart(ap, fmt.Sprintf("agent 进程在响应 get_state 之前退出（%s）", ap.p.Exit()))
		}
		if err := s.killAgent(ap); err != nil {
			return err
		}
		return s.failStart(ap, fmt.Sprintf("agent 进程没有在 start_timeout（%s）内响应 get_state", s.cfg.Agent.StartTimeout))
	}
	if err := s.recordSession(booted); err != nil {
		return errors.Join(err, s.killAgent(ap))
	}
	timer := time.NewTimer(s.cfg.Agent.ReadyTimeout)
	defer timer.Stop()
	select {
	case <-ap.readyCh:
	case <-ap.p.Done():
		return s.failStart(ap, fmt.Sprintf("agent 进程在 bearbot-im 调用 /v1/bridge/ready 之前退出（%s）", ap.p.Exit()))
	case <-timer.C:
		ap.expected.Store(true)
		if err := ap.p.Stop(s.cfg.Daemon.TermGrace); err != nil {
			return err
		}
		return s.failStart(ap, fmt.Sprintf("bearbot-im 没有在 ready_timeout（%s）内调用 /v1/bridge/ready（它要先连接 gateway）", s.cfg.Agent.ReadyTimeout))
	case <-ctx.Done():
		return errors.Join(ctx.Err(), s.stopAgent(ap, false))
	}
	readyAt := time.Now()
	inProbation, err := s.probationAtReady(probation, readyAt)
	if err != nil {
		return errors.Join(err, s.killAgent(ap))
	}
	s.mu.Lock()
	ap.readyAt = readyAt
	ap.probation = inProbation
	s.mu.Unlock()
	s.setWire(wire.Running, "")
	s.log.Info("agent ready", "pid", ap.p.PID(), "session", booted.SessionID, "probation", inProbation)
	return nil
}

// probationAtReady tells whether the process that became ready at readyAt runs in probation (DESIGN 5.4 观察期). A
// start that begins a probation stores its end, readyAt plus [agent] probation, in state.json. Every other start
// (a restart by the daemon, the start after bearbotd started again) keeps a stored end that is still ahead. When the
// stored end passed while no agent process was ready, the probation ended without a process at its end: it is
// cleared and last-good is not moved now; it moves after a continuous run of [agent] probation (needLastGood is
// still set, DESIGN 5.2 step 9).
func (s *Supervisor) probationAtReady(begins bool, readyAt time.Time) (bool, error) {
	if begins {
		end := readyAt.Add(s.cfg.Agent.Probation)
		return true, s.update(func(x *state.State) { x.ProbationUntil = &end })
	}
	s.mu.Lock()
	until := s.st.ProbationUntil
	s.mu.Unlock()
	if until == nil {
		return false, nil
	}
	if readyAt.Before(*until) {
		return true, nil
	}
	s.log.Info("probation ended while no agent process was ready; last-good moves after a continuous run", "probationUntil", *until)
	return false, s.update(func(x *state.State) {
		x.ProbationUntil = nil
		x.LastGoodPending = false
	})
}

// killAgent kills the whole process tree (the daemon initiated it) and finishes the exit bookkeeping.
func (s *Supervisor) killAgent(ap *agentProc) error {
	ap.expected.Store(true)
	if err := ap.p.KillTree(); err != nil {
		return err
	}
	return s.afterExit(ap)
}

// failStart finishes a failed start of ap (the process has exited or was stopped) and returns the failure.
func (s *Supervisor) failStart(ap *agentProc, detail string) error {
	<-ap.p.Done()
	if err := s.afterExit(ap); err != nil {
		return err
	}
	exit := ap.p.Exit()
	f := &incident.Failure{Trigger: incident.TriggerStartFailure, Detail: detail, Exit: &exit, Stderr: ap.stderr.Lines(incidentStderrLines)}
	s.log.Error("agent start failed", "pid", ap.p.PID(), "detail", detail)
	return f
}

// stopAgent runs the daemon initiated stop of DESIGN 5.3: get_state to record the newest session id (when
// preGetState), close stdin, SIGTERM, SIGKILL; then the exit bookkeeping.
func (s *Supervisor) stopAgent(ap *agentProc, preGetState bool) error {
	ap.expected.Store(true)
	if !ap.p.Exited() {
		if preGetState {
			ctx, cancel := s.rpcCtx(context.Background())
			st, err := ap.rpc.GetState(ctx, "")
			cancel()
			if err == nil {
				if err := s.recordSession(st); err != nil {
					return err
				}
			} else {
				s.log.Warn("get_state before stop failed; the last recorded session id stays", "pid", ap.p.PID(), "err", err)
			}
		}
		if err := ap.p.Stop(s.cfg.Daemon.TermGrace); err != nil {
			return err
		}
	}
	return s.afterExit(ap)
}

// exitRecord is one line of logs/agent/exits.jsonl (contracts 8.2).
type exitRecord struct {
	At         time.Time `json:"at"`
	PID        int       `json:"pid"`
	Code       *int      `json:"code"`
	Signal     *string   `json:"signal"`
	Expected   bool      `json:"expected"`
	StderrTail string    `json:"stderrTail"`
}

// afterExit runs once after ap exited: the exits.jsonl line, s.cur, and the CLASSIFIER.md comparison of an open
// member turn (DESIGN 5.3, 7.10).
func (s *Supervisor) afterExit(ap *agentProc) error {
	ap.finishOnce.Do(func() { ap.finishErr = s.finishExit(ap) })
	return ap.finishErr
}

func (s *Supervisor) finishExit(ap *agentProc) error {
	<-ap.p.Done()
	ap.rpc.Close()
	select {
	case <-ap.p.OutputDone():
	case <-time.After(time.Second):
	}
	exit := ap.p.Exit()
	expected := ap.expected.Load()
	s.log.Info("agent process exited", "pid", ap.p.PID(), "exit", exit.String(), "expected", expected)
	if err := s.exitsLog.WriteRecord(exitRecord{
		At: time.Now(), PID: ap.p.PID(), Code: exit.Code, Signal: exit.Signal, Expected: expected,
		StderrTail: strings.Join(ap.stderr.Lines(lastExitStderrLines), "\n"),
	}); err != nil {
		return err
	}
	s.mu.Lock()
	if s.cur == ap {
		s.cur = nil
	}
	s.mu.Unlock()
	return s.compareClassifierAfterExit()
}

// compareClassifierAfterExit ends an open member turn record after the process exited or at startup; a change is
// logged, notified and delivered with the next bridge/ready as classifierRestore (DESIGN 7.10 "进程退出").
func (s *Supervisor) compareClassifierAfterExit() error {
	s.classifierMu.Lock()
	defer s.classifierMu.Unlock()
	pending, err := s.recorder.Pending()
	if err != nil {
		return err
	}
	if pending == nil {
		if s.stateCopy().ClassifierTurn != nil {
			return s.update(func(x *state.State) { x.ClassifierTurn = nil })
		}
		return nil
	}
	res, err := s.recorder.End("")
	if err != nil {
		return err
	}
	s.log.Info("classifier turn compared by the daemon", "turn", res.Turn.TurnID, "changed", res.Changed, "restored", res.Restored)
	return s.finishClassifierTurn(res, true)
}

// finishClassifierTurn records the outcome of a turn end comparison: the turn record leaves state.json; a change is
// written to classifier.jsonl and notified; with deliver it is also kept for the next bridge/ready.
func (s *Supervisor) finishClassifierTurn(res *classifierturn.EndResult, deliver bool) error {
	if err := s.update(func(x *state.State) {
		x.ClassifierTurn = nil
		if deliver && res.Changed {
			x.Deliver.ClassifierRestore = &state.ClassifierRestore{Restored: res.Restored, Detail: res.Detail}
		}
	}); err != nil {
		return err
	}
	if !res.Changed {
		return nil
	}
	if err := s.classifierLog.WriteRecord(res.Record); err != nil {
		return err
	}
	s.notify(res.Detail)
	return nil
}
