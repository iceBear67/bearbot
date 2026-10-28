package agent

import (
	"context"
	"log/slog"

	"bearbot/daemon/internal/check"
	"bearbot/daemon/internal/config"
	"bearbot/daemon/internal/layout"
	"bearbot/daemon/internal/snapshot"
	"bearbot/daemon/internal/state"
	"bearbot/daemon/internal/wire"
)

// host gives the incident and rollback flows access to the supervisor (incident.Host, rollback.Host).
type host struct{ s *Supervisor }

func (h *host) Paths() layout.Paths                       { return h.s.paths }
func (h *host) Users() *layout.Users                      { return h.s.users }
func (h *host) Config() *config.Config                    { return h.s.cfg }
func (h *host) Repo() *snapshot.Repo                      { return h.s.repo }
func (h *host) Logger() *slog.Logger                      { return h.s.log }
func (h *host) State() *state.State                       { return h.s.stateCopy() }
func (h *host) Update(fn func(*state.State)) error        { return h.s.update(fn) }
func (h *host) SetWire(st wire.AgentState, reason string) { h.s.setWire(st, reason) }
func (h *host) Notify(text string)                        { h.s.notify(text) }

func (h *host) Node() string { return h.s.node }

func (h *host) Check(ctx context.Context) (*check.Report, error) { return h.s.Check(ctx) }

// LockChanges takes the change lock of DESIGN 5.4 for the snapshot commits of the incident flows.
func (h *host) LockChanges(ctx context.Context) (func(), error) {
	if err := h.s.change.lock(ctx); err != nil {
		return nil, err
	}
	return h.s.change.unlock, nil
}

func (h *host) StartAgent(ctx context.Context, probation bool) error {
	return h.s.startAgent(ctx, probation)
}

// StopAgent sends abort without waiting for it (a locked turn may never end), then stops the process (DESIGN 5.12
// step 3).
func (h *host) StopAgent(ctx context.Context) error {
	s := h.s
	s.mu.Lock()
	ap := s.cur
	s.mu.Unlock()
	if ap == nil {
		return nil
	}
	go func() {
		c, cancel := s.rpcCtx(context.Background())
		defer cancel()
		if err := ap.rpc.Abort(c); err != nil {
			s.log.Info("abort before the rollback stop did not complete", "pid", ap.p.PID(), "err", err)
		}
	}()
	return s.stopAgent(ap, true)
}
