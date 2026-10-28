package proc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Rotation settings of every log file written through LogWriter.
const (
	LogMaxSizeMB  = 50
	LogMaxBackups = 5
)

// MaxRingLineBytes is the longest line kept in a Ring; longer lines are cut and end with "…".
const MaxRingLineBytes = 16 * 1024

// Ring keeps the last lines written to it. It is safe for concurrent use.
type Ring struct {
	mu      sync.Mutex
	lines   []string
	next    int
	full    bool
	partial []byte
}

// NewRing returns a Ring that keeps the last n complete lines.
func NewRing(n int) *Ring {
	if n <= 0 {
		panic("proc.NewRing: n must be positive")
	}
	return &Ring{lines: make([]string, n)}
}

// Write splits p into lines. It never fails.
func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rest := p
	for len(rest) > 0 {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			r.appendPartial(rest)
			break
		}
		r.appendPartial(rest[:i])
		r.push(string(r.partial))
		r.partial = r.partial[:0]
		rest = rest[i+1:]
	}
	return len(p), nil
}

func (r *Ring) appendPartial(b []byte) {
	room := MaxRingLineBytes - len(r.partial)
	if room <= 0 {
		return
	}
	if len(b) > room {
		r.partial = append(r.partial, b[:room]...)
		r.partial = append(r.partial, "…"...)
		return
	}
	r.partial = append(r.partial, b...)
}

func (r *Ring) push(line string) {
	r.lines[r.next] = line
	r.next = (r.next + 1) % len(r.lines)
	if r.next == 0 {
		r.full = true
	}
}

// Lines returns the last n lines, oldest first; n <= 0 returns all kept lines. An unterminated last line is
// included.
func (r *Ring) Lines(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []string
	if r.full {
		all = append(all, r.lines[r.next:]...)
		all = append(all, r.lines[:r.next]...)
	} else {
		all = append(all, r.lines[:r.next]...)
	}
	if len(r.partial) > 0 {
		all = append(all, string(r.partial))
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}

// LogWriter writes a log file with size based rotation (lumberjack), keeps the last lines in memory, and delivers
// new lines to subscribers. It is safe for concurrent use.
type LogWriter struct {
	mu     sync.Mutex
	file   *lumberjack.Logger
	ring   *Ring
	subs   map[*Subscription]struct{}
	carry  []byte
	closed bool
}

// NewLogWriter opens (lazily, on the first write) the log file at path. ringLines is the number of lines kept in
// memory. New files get mode 0600; the directory must exist or is created with mode 0755.
func NewLogWriter(path string, ringLines int) *LogWriter {
	return &LogWriter{
		file: &lumberjack.Logger{Filename: path, MaxSize: LogMaxSizeMB, MaxBackups: LogMaxBackups},
		ring: NewRing(ringLines),
		subs: make(map[*Subscription]struct{}),
	}
}

// Path returns the log file path.
func (w *LogWriter) Path() string { return w.file.Filename }

// Write appends p to the file and to the in-memory lines.
func (w *LogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	w.ring.Write(p)
	w.deliver(p)
	return w.file.Write(p)
}

// WriteRecord appends v as one JSON line (for the .jsonl logs).
func (w *LogWriter) WriteRecord(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

func (w *LogWriter) deliver(p []byte) {
	if len(w.subs) == 0 {
		w.carry = w.carry[:0]
		return
	}
	data := append(w.carry, p...)
	last := bytes.LastIndexByte(data, '\n')
	if last < 0 {
		w.carry = data
		return
	}
	lines := strings.Split(string(data[:last]), "\n")
	w.carry = append([]byte{}, data[last+1:]...)
	for s := range w.subs {
		s.push(lines)
	}
}

// Tail returns the last n lines kept in memory (n <= 0: all).
func (w *LogWriter) Tail(n int) []string { return w.ring.Lines(n) }

// Close closes the file and ends all subscriptions.
func (w *LogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	for s := range w.subs {
		s.end()
	}
	w.subs = nil
	return w.file.Close()
}

// ErrSubscriptionClosed is returned by Subscription.Next after Close.
var ErrSubscriptionClosed = errors.New("log subscription closed")

// Subscription receives the complete lines written after Subscribe was called.
type Subscription struct {
	w      *LogWriter
	mu     sync.Mutex
	queue  []string
	notify chan struct{}
	closed bool
}

// Subscribe starts delivering new complete lines. The queue is unbounded, so writing never waits for subscribers.
func (w *LogWriter) Subscribe() *Subscription {
	s := &Subscription{w: w, notify: make(chan struct{}, 1)}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		s.closed = true
		return s
	}
	w.subs[s] = struct{}{}
	return s
}

func (s *Subscription) push(lines []string) {
	s.mu.Lock()
	s.queue = append(s.queue, lines...)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *Subscription) end() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Next returns the next line. It blocks until a line arrives, ctx ends (ctx.Err()) or the subscription or the
// writer is closed (ErrSubscriptionClosed, after the queued lines were returned).
func (s *Subscription) Next(ctx context.Context) (string, error) {
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			line := s.queue[0]
			s.queue = s.queue[1:]
			s.mu.Unlock()
			return line, nil
		}
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return "", ErrSubscriptionClosed
		}
		select {
		case <-s.notify:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// Close stops the subscription.
func (s *Subscription) Close() {
	s.w.mu.Lock()
	delete(s.w.subs, s)
	s.w.mu.Unlock()
	s.end()
}

// TailFile returns the last n lines of the file at path (n <= 0: all lines). A missing file has no lines.
func TailFile(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	if n <= 0 {
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, err
		}
		return splitLines(data), nil
	}
	const block = 64 * 1024
	var buf []byte
	pos := size
	for pos > 0 {
		step := int64(block)
		if pos < step {
			step = pos
		}
		pos -= step
		chunk := make([]byte, step)
		if _, err := f.ReadAt(chunk, pos); err != nil {
			return nil, err
		}
		buf = append(chunk, buf...)
		// One more newline than n is needed when the file ends with a newline.
		if bytes.Count(buf, []byte{'\n'}) > n {
			break
		}
	}
	lines := splitLines(buf)
	if pos > 0 && len(lines) > 0 {
		// Reading starts at a block offset, so the first line may be incomplete.
		lines = lines[1:]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

func splitLines(data []byte) []string {
	s := strings.TrimSuffix(string(data), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
