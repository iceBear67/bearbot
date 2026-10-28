package snapshot

import (
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"bearbot/daemon/internal/layout"
	"bearbot/daemon/internal/proc"
)

const pi = "workspace/.pi/"

// gitCmd runs the system git for the test itself (a cloned extension, a repository with a work tree in its
// configuration), without the user's git configuration, and returns its output.
func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@localhost", "-c", "commit.gpgSign=false", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// entries returns the tree entries of the snapshot ref by path.
func (f *fixture) entries(t *testing.T, ref string) map[string]entry {
	t.Helper()
	commit, err := f.repo.Resolve(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.repo.run(ctx, "ls-tree", "-r", "-z", "--full-tree", "--end-of-options", commit)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := parseListing(out, 3)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]entry{}
	for _, row := range rows {
		got[row.path] = entry{mode: row.fields[0], sha: row.fields[2], path: row.path}
	}
	return got
}

func (f *fixture) blob(t *testing.T, sha string) string {
	t.Helper()
	out, err := f.repo.run(ctx, "cat-file", "blob", sha)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func (f *fixture) abs(rel string) string { return filepath.Join(f.env.Paths.SnapshotWorkTree, rel) }

func (f *fixture) symlink(t *testing.T, target, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.abs(rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, f.abs(rel)); err != nil {
		t.Fatal(err)
	}
}

func remove(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
}

func sortedKeys(m map[string]entry) []string {
	return slices.Sorted(func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	})
}

func requireRestored(t *testing.T, res *RestoreResult, want ...string) {
	t.Helper()
	slices.Sort(want)
	if strings.Join(res.Restored, "\n") != strings.Join(want, "\n") {
		t.Fatalf("restored = %q, want %q", res.Restored, want)
	}
}

// dirContents returns every entry below dir with its mode and content (link target for a symlink).
func dirContents(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		v := info.Mode().String()
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			v += " -> " + target
		case d.Type().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			v += " " + string(data)
		}
		out[strings.TrimPrefix(p, dir)] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestGitControlFilesDoNotChangeContent (decision 1): .gitignore files in the workspace, in .pi and in an extension
// hide nothing, and .gitattributes rules (text, eol, ident, filter, working-tree-encoding) change neither the stored
// blobs nor the restored files.
func TestGitControlFilesDoNotChangeContent(t *testing.T) {
	f := newFixture(t)
	f.write(t, "workspace/.gitignore", ".pi/\n*\n")
	f.write(t, pi+".gitignore", "*\n")
	f.write(t, pi+"extensions/ext/.gitignore", "*\n")
	f.write(t, pi+"extensions/ext/index.ts", "export default function () {}\n")
	f.write(t, pi+"extensions/ext/dist/out.js", "out\n")
	f.write(t, pi+".gitattributes", "* text eol=crlf ident filter=upper working-tree-encoding=UTF-16\n")
	f.write(t, pi+"extensions/ext/.gitattributes", "*.txt text\n")
	crlf := "first\r\nsecond\r\n"
	lf := "id $Id$\nnext\n"
	f.write(t, pi+"extensions/ext/crlf.txt", crlf)
	f.write(t, pi+"lf.txt", lf)
	if _, err := f.repo.Init(ctx); err != nil {
		t.Fatal(err)
	}
	got := f.entries(t, InitTag)
	for _, p := range []string{".gitignore", ".gitattributes", "lf.txt", "extensions/ext/.gitignore", "extensions/ext/.gitattributes", "extensions/ext/index.ts", "extensions/ext/dist/out.js", "extensions/ext/crlf.txt"} {
		if _, ok := got[pi+p]; !ok {
			t.Errorf("%s is not in the snapshot: %v", p, sortedKeys(got))
		}
	}
	if b := f.blob(t, got[pi+"extensions/ext/crlf.txt"].sha); b != crlf {
		t.Fatalf("crlf.txt blob = %q", b)
	}
	if b := f.blob(t, got[pi+"lf.txt"].sha); b != lf {
		t.Fatalf("lf.txt blob = %q", b)
	}
	remove(t, f.abs(pi+"extensions/ext/crlf.txt"), f.abs(pi+"lf.txt"))
	res, err := f.repo.Restore(ctx, InitTag, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireRestored(t, res, pi+"extensions/ext/crlf.txt", pi+"lf.txt")
	if got := f.read(t, pi+"extensions/ext/crlf.txt"); got != crlf {
		t.Fatalf("restored crlf.txt = %q", got)
	}
	if got := f.read(t, pi+"lf.txt"); got != lf {
		t.Fatalf("restored lf.txt = %q", got)
	}
}

// TestClonedExtensionStoredFileByFile (decision 1): a package cloned by pi into .pi/git and a cloned extension (real
// repositories with their own .git) are stored file by file, never as a gitlink, including files their own
// .gitignore hides; pi's npm directory keeps its package.json without node_modules. A restore brings the files of the
// clone back, removes a file added to it, and leaves its .git as it is.
func TestClonedExtensionStoredFileByFile(t *testing.T) {
	f := newFixture(t)
	if _, err := f.repo.Init(ctx); err != nil {
		t.Fatal(err)
	}
	pkg := pi + "git/github.com/someone/tools"
	f.write(t, pkg+"/index.ts", "export default function () {}\n")
	f.write(t, pkg+"/package.json", `{"name": "tools", "version": "1.0.0"}`+"\n")
	f.write(t, pkg+"/docs/guide.md", "guide\n")
	f.write(t, pkg+"/.gitignore", "docs/\n")
	gitCmd(t, f.abs(pkg), "init", "-q")
	gitCmd(t, f.abs(pkg), "add", "-A", "--force")
	gitCmd(t, f.abs(pkg), "commit", "-q", "-m", "first")
	cloned := pi + "extensions/tools"
	if err := os.MkdirAll(f.abs(pi+"extensions"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, f.abs(pi+"extensions"), "clone", "-q", f.abs(pkg), "tools")
	f.write(t, pi+"npm/package.json", `{"dependencies": {"x": "1.0.0"}}`+"\n")
	f.write(t, pi+"npm/node_modules/x/index.js", "x\n")
	commit(t, f, ApplyTag(1), "克隆的扩展")

	got := f.entries(t, ApplyTag(1))
	var want []string
	for _, dir := range []string{pkg, cloned} {
		for _, name := range []string{".gitignore", "docs/guide.md", "index.ts", "package.json"} {
			want = append(want, dir+"/"+name)
		}
	}
	want = append(want, pi+"npm/package.json")
	slices.Sort(want)
	if keys := sortedKeys(got); strings.Join(keys, "\n") != strings.Join(want, "\n") {
		t.Fatalf("snapshot paths = %q\nwant %q", keys, want)
	}
	for _, e := range got {
		if e.mode != modeFile {
			t.Errorf("%s has mode %s", e.path, e.mode)
		}
	}
	// The daemon's index has no gitlink either.
	staged, err := f.repo.run(ctx, "ls-files", "-s", "-z")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := parseListing(staged, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(want) {
		t.Fatalf("index has %d entries, want %d", len(rows), len(want))
	}
	for _, row := range rows {
		if row.fields[0] == modeGitlink {
			t.Fatalf("index has a gitlink: %+v", row)
		}
	}

	gitBefore := dirContents(t, f.abs(cloned+"/.git"))
	remove(t, f.abs(cloned+"/index.ts"), f.abs(cloned+"/docs"))
	f.write(t, cloned+"/.gitignore", "changed\n")
	f.write(t, cloned+"/extra.ts", "extra\n")
	res, err := f.repo.Restore(ctx, ApplyTag(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	requireRestored(t, res, cloned+"/.gitignore", cloned+"/docs/guide.md", cloned+"/extra.ts", cloned+"/index.ts")
	if f.read(t, cloned+"/index.ts") != "export default function () {}\n" || f.read(t, cloned+"/docs/guide.md") != "guide\n" || f.read(t, cloned+"/.gitignore") != "docs/\n" {
		t.Fatal("the files of the clone were not restored")
	}
	if f.exists(cloned + "/extra.ts") {
		t.Fatal("extra.ts was not removed")
	}
	gitAfter := dirContents(t, f.abs(cloned+"/.git"))
	if len(gitAfter) != len(gitBefore) {
		t.Fatalf(".git of the clone changed: %d entries before, %d after", len(gitBefore), len(gitAfter))
	}
	for p, v := range gitBefore {
		if gitAfter[p] != v {
			t.Fatalf(".git%s changed: %q -> %q", p, v, gitAfter[p])
		}
	}
	// The clone matches its own HEAD again.
	if out := gitCmd(t, f.abs(cloned), "status", "--porcelain"); out != "" {
		t.Fatalf("git status of the clone:\n%s", out)
	}
	if !f.exists(pi + "npm/node_modules/x/index.js") {
		t.Fatal("node_modules of .pi/npm was removed")
	}
}

// TestLeftOutPaths (decision 1): exactly .git (a directory or a file), node_modules directories, *.db and *.log are
// left out; a file or symlink named node_modules is stored.
func TestLeftOutPaths(t *testing.T) {
	f := newFixture(t)
	f.write(t, pi+"keep.ts", "keep\n")
	f.write(t, pi+"a/node_modules/dep/index.js", "dep\n")
	f.write(t, pi+"b/node_modules", "a file named node_modules\n")
	f.symlink(t, "../a", pi+"c/node_modules")
	f.write(t, pi+"cache.db", "db\n")
	f.write(t, pi+"store.db/inner.txt", "inner\n")
	f.write(t, pi+"debug.log", "log\n")
	f.write(t, pi+"logs/x.log", "log\n")
	f.write(t, pi+"logs/keep.txt", "keep\n")
	f.write(t, pi+"repo/.git/config", "[core]\n")
	f.write(t, pi+"repo/file.ts", "file\n")
	f.write(t, pi+"worktree/.git", "gitdir: /elsewhere\n")
	f.write(t, pi+"worktree/main.ts", "main\n")
	f.write(t, pi+"upper/.GIT/config", "x\n")
	if _, err := f.repo.Init(ctx); err != nil {
		t.Fatal(err)
	}
	got := f.entries(t, InitTag)
	want := []string{pi + "b/node_modules", pi + "c/node_modules", pi + "keep.ts", pi + "logs/keep.txt", pi + "repo/file.ts", pi + "worktree/main.ts"}
	if keys := sortedKeys(got); strings.Join(keys, "\n") != strings.Join(want, "\n") {
		t.Fatalf("snapshot paths = %q\nwant %q", keys, want)
	}
	if got[pi+"c/node_modules"].mode != modeSymlink || got[pi+"b/node_modules"].mode != modeFile {
		t.Fatalf("node_modules entries = %+v %+v", got[pi+"b/node_modules"], got[pi+"c/node_modules"])
	}
}

// TestSymlinksAndExecutableBit (decision 1): a symlink is stored as a symlink (mode 120000, the blob is the link
// target, also when the target does not exist), an executable file as 100755; a restore brings both back.
func TestSymlinksAndExecutableBit(t *testing.T) {
	f := newFixture(t)
	f.write(t, pi+"bin/run.sh", "#!/bin/sh\necho hi\n")
	if err := os.Chmod(f.abs(pi+"bin/run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write(t, pi+"bin/plain.sh", "#!/bin/sh\n")
	f.symlink(t, "bin/run.sh", pi+"link")
	f.symlink(t, "/nonexistent/target", pi+"dangling")
	if _, err := f.repo.Init(ctx); err != nil {
		t.Fatal(err)
	}
	got := f.entries(t, InitTag)
	if got[pi+"bin/run.sh"].mode != modeExec || got[pi+"bin/plain.sh"].mode != modeFile {
		t.Fatalf("modes: run.sh %s, plain.sh %s", got[pi+"bin/run.sh"].mode, got[pi+"bin/plain.sh"].mode)
	}
	for rel, target := range map[string]string{"link": "bin/run.sh", "dangling": "/nonexistent/target"} {
		e := got[pi+rel]
		if e.mode != modeSymlink || f.blob(t, e.sha) != target {
			t.Fatalf("%s = %+v (%q)", rel, e, f.blob(t, e.sha))
		}
	}
	remove(t, f.abs(pi+"link"), f.abs(pi+"dangling"))
	f.write(t, pi+"link", "now a file\n")
	if err := os.Chmod(f.abs(pi+"bin/run.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := f.repo.Restore(ctx, InitTag, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireRestored(t, res, pi+"bin/run.sh", pi+"dangling", pi+"link")
	for rel, target := range map[string]string{"link": "bin/run.sh", "dangling": "/nonexistent/target"} {
		if got, err := os.Readlink(f.abs(pi + rel)); err != nil || got != target {
			t.Fatalf("restored %s -> %q, %v", rel, got, err)
		}
	}
	if info, err := os.Stat(f.abs(pi + "bin/run.sh")); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("run.sh mode after the restore: %v, %v", info.Mode(), err)
	}
}

// TestSpecialFilesAreErrors (decision 1): a FIFO or a socket below .pi, or a file the scan user cannot read, makes the
// snapshot fail with an error that names the path; no snapshot is tagged.
func TestSpecialFilesAreErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("这个测试需要以普通用户运行：root 可以读取权限为 0000 的文件")
	}
	f := newFixture(t)
	if _, err := f.repo.Init(ctx); err != nil {
		t.Fatal(err)
	}
	fifo := f.abs(pi + "extensions/pipe")
	if err := os.MkdirAll(filepath.Dir(fifo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Commit(ctx, ApplyTag(1), Message{Summary: "FIFO"}); err == nil || !strings.Contains(err.Error(), fifo) || !strings.Contains(err.Error(), "不是普通文件、目录或符号链接") {
		t.Fatalf("commit with a FIFO: %v", err)
	}
	remove(t, fifo)
	sock := f.abs(pi + "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Commit(ctx, ApplyTag(1), Message{Summary: "socket"}); err == nil || !strings.Contains(err.Error(), sock) {
		t.Fatalf("commit with a socket: %v", err)
	}
	l.Close()
	remove(t, sock)
	f.write(t, pi+"extensions/secret.ts", "secret\n")
	if err := os.Chmod(f.abs(pi+"extensions/secret.ts"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Commit(ctx, ApplyTag(1), Message{Summary: "unreadable"}); err == nil || !strings.Contains(err.Error(), pi+"extensions/secret.ts") {
		t.Fatalf("commit with an unreadable file: %v", err)
	}
	if n, _ := f.repo.NextApply(ctx); n != 1 {
		t.Fatalf("a failed snapshot was tagged: NextApply = %d", n)
	}
	if err := os.Chmod(f.abs(pi+"extensions/secret.ts"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit(t, f, ApplyTag(1), "readable again")
}

// TestSymlinkTargetsAreNeverRead (decision 2): files and a directory of the agent replaced by symlinks to files
// outside the work tree (one readable, one with mode 0000) are stored as symlinks; the snapshot succeeds although one
// target cannot be read, and no object of the repository holds the content of any target.
func TestSymlinkTargetsAreNeverRead(t *testing.T) {
	f := newFixture(t)
	if _, err := f.repo.Init(ctx); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	secrets := map[string]string{"readable.toml": "readable-secret-7c1f\n", "unreadable.toml": "unreadable-secret-9d2e\n", "dir/inner.ts": "dir-secret-3b8a\n"}
	for name, content := range secrets {
		p := filepath.Join(outside, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(outside, "unreadable.toml"), 0); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		pi + "extensions/a.ts":        filepath.Join(outside, "readable.toml"),
		pi + "extensions/b.ts":        filepath.Join(outside, "unreadable.toml"),
		pi + "extensions/c":           filepath.Join(outside, "dir"),
		"profile/agent/settings.json": filepath.Join(outside, "unreadable.toml"),
	}
	for rel, target := range links {
		f.symlink(t, target, rel)
	}
	commit(t, f, ApplyTag(1), "符号链接")
	got := f.entries(t, ApplyTag(1))
	if len(got) != len(links) {
		t.Fatalf("snapshot paths = %q", sortedKeys(got))
	}
	for rel, target := range links {
		if e := got[rel]; e.mode != modeSymlink || f.blob(t, e.sha) != target {
			t.Fatalf("%s = %+v", rel, e)
		}
	}
	all, err := f.repo.run(ctx, "cat-file", "--batch-all-objects", "--batch")
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range secrets {
		if strings.Contains(string(all), strings.TrimSpace(content)) {
			t.Fatalf("the repository holds the content of %s", name)
		}
	}
}

// TestRestoreNeverWritesThroughSymlinks (decision 2): an extension directory, and then the whole .pi, replaced by a
// symlink to a directory outside the work tree: the restore replaces the symlink with a real directory and creates
// nothing outside, also for a single path below the symlink.
func TestRestoreNeverWritesThroughSymlinks(t *testing.T) {
	f := newFixture(t)
	ext := pi + "extensions/ext"
	f.write(t, ext+"/index.ts", "index\n")
	f.write(t, ext+"/lib/util.ts", "util\n")
	if _, err := f.repo.Init(ctx); err != nil {
		t.Fatal(err)
	}
	requireEmpty := func(dir string) {
		t.Helper()
		if list, err := os.ReadDir(dir); err != nil || len(list) != 0 {
			t.Fatalf("the restore wrote into %s: %v %v", dir, list, err)
		}
	}
	requireRealDir := func(rel string) {
		t.Helper()
		if info, err := os.Lstat(f.abs(rel)); err != nil || !info.IsDir() {
			t.Fatalf("%s after the restore: %v %v", rel, info, err)
		}
	}

	outside := t.TempDir()
	remove(t, f.abs(ext))
	f.symlink(t, outside, ext)
	res, err := f.repo.Restore(ctx, InitTag, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireRestored(t, res, ext, ext+"/index.ts", ext+"/lib/util.ts")
	requireEmpty(outside)
	requireRealDir(ext)
	if f.read(t, ext+"/index.ts") != "index\n" || f.read(t, ext+"/lib/util.ts") != "util\n" {
		t.Fatal("files not restored")
	}

	remove(t, f.abs(ext))
	f.symlink(t, outside, ext)
	res, err = f.repo.Restore(ctx, InitTag, []string{ext + "/lib/util.ts"})
	if err != nil {
		t.Fatal(err)
	}
	requireRestored(t, res, ext+"/lib/util.ts")
	requireEmpty(outside)
	requireRealDir(ext)
	if f.read(t, ext+"/lib/util.ts") != "util\n" {
		t.Fatal("util.ts not restored")
	}

	outside2 := t.TempDir()
	remove(t, f.abs("workspace/.pi"))
	f.symlink(t, outside2, "workspace/.pi")
	res, err = f.repo.Restore(ctx, InitTag, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireRestored(t, res, "workspace/.pi", ext+"/index.ts", ext+"/lib/util.ts")
	requireEmpty(outside2)
	requireRealDir("workspace/.pi")
	if f.read(t, ext+"/index.ts") != "index\n" {
		t.Fatal("index.ts not restored")
	}
}

// TestRestoreSwitchesFilesAndDirectories: a file of the snapshot where the work tree has a directory, and the other
// way round. A directory with left out content (here node_modules) is not replaced; the restore fails and changes
// nothing.
func TestRestoreSwitchesFilesAndDirectories(t *testing.T) {
	f := newFixture(t)
	thing := pi + "extensions/thing"
	f.write(t, thing, "a file\n")
	if _, err := f.repo.Init(ctx); err != nil {
		t.Fatal(err)
	}
	remove(t, f.abs(thing))
	f.write(t, thing+"/index.ts", "dir\n")
	f.write(t, thing+"/node_modules/dep/index.js", "dep\n")
	if _, err := f.repo.Restore(ctx, InitTag, nil); err == nil || !strings.Contains(err.Error(), thing+"/node_modules") {
		t.Fatalf("restore over left out content: %v", err)
	}
	if f.read(t, thing+"/index.ts") != "dir\n" || !f.exists(thing+"/node_modules/dep/index.js") {
		t.Fatal("the failed restore changed files")
	}
	remove(t, f.abs(thing+"/node_modules"))
	res, err := f.repo.Restore(ctx, InitTag, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireRestored(t, res, thing, thing+"/index.ts")
	if f.read(t, thing) != "a file\n" {
		t.Fatal("the file was not restored")
	}

	remove(t, f.abs(thing))
	f.write(t, thing+"/index.ts", "dir\n")
	commit(t, f, ApplyTag(1), "目录")
	remove(t, f.abs(thing))
	f.write(t, thing, "a file again\n")
	res, err = f.repo.Restore(ctx, ApplyTag(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	requireRestored(t, res, thing, thing+"/index.ts")
	if f.read(t, thing+"/index.ts") != "dir\n" {
		t.Fatal("the directory was not restored")
	}
}

// TestRestoreRefusesGitlink: a commit whose tree has a gitlink below the restored paths (git add stores a nested
// repository that way) has no file content for it; the restore fails and changes nothing.
func TestRestoreRefusesGitlink(t *testing.T) {
	f := newFixture(t)
	f.write(t, pi+"a.ts", "a\n")
	if _, err := f.repo.Init(ctx); err != nil {
		t.Fatal(err)
	}
	head, err := f.repo.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.run(ctx, "read-tree", head); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.runInput(ctx, strings.NewReader("160000 "+head+"\t"+pi+"git/sub\x00"), "update-index", "-z", "--index-info"); err != nil {
		t.Fatal(err)
	}
	tree, err := f.repo.run(ctx, "write-tree")
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.repo.run(ctx, "commit-tree", strings.TrimSpace(string(tree)), "-p", head, "-m", "gitlink")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.run(ctx, "tag", ApplyTag(1), strings.TrimSpace(string(c))); err != nil {
		t.Fatal(err)
	}
	f.write(t, pi+"a.ts", "changed\n")
	if _, err := f.repo.Restore(ctx, ApplyTag(1), nil); err == nil || !strings.Contains(err.Error(), "gitlink") || !strings.Contains(err.Error(), pi+"git/sub") {
		t.Fatalf("restore of a gitlink: %v", err)
	}
	if f.read(t, pi+"a.ts") != "changed\n" {
		t.Fatal("the failed restore changed a.ts")
	}
}

// TestInitMakesRepositoryBare: a repository whose configuration names the work tree (git init --work-tree) is made
// bare by Init, and snapshots continue on its history.
func TestInitMakesRepositoryBare(t *testing.T) {
	f := newFixture(t)
	f.write(t, pi+"a.ts", "a\n")
	gd, wt := "--git-dir="+f.env.Paths.SnapshotsGit, "--work-tree="+f.env.Paths.SnapshotWorkTree
	gitCmd(t, f.env.Root, gd, wt, "init", "-q", "--shared=0644", "-b", "main")
	gitCmd(t, f.env.Root, gd, wt, "add", pi+"a.ts")
	gitCmd(t, f.env.Root, gd, wt, "commit", "-q", "-m", "init")
	gitCmd(t, f.env.Root, gd, "tag", InitTag)
	if wtCfg := gitCmd(t, f.env.Root, gd, "config", "--get", "core.worktree"); strings.TrimSpace(wtCfg) != f.env.Paths.SnapshotWorkTree {
		t.Fatalf("core.worktree before Init = %q", wtCfg)
	}
	created, err := f.repo.Init(ctx)
	if err != nil || created {
		t.Fatalf("Init = %v, %v", created, err)
	}
	if bare, err := f.repo.run(ctx, "config", "--get", "core.bare"); err != nil || strings.TrimSpace(string(bare)) != "true" {
		t.Fatalf("core.bare = %q, %v", bare, err)
	}
	res, err := proc.Run(ctx, f.repo.spec([]string{"config", "--get", "core.worktree"}))
	if err != nil || res.Exit.Code == nil || *res.Exit.Code != 1 {
		t.Fatalf("core.worktree after Init: %+v %v", res, err)
	}
	f.write(t, pi+"b.ts", "b\n")
	commit(t, f, ApplyTag(1), "第一次应用")
	if got := f.tracked(t, ApplyTag(1)); strings.Join(got, " ") != pi+"a.ts "+pi+"b.ts" {
		t.Fatalf("apply/1 = %v", got)
	}
	if text, err := f.repo.DiffStat(ctx, ApplyTag(1), ""); err != nil || !strings.Contains(text, "b.ts") || strings.Contains(text, "a.ts") {
		t.Fatalf("diff apply/1 = %q, %v", text, err)
	}
}

// TestDaemonGitHasNoWorkTree (decision 2): the daemon's git commands have no work tree and run in the repository;
// the helper runs with bearbot's credential when the daemon is not rootless.
func TestDaemonGitHasNoWorkTree(t *testing.T) {
	f := newFixture(t)
	if _, err := f.repo.Init(ctx); err != nil {
		t.Fatal(err)
	}
	spec := f.repo.spec([]string{"status"})
	for _, arg := range spec.Args {
		if strings.HasPrefix(arg, "--work-tree") {
			t.Fatalf("root git has %s", arg)
		}
	}
	if spec.Dir != f.env.Paths.SnapshotsGit || spec.Credential != nil {
		t.Fatalf("root git runs in %s with %+v", spec.Dir, spec.Credential)
	}
	if out, err := f.repo.run(ctx, "rev-parse", "--is-bare-repository"); err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Fatalf("is-bare-repository = %q, %v", out, err)
	}
	if out, err := f.repo.run(ctx, "rev-parse", "--show-toplevel"); err == nil {
		t.Fatalf("root git found a work tree: %q", out)
	}

	users := &layout.Users{Root: layout.User{Name: "root"}, Bearbot: layout.User{Name: "bearbot", UID: 4242, GID: 4343, Groups: []uint32{4343, 5000}}}
	repo, err := Open(f.env.Paths, users)
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []proc.RunSpec{repo.helperSpec(nil, "scan"), repo.helperSpec(nil, "restore", "--commit="+strings.Repeat("a", 40), "--", pi)} {
		c := h.Credential
		if c == nil || c.Uid != 4242 || c.Gid != 4343 || !slices.Equal(c.Groups, []uint32{4343, 5000}) {
			t.Fatalf("helper credential = %+v", c)
		}
		if h.Path != self || h.Args[0] != HelperCommand || h.Dir != f.env.Paths.SnapshotWorkTree {
			t.Fatalf("helper = %s %q in %s", h.Path, h.Args, h.Dir)
		}
	}
}

// TestUnusualFileNames: names with a newline, a trailing carriage return, quotes, backslashes, pathspec magic and
// glob characters are stored and restored exactly; a restore of a path with glob characters is literal.
func TestUnusualFileNames(t *testing.T) {
	f := newFixture(t)
	names := []string{"new\nline.ts", "quote\"d.ts", `back\slash.ts`, "cr\r", "-dash.ts", "star*.ts", ":(glob)magic.ts", "space name.ts", "中文.ts", "tab\tname.ts", "\"leading-quote"}
	var paths []string
	for _, n := range names {
		f.write(t, pi+"names/"+n, n+"\n")
		paths = append(paths, pi+"names/"+n)
	}
	if _, err := f.repo.Init(ctx); err != nil {
		t.Fatal(err)
	}
	got := f.entries(t, InitTag)
	for _, p := range paths {
		if e, ok := got[p]; !ok || f.blob(t, e.sha) != filepath.Base(p)+"\n" {
			t.Fatalf("%q in the snapshot: %+v %v", p, e, ok)
		}
	}
	for _, p := range paths {
		remove(t, f.abs(p))
	}
	res, err := f.repo.Restore(ctx, InitTag, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireRestored(t, res, paths...)
	for _, p := range paths {
		if f.read(t, p) != filepath.Base(p)+"\n" {
			t.Fatalf("%q not restored", p)
		}
	}
	remove(t, f.abs(pi+"names/star*.ts"))
	f.write(t, pi+"names/starX.ts", "not in the snapshot\n")
	res, err = f.repo.Restore(ctx, InitTag, []string{pi + "names/star*.ts"})
	if err != nil {
		t.Fatal(err)
	}
	requireRestored(t, res, pi+"names/star*.ts")
	if !f.exists(pi + "names/starX.ts") {
		t.Fatal("a literal path removed starX.ts")
	}
}

// TestHelperOutputIsChecked: the daemon rejects an entry list or a restore result that names anything but stored
// files below the requested paths.
func TestHelperOutputIsChecked(t *testing.T) {
	sha := strings.Repeat("a", 40)
	good := entry{mode: modeFile, sha: sha, path: pi + "a.ts"}.record()
	if e, err := parseEntries([]byte(good), TrackedPaths); err != nil || len(e) != 1 {
		t.Fatalf("good entry list: %v %v", e, err)
	}
	for name, list := range map[string]string{
		"gitlink":         "160000 " + sha + "\t" + pi + "sub\x00",
		"tree":            "040000 " + sha + "\t" + pi + "dir\x00",
		"outside":         "100644 " + sha + "\tconfig/bearbot.toml\x00",
		"auth.json":       "100644 " + sha + "\tprofile/agent/auth.json\x00",
		"dot dot":         "100644 " + sha + "\t" + pi + "../../config/bearbot.toml\x00",
		".git":            "100644 " + sha + "\t" + pi + "x/.git/config\x00",
		"node_modules":    "100644 " + sha + "\t" + pi + "x/node_modules/y.js\x00",
		"db directory":    "100644 " + sha + "\t" + pi + "x.db/y\x00",
		"log":             "100644 " + sha + "\t" + pi + "x.log\x00",
		"empty component": "100644 " + sha + "\t" + pi + "/x\x00",
		"short sha":       "100644 abc\t" + pi + "x\x00",
		"duplicate":       good + good,
		"no NUL":          strings.TrimSuffix(good, "\x00"),
	} {
		if _, err := parseEntries([]byte(list), TrackedPaths); err == nil {
			t.Errorf("entry list %s accepted", name)
		}
	}
	if c, n, err := parseRestore([]byte("C\t"+pi+"x/package.json\x00N\tci\t"+pi+"x\x00"), RestartEffectivePaths); err != nil || len(c) != 1 || len(n) != 1 || n[0] != (npmDir{dir: pi + "x", sub: "ci"}) {
		t.Fatalf("good restore output: %v %v %v", c, n, err)
	}
	for name, out := range map[string]string{
		"outside targets":    "C\tworkspace/AGENTS.md\x00",
		"unsorted":           "C\t" + pi + "b\x00C\t" + pi + "a\x00",
		"npm without change": "C\t" + pi + "x/a.ts\x00N\tci\t" + pi + "x\x00",
		"npm command":        "C\t" + pi + "x/package.json\x00N\trm -rf\t" + pi + "x\x00",
		"unknown record":     "X\t" + pi + "a\x00",
	} {
		if _, _, err := parseRestore([]byte(out), RestartEffectivePaths); err == nil {
			t.Errorf("restore output %s accepted", name)
		}
	}
}
