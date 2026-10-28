package agent

import (
	"log/slog"
	"sync"
	"testing"

	"bearbot/daemon/internal/config"
	"bearbot/daemon/internal/gateway"
	"bearbot/daemon/internal/state"
	"bearbot/daemon/internal/testenv"
	"bearbot/daemon/internal/wire"
)

// TestSubscribeSeesTheNewestState (contracts 6 GET /v1/bridge/events): an events connection opened while the state
// changes starts with the new state or receives the change, so its last state record is always the current state.
// The supervisor and its gateway link are real; the gateway is not started (AgentStateChanged only queues the
// agent.state request).
func TestSubscribeSeesTheNewestState(t *testing.T) {
	e := testenv.New(t)
	u := e.Users(t)
	cfg, err := config.Load(e.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	gw, err := gateway.New(gateway.Options{Paths: e.Paths, Users: u, Config: cfg, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{Paths: e.Paths, Users: u, Config: cfg, Gateway: gw, Logger: log, State: state.New()})
	defer s.Close()
	for i := range 200000 {
		s.setWire(wire.Running, "")
		var sub *Subscription
		var subErr error
		var wg sync.WaitGroup
		wg.Go(func() { sub, subErr = s.Subscribe() })
		wg.Go(func() { s.setWire(wire.Draining, "应用") })
		wg.Wait()
		if subErr != nil {
			t.Fatal(subErr)
		}
		var last wire.AgentState
		for drained := false; !drained; {
			select {
			case it := <-sub.ch:
				if it.ev.Type == wire.EventState {
					last = it.ev.State
				}
			default:
				drained = true
			}
		}
		s.Unsubscribe(sub)
		if last != wire.Draining {
			t.Fatalf("round %d: the last state record of the new events connection is %q, the state is draining", i, last)
		}
	}
}
