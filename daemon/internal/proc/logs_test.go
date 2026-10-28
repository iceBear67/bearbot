package proc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRing(t *testing.T) {
	r := NewRing(3)
	fmt.Fprint(r, "a\nb\n")
	if got := r.Lines(0); strings.Join(got, "|") != "a|b" {
		t.Fatalf("lines = %q", got)
	}
	fmt.Fprint(r, "c\nd\ne")
	if got := r.Lines(0); strings.Join(got, "|") != "b|c|d|e" {
		t.Fatalf("lines = %q", got)
	}
	if got := r.Lines(2); strings.Join(got, "|") != "d|e" {
		t.Fatalf("last 2 = %q", got)
	}
	fmt.Fprint(r, "f\n")
	if got := r.Lines(0); strings.Join(got, "|") != "c|d|ef" {
		t.Fatalf("lines = %q", got)
	}
	long := strings.Repeat("x", MaxRingLineBytes+100)
	fmt.Fprint(r, long+"\n")
	last := r.Lines(1)[0]
	if len(last) != MaxRingLineBytes+len("…") || !strings.HasSuffix(last, "…") {
		t.Fatalf("long line kept with length %d", len(last))
	}
}

func TestLogWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "x.log")
	w := NewLogWriter(path, 2)
	sub := w.Subscribe()
	if _, err := fmt.Fprint(w, "one\ntw"); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprint(w, "o\nthree\n"); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteRecord(map[string]any{"type": "x", "n": 1}); err != nil {
		t.Fatal(err)
	}
	if got := w.Tail(0); strings.Join(got, "|") != `three|{"n":1,"type":"x"}` {
		t.Fatalf("tail = %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got []string
	for range 4 {
		line, err := sub.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, line)
	}
	if strings.Join(got, "|") != `one|two|three|{"n":1,"type":"x"}` {
		t.Fatalf("subscription = %q", got)
	}
	short, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	if _, err := sub.Next(short); err != context.DeadlineExceeded {
		t.Fatalf("Next without data = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := sub.Next(ctx); err != ErrSubscriptionClosed {
		t.Fatalf("Next after close = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one\ntwo\nthree\n{\"n\":1,\"type\":\"x\"}\n" {
		t.Fatalf("file = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode())
	}
	lines, err := TailFile(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, "|") != `three|{"n":1,"type":"x"}` {
		t.Fatalf("TailFile = %q", lines)
	}
}

func TestTailFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.log")
	var b strings.Builder
	for i := range 50000 {
		fmt.Fprintf(&b, "line %06d %s\n", i, strings.Repeat("y", i%40))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, err := TailFile(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "line 049997 ") || !strings.HasPrefix(lines[2], "line 049999 ") {
		t.Fatalf("lines = %q", lines)
	}
	lines, err = TailFile(path, 20000)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 20000 || !strings.HasPrefix(lines[0], "line 030000 ") {
		t.Fatalf("got %d lines, first %q", len(lines), lines[0])
	}
	all, err := TailFile(path, 0)
	if err != nil || len(all) != 50000 {
		t.Fatalf("all = %d, %v", len(all), err)
	}
	none, err := TailFile(filepath.Join(dir, "missing.log"), 10)
	if err != nil || none != nil {
		t.Fatalf("missing = %v, %v", none, err)
	}
	if err := os.WriteFile(path, []byte("no newline"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, err = TailFile(path, 5)
	if err != nil || len(lines) != 1 || lines[0] != "no newline" {
		t.Fatalf("lines = %q, %v", lines, err)
	}
}
