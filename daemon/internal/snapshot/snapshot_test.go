package snapshot

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bearbot/daemon/internal/layout"
	"bearbot/daemon/internal/testenv"
)

var ctx = context.Background()

// TestMain runs HelperCommand when the repository starts this test binary as its helper (Repo.self is the running
// executable, as bearbotd is in production).
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == HelperCommand {
		if err := RunHelper(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type fixture struct {
	env  *testenv.Env
	repo *Repo
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
	repo, err := Open(e.Paths, u)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{env: e, repo: repo}
}

func (f *fixture) write(t *testing.T, rel, content string) {
	t.Helper()
	f.env.WriteFile(t, "/bearbot/"+rel, content)
}

func (f *fixture) read(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.env.Paths.SnapshotWorkTree, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (f *fixture) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(f.env.Paths.SnapshotWorkTree, rel))
	return err == nil
}

func (f *fixture) tracked(t *testing.T, ref string) []string {
	t.Helper()
	commit, err := f.repo.Resolve(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.repo.run(ctx, "ls-tree", "-r", "--name-only", "--end-of-options", commit)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(out))
}

func TestValidateRef(t *testing.T) {
	good := []string{
		"init", "init^", "apply/1", "apply/12^", "incident/inc-20261027083000/before", "incident/inc-20261027083000/attempt-2",
		"incident/inc-20261027083000/attempt-2^", "rollback/3/before", "rollback/3/after^", "resume/1", "last-good", "last-good^",
		strings.Repeat("a", 40), "0123456789abcdefABCDEF0123456789abcdef01",
	}
	bad := []string{
		"", "-x", "--help", "apply/1;rm", "apply/1;rm -rf /", "apply/", "apply/x", "apply/1^^", "apply/1~1", "apply/-1",
		"incident/inc-123/before", "incident/inc-20261027083000", "incident/inc-20261027083000/attempt-", "rollback/1",
		"rollback/1/middle", "resume", "HEAD", "main", "refs/tags/init", " init", "init ", "init\n", "../init",
		strings.Repeat("a", 39), strings.Repeat("a", 41), strings.Repeat("g", 40), "apply/1..apply/2", "init^{tree}",
		"last-good@{1}", "--end-of-options", "-x^",
	}
	for _, r := range good {
		if err := ValidateRef(r); err != nil {
			t.Errorf("ValidateRef(%q) = %v", r, err)
		}
	}
	for _, r := range bad {
		if err := ValidateRef(r); err == nil {
			t.Errorf("ValidateRef(%q) accepted", r)
		}
	}
}

func TestInitAndTrackedPaths(t *testing.T) {
	f := newFixture(t)
	f.write(t, "workspace/AGENTS.md", "memory\n")
	f.write(t, "workspace/CLASSIFIER.md", "rules\n")
	f.write(t, "workspace/.pi/extensions/a/index.ts", "export default function () {}\n")
	f.write(t, "workspace/.pi/extensions/a/node_modules/dep/index.js", "x\n")
	f.write(t, "workspace/.pi/mcp.json", "{}\n")
	f.write(t, "workspace/.pi/debug.log", "log\n")
	f.write(t, "workspace/.pi/cache.db", "db\n")
	f.write(t, "workspace/docs/readme.md", "doc\n")
	f.write(t, "workspace/inbox/2026-10-27/x.pdf", "pdf\n")
	f.write(t, "workspace/notes.txt", "notes\n")
	f.write(t, "profile/agent/settings.json", "{}\n")
	f.write(t, "profile/agent/models.json", "{}\n")
	f.write(t, "profile/agent/auth.json", "{\"secret\":1}\n")
	f.write(t, "profile/agent/mcp-auth.json", "{}\n")
	f.write(t, "profile/agent/sessions/--x--/s.jsonl", "{}\n")
	f.write(t, "config/bearbot.toml.bak", "x\n")

	created, err := f.repo.Init(ctx)
	if err != nil || !created {
		t.Fatalf("Init = %v, %v", created, err)
	}
	want := []string{
		"profile/agent/models.json", "profile/agent/settings.json", "workspace/.pi/extensions/a/index.ts",
		"workspace/.pi/mcp.json", "workspace/AGENTS.md", "workspace/CLASSIFIER.md",
	}
	if got := f.tracked(t, InitTag); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("tracked = %v, want %v", got, want)
	}
	data, err := os.ReadFile(filepath.Join(f.env.Paths.SnapshotsGit, "info", "exclude"))
	if err != nil || string(data) != Exclude {
		t.Fatalf("exclude = %q, %v", data, err)
	}
	data, err = os.ReadFile(filepath.Join(f.env.Paths.SnapshotsGit, "info", "attributes"))
	if err != nil || string(data) != Attributes {
		t.Fatalf("attributes = %q, %v", data, err)
	}
	if bare, err := f.repo.run(ctx, "config", "--get", "core.bare"); err != nil || strings.TrimSpace(string(bare)) != "true" {
		t.Fatalf("core.bare = %q, %v", bare, err)
	}
	info, err := os.Stat(f.env.Paths.SnapshotsGit)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("git dir mode = %v, %v", info.Mode(), err)
	}
	// Everything in the repository must be readable by other users (bearbot reads it with git log).
	err = filepath.WalkDir(f.env.Paths.SnapshotsGit, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o004 == 0 || (d.IsDir() && info.Mode().Perm()&0o001 == 0) {
			t.Errorf("%s is not readable by others: %v", path, info.Mode())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err = f.repo.Init(ctx)
	if err != nil || created {
		t.Fatalf("second Init = %v, %v", created, err)
	}
}

func commit(t *testing.T, f *fixture, tag, summary string) string {
	t.Helper()
	c, err := f.repo.Commit(ctx, tag, Message{Summary: summary, Requester: "telegram:1", Thread: "telegram:-100:7"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCommitsRefsAndList(t *testing.T) {
	f := newFixture(t)
	f.write(t, "workspace/.pi/extensions/a.ts", "v1\n")
	if _, err := f.repo.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.repo.NextApply(ctx); n != 1 {
		t.Fatalf("NextApply = %d", n)
	}
	if _, err := f.repo.DefaultRollbackTarget(ctx); err == nil {
		t.Fatal("default target without applies")
	}
	f.write(t, "workspace/.pi/extensions/a.ts", "v2\n")
	a1 := commit(t, f, ApplyTag(1), "第一次应用\n\n详细说明")
	f.write(t, "workspace/.pi/extensions/b.ts", "b\n")
	commit(t, f, ApplyTag(2), "第二次应用")
	inc := "inc-20261027083000"
	commit(t, f, IncidentBeforeTag(inc), "故障")
	if n, _ := f.repo.NextAttempt(ctx, inc); n != 1 {
		t.Fatalf("NextAttempt = %d", n)
	}
	commit(t, f, IncidentAttemptTag(inc, 1), "修复 1")
	if n, _ := f.repo.NextAttempt(ctx, inc); n != 2 {
		t.Fatalf("NextAttempt = %d", n)
	}
	if n, _ := f.repo.NextRollback(ctx); n != 1 {
		t.Fatalf("NextRollback = %d", n)
	}
	commit(t, f, RollbackBeforeTag(1), "回滚之前")
	after := commit(t, f, RollbackAfterTag(1), "回滚之后")
	if n, _ := f.repo.NextRollback(ctx); n != 2 {
		t.Fatalf("NextRollback = %d", n)
	}
	commit(t, f, ResumeTag(1), "恢复运行")
	if n, _ := f.repo.NextResume(ctx); n != 2 {
		t.Fatalf("NextResume = %d", n)
	}
	f.write(t, "workspace/.pi/extensions/c.ts", "c\n")
	commit(t, f, ApplyTag(3), "第三次应用")
	if n, _ := f.repo.NextApply(ctx); n != 4 {
		t.Fatalf("NextApply = %d", n)
	}

	if _, err := f.repo.Commit(ctx, ApplyTag(3), Message{Summary: "again"}); err == nil || !strings.Contains(err.Error(), "已经存在") {
		t.Fatalf("duplicate tag: %v", err)
	}
	for _, bad := range []string{"apply/x", LastGoodTag, "-x", "apply/4^"} {
		if _, err := f.repo.Commit(ctx, bad, Message{Summary: "x"}); err == nil {
			t.Fatalf("Commit(%q) accepted", bad)
		}
	}
	if _, err := f.repo.Commit(ctx, ApplyTag(4), Message{Summary: "  "}); err == nil {
		t.Fatal("empty summary accepted")
	}

	got, err := f.repo.Resolve(ctx, ApplyTag(1))
	if err != nil || got != a1 {
		t.Fatalf("Resolve(apply/1) = %s, %v", got, err)
	}
	if got, err := f.repo.Resolve(ctx, a1); err != nil || got != a1 {
		t.Fatalf("Resolve(hex) = %s, %v", got, err)
	}
	if got, err := f.repo.Resolve(ctx, strings.ToUpper(a1)); err != nil || got != a1 {
		t.Fatalf("Resolve(HEX) = %s, %v", got, err)
	}
	parent, err := f.repo.Resolve(ctx, ApplyTag(1)+"^")
	if err != nil {
		t.Fatal(err)
	}
	if init, _ := f.repo.Resolve(ctx, InitTag); parent != init {
		t.Fatalf("apply/1^ = %s, want init %s", parent, init)
	}
	for _, missing := range []string{"apply/9", "init^", "resume/7", strings.Repeat("0", 40)} {
		if _, err := f.repo.Resolve(ctx, missing); err == nil || !strings.Contains(err.Error(), "不存在") {
			t.Errorf("Resolve(%q) = %v", missing, err)
		}
	}
	if _, err := f.repo.Resolve(ctx, "-x"); err == nil || !strings.Contains(err.Error(), "无效的快照引用") {
		t.Fatalf("Resolve(-x) = %v", err)
	}

	if target, err := f.repo.DefaultRollbackTarget(ctx); err != nil || target != "apply/3^" {
		t.Fatalf("DefaultRollbackTarget = %q, %v", target, err)
	}
	reverted, err := f.repo.AppliesAfter(ctx, ApplyTag(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(reverted) != 2 || reverted[0].Ref != "apply/2" || reverted[0].Summary != "第二次应用" || reverted[1].Ref != "apply/3" {
		t.Fatalf("AppliesAfter(apply/1) = %+v", reverted)
	}
	if reverted, _ := f.repo.AppliesAfter(ctx, "apply/3^"); len(reverted) != 1 || reverted[0].Ref != "apply/3" {
		t.Fatalf("AppliesAfter(apply/3^) = %+v", reverted)
	}
	if reverted, _ := f.repo.AppliesAfter(ctx, ApplyTag(3)); len(reverted) != 0 {
		t.Fatalf("AppliesAfter(apply/3) = %+v", reverted)
	}

	if _, ok, _ := f.repo.LastGood(ctx); ok {
		t.Fatal("last-good exists before it was moved")
	}
	if err := f.repo.MoveLastGood(ctx, ApplyTag(2)); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.MoveLastGood(ctx, RollbackAfterTag(1)); err != nil {
		t.Fatal(err)
	}
	if lg, ok, err := f.repo.LastGood(ctx); err != nil || !ok || lg != after {
		t.Fatalf("LastGood = %s, %v, %v", lg, ok, err)
	}
	if name, _ := f.repo.NameOf(ctx, after); name != "rollback/1/after" {
		t.Fatalf("NameOf = %s", name)
	}
	head, err := f.repo.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	l, err := f.repo.List(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, e := range l.Entries {
		refs = append(refs, e.Ref)
	}
	wantRefs := []string{"apply/3", "resume/1", "rollback/1/after", "rollback/1/before", "incident/inc-20261027083000/attempt-1", "incident/inc-20261027083000/before", "apply/2", "apply/1", "init"}
	if strings.Join(refs, " ") != strings.Join(wantRefs, " ") {
		t.Fatalf("list = %v", refs)
	}
	e1 := l.Entries[7]
	if e1.Summary != "第一次应用" || e1.Requester != "telegram:1" || e1.Thread != "telegram:-100:7" || time.Since(e1.Time) > time.Minute {
		t.Fatalf("apply/1 entry = %+v", e1)
	}
	if l.Entries[8].Requester != "" {
		t.Fatalf("init entry = %+v", l.Entries[8])
	}
	if l.LastGood != "rollback/1/after" || l.Running != "apply/3" || l.DefaultTarget != "apply/3^" {
		t.Fatalf("listing = %+v", l)
	}
	text := l.Text()
	for _, want := range []string{"最近的 9 个快照", "apply/1", "第一次应用", "发起者：telegram:1", "last-good：rollback/1/after", "runningSnapshot：apply/3", "默认目标：apply/3^"} {
		if !strings.Contains(text, want) {
			t.Errorf("text does not contain %q:\n%s", want, text)
		}
	}
	unnamed, err := f.repo.List(ctx, strings.Repeat("e", 40))
	if err != nil || unnamed.Running != strings.Repeat("e", 40) {
		t.Fatalf("running without a name = %+v, %v", unnamed, err)
	}

	for i := 4; i < 30; i++ {
		commit(t, f, ApplyTag(i), "批量")
	}
	l, err = f.repo.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Entries) != ListLimit || l.Entries[0].Ref != "apply/29" || l.Running != "" {
		t.Fatalf("limited list: %d entries, first %s, running %q", len(l.Entries), l.Entries[0].Ref, l.Running)
	}
	if !strings.Contains(l.Text(), "runningSnapshot：没有记录") {
		t.Fatal(l.Text())
	}
}

func TestDiffStat(t *testing.T) {
	f := newFixture(t)
	f.write(t, "workspace/AGENTS.md", "m1\n")
	if _, err := f.repo.Init(ctx); err != nil {
		t.Fatal(err)
	}
	f.write(t, "workspace/.pi/extensions/a.ts", "line1\nline2\n")
	commit(t, f, ApplyTag(1), "一")
	f.write(t, "workspace/AGENTS.md", "m2\n")
	f.write(t, "workspace/.pi/extensions/b.ts", "b\n")
	commit(t, f, ApplyTag(2), "二")

	text, err := f.repo.DiffStat(ctx, ApplyTag(1), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "apply/1^ → apply/1") || !strings.Contains(text, "workspace/.pi/extensions/a.ts | 2 ++") || !strings.Contains(text, "1 file changed") {
		t.Fatalf("diff apply/1 = %q", text)
	}
	text, err = f.repo.DiffStat(ctx, ApplyTag(1), ApplyTag(2))
	if err != nil || !strings.Contains(text, "b.ts") || !strings.Contains(text, "AGENTS.md") || strings.Contains(text, "a.ts") {
		t.Fatalf("diff apply/1 apply/2 = %q, %v", text, err)
	}
	text, err = f.repo.DiffStat(ctx, InitTag, "")
	if err != nil || !strings.Contains(text, "workspace/AGENTS.md") {
		t.Fatalf("diff init = %q, %v", text, err)
	}
	text, err = f.repo.DiffStat(ctx, ApplyTag(2), ApplyTag(2))
	if err != nil || !strings.Contains(text, "没有差别") {
		t.Fatalf("diff same = %q, %v", text, err)
	}
	for _, bad := range [][2]string{{"-x", ""}, {"apply/1;rm", ""}, {ApplyTag(1), "--output=/tmp/x"}, {"apply/9", ""}} {
		if _, err := f.repo.DiffStat(ctx, bad[0], bad[1]); err == nil {
			t.Errorf("DiffStat(%q, %q) accepted", bad[0], bad[1])
		}
	}
	stat, err := f.repo.DiffStatPaths(ctx, ApplyTag(1), ApplyTag(2), DocumentPaths)
	if err != nil || !strings.Contains(stat, "workspace/AGENTS.md") || strings.Contains(stat, "b.ts") {
		t.Fatalf("DiffStatPaths = %q, %v", stat, err)
	}
	stat, err = f.repo.DiffStatPaths(ctx, ApplyTag(2), ApplyTag(2), []string{"workspace/AGENTS.md"})
	if err != nil || stat != "" {
		t.Fatalf("DiffStatPaths same = %q, %v", stat, err)
	}
}

func TestRestore(t *testing.T) {
	f := newFixture(t)
	f.write(t, "workspace/AGENTS.md", "memory before\n")
	f.write(t, "workspace/CLASSIFIER.md", "rules before\n")
	f.write(t, "workspace/.pi/extensions/keep.ts", "keep v1\n")
	f.write(t, "workspace/.pi/extensions/gone.ts", "gone\n")
	f.write(t, "workspace/.pi/settings.json", "{\"a\":1}\n")
	f.write(t, "profile/agent/settings.json", "{\"quietStartup\":true}\n")
	if _, err := f.repo.Init(ctx); err != nil {
		t.Fatal(err)
	}

	// The apply edits, adds and deletes files, adds a models.json and an extension directory.
	f.write(t, "workspace/.pi/extensions/keep.ts", "keep v2\n")
	if err := os.Remove(filepath.Join(f.env.Paths.SnapshotWorkTree, "workspace/.pi/extensions/gone.ts")); err != nil {
		t.Fatal(err)
	}
	f.write(t, "workspace/.pi/extensions/added/index.ts", "added\n")
	f.write(t, "profile/agent/models.json", "{\"providers\":{}}\n")
	f.write(t, "profile/agent/settings.json", "{\"quietStartup\":false}\n")
	f.write(t, "workspace/AGENTS.md", "memory after\n")
	f.write(t, "workspace/CLASSIFIER.md", "rules after\n")
	commit(t, f, ApplyTag(1), "改动")
	// Files written after the last snapshot are removed too.
	f.write(t, "workspace/.pi/extensions/uncommitted.ts", "new\n")
	f.write(t, "workspace/.pi/extensions/added/node_modules/x/index.js", "ignored\n")

	res, err := f.repo.Restore(ctx, "apply/1^", nil)
	if err != nil {
		t.Fatal(err)
	}
	wantChanged := []string{
		"profile/agent/models.json", "profile/agent/settings.json", "workspace/.pi/extensions/added/index.ts",
		"workspace/.pi/extensions/gone.ts", "workspace/.pi/extensions/keep.ts", "workspace/.pi/extensions/uncommitted.ts",
	}
	if strings.Join(res.Restored, " ") != strings.Join(wantChanged, " ") {
		t.Fatalf("restored = %v", res.Restored)
	}
	if f.read(t, "workspace/.pi/extensions/keep.ts") != "keep v1\n" || f.read(t, "workspace/.pi/extensions/gone.ts") != "gone\n" {
		t.Fatal("tracked files not restored")
	}
	if f.exists("workspace/.pi/extensions/added/index.ts") || f.exists("workspace/.pi/extensions/uncommitted.ts") || f.exists("profile/agent/models.json") {
		t.Fatal("files absent from the snapshot were not removed")
	}
	if !f.exists("workspace/.pi/extensions/added/node_modules/x/index.js") {
		t.Fatal("ignored node_modules was removed")
	}
	if f.read(t, "profile/agent/settings.json") != "{\"quietStartup\":true}\n" {
		t.Fatal("profile settings not restored")
	}
	if f.read(t, "workspace/AGENTS.md") != "memory after\n" || f.read(t, "workspace/CLASSIFIER.md") != "rules after\n" {
		t.Fatal("AGENTS.md or CLASSIFIER.md was restored")
	}
	if !strings.Contains(res.Text, "已把 6 个文件恢复到 apply/1^") {
		t.Fatalf("text = %q", res.Text)
	}

	again, err := f.repo.Restore(ctx, "apply/1^", nil)
	if err != nil || len(again.Restored) != 0 || !strings.Contains(again.Text, "没有需要恢复的文件") {
		t.Fatalf("second restore = %+v, %v", again, err)
	}

	// Restore of a single path brings only that path forward.
	res, err = f.repo.Restore(ctx, ApplyTag(1), []string{"workspace/.pi/extensions/keep.ts"})
	if err != nil || strings.Join(res.Restored, " ") != "workspace/.pi/extensions/keep.ts" {
		t.Fatalf("single restore = %+v, %v", res, err)
	}
	if f.read(t, "workspace/.pi/extensions/keep.ts") != "keep v2\n" || !f.exists("workspace/.pi/extensions/gone.ts") {
		t.Fatal("single path restore changed other files")
	}

	for _, bad := range []string{"workspace/AGENTS.md", "workspace/CLASSIFIER.md", "profile/agent/auth.json", "../etc/passwd", "workspace/.pi/../AGENTS.md", "workspace", ".pi/extensions/keep.ts"} {
		if _, err := f.repo.Restore(ctx, InitTag, []string{bad}); err == nil || !strings.Contains(err.Error(), "重启生效路径") {
			t.Errorf("Restore path %q = %v", bad, err)
		}
	}
	// Paths are relative to the work tree (/bearbot); absolute paths are rejected, also inside it.
	for _, bad := range []string{"/etc/passwd", filepath.Join(f.env.Paths.WorkspaceDir, ".pi/extensions/keep.ts"), filepath.Join(f.env.Root, "bearbot/config/bearbot.toml")} {
		if _, err := f.repo.Restore(ctx, InitTag, []string{bad}); err == nil || !strings.Contains(err.Error(), "相对 /bearbot 的路径") {
			t.Errorf("Restore path %q = %v", bad, err)
		}
	}
	if _, err := f.repo.Restore(ctx, "-x", nil); err == nil {
		t.Fatal("restore of -x accepted")
	}
}

func TestRestoreRunsNpm(t *testing.T) {
	f := newFixture(t)
	f.write(t, "workspace/.pi/extensions/plain.ts", "x\n")
	if _, err := f.repo.Init(ctx); err != nil {
		t.Fatal(err)
	}
	// An extension with a local dependency and a lock file (npm ci), and one without a lock file (npm install).
	ext := "workspace/.pi/extensions/withdep"
	f.write(t, ext+"/index.ts", "export default function () {}\n")
	f.write(t, ext+"/localdep/package.json", `{"name": "localdep", "version": "1.0.0"}`+"\n")
	f.write(t, ext+"/package.json", `{"name": "withdep", "version": "1.0.0", "private": true, "dependencies": {"localdep": "file:./localdep"}}`+"\n")
	res, err := f.repo.installDeps(ctx, npmDir{dir: ext, sub: "install"})
	if err != nil {
		t.Fatalf("npm install to create the lock file: %v", err)
	}
	if res != "npm install" || !f.exists(ext+"/package-lock.json") {
		t.Fatalf("first install = %q", res)
	}
	other := "workspace/.pi/extensions/nolock"
	f.write(t, other+"/index.ts", "export default function () {}\n")
	f.write(t, other+"/package.json", `{"name": "nolock", "version": "1.0.0", "private": true}`+"\n")
	commit(t, f, ApplyTag(1), "带依赖的扩展")
	if err := os.RemoveAll(filepath.Join(f.env.Paths.SnapshotWorkTree, ext, "node_modules")); err != nil {
		t.Fatal(err)
	}
	// Change both package.json files, then go back to apply/1.
	f.write(t, ext+"/package.json", `{"name": "withdep", "version": "2.0.0", "private": true}`+"\n")
	f.write(t, other+"/package.json", `{"name": "nolock", "version": "2.0.0", "private": true}`+"\n")
	res2, err := f.repo.Restore(ctx, ApplyTag(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res2.NpmDirs, " ") != other+" "+ext {
		t.Fatalf("npm dirs = %v", res2.NpmDirs)
	}
	if !strings.Contains(res2.Text, "已在 "+ext+" 执行 npm ci") || !strings.Contains(res2.Text, "已在 "+other+" 执行 npm install") {
		t.Fatalf("text = %q", res2.Text)
	}
	if !f.exists(ext + "/node_modules/localdep/package.json") {
		t.Fatal("npm ci did not install the dependency")
	}

	// A package.json whose dependency cannot be installed makes the restore fail with npm's output.
	f.write(t, ext+"/package.json", `{"name": "withdep", "version": "1.0.0", "private": true, "dependencies": {"missing": "file:./does-not-exist.tgz"}}`+"\n")
	if err := os.Remove(filepath.Join(f.env.Paths.SnapshotWorkTree, ext, "package-lock.json")); err != nil {
		t.Fatal(err)
	}
	commit(t, f, ApplyTag(2), "坏依赖")
	f.write(t, ext+"/package.json", `{"name": "withdep", "version": "3.0.0", "private": true}`+"\n")
	_, err = f.repo.Restore(ctx, ApplyTag(2), nil)
	if err == nil || !strings.Contains(err.Error(), "npm install 失败") {
		t.Fatalf("restore with a broken dependency = %v", err)
	}
}

// TestSummaryControlCharacters: a summary with the ASCII record and unit separators is kept as it is and the history
// stays readable (list, applies after a target, describe); a summary or origin with NUL is rejected before git runs.
func TestSummaryControlCharacters(t *testing.T) {
	f := newFixture(t)
	if _, err := f.repo.Init(ctx); err != nil {
		t.Fatal(err)
	}
	odd := "fix\u001e thing\u001f done"
	f.write(t, "workspace/.pi/extensions/a.ts", "a\n")
	commit(t, f, ApplyTag(1), odd)
	f.write(t, "workspace/.pi/extensions/b.ts", "b\n")
	commit(t, f, ApplyTag(2), "second")
	commit(t, f, IncidentBeforeTag("inc-20261027083000"), "故障")

	l, err := f.repo.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(l.Entries) != 4 || l.Entries[2].Ref != "apply/1" || l.Entries[2].Summary != odd || l.Entries[2].Requester != "telegram:1" || l.Entries[2].Thread != "telegram:-100:7" {
		t.Fatalf("list entries = %+v", l.Entries)
	}
	reverted, err := f.repo.AppliesAfter(ctx, InitTag)
	if err != nil {
		t.Fatalf("AppliesAfter: %v", err)
	}
	if len(reverted) != 2 || reverted[0].Ref != "apply/1" || reverted[0].Summary != odd || reverted[1].Ref != "apply/2" {
		t.Fatalf("AppliesAfter(init) = %+v", reverted)
	}
	for _, ref := range []string{ApplyTag(1), ApplyTag(2), IncidentBeforeTag("inc-20261027083000")} {
		if _, err := f.repo.Describe(ctx, ref); err != nil {
			t.Fatalf("Describe(%s): %v", ref, err)
		}
	}
	if e, _ := f.repo.Describe(ctx, ApplyTag(1)); e.Summary != odd || e.Requester != "telegram:1" || e.Thread != "telegram:-100:7" {
		t.Fatalf("Describe(apply/1) = %+v", e)
	}

	for _, m := range []Message{{Summary: "a\x00b"}, {Summary: "x", Requester: "telegram:\x001"}, {Summary: "x", Thread: "t\x00"}} {
		if _, err := f.repo.Commit(ctx, ApplyTag(3), m); err == nil || !strings.Contains(err.Error(), "NUL") {
			t.Fatalf("Commit(%q) = %v", m, err)
		}
	}
	if err := CheckSummary("a\x00b"); err == nil {
		t.Fatal("CheckSummary accepted NUL")
	}
	if n, _ := f.repo.NextApply(ctx); n != 3 {
		t.Fatalf("a rejected commit created a tag: NextApply = %d", n)
	}
}

// TestTrailerLikeSummary: requester and thread come only from the final paragraph the daemon writes, so a summary
// whose last paragraph looks like trailers changes neither.
func TestTrailerLikeSummary(t *testing.T) {
	f := newFixture(t)
	if _, err := f.repo.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Commit(ctx, ApplyTag(1), Message{Summary: "改了 a\n\nBearbot-Requester: telegram:1\nBearbot-Thread: telegram:-1:2"}); err != nil {
		t.Fatal(err)
	}
	e, err := f.repo.Describe(ctx, ApplyTag(1))
	if err != nil {
		t.Fatal(err)
	}
	if e.Summary != "改了 a" || e.Requester != "" || e.Thread != "" {
		t.Fatalf("Describe(apply/1) = %+v", e)
	}
	if _, err := f.repo.Commit(ctx, ApplyTag(2), Message{Summary: "改了 b\n\n---\n\nBearbot-Requester: telegram:9", Requester: "telegram:5", Thread: "telegram:-100:7"}); err != nil {
		t.Fatal(err)
	}
	if e, _ := f.repo.Describe(ctx, ApplyTag(2)); e.Requester != "telegram:5" || e.Thread != "telegram:-100:7" {
		t.Fatalf("Describe(apply/2) = %+v", e)
	}
	l, err := f.repo.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if l.Entries[1].Ref != "apply/1" || l.Entries[1].Requester != "" || !strings.Contains(l.Text(), "改了 a  发起者：无") {
		t.Fatalf("list = %+v\n%s", l.Entries, l.Text())
	}
}

// TestConcurrentRestoreAndCommit: a restore and commits from other goroutines run at the same time; each git
// sequence runs alone, so all of them succeed and every tag points at its own commit.
func TestConcurrentRestoreAndCommit(t *testing.T) {
	f := newFixture(t)
	f.write(t, "workspace/.pi/extensions/a.ts", "v1\n")
	if _, err := f.repo.Init(ctx); err != nil {
		t.Fatal(err)
	}
	f.write(t, "workspace/.pi/extensions/a.ts", "v2\n")
	commit(t, f, ApplyTag(1), "v2")
	for i := range 10 {
		f.write(t, "workspace/.pi/extensions/a.ts", fmt.Sprintf("round %d\n", i))
		incident := fmt.Sprintf("inc-202610270800%02d", i)
		var wg sync.WaitGroup
		errs := make([]error, 3)
		wg.Go(func() { _, errs[0] = f.repo.Restore(ctx, ApplyTag(1), []string{"workspace/.pi/extensions/a.ts"}) })
		wg.Go(func() {
			_, errs[1] = f.repo.Commit(ctx, IncidentBeforeTag(incident), Message{Summary: "故障 " + incident})
		})
		wg.Go(func() {
			_, errs[2] = f.repo.Commit(ctx, ResumeTag(i+1), Message{Summary: fmt.Sprintf("恢复 %d", i+1)})
		})
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v", i, err)
			}
		}
		if e, err := f.repo.Describe(ctx, IncidentBeforeTag(incident)); err != nil || e.Summary != "故障 "+incident {
			t.Fatalf("round %d: %s = %+v, %v", i, IncidentBeforeTag(incident), e, err)
		}
		if e, err := f.repo.Describe(ctx, ResumeTag(i+1)); err != nil || e.Summary != fmt.Sprintf("恢复 %d", i+1) {
			t.Fatalf("round %d: %s = %+v, %v", i, ResumeTag(i+1), e, err)
		}
		if got := f.read(t, "workspace/.pi/extensions/a.ts"); got != "v2\n" {
			t.Fatalf("round %d: a.ts = %q", i, got)
		}
	}
}
