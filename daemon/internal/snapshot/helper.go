package snapshot

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"bearbot/daemon/internal/proc"
)

// HelperCommand is the hidden bearbotd subcommand that does the work tree side of the snapshots. The daemon runs it
// as bearbot, so every file of the work tree is read and written with bearbot's permissions:
//
//	__snapshot scan --git=<git> --repo=<SnapshotsGit> --work-tree=<SnapshotWorkTree>
//	__snapshot restore --git=<git> --repo=<SnapshotsGit> --work-tree=<SnapshotWorkTree> --commit=<40 hex> -- <path>...
//
// The helper keeps its objects and its index in a bare repository in a temporary directory of its own, with
// <SnapshotsGit>/objects as alternate object directory: it reads the snapshot repository and never writes it.
//
// scan walks TrackedPaths and writes to stdout "<n>\n", an entry list of n bytes (records "<mode> <sha>\t<path>\0")
// and then, when the snapshot repository lacks some of the blobs, a pack with exactly those blobs.
//
// restore makes the paths (restart effective paths, checked like RestorePath) equal to the commit: changed files are
// written, files the commit does not have are removed, left out paths (leftOut) stay as they are. It writes
// "C\t<path>\0" for every changed path and "N\t<ci|install>\t<dir>\0" for every directory where npm has to run.
const HelperCommand = "__snapshot"

// RunHelper runs HelperCommand with the arguments that follow it.
func RunHelper(args []string) error {
	if len(args) == 0 {
		return errors.New(HelperCommand + "：缺少 scan 或 restore")
	}
	fl := flag.NewFlagSet(HelperCommand+" "+args[0], flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	git := fl.String("git", "", "git 的绝对路径")
	repo := fl.String("repo", "", "快照仓库")
	workTree := fl.String("work-tree", "", "工作目录")
	commit := fl.String("commit", "", "restore 的目标提交")
	if err := fl.Parse(args[1:]); err != nil {
		return fmt.Errorf("%s：%w", HelperCommand, err)
	}
	for name, v := range map[string]string{"git": *git, "repo": *repo, "work-tree": *workTree} {
		if !filepath.IsAbs(v) {
			return fmt.Errorf("%s：--%s 必须是绝对路径，收到 %q", HelperCommand, name, v)
		}
	}
	ctx := context.Background()
	switch args[0] {
	case "scan":
		if *commit != "" || fl.NArg() != 0 {
			return errors.New(HelperCommand + " scan 不接受 --commit 和路径")
		}
		return withHelper(ctx, *git, *repo, *workTree, func(h *helper) error { return h.scan(ctx, os.Stdout) })
	case "restore":
		if !objectName.MatchString(*commit) {
			return fmt.Errorf("%s restore：--commit 必须是 40 位十六进制的提交，收到 %q", HelperCommand, *commit)
		}
		if fl.NArg() == 0 {
			return errors.New(HelperCommand + " restore 需要路径")
		}
		var targets []string
		for _, p := range fl.Args() {
			rel, err := restorePath(p)
			if err != nil {
				return err
			}
			targets = append(targets, rel)
		}
		return withHelper(ctx, *git, *repo, *workTree, func(h *helper) error { return h.restore(ctx, *commit, targets, os.Stdout) })
	default:
		return fmt.Errorf("%s：未知的操作 %q", HelperCommand, args[0])
	}
}

// helper is one run of HelperCommand.
type helper struct {
	git      string
	workTree string
	// tmp is the helper's temporary directory; repo is the bare repository in it.
	tmp  string
	repo string
}

// withHelper creates the temporary repository, runs fn and removes the temporary directory.
func withHelper(ctx context.Context, git, snapshots, workTree string, fn func(*helper) error) (err error) {
	tmp, err := os.MkdirTemp("", "bearbot-snapshot-")
	if err != nil {
		return err
	}
	defer func() {
		if rerr := os.RemoveAll(tmp); err == nil {
			err = rerr
		}
	}()
	h := &helper{git: git, workTree: workTree, tmp: tmp, repo: filepath.Join(tmp, "repo")}
	if _, err := h.run(ctx, gitCall{}, "init", "-q", "--bare", "--template="); err != nil {
		return fmt.Errorf("创建临时仓库失败：%w", err)
	}
	if err := os.MkdirAll(filepath.Join(h.repo, "info"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(h.repo, "info", "attributes"), []byte(Attributes), 0o600); err != nil {
		return err
	}
	alternates := filepath.Join(snapshots, "objects") + "\n"
	if err := os.WriteFile(filepath.Join(h.repo, "objects", "info", "alternates"), []byte(alternates), 0o600); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(tmp, "links"), 0o700); err != nil {
		return err
	}
	return fn(h)
}

// gitCall holds the optional parts of a git command of the helper.
type gitCall struct {
	stdin  io.Reader
	stdout io.Writer
	// workTree adds --work-tree; the commands that only read or write objects and the index run without it.
	workTree bool
	env      []string
}

// run runs git on the temporary repository in the work tree directory and returns stdout.
func (h *helper) run(ctx context.Context, c gitCall, args ...string) ([]byte, error) {
	full := []string{"--git-dir=" + h.repo}
	if c.workTree {
		full = append(full, "--work-tree="+h.workTree)
	}
	full = append(full,
		"-c", "core.attributesFile="+filepath.Join(h.repo, "info", "attributes"),
		"-c", "core.excludesFile=/dev/null",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.quotePath=false",
		"-c", "core.autocrlf=false",
		"-c", "core.symlinks=true",
		"-c", "core.fileMode=true",
		"-c", "core.fsmonitor=false",
		"-c", "gc.auto=0",
	)
	res, err := proc.RunChecked(ctx, proc.RunSpec{
		Path:    h.git,
		Args:    append(full, args...),
		Env:     append(os.Environ(), c.env...),
		Dir:     h.workTree,
		Stdin:   c.stdin,
		Stdout:  c.stdout,
		Timeout: GitTimeout,
	})
	if err != nil {
		return nil, err
	}
	return res.Stdout, nil
}

// file is a non-directory found by walk.
type file struct {
	path string
	mode string
	// source is the path given to git hash-object: the path itself, or for a symlink a file in the temporary
	// directory that holds the link target.
	source string
}

// realDirs reports whether every directory above root (a path relative to workTree) is a real directory that the
// walk would enter; below a symlink or a left out directory nothing is stored.
func realDirs(workTree, root string) (bool, error) {
	parts := strings.Split(root, "/")
	for i := range len(parts) - 1 {
		if leftOut(parts[i], true) {
			return false, nil
		}
		info, err := os.Lstat(filepath.Join(workTree, filepath.Join(parts[:i+1]...)))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.IsDir() {
			return false, nil
		}
	}
	return true, nil
}

// walk lists the files below roots, sorted by path, and the paths it left out (leftOut; a left out directory is
// listed once, without its content). Symlinks are never followed, so a symlink is a file of its own and nothing
// below a symlinked directory is listed. A FIFO, socket or device is an error.
func (h *helper) walk(roots []string) ([]file, []string, error) {
	var out []file
	var left []string
	seen := map[string]bool{}
	for _, root := range roots {
		ok, err := realDirs(h.workTree, root)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			continue
		}
		abs := filepath.Join(h.workTree, root)
		if _, err := os.Lstat(abs); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, nil, err
		}
		err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return fmt.Errorf("读取 %s 失败：%w", p, err)
			}
			rel := root + strings.TrimPrefix(p, abs)
			if leftOut(d.Name(), d.IsDir()) {
				left = append(left, rel)
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			typ := d.Type()
			var f file
			switch {
			case typ.IsDir():
				return nil
			case typ&fs.ModeSymlink != 0:
				target, err := os.Readlink(p)
				if err != nil {
					return fmt.Errorf("读取符号链接 %s 失败：%w", p, err)
				}
				source := filepath.Join(h.tmp, "links", strconv.Itoa(len(out)))
				if err := os.WriteFile(source, []byte(target), 0o600); err != nil {
					return err
				}
				f = file{path: rel, mode: modeSymlink, source: source}
			case typ.IsRegular():
				info, err := d.Info()
				if err != nil {
					return fmt.Errorf("读取 %s 失败：%w", p, err)
				}
				mode := modeFile
				if info.Mode().Perm()&0o100 != 0 {
					mode = modeExec
				}
				f = file{path: rel, mode: mode, source: rel}
			default:
				return fmt.Errorf("%s 不是普通文件、目录或符号链接（%s），快照无法保存它", p, typ)
			}
			if !seen[rel] {
				seen[rel] = true
				out = append(out, f)
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, left, nil
}

// hash returns the blob of every file (`git hash-object --no-filters`: no attribute or filter applies); with write
// the blobs are written into the temporary repository.
func (h *helper) hash(ctx context.Context, files []file, write bool) ([]string, error) {
	if len(files) == 0 {
		return nil, nil
	}
	var list strings.Builder
	for _, f := range files {
		list.WriteString(quoteC(f.source))
		list.WriteByte('\n')
	}
	args := []string{"hash-object", "--no-filters", "--stdin-paths"}
	if write {
		args = append(args, "-w")
	}
	out, err := h.run(ctx, gitCall{stdin: strings.NewReader(list.String())}, args...)
	if err != nil {
		return nil, fmt.Errorf("计算快照内容的对象名失败：%w", err)
	}
	shas := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(shas) != len(files) {
		return nil, fmt.Errorf("git hash-object 返回了 %d 个对象名，应为 %d 个", len(shas), len(files))
	}
	for _, s := range shas {
		if !objectName.MatchString(s) {
			return nil, fmt.Errorf("git hash-object 返回了无效的对象名 %q", s)
		}
	}
	return shas, nil
}

// missing returns the blobs among shas that neither the temporary repository nor the snapshot repository has.
func (h *helper) missing(ctx context.Context, shas []string) (map[string]bool, error) {
	types, err := objectTypes(shas, func(stdin io.Reader, args ...string) ([]byte, error) {
		return h.run(ctx, gitCall{stdin: stdin}, args...)
	})
	if err != nil {
		return nil, err
	}
	missing := map[string]bool{}
	for sha, typ := range types {
		switch typ {
		case "blob":
		case "missing":
			missing[sha] = true
		default:
			return nil, fmt.Errorf("快照仓库中的对象 %s 不是 blob（%s）", sha, typ)
		}
	}
	return missing, nil
}

// scan is `__snapshot scan`.
func (h *helper) scan(ctx context.Context, out io.Writer) error {
	files, _, err := h.walk(TrackedPaths)
	if err != nil {
		return err
	}
	shas, err := h.hash(ctx, files, false)
	if err != nil {
		return err
	}
	missing, err := h.missing(ctx, shas)
	if err != nil {
		return err
	}
	// The missing blobs are hashed again with -w; the names of that run are the ones stored, so a file that changed
	// meanwhile is still stored with the content that was written.
	var idx []int
	var toWrite []file
	for i, s := range shas {
		if missing[s] {
			idx = append(idx, i)
			toWrite = append(toWrite, files[i])
		}
	}
	written, err := h.hash(ctx, toWrite, true)
	if err != nil {
		return err
	}
	for j, i := range idx {
		shas[i] = written[j]
	}
	var list bytes.Buffer
	for i, f := range files {
		list.WriteString(entry{mode: f.mode, sha: shas[i], path: f.path}.record())
	}
	if _, err := fmt.Fprintf(out, "%d\n", list.Len()); err != nil {
		return err
	}
	if _, err := out.Write(list.Bytes()); err != nil {
		return err
	}
	if len(written) == 0 {
		return nil
	}
	pack := slices.Compact(slices.Sorted(slices.Values(written)))
	if _, err := h.run(ctx, gitCall{stdin: strings.NewReader(strings.Join(pack, "\n") + "\n"), stdout: out}, "pack-objects", "--stdout", "-q"); err != nil {
		return fmt.Errorf("打包新的快照对象失败：%w", err)
	}
	return nil
}

// restore is `__snapshot restore`.
func (h *helper) restore(ctx context.Context, commit string, targets []string, out io.Writer) error {
	files, left, err := h.walk(targets)
	if err != nil {
		return err
	}
	shas, err := h.hash(ctx, files, false)
	if err != nil {
		return err
	}
	current := map[string]entry{}
	var currentList []entry
	for i, f := range files {
		e := entry{mode: f.mode, sha: shas[i], path: f.path}
		current[f.path] = e
		currentList = append(currentList, e)
	}
	listing, err := h.run(ctx, gitCall{}, "ls-tree", "-r", "-z", "--full-tree", "--end-of-options", commit)
	if err != nil {
		return err
	}
	rows, err := parseListing(listing, 3)
	if err != nil {
		return err
	}
	target := map[string]entry{}
	for _, row := range rows {
		if !belowAny(row.path, targets) {
			continue
		}
		if row.fields[0] == modeGitlink {
			return fmt.Errorf("快照提交 %s 中的 %s 是子模块链接（gitlink），其中没有文件内容，无法恢复", commit, row.path)
		}
		target[row.path] = entry{mode: row.fields[0], sha: row.fields[2], path: row.path}
	}
	// A file of the commit where the work tree has a directory replaces the whole directory; it must not take left
	// out content (a nested .git, node_modules, databases, logs) with it.
	for _, l := range left {
		for p := l; p != "."; p = filepath.Dir(p) {
			if _, ok := target[p]; ok {
				return fmt.Errorf("快照提交 %s 中的 %s 是文件或符号链接，而现在这里是目录，其中有快照不保存的 %s；恢复会把整个目录删除，所以没有恢复。请先移走 %s", commit, p, l, l)
			}
		}
	}
	// changed: every path that differs; existing: the changed paths the work tree has now (removed or rewritten);
	// added: the changed paths only the commit has.
	var changed, existing, added []string
	for path, e := range current {
		if t, ok := target[path]; !ok || t != e {
			changed = append(changed, path)
			existing = append(existing, path)
		}
	}
	for path := range target {
		if _, ok := current[path]; !ok {
			changed = append(changed, path)
			added = append(added, path)
		}
	}
	sort.Strings(changed)
	sort.Strings(existing)
	sort.Strings(added)
	if len(changed) > 0 {
		// The index holds the current state, so `git restore` removes what the commit does not have. It gets the
		// changed paths as literal paths (GIT_LITERAL_PATHSPECS), first the existing ones, then the added ones: a
		// directory that becomes a file is emptied before the file is written.
		var info strings.Builder
		for _, e := range currentList {
			info.WriteString(e.record())
		}
		if _, err := h.run(ctx, gitCall{stdin: strings.NewReader(info.String())}, "update-index", "-z", "--index-info"); err != nil {
			return fmt.Errorf("写入当前内容的索引失败：%w", err)
		}
		staged, err := h.run(ctx, gitCall{}, "ls-files", "-s", "-z")
		if err != nil {
			return err
		}
		indexRows, err := parseListing(staged, 3)
		if err != nil {
			return err
		}
		inIndex := map[string]entry{}
		for _, row := range indexRows {
			inIndex[row.path] = entry{mode: row.fields[0], sha: row.fields[1], path: row.path}
		}
		if err := sameEntries(currentList, inIndex, "当前内容的索引"); err != nil {
			return err
		}
		for _, pass := range [][]string{existing, added} {
			if len(pass) == 0 {
				continue
			}
			var paths strings.Builder
			for _, p := range pass {
				paths.WriteString(p)
				paths.WriteByte(0)
			}
			if _, err := h.run(ctx, gitCall{stdin: strings.NewReader(paths.String()), workTree: true, env: []string{"GIT_LITERAL_PATHSPECS=1"}},
				"restore", "--source="+commit, "--staged", "--worktree", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
				return fmt.Errorf("git restore 失败：%w", err)
			}
		}
	}
	var res strings.Builder
	for _, p := range changed {
		res.WriteString("C\t" + p + "\x00")
	}
	npm, err := h.npmDirs(changed)
	if err != nil {
		return err
	}
	for _, d := range npm {
		res.WriteString("N\t" + d.sub + "\t" + d.dir + "\x00")
	}
	_, err = io.WriteString(out, res.String())
	return err
}

// npmDir is a directory where npm runs after a restore: sub is "ci" (with a lock file) or "install".
type npmDir struct {
	dir string
	sub string
}

var depFiles = []string{"package.json", "package-lock.json", "npm-shrinkwrap.json"}

// npmDirs returns the directories (relative to the work tree) whose package.json or lock file changed and that
// still have a package.json, sorted.
func (h *helper) npmDirs(changed []string) ([]npmDir, error) {
	var out []npmDir
	for _, dir := range dependencyDirs(changed) {
		abs := filepath.Join(h.workTree, dir)
		if _, err := os.Lstat(filepath.Join(abs, "package.json")); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		sub := "install"
		for _, lock := range []string{"package-lock.json", "npm-shrinkwrap.json"} {
			if _, err := os.Lstat(filepath.Join(abs, lock)); err == nil {
				sub = "ci"
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
		out = append(out, npmDir{dir: dir, sub: sub})
	}
	return out, nil
}

// dependencyDirs returns the sorted directories of the changed package.json and lock files.
func dependencyDirs(changed []string) []string {
	var dirs []string
	for _, f := range changed {
		if slices.Contains(depFiles, filepath.Base(f)) && !slices.Contains(dirs, filepath.Dir(f)) {
			dirs = append(dirs, filepath.Dir(f))
		}
	}
	sort.Strings(dirs)
	return dirs
}
