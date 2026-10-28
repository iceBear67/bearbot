// Package agent is the agent supervisor: the state machine of DESIGN 5.4 with the change lock, the agent process
// (DESIGN 5.3: start, readiness, heartbeat, stop, session id), draining and restarts, unexpected exits, probation
// and last-good, the apply flow of DESIGN 5.6, the deliveries of bridge/ready and the bridge events (contracts 6),
// and the CLASSIFIER.md turn records (DESIGN 7.10). Incidents and the maintenance state (package incident) and
// rollbacks (package rollback) run on the supervisor's lifecycle goroutine.
//
// All state transitions happen on the lifecycle goroutine (Run). Requests from agent.sock, admin.sock and
// gateway-ctl.sock are checked against the current state and handed to that goroutine as commands; a command is only
// accepted while the goroutine waits for commands (RUNNING, DRAINING, MAINTENANCE), so a caller never waits behind
// a long flow. state.json is changed under the supervisor mutex and saved at once.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"bearbot/daemon/internal/check"
	"bearbot/daemon/internal/classifierturn"
	"bearbot/daemon/internal/config"
	"bearbot/daemon/internal/incident"
	"bearbot/daemon/internal/layout"
	"bearbot/daemon/internal/proc"
	"bearbot/daemon/internal/snapshot"
	"bearbot/daemon/internal/state"
	"bearbot/daemon/internal/wire"
)

// Gateway is what the supervisor needs from the gateway link.
type Gateway interface {
	// Notify queues a notice for every administrator.
	Notify(text string)
	// AgentStateChanged tells the link to send agent.state with the current state.
	AgentStateChanged()
	// WaitConnected waits until gateway-ctl.sock completed hello.
	WaitConnected(ctx context.Context) error
}

// Options holds the dependencies of a Supervisor.
type Options struct {
	Paths   layout.Paths
	Users   *layout.Users
	Config  *config.Config
	Repo    *snapshot.Repo
	Gateway Gateway
	Logger  *slog.Logger
	// State is the loaded state.json.
	State *state.State
	// Console tells whether the console is enabled (StatusInfo.console).
	Console bool
	// Node is node found in the daemon's PATH. pi and tsc are scripts with `#!/usr/bin/env node`; they are started
	// as `<Node> <script>`, because the child PATH begins with directories bearbot can write.
	Node string
}

// StateError means a request is not accepted in the current state.
type StateError struct {
	State wire.AgentState
	// Op names the request in the message.
	Op string
}

func (e *StateError) Error() string {
	if e.Op == "" {
		return fmt.Sprintf("当前状态为 %s，不接受这个请求", e.State)
	}
	return fmt.Sprintf("当前状态为 %s，不能执行 %s", e.State, e.Op)
}

// UserError is a request error caused by the caller's input (bad reference, missing field, …).
type UserError struct{ Err error }

func (e *UserError) Error() string { return e.Err.Error() }
func (e *UserError) Unwrap() error { return e.Err }

func userErr(format string, args ...any) error { return &UserError{fmt.Errorf(format, args...)} }

// Supervisor is the agent supervisor.
type Supervisor struct {
	paths     layout.Paths
	users     *layout.Users
	cfg       *config.Config
	repo      *snapshot.Repo
	gw        Gateway
	log       *slog.Logger
	console   bool
	node      string
	checker   *check.Checker
	recorder  *classifierturn.Recorder
	incidents *incident.Manager

	stderrLog     *proc.LogWriter
	rpcLog        *proc.LogWriter
	exitsLog      *proc.LogWriter
	classifierLog *proc.LogWriter

	hub    *hub
	change changeLock
	// classifierMu serializes the CLASSIFIER.md turn operations.
	classifierMu sync.Mutex

	cmds       chan command
	rollbackCh chan *rollbackCmd

	mu        sync.Mutex
	st        *state.State
	wire      wire.AgentState
	reason    string
	cur       *agentProc
	gen       int
	starts    int
	listening bool
	// needLastGood is true until last-good was moved after a probation (or the first continuous run after the
	// daemon started).
	needLastGood     bool
	pendingRollbacks map[*pendingRollback]struct{}
	// rollbacks counts the rollbacks that ran; a request that waited for the change lock compares it (DESIGN 5.6
	// apply step 1).
	rollbacks uint64
}

// New creates the supervisor and opens the agent log files.
func New(o Options) *Supervisor {
	s := &Supervisor{
		paths:            o.Paths,
		users:            o.Users,
		cfg:              o.Config,
		repo:             o.Repo,
		gw:               o.Gateway,
		log:              o.Logger,
		console:          o.Console,
		node:             o.Node,
		checker:          &check.Checker{Paths: o.Paths, Users: o.Users, Config: o.Config, Node: o.Node},
		recorder:         classifierturn.New(o.Paths, o.Users),
		stderrLog:        proc.NewLogWriter(o.Paths.AgentStderrLog, 500),
		rpcLog:           proc.NewLogWriter(o.Paths.AgentRPCEventsLog, 100),
		exitsLog:         proc.NewLogWriter(o.Paths.AgentExitsLog, 100),
		classifierLog:    proc.NewLogWriter(o.Paths.AgentClassifierLog, 100),
		hub:              newHub(),
		change:           newChangeLock(),
		cmds:             make(chan command, 16),
		rollbackCh:       make(chan *rollbackCmd),
		st:               o.State,
		wire:             wire.Stopped,
		needLastGood:     true,
		pendingRollbacks: map[*pendingRollback]struct{}{},
	}
	s.incidents = incident.New(&host{s})
	return s
}

// Close closes the log files; call it after Run returned.
func (s *Supervisor) Close() error {
	return errors.Join(s.stderrLog.Close(), s.rpcLog.Close(), s.exitsLog.Close(), s.classifierLog.Close())
}

// StderrLog is the writer of logs/agent/stderr.log (for `logs agent -f`).
func (s *Supervisor) StderrLog() *proc.LogWriter { return s.stderrLog }

// RecoveryLog returns the writer of logs/recovery/<id>.log while a repair of incident id runs, otherwise nil.
func (s *Supervisor) RecoveryLog(id string) *proc.LogWriter { return s.incidents.RecoveryLog(id) }

// WireState returns the current agent state and reason (nil when empty) for agent.state.
func (s *Supervisor) WireState() (wire.AgentState, *string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wire, reasonPtr(s.reason)
}

func reasonPtr(r string) *string {
	if r == "" {
		return nil
	}
	return &r
}

// CurrentState returns the current agent state.
func (s *Supervisor) CurrentState() wire.AgentState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wire
}

// Require returns a StateError unless the current state is one of allowed.
func (s *Supervisor) Require(op string, allowed ...wire.AgentState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(allowed, s.wire) {
		return &StateError{State: s.wire, Op: op}
	}
	return nil
}

func (s *Supervisor) setWire(st wire.AgentState, reason string) {
	s.mu.Lock()
	changed := s.wire != st || s.reason != reason
	prev := s.wire
	s.wire, s.reason = st, reason
	if changed {
		// Published under s.mu (lock order s.mu, then hub.mu): Subscribe registers under s.mu too, so a new events
		// connection starts with the new state or receives this record.
		s.hub.publish(wire.BridgeEvent{Type: wire.EventState, State: st, Reason: reasonPtr(reason)})
	}
	s.mu.Unlock()
	if !changed {
		return
	}
	s.log.Info("agent state", "from", prev, "to", st, "reason", reason)
	s.gw.AgentStateChanged()
}

// update changes the state under the mutex and saves state.json.
func (s *Supervisor) update(fn func(*state.State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.st)
	return state.Save(s.paths.StateFile, s.st)
}

// stateCopy returns a deep copy of the state.
func (s *Supervisor) stateCopy() *state.State {
	s.mu.Lock()
	data, err := json.Marshal(s.st)
	s.mu.Unlock()
	if err != nil {
		panic(err)
	}
	var out state.State
	if err := json.Unmarshal(data, &out); err != nil {
		panic(err)
	}
	return &out
}

// notify sends a notice to the administrators and writes it to the daemon log.
func (s *Supervisor) notify(text string) {
	s.log.Info("notify", "text", text)
	s.gw.Notify(text)
}

// changeLock is the change lock of DESIGN 5.4; waiting for it can be cancelled.
type changeLock chan struct{}

func newChangeLock() changeLock { return make(changeLock, 1) }

func (l changeLock) lock(ctx context.Context) error {
	select {
	case l <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l changeLock) unlock() { <-l }

// Status returns StatusInfo (contracts 4).
func (s *Supervisor) Status(ctx context.Context) (*wire.StatusInfo, error) {
	s.mu.Lock()
	info := &wire.StatusInfo{
		State:   s.wire,
		Reason:  reasonPtr(s.reason),
		Console: s.console,
	}
	info.Restarts = max(s.starts-1, 0)
	if ap := s.cur; ap != nil {
		pid := ap.p.PID()
		info.PID = &pid
		info.StartedAt = state.Ptr(wire.Time(ap.p.StartedAt))
	}
	st := s.st
	info.ProbationUntil = wire.TimePtr(st.ProbationUntil)
	info.CrashesInWindow = len(recentCrashes(st.Crashes, s.cfg.Agent.CrashWindow, time.Now()))
	if st.SessionID != "" {
		info.SessionID = state.Ptr(st.SessionID)
	}
	running := st.RunningSnapshot
	if pa := st.PendingApply; pa != nil {
		info.PendingApply = &wire.PendingApplyInfo{Snapshot: pa.Snapshot, Summary: pa.Summary}
	}
	if oi := st.OpenIncident; oi != nil {
		info.Incident = &wire.IncidentInfo{ID: oi.ID, Trigger: oi.Trigger, Attempts: oi.Attempts}
	}
	if m := st.Maintenance; m != nil {
		info.Maintenance = &wire.MaintenanceInfo{Reason: m.Reason, IncidentID: m.IncidentID, Since: wire.Time(m.Since)}
	}
	if r := st.Rollback; r != nil {
		info.Rollback = &wire.RollbackInfo{ID: r.ID, Target: r.Target, Notified: r.Notified}
	}
	s.mu.Unlock()

	if running != "" {
		name, err := s.repo.NameOf(ctx, running)
		if err != nil {
			return nil, err
		}
		info.RunningSnapshot = &name
	}
	if commit, ok, err := s.repo.LastGood(ctx); err != nil {
		return nil, err
	} else if ok {
		name, err := s.repo.NameOf(ctx, commit)
		if err != nil {
			return nil, err
		}
		info.LastGood = &name
	}
	return info, nil
}

func recentCrashes(crashes []time.Time, window time.Duration, now time.Time) []time.Time {
	var out []time.Time
	for _, c := range crashes {
		if now.Sub(c) <= window {
			out = append(out, c)
		}
	}
	return out
}

// hub fans out the bridge events to the open GET /v1/bridge/events connections (contracts 6).
type hub struct {
	mu   sync.Mutex
	subs map[*Subscription]struct{}
	gen  int
	// backlog keeps the extension errors of the current process that arrived while it had no events connection.
	backlog []wire.BridgeEvent
}

const (
	subscriptionBuffer = 1024
	backlogLimit       = 500
)

type hubItem struct {
	ev  wire.BridgeEvent
	ack chan error
}

// Subscription is one events connection.
type Subscription struct {
	gen  int
	ch   chan hubItem
	gone chan struct{}
	once sync.Once
}

func newHub() *hub { return &hub{subs: map[*Subscription]struct{}{}} }

func (h *hub) setGen(gen int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gen = gen
	h.backlog = nil
}

func (s *Subscription) drop() { s.once.Do(func() { close(s.gone) }) }

func (h *hub) send(sub *Subscription, it hubItem) bool {
	select {
	case sub.ch <- it:
		return true
	default:
		// The reader does not keep up; drop the connection, bearbot-im opens a new one.
		sub.drop()
		delete(h.subs, sub)
		return false
	}
}

func (h *hub) subscribe(initial wire.BridgeEvent) *Subscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	sub := &Subscription{gen: h.gen, ch: make(chan hubItem, subscriptionBuffer), gone: make(chan struct{})}
	h.subs[sub] = struct{}{}
	h.send(sub, hubItem{ev: initial})
	for _, ev := range h.backlog {
		h.send(sub, hubItem{ev: ev})
	}
	h.backlog = nil
	return sub
}

func (h *hub) unsubscribe(sub *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, sub)
	sub.drop()
}

func (h *hub) publish(ev wire.BridgeEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		h.send(sub, hubItem{ev: ev})
	}
}

func (h *hub) extensionError(gen int, ev wire.BridgeEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if gen != h.gen {
		return
	}
	delivered := false
	for sub := range h.subs {
		if sub.gen == gen && h.send(sub, hubItem{ev: ev}) {
			delivered = true
		}
	}
	if !delivered && len(h.backlog) < backlogLimit {
		h.backlog = append(h.backlog, ev)
	}
}

// pushIncident writes the incident event to the events connections of process gen and reports whether one of them
// wrote it.
func (h *hub) pushIncident(gen int, ev wire.BridgeEvent) bool {
	h.mu.Lock()
	var acks []chan error
	for sub := range h.subs {
		if sub.gen != gen {
			continue
		}
		ack := make(chan error, 1)
		if h.send(sub, hubItem{ev: ev, ack: ack}) {
			acks = append(acks, ack)
		}
	}
	h.mu.Unlock()
	ok := false
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for _, ack := range acks {
		select {
		case err := <-ack:
			if err == nil {
				ok = true
			}
		case <-timeout.C:
			return ok
		}
	}
	return ok
}

// Subscribe opens an events subscription for GET /v1/bridge/events. The first event is the current state.
func (s *Supervisor) Subscribe() (*Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.wire.AgentProcessExists() {
		return nil, &StateError{State: s.wire}
	}
	return s.hub.subscribe(wire.BridgeEvent{Type: wire.EventState, State: s.wire, Reason: reasonPtr(s.reason)}), nil
}

// Unsubscribe ends a subscription.
func (s *Supervisor) Unsubscribe(sub *Subscription) { s.hub.unsubscribe(sub) }

// Next returns the next event of the subscription and a function to report whether writing it succeeded. It
// returns false when the subscription was dropped or ctx ended.
func (sub *Subscription) Next(ctx context.Context) (wire.BridgeEvent, func(error), bool) {
	select {
	case it := <-sub.ch:
		return it.ev, func(err error) {
			if it.ack != nil {
				it.ack <- err
			}
		}, true
	case <-sub.gone:
		return wire.BridgeEvent{}, nil, false
	case <-ctx.Done():
		return wire.BridgeEvent{}, nil, false
	}
}
