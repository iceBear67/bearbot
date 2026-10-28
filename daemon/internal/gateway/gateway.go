// Package gateway supervises the gateway process (DESIGN 5.5) and keeps the gateway-ctl.sock connection
// (contracts 4): requests in both directions, agent.state after every hello and on every change, and the queue of
// notify requests that waits while the gateway is not connected. It also writes gateway.json (contracts 3) and hands
// the daemon's stdin and stdout to the gateway when the console is enabled (DESIGN 5.13).
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sourcegraph/jsonrpc2"

	"bearbot/daemon/internal/config"
	"bearbot/daemon/internal/layout"
	"bearbot/daemon/internal/proc"
	"bearbot/daemon/internal/wire"
)

// JSON-RPC error codes of gateway-ctl.sock (contracts 4).
const (
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeApp            = -32000
)

// Time limits.
const (
	// StartTimeout bounds the time from the start of the gateway process until gateway-ctl.sock answers hello.
	StartTimeout = 60 * time.Second
	// RequestTimeout bounds the daemon's requests other than notify and drain.
	RequestTimeout = 30 * time.Second
	// DrainTimeout bounds the drain request at shutdown.
	DrainTimeout = 60 * time.Second
	// StableAfter is how long a gateway must stay connected for its crash count to be reset.
	StableAfter = 60 * time.Second
	minBackoff  = time.Second
	maxBackoff  = 60 * time.Second
)

// CtlError is an error answered to the gateway with a JSON-RPC error code.
type CtlError struct {
	Code    int64
	Message string
}

func (e *CtlError) Error() string { return e.Message }

// Handler handles the requests of the gateway (contracts 4.2). Errors that are not *CtlError are answered with
// code -32000 and their message.
type Handler interface {
	HandleCtl(ctx context.Context, method string, params json.RawMessage) (any, error)
}

// Options holds the dependencies of a Supervisor.
type Options struct {
	Paths  layout.Paths
	Users  *layout.Users
	Config *config.Config
	Logger *slog.Logger
	// Console hands Stdin and Stdout to the gateway as fd 3 and 4.
	Console bool
	Stdin   *os.File
	Stdout  *os.File
	// OnCrashLimit runs when the gateway exceeded [gateway] crash_limit (DESIGN 5.5: the on_enter hooks).
	OnCrashLimit func(reason, report string)
}

// Info describes the gateway for `bearbotd status`.
type Info struct {
	PID         *int
	StartedAt   *time.Time
	Connected   bool
	ConnectedAt *time.Time
	Starts      int
	Crashes     int
}

// Supervisor runs the gateway.
type Supervisor struct {
	o    Options
	node string
	log  *slog.Logger
	glog *proc.LogWriter

	handler Handler
	stateFn func() (wire.AgentState, *string)
	baseCtx context.Context

	mu          sync.Mutex
	conn        *jsonrpc2.Conn
	connectedAt time.Time
	connectedCh chan struct{}
	// wake belongs to the current connection; its senders wait on it.
	wake         *wakers
	process      *proc.Process
	queue        []notice
	nextNotice   uint64
	stateVersion uint64
	nextID       uint64
	starts       int
	crashes      int

	stopOnce sync.Once
	stopCh   chan struct{}
	done     chan struct{}
}

// New prepares the supervisor; node is looked up in the daemon's PATH (contracts 2.4).
func New(o Options) (*Supervisor, error) {
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, fmt.Errorf("找不到 node 命令：%w", err)
	}
	return &Supervisor{
		o:           o,
		node:        node,
		log:         o.Logger,
		glog:        proc.NewLogWriter(o.Paths.GatewayLog, 500),
		connectedCh: make(chan struct{}),
		stopCh:      make(chan struct{}),
		done:        make(chan struct{}),
	}, nil
}

// notice is one queued notify request; id tells it apart from a later notice with the same text.
type notice struct {
	id   uint64
	text string
}

// wakers wake the two senders of one connection: agent.state and the notify queue have separate channels, so
// neither sender takes the other's signal.
type wakers struct {
	state  chan struct{}
	notify chan struct{}
}

func newWakers() *wakers {
	return &wakers{state: make(chan struct{}, 1), notify: make(chan struct{}, 1)}
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// SetHandler sets the handler of the gateway's requests; call before Run.
func (g *Supervisor) SetHandler(h Handler) { g.handler = h }

// SetStateSource sets the function that gives the current agent state; call before Run.
func (g *Supervisor) SetStateSource(fn func() (wire.AgentState, *string)) { g.stateFn = fn }

// Log is the writer of gateway.log.
func (g *Supervisor) Log() *proc.LogWriter { return g.glog }

// Notify queues a notice for every administrator (contracts 4.1 notify); it is sent when the gateway is connected.
func (g *Supervisor) Notify(text string) {
	g.mu.Lock()
	g.nextNotice++
	g.queue = append(g.queue, notice{id: g.nextNotice, text: text})
	w := g.wake
	g.mu.Unlock()
	if w != nil {
		signal(w.notify)
	}
}

// AgentStateChanged makes the state sender send agent.state with the current state.
func (g *Supervisor) AgentStateChanged() {
	g.mu.Lock()
	g.stateVersion++
	w := g.wake
	g.mu.Unlock()
	if w != nil {
		signal(w.state)
	}
}

// WaitConnected waits until gateway-ctl.sock completed hello.
func (g *Supervisor) WaitConnected(ctx context.Context) error {
	for {
		g.mu.Lock()
		if g.conn != nil {
			g.mu.Unlock()
			return nil
		}
		ch := g.connectedCh
		g.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Info returns the gateway's process and connection state.
func (g *Supervisor) Info() Info {
	g.mu.Lock()
	defer g.mu.Unlock()
	info := Info{Connected: g.conn != nil, Starts: g.starts, Crashes: g.crashes}
	if g.process != nil && !g.process.Exited() {
		pid := g.process.PID()
		started := g.process.StartedAt
		info.PID, info.StartedAt = &pid, &started
	}
	if g.conn != nil {
		t := g.connectedAt
		info.ConnectedAt = &t
	}
	return info
}

func (g *Supervisor) id() jsonrpc2.ID {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nextID++
	return jsonrpc2.ID{Str: "d" + strconv.FormatUint(g.nextID, 10), IsString: true}
}

// ErrNotConnected is returned by Call while gateway-ctl.sock is not connected.
var ErrNotConnected = errors.New("gateway 没有连接，请稍后再试")

func (g *Supervisor) call(ctx context.Context, conn *jsonrpc2.Conn, method string, params, result any) error {
	err := conn.Call(ctx, method, params, result, jsonrpc2.PickID(g.id()))
	var rpcErr *jsonrpc2.Error
	if errors.As(err, &rpcErr) {
		return &CtlError{Code: rpcErr.Code, Message: rpcErr.Message}
	}
	return err
}

// Call sends a request to the gateway (platforms.status, admin.*) and decodes the result. An error answer of the
// gateway is returned as *CtlError with its Chinese message.
func (g *Supervisor) Call(ctx context.Context, method string, params, result any) error {
	g.mu.Lock()
	conn := g.conn
	g.mu.Unlock()
	if conn == nil {
		return ErrNotConnected
	}
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	if err := g.call(ctx, conn, method, params, result); err != nil {
		var ce *CtlError
		if errors.As(err, &ce) {
			return err
		}
		return fmt.Errorf("gateway 没有回应 %s：%w", method, err)
	}
	return nil
}

// Drain asks the gateway to stop receiving from all platforms (shutdown, DESIGN 5.2).
func (g *Supervisor) Drain(ctx context.Context) error {
	g.mu.Lock()
	conn := g.conn
	g.mu.Unlock()
	if conn == nil {
		g.log.Warn("gateway not connected, drain skipped")
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, DrainTimeout)
	defer cancel()
	if err := g.call(ctx, conn, "drain", struct{}{}, nil); err != nil {
		return fmt.Errorf("gateway drain 失败：%w", err)
	}
	g.log.Info("gateway drained")
	return nil
}

// Run supervises the gateway until Stop. ctx is given to the handler of the gateway's requests: requests the
// daemon started run to the end even when the connection is lost (DESIGN 3).
func (g *Supervisor) Run(ctx context.Context) {
	defer close(g.done)
	g.baseCtx = ctx
	backoff := minBackoff
	hooked := false
	for {
		p, err := g.start()
		if errors.Is(err, errStopping) {
			return
		}
		if err != nil {
			g.log.Error("gateway did not start", "err", err)
		} else {
			stable := g.serve(p)
			if g.stopping() {
				return
			}
			exit := p.Exit()
			g.log.Error("gateway exited unexpectedly", "pid", p.PID(), "exit", exit.String(), "stable", stable)
			if stable {
				g.mu.Lock()
				g.crashes = 0
				g.mu.Unlock()
				backoff, hooked = minBackoff, false
			}
		}
		g.mu.Lock()
		g.crashes++
		crashes := g.crashes
		g.mu.Unlock()
		if crashes > g.o.Config.Gateway.CrashLimit {
			backoff = maxBackoff
			if !hooked {
				hooked = true
				reason := fmt.Sprintf("gateway 连续意外退出 %d 次，超过 [gateway] crash_limit（%d），之后每 %s 重启一次", crashes, g.o.Config.Gateway.CrashLimit, maxBackoff)
				g.log.Error("gateway crash limit exceeded", "crashes", crashes)
				lines, err := proc.TailFile(g.o.Paths.GatewayLog, 50)
				if err != nil {
					g.log.Error("read gateway.log", "err", err)
				}
				report := strings.Join(lines, "\n")
				if g.o.OnCrashLimit != nil {
					g.o.OnCrashLimit(reason, report)
				}
				g.Notify(reason + "。gateway 恢复连接后收到这条通知；详情见 gateway.log。")
			}
		}
		t := time.NewTimer(backoff)
		select {
		case <-t.C:
		case <-g.stopCh:
			t.Stop()
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (g *Supervisor) stopping() bool {
	select {
	case <-g.stopCh:
		return true
	default:
		return false
	}
}

// errStopping is returned by start after Stop was called.
var errStopping = errors.New("bearbotd 正在关闭")

// start starts the gateway process. It decides under g.mu, as Stop does: either Stop sees the new process and
// terminates it, or start sees that Stop was called and starts nothing.
func (g *Supervisor) start() (*proc.Process, error) {
	o := g.o
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopping() {
		return nil, errStopping
	}
	var extra []*os.File
	if o.Console {
		extra = []*os.File{o.Stdin, o.Stdout}
	}
	fmt.Fprintf(g.glog, "=== gateway 启动于 %s ===\n", time.Now().Format(time.RFC3339))
	p, err := proc.Start(proc.Spec{
		Path:           g.node,
		Args:           o.Paths.GatewayArgs(),
		Env:            o.Paths.GatewayEnv(),
		Dir:            o.Paths.GatewayCwd(),
		Credential:     o.Users.Credential(o.Users.Gateway),
		ExtraFiles:     extra,
		Stdout:         g.glog,
		StderrToStdout: true,
	})
	if err != nil {
		return nil, err
	}
	g.process = p
	g.starts++
	g.log.Info("gateway started", "pid", p.PID(), "console", o.Console)
	return p, nil
}

// serve connects to gateway-ctl.sock, keeps the connection while the process runs (reconnecting when the
// connection alone is lost) and returns after the process exited. stable tells whether it stayed connected for
// StableAfter.
func (g *Supervisor) serve(p *proc.Process) (stable bool) {
	deadline := time.Now().Add(StartTimeout)
	for {
		conn, err := g.connect(p, deadline)
		if err != nil {
			if !p.Exited() && !g.stopping() {
				g.log.Error("gateway-ctl.sock not usable, killing the gateway", "err", err)
				if err := p.KillTree(); err != nil {
					g.log.Error("kill gateway", "err", err)
				}
			}
			// Stop terminates the process itself.
			<-p.Done()
			return stable
		}
		connectedAt := time.Now()
		select {
		case <-p.Done():
		case <-conn.DisconnectNotify():
		}
		g.disconnected(conn)
		if time.Since(connectedAt) >= StableAfter {
			stable = true
		}
		if p.Exited() || g.stopping() {
			<-p.Done()
			return stable
		}
		g.log.Warn("gateway-ctl.sock connection lost while the gateway runs, reconnecting")
		deadline = time.Now().Add(StartTimeout)
	}
}

// connect dials gateway-ctl.sock until it accepts (or the process exits, Stop is called or deadline passes) and
// completes hello. The gateway accepts one connection at a time and drops its previous one only when it saw that
// connection close, so right after a reconnect it may refuse the new connection; then the dial is repeated until
// the deadline.
func (g *Supervisor) connect(p *proc.Process, deadline time.Time) (*jsonrpc2.Conn, error) {
	for {
		nc, err := g.dial(p, deadline)
		if err != nil {
			return nil, err
		}
		conn := jsonrpc2.NewConn(context.Background(), jsonrpc2.NewPlainObjectStream(nc), jsonrpc2.AsyncHandler(&rpcHandler{g}), jsonrpc2.SetLogger(slogPrintf{g.log}))
		ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
		err = g.call(ctx, conn, "hello", struct{}{}, nil)
		cancel()
		if err == nil {
			w := newWakers()
			g.mu.Lock()
			g.conn = conn
			g.wake = w
			g.connectedAt = time.Now()
			close(g.connectedCh)
			g.mu.Unlock()
			g.log.Info("gateway-ctl.sock connected", "pid", p.PID())
			firstState := make(chan struct{})
			go g.stateSender(conn, w, firstState)
			go g.notifySender(conn, w, firstState)
			return conn, nil
		}
		conn.Close()
		if !closedByPeer(err) {
			return nil, fmt.Errorf("gateway 没有回应 hello：%w", err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("gateway 在 %s 内一直关闭新的 gateway-ctl.sock 连接：%w", StartTimeout, err)
		}
		g.log.Info("gateway closed the new gateway-ctl.sock connection before hello, dialing again", "err", err)
		if err := g.pause(p); err != nil {
			return nil, err
		}
	}
}

// closedByPeer reports whether a request failed because the other side closed the connection.
func closedByPeer(err error) bool {
	return errors.Is(err, jsonrpc2.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}

// dial connects to gateway-ctl.sock, retrying every 100 ms until deadline.
func (g *Supervisor) dial(p *proc.Process, deadline time.Time) (net.Conn, error) {
	for {
		c, err := net.Dial("unix", g.o.Paths.GatewayCtlSock)
		if err == nil {
			return c, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s 在 %s 内没有接受连接：%w", g.o.Paths.GatewayCtlSock, StartTimeout, err)
		}
		if err := g.pause(p); err != nil {
			return nil, err
		}
	}
}

// pause waits 100 ms before the next connection attempt; it fails when the process exits or Stop is called.
func (g *Supervisor) pause(p *proc.Process) error {
	t := time.NewTimer(100 * time.Millisecond)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-p.Done():
		return fmt.Errorf("gateway 在接受 gateway-ctl.sock 连接之前退出（%s）", p.Exit())
	case <-g.stopCh:
		return errStopping
	}
}

func (g *Supervisor) disconnected(conn *jsonrpc2.Conn) {
	conn.Close()
	g.mu.Lock()
	if g.conn == conn {
		g.conn = nil
		g.wake = nil
		g.connectedCh = make(chan struct{})
	}
	g.mu.Unlock()
	g.log.Info("gateway-ctl.sock disconnected")
}

// stateSender sends agent.state after hello and on every change, and closes firstState after the first answer. It
// runs apart from the notify requests, so a notify that waits for a slow platform never delays a state change
// (DESIGN 5.4: the gateway knows the current state at all times).
func (g *Supervisor) stateSender(conn *jsonrpc2.Conn, w *wakers, firstState chan struct{}) {
	var sent uint64
	first := true
	for {
		g.mu.Lock()
		version := g.stateVersion
		g.mu.Unlock()
		if first || version != sent {
			st, reason := g.stateFn()
			ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
			err := g.call(ctx, conn, "agent.state", map[string]any{"state": st, "reason": reason}, nil)
			cancel()
			if !g.handleSendErr(conn, "agent.state", err) {
				return
			}
			if first {
				close(firstState)
			}
			first, sent = false, version
			continue
		}
		select {
		case <-w.state:
		case <-conn.DisconnectNotify():
			return
		}
	}
}

// notifySender sends the queued notify requests in order once the first agent.state of the connection was answered
// (contracts 4.1). The gateway answers a notify after it posted to every admin thread, so the request has no time
// limit. A notify that fails because the connection is lost stays queued.
func (g *Supervisor) notifySender(conn *jsonrpc2.Conn, w *wakers, firstState <-chan struct{}) {
	select {
	case <-firstState:
	case <-conn.DisconnectNotify():
		return
	}
	for {
		g.mu.Lock()
		var head notice
		pending := len(g.queue) > 0
		if pending {
			head = g.queue[0]
		}
		g.mu.Unlock()
		if !pending {
			select {
			case <-w.notify:
			case <-conn.DisconnectNotify():
				return
			}
			continue
		}
		err := g.call(context.Background(), conn, "notify", map[string]any{"text": head.text}, nil)
		if !g.handleSendErr(conn, "notify", err) {
			return
		}
		g.mu.Lock()
		if len(g.queue) > 0 && g.queue[0].id == head.id {
			g.queue = g.queue[1:]
		}
		g.mu.Unlock()
	}
}

// handleSendErr reports whether the sender may continue: an error answer is logged and the item is done; any other
// error means the connection is unusable.
func (g *Supervisor) handleSendErr(conn *jsonrpc2.Conn, method string, err error) bool {
	if err == nil {
		return true
	}
	var ce *CtlError
	if errors.As(err, &ce) {
		g.log.Error("gateway rejected a request", "method", method, "code", ce.Code, "message", ce.Message)
		return true
	}
	g.log.Warn("request to the gateway failed, closing the connection", "method", method, "err", err)
	conn.Close()
	return false
}

// Stop terminates the gateway (SIGTERM, then SIGKILL after grace) and waits for Run to return. stopCh is closed and
// the process is read under g.mu, together with start's decision.
func (g *Supervisor) Stop(grace time.Duration) error {
	g.mu.Lock()
	g.stopOnce.Do(func() { close(g.stopCh) })
	p := g.process
	g.mu.Unlock()
	var err error
	if p != nil && !p.Exited() {
		err = p.Terminate(grace)
	}
	<-g.done
	return errors.Join(err, g.glog.Close())
}

type rpcHandler struct{ g *Supervisor }

func (h *rpcHandler) Handle(_ context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	g := h.g
	if req.Notif {
		g.log.Warn("notification on gateway-ctl.sock ignored", "method", req.Method)
		return
	}
	var params json.RawMessage
	if req.Params != nil {
		params = *req.Params
	}
	g.log.Info("gateway request", "method", req.Method, "id", req.ID.String())
	result, err := g.handler.HandleCtl(g.baseCtx, req.Method, params)
	ctx := context.Background()
	if err != nil {
		code, msg := int64(CodeApp), err.Error()
		var ce *CtlError
		if errors.As(err, &ce) {
			code, msg = ce.Code, ce.Message
		}
		g.log.Info("gateway request failed", "method", req.Method, "code", code, "message", msg)
		if err := conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{Code: code, Message: msg}); err != nil {
			g.log.Warn("answer to the gateway lost", "method", req.Method, "err", err)
		}
		return
	}
	if err := conn.Reply(ctx, req.ID, result); err != nil {
		g.log.Warn("answer to the gateway lost", "method", req.Method, "err", err)
	}
}

// slogPrintf adapts slog to the jsonrpc2 logger.
type slogPrintf struct{ log *slog.Logger }

func (l slogPrintf) Printf(format string, v ...any) { l.log.Warn(fmt.Sprintf(format, v...)) }
