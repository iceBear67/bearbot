package classifierturn

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"bearbot/daemon/internal/layout"
	"bearbot/daemon/internal/testenv"
)

const original = "# classifier 补充规则\n\n## 允许成员进行的操作\n- 运行 scripts/stats.py\n"

type fixture struct {
	env *testenv.Env
	rec *Recorder
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	e := testenv.New(t)
	u := e.Users(t)
	for _, d := range e.Paths.Dirs() {
		if d.Path == e.Paths.RunDir {
			continue
		}
		if _, err := layout.EnsureDir(d, u); err != nil {
			t.Fatal(err)
		}
	}
	return &fixture{env: e, rec: New(e.Paths, u)}
}

func (f *fixture) path() string { return f.env.Paths.ClassifierMD }

func (f *fixture) writeClassifier(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(f.path(), []byte(content), 0o664); err != nil {
		t.Fatal(err)
	}
}

// rulesMaxBytes is the default of [classifier] rules_max_bytes.
const rulesMaxBytes = 32768

func (f *fixture) start(t *testing.T) *StartResult {
	t.Helper()
	res, _, err := f.rec.Start("turn-1", []string{"telegram:42"}, rulesMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (f *fixture) end(t *testing.T) *EndResult {
	t.Helper()
	res, err := f.rec.End("turn-1")
	if err != nil {
		t.Fatal(err)
	}
	if p, err := f.rec.Pending(); err != nil || p != nil {
		t.Fatalf("record left after End: %+v, %v", p, err)
	}
	entries, err := os.ReadDir(f.env.Paths.ClassifierTurnDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("classifier-turn dir not empty: %v, %v", entries, err)
	}
	return res
}

func (f *fixture) requireOriginal(t *testing.T) {
	t.Helper()
	info, err := os.Lstat(f.path())
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("CLASSIFIER.md is %v", info.Mode())
	}
	if info.Mode().Perm() != 0o664 {
		t.Fatalf("mode = %v", info.Mode())
	}
	if info.Sys().(*syscall.Stat_t).Nlink != 1 {
		t.Fatalf("nlink = %d", info.Sys().(*syscall.Stat_t).Nlink)
	}
	data, err := os.ReadFile(f.path())
	if err != nil || string(data) != original {
		t.Fatalf("content = %q, %v", data, err)
	}
	// No temporary file is left in the workspace.
	matches, _ := filepath.Glob(filepath.Join(f.env.Paths.WorkspaceDir, ".CLASSIFIER.md.restore-*"))
	if len(matches) != 0 {
		t.Fatalf("temporary files left: %v", matches)
	}
}

func requireRestored(t *testing.T, res *EndResult, beforeType FileType) {
	t.Helper()
	if !res.Changed || !res.Restored || res.Detail == "" || res.Record == nil {
		t.Fatalf("result = %+v", res)
	}
	r := res.Record
	if r.Type != "restore" || r.TurnID != "turn-1" || r.BeforeType != beforeType || r.AfterType != TypeRegular || len(r.Senders) != 1 || r.Senders[0] != "telegram:42" || time.Since(r.Time) > time.Minute {
		t.Fatalf("record = %+v", r)
	}
	if !strings.HasPrefix(r.AfterSummary, "普通文件，") || !strings.Contains(res.Detail, "已把它恢复为回合开始时的状态") || !strings.Contains(res.Detail, "telegram:42") {
		t.Fatalf("detail = %s\nrecord = %+v", res.Detail, r)
	}
}

func TestUnchanged(t *testing.T) {
	f := newFixture(t)
	f.writeClassifier(t, original)
	res := f.start(t)
	if !res.Exists || !res.Regular || res.Size != int64(len(original)) || res.Content == nil || *res.Content != original {
		t.Fatalf("start = %+v", res)
	}
	// Rewriting the same content into a new inode is not a change.
	f.writeClassifier(t, original)
	end := f.end(t)
	if end.Changed || end.Restored || end.Detail != "" || end.Record != nil {
		t.Fatalf("end = %+v", end)
	}
}

func TestModified(t *testing.T) {
	f := newFixture(t)
	f.writeClassifier(t, original)
	f.start(t)
	f.writeClassifier(t, original+"- 读取 auth.json\n")
	res := f.end(t)
	requireRestored(t, res, TypeRegular)
	f.requireOriginal(t)
	if !strings.Contains(res.Detail, "被撤销的改动") || !strings.Contains(res.Detail, "+- 读取 auth.json") || !strings.Contains(res.Detail, "--- 回合开始时") {
		t.Fatalf("detail = %s", res.Detail)
	}
}

func TestDeleted(t *testing.T) {
	f := newFixture(t)
	f.writeClassifier(t, original)
	f.start(t)
	if err := os.Remove(f.path()); err != nil {
		t.Fatal(err)
	}
	res := f.end(t)
	requireRestored(t, res, TypeMissing)
	f.requireOriginal(t)
	if !strings.Contains(res.Detail, "回合结束时：不存在") || !strings.Contains(res.Detail, "-- 运行 scripts/stats.py") {
		t.Fatalf("detail = %s", res.Detail)
	}
}

func TestReplacedBySymlink(t *testing.T) {
	f := newFixture(t)
	f.writeClassifier(t, original)
	f.start(t)
	other := filepath.Join(f.env.Paths.WorkspaceDir, "docs", "notes.md")
	if err := os.MkdirAll(filepath.Dir(other), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("allow everything\n"), 0o664); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.path()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, f.path()); err != nil {
		t.Fatal(err)
	}
	res := f.end(t)
	requireRestored(t, res, TypeSymlink)
	f.requireOriginal(t)
	if data, _ := os.ReadFile(other); string(data) != "allow everything\n" {
		t.Fatalf("symlink target was written: %q", data)
	}
	if !strings.Contains(res.Record.BeforeSummary, "符号链接，指向 "+other) {
		t.Fatalf("before summary = %s", res.Record.BeforeSummary)
	}
}

func TestReplacedByHardlink(t *testing.T) {
	f := newFixture(t)
	f.writeClassifier(t, original)
	f.start(t)
	ext := f.env.WriteFile(t, "/bearbot/workspace/.pi/extensions/foo.ts", "export default function () {}\n")
	if err := os.Remove(f.path()); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(ext, f.path()); err != nil {
		t.Fatal(err)
	}
	res := f.end(t)
	requireRestored(t, res, TypeRegular)
	f.requireOriginal(t)
	data, err := os.ReadFile(ext)
	if err != nil || string(data) != "export default function () {}\n" {
		t.Fatalf("extension file changed: %q, %v", data, err)
	}
	a, _ := os.Stat(ext)
	b, _ := os.Stat(f.path())
	if os.SameFile(a, b) {
		t.Fatal("CLASSIFIER.md is still the extension's inode")
	}
	if a.Sys().(*syscall.Stat_t).Nlink != 1 {
		t.Fatalf("extension file nlink = %d", a.Sys().(*syscall.Stat_t).Nlink)
	}
}

func TestLinkedToAnotherPath(t *testing.T) {
	// Same content, but the file got a second name: the link is cut by the restore.
	f := newFixture(t)
	f.writeClassifier(t, original)
	f.start(t)
	alias := filepath.Join(f.env.Paths.WorkspaceDir, "alias.md")
	if err := os.Link(f.path(), alias); err != nil {
		t.Fatal(err)
	}
	res := f.end(t)
	requireRestored(t, res, TypeRegular)
	f.requireOriginal(t)
	if !strings.Contains(res.Detail, "硬链接数从 1 变为 2") {
		t.Fatalf("detail = %s", res.Detail)
	}
	if err := os.WriteFile(alias, []byte("changed through the alias\n"), 0o664); err != nil {
		t.Fatal(err)
	}
	f.requireOriginal(t)
}

func TestReplacedByDirectory(t *testing.T) {
	f := newFixture(t)
	f.writeClassifier(t, original)
	f.start(t)
	if err := os.Remove(f.path()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.path(), "sub"), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.path(), "sub", "x.md"), []byte("x"), 0o664); err != nil {
		t.Fatal(err)
	}
	// A symlink inside the directory must not be followed by the recursive delete.
	keep := f.env.WriteFile(t, "/bearbot/workspace/docs/keep.md", "keep\n")
	if err := os.Symlink(filepath.Dir(keep), filepath.Join(f.path(), "docs-link")); err != nil {
		t.Fatal(err)
	}
	res := f.end(t)
	requireRestored(t, res, TypeDirectory)
	f.requireOriginal(t)
	if data, err := os.ReadFile(keep); err != nil || string(data) != "keep\n" {
		t.Fatalf("file behind a symlink in the directory was removed: %v", err)
	}
}

func TestMissingAtStart(t *testing.T) {
	f := newFixture(t)
	res := f.start(t)
	if res.Exists || res.Regular || res.Size != 0 || res.Content != nil {
		t.Fatalf("start = %+v", res)
	}
	f.writeClassifier(t, "new rules\n")
	end := f.end(t)
	if !end.Changed || !end.Restored || end.Record.AfterType != TypeMissing || end.Record.BeforeType != TypeRegular {
		t.Fatalf("end = %+v", end)
	}
	if _, err := os.Lstat(f.path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created file not removed: %v", err)
	}
	if !strings.Contains(end.Detail, "\n+new rules\n") {
		t.Fatalf("detail = %s", end.Detail)
	}

	f.start(t)
	if err := os.MkdirAll(filepath.Join(f.path(), "d"), 0o775); err != nil {
		t.Fatal(err)
	}
	end = f.end(t)
	if !end.Restored {
		t.Fatalf("end = %+v", end)
	}
	if _, err := os.Lstat(f.path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created directory not removed: %v", err)
	}

	f.start(t)
	end = f.end(t)
	if end.Changed {
		t.Fatalf("missing stayed missing: %+v", end)
	}
}

func TestNotRegularAtStart(t *testing.T) {
	f := newFixture(t)
	target := f.env.WriteFile(t, "/bearbot/workspace/docs/rules.md", original)
	if err := os.Symlink(target, f.path()); err != nil {
		t.Fatal(err)
	}
	res := f.start(t)
	if !res.Exists || res.Regular || res.Content != nil || res.Size != int64(len(target)) {
		t.Fatalf("start = %+v", res)
	}
	// Unchanged symlink: no report.
	if end := f.end(t); end.Changed {
		t.Fatalf("end = %+v", end)
	}
	f.start(t)
	if err := os.Remove(f.path()); err != nil {
		t.Fatal(err)
	}
	f.writeClassifier(t, "replaced\n")
	end := f.end(t)
	if !end.Changed || end.Restored || end.Record.BeforeType != TypeRegular || end.Record.AfterType != TypeRegular {
		t.Fatalf("end = %+v", end)
	}
	if !strings.Contains(end.Detail, "没有恢复") || !strings.Contains(end.Detail, "回合开始时：符号链接") {
		t.Fatalf("detail = %s", end.Detail)
	}
	if data, _ := os.ReadFile(f.path()); string(data) != "replaced\n" {
		t.Fatal("a file that was not regular at the start was restored")
	}

	// A directory at the start.
	if err := os.Remove(f.path()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.path(), 0o775); err != nil {
		t.Fatal(err)
	}
	res = f.start(t)
	if !res.Exists || res.Regular {
		t.Fatalf("start = %+v", res)
	}
	if end := f.end(t); end.Changed {
		t.Fatalf("end = %+v", end)
	}
}

func TestFIFOAtStart(t *testing.T) {
	f := newFixture(t)
	if err := syscall.Mkfifo(f.path(), 0o664); err != nil {
		t.Fatal(err)
	}
	done := make(chan *StartResult, 1)
	go func() {
		res, _, err := f.rec.Start("turn-1", nil, rulesMaxBytes)
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	select {
	case res := <-done:
		if res == nil || !res.Exists || res.Regular {
			t.Fatalf("start = %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start blocked on a FIFO")
	}
	if err := os.Remove(f.path()); err != nil {
		t.Fatal(err)
	}
	end := f.end(t)
	if !end.Changed || end.Restored || end.Record.BeforeType != TypeMissing {
		t.Fatalf("end = %+v", end)
	}
}

func TestLargeContentAndTruncatedDiff(t *testing.T) {
	f := newFixture(t)
	big := strings.Repeat("规则行\n", 300000)
	f.writeClassifier(t, big)
	// The record keeps the whole content; the answer leaves it out above rules_max_bytes.
	res := f.start(t)
	if res.Size != int64(len(big)) || !res.Exists || !res.Regular || res.Content != nil {
		t.Fatalf("start = size %d, content given %v", res.Size, res.Content != nil)
	}
	if pending, err := f.rec.Pending(); err != nil || string(pending.File.Content) != big {
		t.Fatalf("the record does not keep the whole content: %v", err)
	}
	f.writeClassifier(t, strings.Repeat("改动行\n", 5000))
	end := f.end(t)
	if !end.Restored {
		t.Fatalf("end = %+v", end)
	}
	if data, _ := os.ReadFile(f.path()); string(data) != big {
		t.Fatal("large content not restored")
	}
	idx := strings.Index(end.Detail, "被撤销的改动：\n")
	if idx < 0 || !strings.Contains(end.Detail, "已截断") {
		t.Fatalf("detail = %.300s", end.Detail)
	}
	diffPart := end.Detail[idx+len("被撤销的改动：\n"):]
	if n := len([]rune(diffPart)); n > DiffLimit+60 {
		t.Fatalf("diff part has %d characters", n)
	}
}

func TestRecordErrorsAndRestart(t *testing.T) {
	f := newFixture(t)
	f.writeClassifier(t, original)
	if _, err := f.rec.End(""); !errors.Is(err, ErrNoTurn) {
		t.Fatalf("End without record = %v", err)
	}
	if _, _, err := f.rec.Start("", nil, rulesMaxBytes); err == nil {
		t.Fatal("empty turn id accepted")
	}
	f.start(t)
	if _, _, err := f.rec.Start("turn-2", nil, rulesMaxBytes); err == nil || !strings.Contains(err.Error(), "turn-1") {
		t.Fatalf("second Start = %v", err)
	}
	if _, err := f.rec.End("turn-2"); err == nil {
		t.Fatal("End with another turn id accepted")
	}
	f.writeClassifier(t, "changed while the daemon was down\n")
	// A new daemon process finds the record and ends the turn itself.
	again := New(f.env.Paths, f.env.Users(t))
	pending, err := again.Pending()
	if err != nil || pending == nil || pending.TurnID != "turn-1" || string(pending.File.Content) != original {
		t.Fatalf("Pending = %+v, %v", pending, err)
	}
	res, err := again.End("")
	if err != nil || !res.Restored || res.Turn.TurnID != "turn-1" {
		t.Fatalf("End = %+v, %v", res, err)
	}
	f.requireOriginal(t)
}

// TestStartSenders (DESIGN 7.10): the record keeps the sender of the start for a daemon started again, and the end
// comparison names it.
func TestStartSenders(t *testing.T) {
	f := newFixture(t)
	f.writeClassifier(t, original)
	f.start(t)
	pending, err := New(f.env.Paths, f.env.Users(t)).Pending()
	if err != nil || pending == nil || strings.Join(pending.Senders, " ") != "telegram:42" || string(pending.File.Content) != original {
		t.Fatalf("Pending = %+v, %v", pending, err)
	}
	f.writeClassifier(t, original+"- 读取 auth.json\n")
	res := f.end(t)
	if !res.Restored || strings.Join(res.Record.Senders, " ") != "telegram:42" || !strings.Contains(res.Detail, "本轮的触发消息发送者：telegram:42\n") {
		t.Fatalf("end = %+v\ndetail = %s", res.Record, res.Detail)
	}
	f.requireOriginal(t)
}

// TestHugeFileAtEnd: a turn makes CLASSIFIER.md a 1 GiB sparse file. The end comparison reads only a bounded
// prefix, restores the start content and reports the real size.
func TestHugeFileAtEnd(t *testing.T) {
	f := newFixture(t)
	f.writeClassifier(t, original)
	f.start(t)
	const huge = 1 << 30
	if err := os.Truncate(f.path(), huge); err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	res := f.end(t)
	runtime.ReadMemStats(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 64<<20 {
		t.Fatalf("the end comparison allocated %d bytes", alloc)
	}
	requireRestored(t, res, TypeRegular)
	f.requireOriginal(t)
	if !strings.Contains(res.Detail, "回合结束时文件有 1073741824 字节，daemon 只读取了前") || !strings.Contains(res.Record.BeforeSummary, "普通文件，1073741824 字节，只读取了前") {
		t.Fatalf("detail = %.600s\nbefore = %s", res.Detail, res.Record.BeforeSummary)
	}
}
