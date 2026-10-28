package pirpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"bearbot/daemon/internal/proc"
	"bearbot/daemon/internal/testenv"
)

const testExtension = `
export default function (pi: any) {
  pi.on("session_start", async () => {
    if (process.env.PIRPC_TEST_THROW_START === "1") throw new Error("session_start boom");
  });
  pi.registerCommand("ask", {
    description: "dialogs",
    handler: async (_args: string, ctx: any) => {
      const s = await ctx.ui.select("Pick one", ["A", "B"]);
      const c = await ctx.ui.confirm("Sure?", "Really sure?");
      const i = await ctx.ui.input("Name?", "placeholder");
      const e = await ctx.ui.editor("Edit", "prefill");
      ctx.ui.notify("results select=" + String(s) + " confirm=" + String(c) + " input=" + String(i) + " editor=" + String(e), "info");
    },
  });
  pi.registerCommand("slow", {
    description: "waits",
    handler: async (args: string, ctx: any) => {
      await new Promise((r) => setTimeout(r, Number(args) || 1500));
      ctx.ui.notify("slow done " + args, "info");
    },
  });
  pi.registerCommand("status", {
    description: "status",
    handler: async (_args: string, ctx: any) => {
      ctx.ui.setStatus("probe", "status text");
    },
  });
  pi.registerCommand("boom", {
    description: "throws",
    handler: async () => {
      throw new Error("command boom");
    },
  });
  // Like bearbot-im with a queued turn: the next run starts from the agent_settled handler of an aborted run.
  let redispatched = false;
  pi.on("agent_settled", (event: any) => {
    if (process.env.PIRPC_TEST_REDISPATCH === "1" && event.aborted && !redispatched) {
      redispatched = true;
      pi.sendUserMessage("second run");
    }
  });
}
`

type events struct {
	mu   sync.Mutex
	list []Event
	ch   chan struct{}
}

func newEvents() *events { return &events{ch: make(chan struct{}, 1)} }

func (e *events) add(ev Event) {
	e.mu.Lock()
	e.list = append(e.list, ev)
	e.mu.Unlock()
	select {
	case e.ch <- struct{}{}:
	default:
	}
}

func (e *events) snapshot() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Event{}, e.list...)
}

func (e *events) wait(t *testing.T, timeout time.Duration, match func(Event) bool) Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		for _, ev := range e.snapshot() {
			if match(ev) {
				return ev
			}
		}
		select {
		case <-e.ch:
		case <-deadline:
			var types []string
			for _, ev := range e.snapshot() {
				types = append(types, string(ev.Raw))
			}
			t.Fatalf("expected event did not arrive; events:\n%s", strings.Join(types, "\n"))
		}
	}
}

type piProcess struct {
	proc   *proc.Process
	client *Client
	events *events
	stderr *proc.Ring
}

func startPi(t *testing.T, extraEnv ...string) *piProcess {
	t.Helper()
	return startPiWithProfile(t, nil, extraEnv...)
}

// startPiWithProfile writes the profile files (name to content) into pi's agent directory before the start.
func startPiWithProfile(t *testing.T, profile map[string]string, extraEnv ...string) *piProcess {
	t.Helper()
	repo, err := testenv.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ext := filepath.Join(dir, "probe.ts")
	if err := os.WriteFile(ext, []byte(testExtension), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"home", "agent", "ws"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range profile {
		if err := os.WriteFile(filepath.Join(dir, "agent", name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env := append([]string{
		"HOME=" + filepath.Join(dir, "home"),
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"PI_CODING_AGENT_DIR=" + filepath.Join(dir, "agent"),
		"PI_SKIP_VERSION_CHECK=1",
		"PI_OFFLINE=1",
	}, extraEnv...)
	stderr := proc.NewRing(500)
	p, err := proc.Start(proc.Spec{
		Path:      filepath.Join(repo, "node_modules/.bin/pi"),
		Args:      []string{"--mode", "rpc", "--no-session", "-ne", "-e", ext},
		Env:       env,
		Dir:       filepath.Join(dir, "ws"),
		StdinPipe: true,
		Stderr:    stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	ev := newEvents()
	c := New(p.Stdin, p.Stdout, Options{OnEvent: ev.add})
	t.Cleanup(func() {
		if err := p.Stop(2 * time.Second); err != nil {
			t.Error(err)
		}
		if t.Failed() {
			t.Logf("pi stderr:\n%s", strings.Join(stderr.Lines(0), "\n"))
		}
	})
	return &piProcess{proc: p, client: c, events: ev, stderr: stderr}
}

func ctxTimeout(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func TestGetState(t *testing.T) {
	pi := startPi(t)
	st, err := pi.client.GetState(ctxTimeout(t, 60*time.Second), "boot")
	if err != nil {
		t.Fatal(err)
	}
	if st.SessionID == "" || st.IsStreaming || st.SessionFile != "" {
		t.Fatalf("state = %+v", st)
	}
	st2, err := pi.client.GetState(ctxTimeout(t, 10*time.Second), "")
	if err != nil || st2.SessionID != st.SessionID {
		t.Fatalf("second get_state = %+v, %v", st2, err)
	}
}

func TestOutOfOrderResponses(t *testing.T) {
	pi := startPi(t)
	if _, err := pi.client.GetState(ctxTimeout(t, 60*time.Second), "boot"); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		disposition string
		err         error
		at          time.Time
	}
	slow := make(chan outcome, 1)
	go func() {
		d, err := pi.client.Prompt(ctxTimeout(t, 30*time.Second), "/slow 1500")
		slow <- outcome{d, err, time.Now()}
	}()
	// Give the prompt a head start so that it is written first.
	time.Sleep(100 * time.Millisecond)
	if _, err := pi.client.GetState(ctxTimeout(t, 10*time.Second), ""); err != nil {
		t.Fatal(err)
	}
	stateAt := time.Now()
	select {
	case <-slow:
		t.Fatal("the slow prompt answered before get_state")
	default:
	}
	o := <-slow
	if o.err != nil || o.disposition != "handled" {
		t.Fatalf("slow prompt = %+v", o)
	}
	if !o.at.After(stateAt) {
		t.Fatal("responses were not out of order")
	}
	// The notify written before the prompt response was dispatched before Prompt returned.
	found := false
	for _, ev := range pi.events.snapshot() {
		if r, ok := ev.UIRequest(); ok && r.Method == "notify" && r.Message == "slow done 1500" {
			found = true
		}
	}
	if !found {
		t.Fatal("notify of the slow command was not dispatched before its response")
	}
}

func TestDialogsAreCancelled(t *testing.T) {
	pi := startPi(t)
	if _, err := pi.client.GetState(ctxTimeout(t, 60*time.Second), "boot"); err != nil {
		t.Fatal(err)
	}
	d, err := pi.client.Prompt(ctxTimeout(t, 20*time.Second), "/ask")
	if err != nil || d != "handled" {
		t.Fatalf("prompt /ask = %q, %v", d, err)
	}
	ev := pi.events.wait(t, 5*time.Second, func(ev Event) bool {
		r, ok := ev.UIRequest()
		return ok && r.Method == "notify" && strings.HasPrefix(r.Message, "results ")
	})
	r, _ := ev.UIRequest()
	if r.Message != "results select=undefined confirm=false input=undefined editor=undefined" {
		t.Fatalf("dialog results = %q", r.Message)
	}
	if ev.AutoCancelled {
		t.Fatal("notify marked as cancelled")
	}
	methods := map[string]bool{}
	for _, ev := range pi.events.snapshot() {
		if r, ok := ev.UIRequest(); ok && DialogMethods[r.Method] {
			if !ev.AutoCancelled {
				t.Errorf("dialog %s not cancelled", r.Method)
			}
			methods[r.Method] = true
		}
	}
	if len(methods) != 4 {
		t.Fatalf("dialog requests seen: %v", methods)
	}

	if _, err := pi.client.Prompt(ctxTimeout(t, 10*time.Second), "/status"); err != nil {
		t.Fatal(err)
	}
	ev = pi.events.wait(t, 5*time.Second, func(ev Event) bool {
		r, ok := ev.UIRequest()
		return ok && r.Method == "setStatus"
	})
	if r, _ := ev.UIRequest(); r.StatusKey != "probe" || r.StatusText != "status text" || ev.AutoCancelled {
		t.Fatalf("setStatus = %+v", r)
	}
}

func TestExtensionErrors(t *testing.T) {
	pi := startPi(t, "PIRPC_TEST_THROW_START=1")
	if _, err := pi.client.GetState(ctxTimeout(t, 60*time.Second), "boot"); err != nil {
		t.Fatal(err)
	}
	// session_start errors are written before the boot response, so they are already dispatched.
	var startErr *ExtensionError
	for _, ev := range pi.events.snapshot() {
		if x, ok := ev.ExtensionError(); ok && x.Event == "session_start" {
			startErr = &x
		}
	}
	if startErr == nil || startErr.Error != "session_start boom" || !strings.HasSuffix(startErr.ExtensionPath, "probe.ts") {
		t.Fatalf("session_start error = %+v", startErr)
	}
	if _, err := pi.client.Prompt(ctxTimeout(t, 10*time.Second), "/boom"); err != nil {
		t.Fatal(err)
	}
	ev := pi.events.wait(t, 5*time.Second, func(ev Event) bool {
		x, ok := ev.ExtensionError()
		return ok && x.Event == "command"
	})
	x, _ := ev.ExtensionError()
	if x.ExtensionPath != "command:boom" || x.Error != "command boom" {
		t.Fatalf("command error = %+v", x)
	}

	// rpc-events.jsonl line of the record.
	if !ev.RPCLogged() {
		t.Fatal("extension_error is not logged")
	}
	var logged map[string]any
	if err := json.Unmarshal(ev.LogRecord(), &logged); err != nil {
		t.Fatalf("LogRecord %s: %v", ev.LogRecord(), err)
	}
	at, err := time.Parse(time.RFC3339, logged["at"].(string))
	if err != nil || at.Sub(ev.At).Abs() > time.Second || logged["type"] != "extension_error" || logged["error"] != "command boom" {
		t.Fatalf("logged = %v (%v)", logged, err)
	}
	for _, other := range pi.events.snapshot() {
		if other.Type == "extension_ui_request" && other.RPCLogged() {
			t.Fatalf("%s is logged", other.Raw)
		}
	}
}

func TestCommandError(t *testing.T) {
	pi := startPi(t)
	resp, err := pi.client.Call(ctxTimeout(t, 60*time.Second), Command{"type": "no_such_command"})
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) || !strings.Contains(cmdErr.Message, "Unknown command") || resp == nil || resp.Success {
		t.Fatalf("Call = %+v, %v", resp, err)
	}
	// compact on an empty session fails with a normal error response.
	_, err = pi.client.Compact(ctxTimeout(t, 10*time.Second), "focus")
	if !errors.As(err, &cmdErr) || cmdErr.Command != "compact" {
		t.Fatalf("Compact = %v", err)
	}
	if err := pi.client.Abort(ctxTimeout(t, 10*time.Second)); err != nil {
		t.Fatalf("Abort while idle: %v", err)
	}
}

// TestAbortWithNextRun: the extension starts the next run from its agent_settled handler of the aborted run, as
// bearbot-im does with a queued turn. pi answers abort only after that next run ended, so Abort returns on the
// agent_settled record with aborted:true. The model endpoint refuses connections and pi retries (4 s, 8 s, ...), so
// each run stays open until it is aborted.
func TestAbortWithNextRun(t *testing.T) {
	if c, err := net.DialTimeout("tcp", "127.0.0.1:9", time.Second); err == nil {
		c.Close()
		t.Fatal("127.0.0.1:9 accepts connections; this test needs a model endpoint that refuses them")
	} else if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("127.0.0.1:9 does not refuse the connection: %v", err)
	}
	pi := startPiWithProfile(t, map[string]string{
		"models.json":   `{"providers": {"testprovider": {"baseUrl": "http://127.0.0.1:9/v1", "api": "openai-completions", "apiKey": "invalid-key", "models": [{"id": "unreachable"}]}}}`,
		"settings.json": `{"defaultProvider": "testprovider", "defaultModel": "unreachable", "retry": {"enabled": true, "maxRetries": 5, "baseDelayMs": 4000}}`,
	}, "PIRPC_TEST_REDISPATCH=1")
	if _, err := pi.client.GetState(ctxTimeout(t, 60*time.Second), "boot"); err != nil {
		t.Fatal(err)
	}
	count := func(match func(Event) bool) int {
		n := 0
		for _, ev := range pi.events.snapshot() {
			if match(ev) {
				n++
			}
		}
		return n
	}
	isStart := func(ev Event) bool { return ev.Type == "agent_start" }
	isAbortedSettle := func(ev Event) bool { aborted, ok := ev.AgentSettled(); return ok && aborted }
	isAbortAnswer := func(ev Event) bool {
		var r Response
		return ev.Type == "response" && ev.Decode(&r) == nil && r.Command == "abort"
	}

	d, err := pi.client.Prompt(ctxTimeout(t, 30*time.Second), "first run")
	if err != nil || d != "started" {
		t.Fatalf("prompt = %q, %v", d, err)
	}
	pi.events.wait(t, 30*time.Second, isStart)
	if err := pi.client.Abort(ctxTimeout(t, 20*time.Second)); err != nil {
		t.Fatalf("Abort with the next run starting from agent_settled: %v", err)
	}
	if count(isAbortedSettle) != 1 || count(isAbortAnswer) != 0 {
		t.Fatalf("after Abort: %d aborted settles, %d abort answers", count(isAbortedSettle), count(isAbortAnswer))
	}
	// The second run started from the handler and keeps pi busy, so pi has not answered the first abort.
	pi.events.wait(t, 10*time.Second, func(Event) bool { return count(isStart) == 2 })
	st, err := pi.client.GetState(ctxTimeout(t, 10*time.Second), "")
	if err != nil || !st.IsStreaming {
		t.Fatalf("state while the second run is open = %+v, %v", st, err)
	}
	if count(isAbortAnswer) != 0 {
		t.Fatal("pi answered the first abort while the second run is open")
	}
	// The second abort ends the second run; no further run starts. Only now pi answers the first abort, as a
	// response nobody waits for.
	if err := pi.client.Abort(ctxTimeout(t, 20*time.Second)); err != nil {
		t.Fatalf("second Abort: %v", err)
	}
	pi.events.wait(t, 10*time.Second, isAbortAnswer)
	events := pi.events.snapshot()
	firstAnswer := slices.IndexFunc(events, isAbortAnswer)
	var settles []int
	for i, ev := range events {
		if isAbortedSettle(ev) {
			settles = append(settles, i)
		}
	}
	if len(settles) != 2 || count(isStart) != 2 || firstAnswer < settles[1] {
		t.Fatalf("after the second Abort: aborted settles at %v, first abort answer at %d, %d runs", settles, firstAnswer, count(isStart))
	}
	if st, err := pi.client.GetState(ctxTimeout(t, 10*time.Second), ""); err != nil || st.IsStreaming {
		t.Fatalf("state after the second abort = %+v, %v", st, err)
	}
}

func TestTimeoutAndLateResponse(t *testing.T) {
	pi := startPi(t)
	if _, err := pi.client.GetState(ctxTimeout(t, 60*time.Second), "boot"); err != nil {
		t.Fatal(err)
	}
	_, err := pi.client.Call(ctxTimeout(t, 200*time.Millisecond), Command{"id": "late", "type": "prompt", "message": "/slow 800"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	ev := pi.events.wait(t, 5*time.Second, func(ev Event) bool { return ev.Type == "response" })
	var resp Response
	if err := ev.Decode(&resp); err != nil || resp.ID != "late" || resp.Command != "prompt" {
		t.Fatalf("late response = %s, %v", ev.Raw, err)
	}
	if _, err := pi.client.Call(ctxTimeout(t, 10*time.Second), Command{"id": "dup", "type": "prompt", "message": "/slow 500"}); err != nil {
		t.Fatal(err)
	}
}

func TestEndOfStream(t *testing.T) {
	pi := startPi(t)
	if _, err := pi.client.GetState(ctxTimeout(t, 60*time.Second), "boot"); err != nil {
		t.Fatal(err)
	}
	pending := make(chan error, 1)
	go func() {
		_, err := pi.client.Prompt(ctxTimeout(t, 30*time.Second), "/slow 20000")
		pending <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if err := pi.proc.CloseStdin(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pi.client.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("client did not see the end of stdout")
	}
	if !errors.Is(pi.client.Err(), io.EOF) {
		t.Fatalf("Err = %v", pi.client.Err())
	}
	if err := <-pending; !errors.Is(err, ErrStreamEnded) || !errors.Is(err, io.EOF) {
		t.Fatalf("pending call = %v", err)
	}
	if _, err := pi.client.GetState(ctxTimeout(t, time.Second), ""); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("call after end = %v", err)
	}
	<-pi.proc.Done()
	if !pi.proc.Exit().Success() {
		t.Fatalf("pi exit = %v", pi.proc.Exit())
	}
}

func TestClose(t *testing.T) {
	pi := startPi(t)
	if _, err := pi.client.GetState(ctxTimeout(t, 60*time.Second), "boot"); err != nil {
		t.Fatal(err)
	}
	pending := make(chan error, 1)
	go func() {
		_, err := pi.client.Prompt(ctxTimeout(t, 30*time.Second), "/slow 5000")
		pending <- err
	}()
	time.Sleep(200 * time.Millisecond)
	pi.client.Close()
	if err := <-pending; !errors.Is(err, ErrClosed) {
		t.Fatalf("pending = %v", err)
	}
	if _, err := pi.client.GetState(ctxTimeout(t, time.Second), ""); !errors.Is(err, ErrClosed) {
		t.Fatalf("after close = %v", err)
	}
}
