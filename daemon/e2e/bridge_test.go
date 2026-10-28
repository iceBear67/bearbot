package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"bearbot/daemon/internal/testenv"
)

// eventStream reads the NDJSON records of one GET /v1/bridge/events connection in a goroutine.
type eventStream struct {
	ch  chan map[string]any
	err chan error
}

// bridgeEvents opens an events connection next to bearbot-im's; it is closed when the test ends.
func (b *bed) bridgeEvents() *eventStream {
	b.t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://bearbotd/v1/bridge/events", nil)
	if err != nil {
		b.t.Fatal(err)
	}
	resp, err := testenv.UnixClient(b.env.Paths.AgentSock).Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	b.t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/x-ndjson" {
		b.t.Fatalf("GET /v1/bridge/events: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	s := &eventStream{ch: make(chan map[string]any, 1000), err: make(chan error, 1)}
	go func() {
		dec := json.NewDecoder(resp.Body)
		for {
			var ev map[string]any
			if err := dec.Decode(&ev); err != nil {
				s.err <- err
				close(s.ch)
				return
			}
			s.ch <- ev
		}
	}()
	return s
}

// waitFor returns the next record that matches; the test fails when the stream ends or timeout passes.
func (s *eventStream) waitFor(t *testing.T, what string, timeout time.Duration, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-s.ch:
			if !ok {
				t.Fatalf("%s: the events stream ended: %v", what, <-s.err)
			}
			if match(ev) {
				return ev
			}
		case <-deadline:
			t.Fatalf("%s: no such record within %v", what, timeout)
		}
	}
}

// TestBridgeEventsStream (contracts 6 GET /v1/bridge/events): an events connection next to bearbot-im's gets the
// current state first, the extension_error records of the agent process (including the <runtime>
// send_user_message error of a console message that pi rejects without a model), and every state change.
func TestBridgeEventsStream(t *testing.T) {
	b := newBed(t, nil)
	d := b.start()
	d.WaitOutputContains(t, 90*time.Second, "控制台已连接")
	b.waitState("running", 90*time.Second)
	events := b.bridgeEvents()
	first := events.waitFor(t, "first record", 10*time.Second, func(map[string]any) bool { return true })
	if first["type"] != "state" || first["state"] != "running" {
		t.Fatalf("first record = %v", first)
	}

	d.Send(t, "没有模型时的消息")
	ev := events.waitFor(t, "extension_error", 60*time.Second, func(ev map[string]any) bool { return ev["type"] == "extension_error" })
	msg, _ := ev["error"].(string)
	if ev["extensionPath"] != "<runtime>" || ev["event"] != "send_user_message" || !strings.Contains(msg, "No API key") {
		t.Fatalf("extension_error record = %v", ev)
	}

	b.ext("good.ts", goodExtension)
	b.apply("事件流中的应用", "apply/1")
	events.waitFor(t, "draining", 60*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "state" && ev["state"] == "draining"
	})
}
