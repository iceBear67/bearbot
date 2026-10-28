// Package pirpc is a client for pi's RPC mode (`pi --mode rpc`): JSON records separated by LF on stdin and stdout
// (pi-rpc-cli.md sections 1-3).
//
// Requests get ids and their responses are matched by id, so responses may arrive in any order. Every other record
// is delivered as an Event, in stdout order, from one dispatch goroutine; responses go through the same ordered
// queue, so events written before a response are handled before the Call returns. Reading stdout never waits for the
// handler. Dialog requests (select, confirm, input, editor) are answered with cancelled:true at once.
package pirpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"
)

// Command is one RPC command object, for example {"type": "get_state"}. Call adds "id" unless it is set.
type Command map[string]any

// Response is a {"type":"response"} record.
type Response struct {
	ID      string          `json:"id"`
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`
}

// Decode decodes the data field into v.
func (r *Response) Decode(v any) error {
	if len(r.Data) == 0 {
		return fmt.Errorf("response to %s has no data", r.Command)
	}
	return json.Unmarshal(r.Data, v)
}

// CommandError is a response with success:false.
type CommandError struct {
	Command string
	Message string
}

func (e *CommandError) Error() string { return fmt.Sprintf("pi %s 失败：%s", e.Command, e.Message) }

// Event is a record that is not the response to a pending call: an agent or session event, extension_error,
// extension_ui_request, or a response nobody waits for (Type "response", for example after a call timed out).
type Event struct {
	Type string
	// Raw is the complete record.
	Raw json.RawMessage
	// At is when the record was read.
	At time.Time
	// AutoCancelled is true for dialog requests that were answered with cancelled:true.
	AutoCancelled bool
}

// Decode decodes the record into v.
func (e Event) Decode(v any) error { return json.Unmarshal(e.Raw, v) }

// ExtensionError is the payload of an extension_error record.
type ExtensionError struct {
	ExtensionPath string `json:"extensionPath"`
	Event         string `json:"event"`
	Error         string `json:"error"`
}

// ExtensionError returns the payload when e is an extension_error record.
func (e Event) ExtensionError() (ExtensionError, bool) {
	if e.Type != "extension_error" {
		return ExtensionError{}, false
	}
	var x ExtensionError
	if err := e.Decode(&x); err != nil {
		return ExtensionError{}, false
	}
	return x, true
}

// UIRequest holds the common fields of an extension_ui_request record.
type UIRequest struct {
	ID         string   `json:"id"`
	Method     string   `json:"method"`
	Title      string   `json:"title"`
	Message    string   `json:"message"`
	NotifyType string   `json:"notifyType"`
	Options    []string `json:"options"`
	StatusKey  string   `json:"statusKey"`
	StatusText string   `json:"statusText"`
}

// UIRequest returns the request when e is an extension_ui_request record.
func (e Event) UIRequest() (UIRequest, bool) {
	if e.Type != "extension_ui_request" {
		return UIRequest{}, false
	}
	var x UIRequest
	if err := e.Decode(&x); err != nil {
		return UIRequest{}, false
	}
	return x, true
}

// AgentSettled returns the aborted flag when e is an agent_settled record.
func (e Event) AgentSettled() (aborted bool, ok bool) {
	if e.Type != "agent_settled" {
		return false, false
	}
	var x struct {
		Aborted bool `json:"aborted"`
	}
	if err := e.Decode(&x); err != nil {
		return false, false
	}
	return x.Aborted, true
}

// AssistantEnd is the outcome of an assistant message.
type AssistantEnd struct {
	StopReason   string `json:"stopReason"`
	ErrorMessage string `json:"errorMessage"`
}

// AssistantMessageEnd returns the stop reason when e is a message_end record of an assistant message.
func (e Event) AssistantMessageEnd() (AssistantEnd, bool) {
	if e.Type != "message_end" {
		return AssistantEnd{}, false
	}
	var x struct {
		Message struct {
			Role string `json:"role"`
			AssistantEnd
		} `json:"message"`
	}
	if err := e.Decode(&x); err != nil || x.Message.Role != "assistant" {
		return AssistantEnd{}, false
	}
	return x.Message.AssistantEnd, true
}

// RPCLogged reports whether e belongs to logs/agent/rpc-events.jsonl (DESIGN 5.3): extension_error, agent_start,
// agent_settled, and the message_end of an assistant message with stopReason "error".
func (e Event) RPCLogged() bool {
	switch e.Type {
	case "extension_error", "agent_start", "agent_settled":
		return true
	case "message_end":
		end, ok := e.AssistantMessageEnd()
		return ok && end.StopReason == "error"
	default:
		return false
	}
}

// LogRecord returns the record with an added "at" field (the read time, RFC 3339 with milliseconds), the line format
// of rpc-events.jsonl (contracts 8.2).
func (e Event) LogRecord() []byte {
	at, _ := json.Marshal(e.At.Format("2006-01-02T15:04:05.000Z07:00"))
	rest := bytes.TrimLeft(e.Raw, " \t\r\n")
	out := make([]byte, 0, len(rest)+len(at)+8)
	out = append(out, `{"at":`...)
	out = append(out, at...)
	body := bytes.TrimSpace(rest[1:])
	if len(body) > 0 && body[0] != '}' {
		out = append(out, ',')
	}
	return append(out, body...)
}

// DialogMethods are the extension_ui_request methods that wait for an answer; the client cancels them.
var DialogMethods = map[string]bool{"select": true, "confirm": true, "input": true, "editor": true}

var (
	// ErrClosed is returned by calls after Close.
	ErrClosed = errors.New("pi RPC client closed")
	// ErrStreamEnded is returned by calls when pi's stdout ended; it wraps the reason (see Err).
	ErrStreamEnded = errors.New("pi stdout ended")
)

// Options configures a Client.
type Options struct {
	// OnEvent receives every event in stdout order, from a single goroutine. It must not call Call and wait for
	// the result (that would wait for itself).
	OnEvent func(Event)
	// IDPrefix prefixes generated request ids (default "rpc-").
	IDPrefix string
}

type result struct {
	resp *Response
	err  error
}

type item struct {
	event Event
	resp  *Response
	end   bool
}

// Client talks to one pi process.
type Client struct {
	opts Options

	wmu sync.Mutex
	w   io.Writer

	mu      sync.Mutex
	pending map[string]chan result
	nextID  uint64
	closed  bool
	ended   bool
	endErr  error

	qmu    sync.Mutex
	queue  []item
	qready chan struct{}

	// abortedSettle is closed, and replaced under mu, when an agent_settled record with aborted:true was dispatched.
	abortedSettle chan struct{}

	done chan struct{}
}

// New starts reading stdout. stdin receives the commands.
func New(stdin io.Writer, stdout io.Reader, opts Options) *Client {
	if opts.IDPrefix == "" {
		opts.IDPrefix = "rpc-"
	}
	c := &Client{
		opts:          opts,
		w:             stdin,
		pending:       make(map[string]chan result),
		qready:        make(chan struct{}, 1),
		abortedSettle: make(chan struct{}),
		done:          make(chan struct{}),
	}
	go c.read(stdout)
	go c.dispatch()
	return c
}

// Done is closed after stdout ended and every record read before was dispatched.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns why stdout ended: io.EOF after a clean end, another error for a read error, a record cut by the end
// of the stream, or a line that is not a JSON record. It is nil while stdout is open.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.endErr
}

// Close makes pending and later calls fail with ErrClosed. Reading and event dispatch continue until stdout ends.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for id, ch := range c.pending {
		ch <- result{err: ErrClosed}
		delete(c.pending, id)
	}
}

func (c *Client) enqueue(it item) {
	c.qmu.Lock()
	c.queue = append(c.queue, it)
	c.qmu.Unlock()
	select {
	case c.qready <- struct{}{}:
	default:
	}
}

func (c *Client) read(stdout io.Reader) {
	r := bufio.NewReaderSize(stdout, 1<<20)
	var endErr error
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			if perr := c.handleLine(line[:len(line)-1]); perr != nil {
				endErr = perr
				break
			}
		} else if len(line) > 0 {
			endErr = fmt.Errorf("pi stdout ended inside a record: %s", truncate(line))
			break
		}
		if err != nil {
			endErr = err
			break
		}
	}
	c.mu.Lock()
	c.endErr = endErr
	c.mu.Unlock()
	c.enqueue(item{end: true})
}

func truncate(b []byte) string {
	const max = 300
	if len(b) > max {
		return strconv.Quote(string(b[:max])) + "…"
	}
	return strconv.Quote(string(b))
}

func (c *Client) handleLine(line []byte) error {
	var probe struct {
		Type   string  `json:"type"`
		ID     *string `json:"id"`
		Method string  `json:"method"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return fmt.Errorf("pi wrote a line that is not a JSON record: %s: %w", truncate(line), err)
	}
	if probe.Type == "" {
		return fmt.Errorf("pi wrote a record without type: %s", truncate(line))
	}
	raw := json.RawMessage(bytes.Clone(line))
	at := time.Now()
	if probe.Type == "response" && probe.ID != nil {
		var resp Response
		if err := json.Unmarshal(line, &resp); err != nil {
			return fmt.Errorf("pi wrote an invalid response: %s: %w", truncate(line), err)
		}
		c.enqueue(item{resp: &resp, event: Event{Type: probe.Type, Raw: raw, At: at}})
		return nil
	}
	ev := Event{Type: probe.Type, Raw: raw, At: at}
	if probe.Type == "extension_ui_request" && DialogMethods[probe.Method] && probe.ID != nil {
		if err := c.write(map[string]any{"type": "extension_ui_response", "id": *probe.ID, "cancelled": true}); err != nil {
			return fmt.Errorf("cancel dialog request %s: %w", *probe.ID, err)
		}
		ev.AutoCancelled = true
	}
	c.enqueue(item{event: ev})
	return nil
}

func (c *Client) dispatch() {
	for {
		c.qmu.Lock()
		batch := c.queue
		c.queue = nil
		c.qmu.Unlock()
		for _, it := range batch {
			if it.end {
				c.finish()
				return
			}
			if it.resp != nil {
				c.mu.Lock()
				ch, ok := c.pending[it.resp.ID]
				if ok {
					delete(c.pending, it.resp.ID)
				}
				c.mu.Unlock()
				if ok {
					ch <- result{resp: it.resp}
					continue
				}
			}
			if c.opts.OnEvent != nil {
				c.opts.OnEvent(it.event)
			}
			if aborted, ok := it.event.AgentSettled(); ok && aborted {
				c.mu.Lock()
				close(c.abortedSettle)
				c.abortedSettle = make(chan struct{})
				c.mu.Unlock()
			}
		}
		if len(batch) == 0 {
			<-c.qready
		}
	}
}

func (c *Client) finish() {
	c.mu.Lock()
	c.ended = true
	err := fmt.Errorf("%w: %w", ErrStreamEnded, c.endErr)
	for id, ch := range c.pending {
		ch <- result{err: err}
		delete(c.pending, id)
	}
	c.mu.Unlock()
	close(c.done)
}

func (c *Client) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(data)
	return err
}

// Call sends cmd and waits for its response or for ctx to end. A response with success:false returns the response
// together with a *CommandError. Every call needs a deadline: pi may never answer (for example abort while a handler
// hangs).
func (c *Client) Call(ctx context.Context, cmd Command) (*Response, error) {
	typ, _ := cmd["type"].(string)
	if typ == "" {
		return nil, errors.New("pirpc: command without type")
	}
	out := make(Command, len(cmd)+1)
	for k, v := range cmd {
		out[k] = v
	}
	ch := make(chan result, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if c.ended {
		err := fmt.Errorf("%w: %w", ErrStreamEnded, c.endErr)
		c.mu.Unlock()
		return nil, err
	}
	id, ok := out["id"].(string)
	if !ok || id == "" {
		c.nextID++
		id = c.opts.IDPrefix + strconv.FormatUint(c.nextID, 10)
		out["id"] = id
	}
	if _, dup := c.pending[id]; dup {
		c.mu.Unlock()
		return nil, fmt.Errorf("pirpc: request id %q is already pending", id)
	}
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(out); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("向 pi 发送 %s 失败：%w", typ, err)
	}
	select {
	case r := <-ch:
		return r.unpack()
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		// The response may have been delivered in the meantime.
		select {
		case r := <-ch:
			return r.unpack()
		default:
		}
		return nil, fmt.Errorf("pi 没有在期限内回应 %s：%w", typ, ctx.Err())
	}
}

func (r result) unpack() (*Response, error) {
	if r.err != nil {
		return nil, r.err
	}
	if !r.resp.Success {
		return r.resp, &CommandError{Command: r.resp.Command, Message: r.resp.Error}
	}
	return r.resp, nil
}

// State is the part of the get_state data the daemon uses.
type State struct {
	SessionID           string `json:"sessionId"`
	SessionFile         string `json:"sessionFile"`
	SessionName         string `json:"sessionName"`
	IsStreaming         bool   `json:"isStreaming"`
	IsCompacting        bool   `json:"isCompacting"`
	MessageCount        int    `json:"messageCount"`
	PendingMessageCount int    `json:"pendingMessageCount"`
	Model               *struct {
		ID       string `json:"id"`
		Provider string `json:"provider"`
	} `json:"model"`
}

// GetState sends get_state. id may be empty for a generated id (DESIGN 5.3 uses "boot" for the first one).
func (c *Client) GetState(ctx context.Context, id string) (*State, error) {
	cmd := Command{"type": "get_state"}
	if id != "" {
		cmd["id"] = id
	}
	resp, err := c.Call(ctx, cmd)
	if err != nil {
		return nil, err
	}
	var st State
	if err := resp.Decode(&st); err != nil {
		return nil, fmt.Errorf("无法解析 get_state 的响应：%w", err)
	}
	return &st, nil
}

// Prompt sends a prompt and returns the disposition ("started", "queued" or "handled").
func (c *Client) Prompt(ctx context.Context, message string) (string, error) {
	resp, err := c.Call(ctx, Command{"type": "prompt", "message": message})
	if err != nil {
		return "", err
	}
	var d struct {
		Disposition string `json:"disposition"`
	}
	if err := resp.Decode(&d); err != nil {
		return "", fmt.Errorf("无法解析 prompt 的响应：%w", err)
	}
	return d.Disposition, nil
}

// Abort sends abort and returns when pi answered it or when an agent_settled record with aborted:true was dispatched
// after Abort was called, whichever comes first. pi answers abort only once the session is idle; an extension may
// start the next run from its agent_settled handler (bearbot-im dispatches a queued turn there), and then the answer
// comes only after that run ended. The settled record already shows that the aborted run ended (contracts 4.2).
func (c *Client) Abort(ctx context.Context) error {
	c.mu.Lock()
	settled := c.abortedSettle
	c.mu.Unlock()
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	answered := make(chan error, 1)
	go func() {
		_, err := c.Call(callCtx, Command{"type": "abort"})
		answered <- err
	}()
	select {
	case err := <-answered:
		return err
	case <-settled:
		return nil
	}
}

// CompactionResult is the data of a successful compact.
type CompactionResult struct {
	Summary              string `json:"summary"`
	FirstKeptEntryID     string `json:"firstKeptEntryId"`
	TokensBefore         int    `json:"tokensBefore"`
	EstimatedTokensAfter *int   `json:"estimatedTokensAfter"`
}

// Compact sends compact with optional custom instructions.
func (c *Client) Compact(ctx context.Context, instructions string) (*CompactionResult, error) {
	cmd := Command{"type": "compact"}
	if instructions != "" {
		cmd["customInstructions"] = instructions
	}
	resp, err := c.Call(ctx, cmd)
	if err != nil {
		return nil, err
	}
	var res CompactionResult
	if err := resp.Decode(&res); err != nil {
		return nil, fmt.Errorf("无法解析 compact 的响应：%w", err)
	}
	return &res, nil
}
