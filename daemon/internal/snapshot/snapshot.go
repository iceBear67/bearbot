// Package snapshot keeps the snapshot repository of DESIGN 5.9 with the git command line. The repository SnapshotsGit
// belongs to root and the daemon's git commands run on it as root without a work tree: they import objects, build
// the index and the tree from a list of entries, commit, tag and compare commits. Everything that reads or writes
// the work tree SnapshotWorkTree (<R>/bearbot) runs as bearbot in the hidden subcommand HelperCommand, so root never
// opens a file below the work tree. Only TrackedPaths are stored; every commit stores all of them, so the history is
// a straight line and the parent of any commit is the state before it. Snapshots are lightweight tags.
package snapshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bearbot/daemon/internal/layout"
	"bearbot/daemon/internal/proc"
	"bearbot/daemon/internal/state"
)

// Tag names without a sequence number.
const (
	InitTag     = "init"
	LastGoodTag = "last-good"
)

var (
	refPattern = regexp.MustCompile(`^(init|apply/[0-9]+|incident/inc-[0-9]{14}/(before|attempt-[0-9]+)|rollback/[0-9]+/(before|after)|resume/[0-9]+|last-good)\^?$`)
	hexPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

	applyTag    = regexp.MustCompile(`^apply/([0-9]+)$`)
	rollbackTag = regexp.MustCompile(`^rollback/([0-9]+)/(before|after)$`)
	resumeTag   = regexp.MustCompile(`^resume/([0-9]+)$`)
	incidentID  = regexp.MustCompile(`^inc-[0-9]{14}$`)
)

// ValidateRef accepts the snapshot references of DESIGN 5.9 (optionally followed by "^") and 40-hex commit hashes.
func ValidateRef(ref string) error {
	if refPattern.MatchString(ref) || hexPattern.MatchString(ref) {
		return nil
	}
	return fmt.Errorf("无效的快照引用 %q：可用的引用为 init、apply/<n>、incident/<故障id>/before、incident/<故障id>/attempt-<n>、rollback/<n>/before、rollback/<n>/after、resume/<n>、last-good（都可以在末尾加 ^ 表示前一个状态），或者 40 位十六进制的提交哈希", ref)
}

// ApplyTag returns "apply/<n>".
func ApplyTag(n int) string { return "apply/" + strconv.Itoa(n) }

// IncidentBeforeTag returns "incident/<id>/before".
func IncidentBeforeTag(id string) string { return "incident/" + id + "/before" }

// IncidentAttemptTag returns "incident/<id>/attempt-<n>".
func IncidentAttemptTag(id string, n int) string {
	return "incident/" + id + "/attempt-" + strconv.Itoa(n)
}

// RollbackBeforeTag returns "rollback/<n>/before".
func RollbackBeforeTag(n int) string { return "rollback/" + strconv.Itoa(n) + "/before" }

// RollbackAfterTag returns "rollback/<n>/after".
func RollbackAfterTag(n int) string { return "rollback/" + strconv.Itoa(n) + "/after" }

// ResumeTag returns "resume/<n>".
func ResumeTag(n int) string { return "resume/" + strconv.Itoa(n) }

func isTagName(name string) bool {
	return refPattern.MatchString(name) && !strings.HasSuffix(name, "^")
}

func shortHash(commit string) string { return commit[:min(7, len(commit))] }

func validIncidentID(id string) bool { return incidentID.MatchString(id) }

// RestartEffectivePaths are the paths (relative to the work tree) that need an agent restart to take effect; only
// they are restored (DESIGN 1, 5.9).
var RestartEffectivePaths = []string{"workspace/.pi", "profile/agent/settings.json", "profile/agent/models.json"}

// DocumentPaths are the tracked workspace documents that a restore keeps as they are; a rollback reports whether
// they differ from the target (DESIGN 5.12 step 9, DiffStatPaths).
var DocumentPaths = []string{"workspace/AGENTS.md", "workspace/CLASSIFIER.md"}

// Exclude is the content of info/exclude, for the git commands bearbot runs itself with --work-tree=/bearbot (git
// status): everything is excluded, then the tracked paths are re-included level by level (git cannot re-include a
// file whose parent directory is excluded), then node_modules, databases and logs are excluded again. The snapshots
// themselves do not use it: the helper decides their content (TrackedPaths, leftOut).
const Exclude = `# Written by bearbotd. Only these paths are snapshotted:
# workspace/AGENTS.md workspace/CLASSIFIER.md workspace/.pi/** profile/agent/settings.json profile/agent/models.json
/*
!/workspace/
/workspace/*
!/workspace/AGENTS.md
!/workspace/CLASSIFIER.md
!/workspace/.pi/
!/profile/
/profile/*
!/profile/agent/
/profile/agent/*
!/profile/agent/settings.json
!/profile/agent/models.json
node_modules/
*.db
*.log
`

// Time limits of git and npm.
const (
	GitTimeout = 5 * time.Minute
	NpmTimeout = 15 * time.Minute
)

// Repo is the snapshot repository.
type Repo struct {
	paths layout.Paths
	users *layout.Users
	git   string
	// self is the running executable, which provides HelperCommand.
	self string
	// node and npm run npm as `<node> <npm>`: the child PATH starts with directories bearbot can write, so the
	// interpreter of npm's `#!/usr/bin/env node` line is not looked up there.
	node string
	npm  string
	// mu serializes the snapshot operations: each method that writes the index, the refs or the work tree runs its
	// whole sequence alone.
	mu sync.Mutex
}

// Open prepares access to the repository; it looks up git, node and npm in the daemon's PATH. Call Init before
// anything else.
func Open(paths layout.Paths, users *layout.Users) (*Repo, error) {
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("找不到 git 命令：%w", err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, fmt.Errorf("找不到 node 命令：%w", err)
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		return nil, fmt.Errorf("找不到 npm 命令：%w", err)
	}
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("找不到 bearbotd 自身的可执行文件：%w", err)
	}
	return &Repo{paths: paths, users: users, git: git, self: self, node: node, npm: npm}, nil
}

// gitEnv is added to the environment of every git command of the snapshots.
var gitEnv = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}

// spec returns a git command as root on the repository. It has no work tree (the repository is bare) and runs in
// the repository directory, so git reads no file of the work tree; attributes come only from info/attributes.
func (r *Repo) spec(args []string) proc.RunSpec {
	full := []string{
		"--git-dir=" + r.paths.SnapshotsGit,
		"-c", "user.name=bearbotd",
		"-c", "user.email=bearbotd@localhost",
		"-c", "core.excludesFile=/dev/null",
		"-c", "core.attributesFile=" + filepath.Join(r.paths.SnapshotsGit, "info", "attributes"),
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.quotePath=false",
		"-c", "commit.gpgSign=false",
		"-c", "tag.gpgSign=false",
		"-c", "gc.auto=0",
	}
	env := append(r.paths.RootEnv(), gitEnv...)
	return proc.RunSpec{Path: r.git, Args: append(full, args...), Env: env, Dir: r.paths.SnapshotsGit, Timeout: GitTimeout}
}

// run runs git and returns stdout; any failure is an error.
func (r *Repo) run(ctx context.Context, args ...string) ([]byte, error) {
	res, err := proc.RunChecked(ctx, r.spec(args))
	if err != nil {
		return nil, err
	}
	return res.Stdout, nil
}

// runInput is run with stdin.
func (r *Repo) runInput(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	spec := r.spec(args)
	spec.Stdin = stdin
	res, err := proc.RunChecked(ctx, spec)
	if err != nil {
		return nil, err
	}
	return res.Stdout, nil
}

// helperSpec returns HelperCommand with the operation op and its arguments, run as bearbot (the daemon's own user
// in rootless mode) in the work tree; stdout goes to stdout when it is not nil.
func (r *Repo) helperSpec(stdout io.Writer, op string, args ...string) proc.RunSpec {
	full := []string{HelperCommand, op, "--git=" + r.git, "--repo=" + r.paths.SnapshotsGit, "--work-tree=" + r.paths.SnapshotWorkTree}
	env := append(r.paths.RootEnv(), "HOME="+r.paths.BearbotHome)
	return proc.RunSpec{
		Path:       r.self,
		Args:       append(full, args...),
		Env:        append(env, gitEnv...),
		Dir:        r.paths.SnapshotWorkTree,
		Credential: r.users.Credential(r.users.Bearbot),
		Stdout:     stdout,
		Timeout:    GitTimeout,
	}
}

// Init creates the repository when it does not exist, makes sure it is bare, writes info/exclude and
// info/attributes and makes the initial commit tagged init (DESIGN 5.2 step 3). It reports whether the init commit
// was made now.
func (r *Repo) Init(ctx context.Context) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	info, err := os.Lstat(r.paths.SnapshotsGit)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.Mkdir(r.paths.SnapshotsGit, 0o755); err != nil {
			return false, fmt.Errorf("创建快照仓库失败：%w", err)
		}
		if _, err := r.run(ctx, "init", "-q", "--bare", "--shared=0644", "-b", "main"); err != nil {
			return false, fmt.Errorf("创建快照仓库失败：%w", err)
		}
	case err != nil:
		return false, fmt.Errorf("无法访问快照仓库 %s：%w", r.paths.SnapshotsGit, err)
	case !info.IsDir():
		return false, fmt.Errorf("快照仓库 %s 不是目录", r.paths.SnapshotsGit)
	default:
		if _, err := r.run(ctx, "rev-parse", "--git-dir"); err != nil {
			return false, fmt.Errorf("%s 不是可用的快照仓库：%w", r.paths.SnapshotsGit, err)
		}
	}
	if err := r.ensureBare(ctx); err != nil {
		return false, err
	}
	if err := os.Chmod(r.paths.SnapshotsGit, 0o755); err != nil {
		return false, err
	}
	infoDir := filepath.Join(r.paths.SnapshotsGit, "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		return false, err
	}
	for _, f := range []struct{ name, content string }{{"exclude", Exclude}, {"attributes", Attributes}} {
		if err := os.WriteFile(filepath.Join(infoDir, f.name), []byte(f.content), 0o644); err != nil {
			return false, fmt.Errorf("写入快照仓库的 info/%s 失败：%w", f.name, err)
		}
		if err := os.Chmod(filepath.Join(infoDir, f.name), 0o644); err != nil {
			return false, err
		}
	}
	if _, ok, err := r.tagCommit(ctx, InitTag); err != nil {
		return false, err
	} else if ok {
		return false, nil
	}
	if _, ok, err := r.revParse(ctx, "HEAD"); err != nil {
		return false, err
	} else if ok {
		return false, errors.New("快照仓库中已有提交，但没有 init 标签")
	}
	if _, err := r.commitLocked(ctx, InitTag, Message{Summary: "init"}); err != nil {
		return false, err
	}
	return true, nil
}

// ensureBare makes the configuration say that the repository has no work tree: core.worktree is removed (a
// repository created with --work-tree has it) and core.bare is true, so no git command of the daemon gets a work
// tree from the configuration. bearbot still gives --work-tree=/bearbot to its own git commands.
func (r *Repo) ensureBare(ctx context.Context) error {
	res, err := proc.Run(ctx, r.spec([]string{"config", "--unset-all", "core.worktree"}))
	if err != nil {
		return err
	}
	// Exit code 5: core.worktree is not set.
	if !res.OK() && (res.TimedOut || res.Exit.Code == nil || *res.Exit.Code != 5) {
		return fmt.Errorf("修改快照仓库的配置失败：%w", res.Err())
	}
	if _, err := r.run(ctx, "config", "core.bare", "true"); err != nil {
		return fmt.Errorf("修改快照仓库的配置失败：%w", err)
	}
	return nil
}

// revParse resolves expr to a commit; ok is false when it does not exist.
func (r *Repo) revParse(ctx context.Context, expr string) (string, bool, error) {
	res, err := proc.Run(ctx, r.spec([]string{"rev-parse", "--verify", "--quiet", "--end-of-options", expr + "^{commit}"}))
	if err != nil {
		return "", false, err
	}
	if res.TimedOut {
		return "", false, res.Err()
	}
	if res.Exit.Code != nil && *res.Exit.Code == 1 && len(bytes.TrimSpace(res.Stdout)) == 0 {
		return "", false, nil
	}
	if !res.OK() {
		return "", false, res.Err()
	}
	return strings.TrimSpace(string(res.Stdout)), true, nil
}

func (r *Repo) tagCommit(ctx context.Context, tag string) (string, bool, error) {
	return r.revParse(ctx, "refs/tags/"+tag)
}

// Resolve validates ref (ValidateRef) and returns the 40-hex commit it names.
func (r *Repo) Resolve(ctx context.Context, ref string) (string, error) {
	if err := ValidateRef(ref); err != nil {
		return "", err
	}
	expr := ref
	if !hexPattern.MatchString(ref) {
		expr = "refs/tags/" + ref
	}
	commit, ok, err := r.revParse(ctx, expr)
	if err != nil {
		return "", err
	}
	if !ok {
		if strings.HasSuffix(ref, "^") {
			return "", fmt.Errorf("快照 %s 不存在（%s 不存在或没有前一个状态）", ref, strings.TrimSuffix(ref, "^"))
		}
		return "", fmt.Errorf("快照 %s 不存在", ref)
	}
	return commit, nil
}

// Message is the commit message content: summary, requester identity and thread (DESIGN 5.6 apply step 3).
type Message struct {
	Summary   string
	Requester string
	Thread    string
}

// Trailer keys of the commit message.
const (
	RequesterTrailer = "Bearbot-Requester"
	ThreadTrailer    = "Bearbot-Thread"
)

// CheckSummary rejects a summary that cannot be passed to git as a command line argument (it contains NUL).
func CheckSummary(summary string) error {
	if strings.ContainsRune(summary, 0) {
		return errors.New("摘要不能包含 NUL 字符")
	}
	return nil
}

// text returns the commit message: the summary, then a final paragraph written by the daemon with both trailers
// (an empty value when there is none), so a trailer-like last paragraph of the summary is never read as trailers.
func (m Message) text() (string, error) {
	summary := strings.TrimSpace(m.Summary)
	if summary == "" {
		return "", errors.New("快照的摘要不能为空")
	}
	if err := CheckSummary(summary); err != nil {
		return "", err
	}
	if strings.ContainsAny(m.Requester, "\r\n\x00") || strings.ContainsAny(m.Thread, "\r\n\x00") {
		return "", errors.New("发起者与会话串不能包含换行或 NUL 字符")
	}
	trailer := func(key, value string) string {
		if value == "" {
			return key + ":"
		}
		return key + ": " + value
	}
	var b strings.Builder
	b.WriteString(summary)
	b.WriteString("\n\n")
	b.WriteString(trailer(RequesterTrailer, m.Requester))
	b.WriteString("\n")
	b.WriteString(trailer(ThreadTrailer, m.Thread))
	b.WriteString("\n")
	return b.String(), nil
}

// Commit stores all tracked paths, commits (also when nothing changed) and tags the commit with tag. It returns the
// commit hash. The tag must be a snapshot name of DESIGN 5.9 other than last-good, and must not exist yet.
func (r *Repo) Commit(ctx context.Context, tag string, msg Message) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.commitLocked(ctx, tag, msg)
}

// commitLocked is Commit for a caller that holds r.mu.
func (r *Repo) commitLocked(ctx context.Context, tag string, msg Message) (string, error) {
	if !isTagName(tag) || tag == LastGoodTag {
		return "", fmt.Errorf("无效的快照名 %q", tag)
	}
	text, err := msg.text()
	if err != nil {
		return "", err
	}
	if _, exists, err := r.tagCommit(ctx, tag); err != nil {
		return "", err
	} else if exists {
		return "", fmt.Errorf("快照 %s 已经存在", tag)
	}
	tree, err := r.snapshotTree(ctx)
	if err != nil {
		return "", fmt.Errorf("保存快照 %s 的内容失败：%w", tag, err)
	}
	parent, hasParent, err := r.revParse(ctx, "HEAD")
	if err != nil {
		return "", err
	}
	// The message is cleaned like `git commit --cleanup=whitespace` does.
	cleaned, err := r.runInput(ctx, strings.NewReader(text), "stripspace")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", tree}
	old := strings.Repeat("0", 40)
	if hasParent {
		args = append(args, "-p", parent)
		old = parent
	}
	out, err := r.runInput(ctx, bytes.NewReader(cleaned), append(args, "-F", "-")...)
	if err != nil {
		return "", fmt.Errorf("提交快照 %s 失败：%w", tag, err)
	}
	commit := strings.TrimSpace(string(out))
	if !objectName.MatchString(commit) {
		return "", fmt.Errorf("git commit-tree 返回了无效的提交 %q", commit)
	}
	if _, err := r.run(ctx, "update-ref", "HEAD", commit, old); err != nil {
		return "", fmt.Errorf("提交快照 %s 失败：%w", tag, err)
	}
	if _, err := r.run(ctx, "tag", "--end-of-options", tag, commit); err != nil {
		return "", fmt.Errorf("创建快照标签 %s 失败：%w", tag, err)
	}
	return commit, nil
}

// snapshotTree runs the scan of HelperCommand as bearbot and writes the tree of its entries. The scan output goes to
// a file that root creates and unlinks in the repository directory, so root reads only bytes the helper wrote, never
// a path of the work tree or of a bearbot directory. git unpack-objects --strict checks every object of the pack, the
// entries are checked (parseEntries, every object a blob), and the written tree must hold exactly the entries.
func (r *Repo) snapshotTree(ctx context.Context) (string, error) {
	f, err := os.CreateTemp(r.paths.SnapshotsGit, "scan-")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := os.Remove(f.Name()); err != nil {
		return "", err
	}
	if _, err := proc.RunChecked(ctx, r.helperSpec(f, "scan")); err != nil {
		return "", fmt.Errorf("以 %s 身份读取快照内容失败：%w", r.users.Bearbot.Name, err)
	}
	entries, pack, err := readScan(f)
	if err != nil {
		return "", err
	}
	if pack != nil {
		if _, err := r.runInput(ctx, pack, "unpack-objects", "-q", "--strict"); err != nil {
			return "", fmt.Errorf("导入快照对象失败：%w", err)
		}
	}
	if err := r.checkBlobs(ctx, entries); err != nil {
		return "", err
	}
	if _, err := r.run(ctx, "read-tree", "--empty"); err != nil {
		return "", err
	}
	var info strings.Builder
	for _, e := range entries {
		info.WriteString(e.record())
	}
	if _, err := r.runInput(ctx, strings.NewReader(info.String()), "update-index", "-z", "--index-info"); err != nil {
		return "", err
	}
	out, err := r.run(ctx, "write-tree")
	if err != nil {
		return "", err
	}
	tree := strings.TrimSpace(string(out))
	listing, err := r.run(ctx, "ls-tree", "-r", "-z", "--full-tree", "--end-of-options", tree)
	if err != nil {
		return "", err
	}
	rows, err := parseListing(listing, 3)
	if err != nil {
		return "", err
	}
	got := map[string]entry{}
	for _, row := range rows {
		got[row.path] = entry{mode: row.fields[0], sha: row.fields[2], path: row.path}
	}
	if err := sameEntries(entries, got, "快照的树"); err != nil {
		return "", err
	}
	return tree, nil
}

// readScan reads the scan output "<n>\n<entry list of n bytes><pack>" from f; pack is nil when there is none.
func readScan(f *os.File) ([]entry, io.Reader, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	size := info.Size()
	head := make([]byte, min(size, 32))
	if _, err := f.ReadAt(head, 0); err != nil {
		return nil, nil, err
	}
	nl := bytes.IndexByte(head, '\n')
	if nl <= 0 {
		return nil, nil, fmt.Errorf("快照扫描的输出没有长度行：%q", head)
	}
	n, err := strconv.ParseInt(string(head[:nl]), 10, 64)
	start := int64(nl + 1)
	if err != nil || n < 0 || n > size-start {
		return nil, nil, fmt.Errorf("快照扫描的输出长度行 %q 无效（输出共 %d 字节）", head[:nl], size)
	}
	list := make([]byte, n)
	if _, err := f.ReadAt(list, start); err != nil {
		return nil, nil, err
	}
	entries, err := parseEntries(list, TrackedPaths)
	if err != nil {
		return nil, nil, err
	}
	if rest := size - start - n; rest > 0 {
		return entries, io.NewSectionReader(f, start+n, rest), nil
	}
	return entries, nil, nil
}

// checkBlobs makes sure that the repository has every object of entries and that each is a blob.
func (r *Repo) checkBlobs(ctx context.Context, entries []entry) error {
	var shas []string
	for _, e := range entries {
		shas = append(shas, e.sha)
	}
	types, err := objectTypes(shas, func(stdin io.Reader, args ...string) ([]byte, error) {
		return r.runInput(ctx, stdin, args...)
	})
	if err != nil {
		return err
	}
	for sha, typ := range types {
		if typ != "blob" {
			return fmt.Errorf("快照内容的对象 %s 不在仓库中或者不是 blob（%s）", sha, typ)
		}
	}
	return nil
}

// Head returns the newest commit.
func (r *Repo) Head(ctx context.Context) (string, error) {
	commit, ok, err := r.revParse(ctx, "HEAD")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("快照仓库还没有提交")
	}
	return commit, nil
}

// MoveLastGood points last-good at the commit ref names.
func (r *Repo) MoveLastGood(ctx context.Context, ref string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	commit, err := r.Resolve(ctx, ref)
	if err != nil {
		return err
	}
	if _, err := r.run(ctx, "tag", "-f", "--end-of-options", LastGoodTag, commit); err != nil {
		return fmt.Errorf("移动 last-good 失败：%w", err)
	}
	return nil
}

// LastGood returns the commit last-good points at; ok is false when it does not exist yet.
func (r *Repo) LastGood(ctx context.Context) (string, bool, error) {
	return r.tagCommit(ctx, LastGoodTag)
}

// tags returns every tag name with its commit.
func (r *Repo) tags(ctx context.Context) (map[string]string, error) {
	out, err := r.run(ctx, "for-each-ref", "--format=%(objectname)%09%(refname:strip=2)", "refs/tags")
	if err != nil {
		return nil, err
	}
	tags := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		commit, name, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("unexpected for-each-ref line %q", line)
		}
		tags[name] = commit
	}
	return tags, nil
}

func maxSeq(tags map[string]string, pattern *regexp.Regexp) int {
	n := 0
	for name := range tags {
		if m := pattern.FindStringSubmatch(name); m != nil {
			v, err := strconv.Atoi(m[1])
			if err == nil && v > n {
				n = v
			}
		}
	}
	return n
}

// NextApply returns the next apply sequence number (1 for the first).
func (r *Repo) NextApply(ctx context.Context) (int, error) {
	tags, err := r.tags(ctx)
	if err != nil {
		return 0, err
	}
	return maxSeq(tags, applyTag) + 1, nil
}

// NextRollback returns the next rollback sequence number.
func (r *Repo) NextRollback(ctx context.Context) (int, error) {
	tags, err := r.tags(ctx)
	if err != nil {
		return 0, err
	}
	return maxSeq(tags, rollbackTag) + 1, nil
}

// NextResume returns the next resume sequence number.
func (r *Repo) NextResume(ctx context.Context) (int, error) {
	tags, err := r.tags(ctx)
	if err != nil {
		return 0, err
	}
	return maxSeq(tags, resumeTag) + 1, nil
}

// NextAttempt returns the next repair attempt number of an incident.
func (r *Repo) NextAttempt(ctx context.Context, id string) (int, error) {
	if !validIncidentID(id) {
		return 0, fmt.Errorf("无效的故障 id %q", id)
	}
	tags, err := r.tags(ctx)
	if err != nil {
		return 0, err
	}
	return maxSeq(tags, regexp.MustCompile(`^incident/`+regexp.QuoteMeta(id)+`/attempt-([0-9]+)$`)) + 1, nil
}

// nameOf picks the tag name of commit, ignoring last-good; empty when it has none.
func nameOf(tags map[string]string, commit string) string {
	var names []string
	for name, c := range tags {
		if c == commit && name != LastGoodTag {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return names[0]
}

// NameOf returns the snapshot name of commit (a tag other than last-good), or the commit itself when it has none
// (contracts 4 StatusInfo.runningSnapshot).
func (r *Repo) NameOf(ctx context.Context, commit string) (string, error) {
	tags, err := r.tags(ctx)
	if err != nil {
		return "", err
	}
	if name := nameOf(tags, commit); name != "" {
		return name, nil
	}
	return commit, nil
}

// Entry is one snapshot of the list.
type Entry struct {
	Ref       string
	Commit    string
	Time      time.Time
	Summary   string
	Requester string
	Thread    string
}

type logEntry struct {
	commit    string
	time      time.Time
	summary   string
	requester string
	thread    string
}

// history returns the commits reachable from HEAD, newest first; range limits it (for example "<a>..HEAD"). The
// fields are separated by NUL, which a commit message passed through argv cannot contain; with -z every record
// ends with NUL too.
func (r *Repo) history(ctx context.Context, rangeExpr string) ([]logEntry, error) {
	const fields = 5
	format := "--format=%H%x00%ct%x00%s%x00%(trailers:key=" + RequesterTrailer + ",valueonly,separator=%x2c)%x00%(trailers:key=" + ThreadTrailer + ",valueonly,separator=%x2c)"
	out, err := r.run(ctx, "log", "-z", format, "--end-of-options", rangeExpr)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	if out[len(out)-1] != 0 {
		return nil, fmt.Errorf("unexpected end of git log output %q", out)
	}
	all := strings.Split(string(out[:len(out)-1]), "\x00")
	if len(all)%fields != 0 {
		return nil, fmt.Errorf("unexpected git log output: %d fields", len(all))
	}
	var entries []logEntry
	for i := 0; i < len(all); i += fields {
		f := all[i : i+fields]
		sec, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("unexpected commit time %q", f[1])
		}
		entries = append(entries, logEntry{
			commit:    f[0],
			time:      time.Unix(sec, 0),
			summary:   f[2],
			requester: strings.TrimSpace(f[3]),
			thread:    strings.TrimSpace(f[4]),
		})
	}
	return entries, nil
}

// ListLimit is the number of snapshot references List returns (DESIGN 5.11).
const ListLimit = 20

// Listing is the result of List.
type Listing struct {
	// Entries are the newest ListLimit snapshot references, newest first (last-good not included).
	Entries []Entry
	// LastGood and LastGoodCommit describe last-good; both empty when it does not exist.
	LastGood       string
	LastGoodCommit string
	// Running and RunningCommit describe runningSnapshot; both empty when the state has none.
	Running       string
	RunningCommit string
	// DefaultTarget is the default rollback target ("apply/<n>^"); empty when there is no apply.
	DefaultTarget string
}

// List returns the newest snapshot references with time, summary and requester, plus last-good, runningSnapshot
// (the 40-hex commit from state.json, empty for none) and the default rollback target (DESIGN 5.11).
func (r *Repo) List(ctx context.Context, runningSnapshot string) (*Listing, error) {
	tags, err := r.tags(ctx)
	if err != nil {
		return nil, err
	}
	hist, err := r.history(ctx, "HEAD")
	if err != nil {
		return nil, err
	}
	byCommit := map[string][]string{}
	for name, c := range tags {
		if name != LastGoodTag {
			byCommit[c] = append(byCommit[c], name)
		}
	}
	l := &Listing{}
	for _, h := range hist {
		names := byCommit[h.commit]
		sort.Strings(names)
		for _, name := range names {
			if len(l.Entries) == ListLimit {
				break
			}
			l.Entries = append(l.Entries, Entry{Ref: name, Commit: h.commit, Time: h.time, Summary: h.summary, Requester: h.requester, Thread: h.thread})
		}
		if len(l.Entries) == ListLimit {
			break
		}
	}
	if c, ok := tags[LastGoodTag]; ok {
		l.LastGoodCommit = c
		l.LastGood = nameOf(tags, c)
		if l.LastGood == "" {
			l.LastGood = c
		}
	}
	if runningSnapshot != "" {
		l.RunningCommit = runningSnapshot
		l.Running = nameOf(tags, runningSnapshot)
		if l.Running == "" {
			l.Running = runningSnapshot
		}
	}
	if n := maxSeq(tags, applyTag); n > 0 {
		l.DefaultTarget = ApplyTag(n) + "^"
	}
	return l, nil
}

// Text formats the listing for the CLI `snapshot list` and `/bb snapshot list` (same text for both).
func (l *Listing) Text() string {
	var b strings.Builder
	if len(l.Entries) == 0 {
		b.WriteString("还没有快照\n")
	} else {
		fmt.Fprintf(&b, "最近的 %d 个快照（最新的在前）：\n", len(l.Entries))
		for _, e := range l.Entries {
			requester := e.Requester
			if requester == "" {
				requester = "无"
			}
			fmt.Fprintf(&b, "  %s  %s  %s  发起者：%s\n", e.Ref, e.Time.Format("2006-01-02 15:04:05"), e.Summary, requester)
		}
	}
	if l.LastGood == "" {
		b.WriteString("last-good：还没有\n")
	} else {
		fmt.Fprintf(&b, "last-good：%s（%s）\n", l.LastGood, shortHash(l.LastGoodCommit))
	}
	if l.Running == "" {
		b.WriteString("runningSnapshot：没有记录\n")
	} else {
		fmt.Fprintf(&b, "runningSnapshot：%s（%s）\n", l.Running, shortHash(l.RunningCommit))
	}
	if l.DefaultTarget == "" {
		b.WriteString("rollback 不指定快照时的默认目标：没有（还没有应用记录，回滚时需要指定快照）\n")
	} else {
		fmt.Fprintf(&b, "rollback 不指定快照时的默认目标：%s\n", l.DefaultTarget)
	}
	return b.String()
}

// DefaultRollbackTarget returns "apply/<n>^" for the largest n (DESIGN 5.12); an error when there is no apply.
func (r *Repo) DefaultRollbackTarget(ctx context.Context) (string, error) {
	tags, err := r.tags(ctx)
	if err != nil {
		return "", err
	}
	n := maxSeq(tags, applyTag)
	if n == 0 {
		return "", errors.New("还没有任何应用记录，请指定要回滚到的快照")
	}
	return ApplyTag(n) + "^", nil
}

// AppliesAfter returns the apply snapshots committed after target, oldest first, with their summaries (the applies
// a rollback to target reverts).
func (r *Repo) AppliesAfter(ctx context.Context, target string) ([]state.Reverted, error) {
	commit, err := r.Resolve(ctx, target)
	if err != nil {
		return nil, err
	}
	tags, err := r.tags(ctx)
	if err != nil {
		return nil, err
	}
	hist, err := r.history(ctx, commit+"..HEAD")
	if err != nil {
		return nil, err
	}
	var out []state.Reverted
	for i := len(hist) - 1; i >= 0; i-- {
		var names []string
		for name, c := range tags {
			if c == hist[i].commit && applyTag.MatchString(name) {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			out = append(out, state.Reverted{Ref: name, Summary: hist[i].summary})
		}
	}
	return out, nil
}

func (r *Repo) parentOrEmpty(ctx context.Context, commit string) (string, error) {
	parent, ok, err := r.revParse(ctx, commit+"^")
	if err != nil {
		return "", err
	}
	if ok {
		return parent, nil
	}
	out, err := r.run(ctx, "hash-object", "-t", "tree", "/dev/null")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// DiffStat returns `git diff --stat` between two snapshots as text for users; with ref2 empty it compares ref with
// its parent (DESIGN 5.11).
func (r *Repo) DiffStat(ctx context.Context, ref, ref2 string) (string, error) {
	to, err := r.Resolve(ctx, ref)
	if err != nil {
		return "", err
	}
	from := ""
	fromName := ref + "^"
	toName := ref
	if ref2 == "" {
		if from, err = r.parentOrEmpty(ctx, to); err != nil {
			return "", err
		}
	} else {
		from = to
		fromName = ref
		if to, err = r.Resolve(ctx, ref2); err != nil {
			return "", err
		}
		toName = ref2
	}
	out, err := r.run(ctx, "diff", "--stat=200", "--end-of-options", from, to)
	if err != nil {
		return "", err
	}
	stat := strings.TrimRight(string(out), "\n")
	if stat == "" {
		return fmt.Sprintf("%s 与 %s 之间没有差别\n", fromName, toName), nil
	}
	return fmt.Sprintf("%s → %s 的改动：\n%s\n", fromName, toName, stat), nil
}

// DiffStatPaths returns the raw `git diff --stat` between two snapshots limited to paths (relative to the work tree);
// empty when they do not differ there.
func (r *Repo) DiffStatPaths(ctx context.Context, from, to string, paths []string) (string, error) {
	a, err := r.Resolve(ctx, from)
	if err != nil {
		return "", err
	}
	b, err := r.Resolve(ctx, to)
	if err != nil {
		return "", err
	}
	args := []string{"diff", "--stat=200", "--end-of-options", a, b, "--"}
	for _, p := range paths {
		clean := filepath.Clean(p)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			return "", fmt.Errorf("路径 %q 必须是工作目录中的相对路径", p)
		}
		args = append(args, clean)
	}
	out, err := r.run(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// RestorePath checks a path given to restore: it is relative to the work tree (/bearbot) and one of the restart
// effective paths. It returns the cleaned path.
func (r *Repo) RestorePath(p string) (string, error) { return restorePath(p) }

func restorePath(p string) (string, error) {
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("restore 的路径必须是相对 /bearbot 的路径（例如 workspace/.pi/extensions/x.ts），收到绝对路径 %s", p)
	}
	rel := filepath.Clean(p)
	ok := rel == "workspace/.pi" || strings.HasPrefix(rel, "workspace/.pi/") || rel == "profile/agent/settings.json" || rel == "profile/agent/models.json"
	if !ok {
		return "", fmt.Errorf("只能恢复重启生效路径（workspace/.pi/**、profile/agent/settings.json、profile/agent/models.json）：%s", p)
	}
	return rel, nil
}

// RestoreResult describes a finished restore.
type RestoreResult struct {
	// Commit is the restored snapshot.
	Commit string
	// Restored lists the changed files, relative to the work tree.
	Restored []string
	// NpmDirs lists the extension directories where npm ci or npm install ran, relative to the work tree.
	NpmDirs []string
	// Text describes the restore for users.
	Text string
}

// Restore brings restart effective paths back to the snapshot ref (DESIGN 5.9). The restore of HelperCommand runs as
// bearbot, so the files it writes belong to bearbot: it compares the current files with the commit, writes the
// changed ones with `git restore --source=<commit>` and removes the files that the snapshot does not have; left out
// paths (a nested .git, node_modules, databases, logs) stay as they are. Then npm ci (with a lock file) or npm
// install runs as bearbot in every directory whose package.json or lock file changed. paths empty means all restart
// effective paths; others are checked with RestorePath. AGENTS.md and CLASSIFIER.md are never restored.
func (r *Repo) Restore(ctx context.Context, ref string, paths []string) (*RestoreResult, error) {
	commit, err := r.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	var targets []string
	if len(paths) == 0 {
		targets = append(targets, RestartEffectivePaths...)
	}
	for _, p := range paths {
		rel, err := r.RestorePath(p)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(targets, rel) {
			targets = append(targets, rel)
		}
	}
	changed, npmDirs, err := r.restoreFiles(ctx, commit, targets)
	if err != nil {
		return nil, err
	}
	res := &RestoreResult{Commit: commit, Restored: changed}
	if len(changed) == 0 {
		res.Text = fmt.Sprintf("重启生效路径的内容与 %s 相同，没有需要恢复的文件\n", ref)
		return res, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "已把 %d 个文件恢复到 %s（%s）：\n", len(changed), ref, shortHash(commit))
	for _, f := range changed {
		fmt.Fprintf(&b, "  %s\n", f)
	}
	for _, d := range npmDirs {
		cmd, err := r.installDeps(ctx, d)
		if err != nil {
			return nil, err
		}
		res.NpmDirs = append(res.NpmDirs, d.dir)
		fmt.Fprintf(&b, "已在 %s 执行 %s\n", d.dir, cmd)
	}
	res.Text = b.String()
	return res, nil
}

// restoreFiles runs the restore of HelperCommand as bearbot holding r.mu and returns the changed files and the npm
// directories it reported.
func (r *Repo) restoreFiles(ctx context.Context, commit string, targets []string) ([]string, []npmDir, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res, err := proc.RunChecked(ctx, r.helperSpec(nil, "restore", append([]string{"--commit=" + commit, "--"}, targets...)...))
	if err != nil {
		return nil, nil, fmt.Errorf("以 %s 身份恢复文件失败：%w", r.users.Bearbot.Name, err)
	}
	return parseRestore(res.Stdout, targets)
}

// parseRestore reads the output of the helper's restore and checks it: the changed paths are sorted, unique and
// below targets (checkEntryPath); every npm directory is the directory of a changed package.json or lock file, with
// npm ci or npm install.
func parseRestore(out []byte, targets []string) ([]string, []npmDir, error) {
	if len(out) == 0 {
		return nil, nil, nil
	}
	if out[len(out)-1] != 0 {
		return nil, nil, errors.New("恢复的输出没有以 NUL 结束")
	}
	var changed []string
	var npm []npmDir
	for _, rec := range strings.Split(string(out[:len(out)-1]), "\x00") {
		kind, rest, _ := strings.Cut(rec, "\t")
		switch kind {
		case "C":
			if err := checkEntryPath(rest, targets); err != nil {
				return nil, nil, err
			}
			if len(changed) > 0 && changed[len(changed)-1] >= rest {
				return nil, nil, fmt.Errorf("恢复的输出中的路径没有按顺序排列：%q 在 %q 之后", rest, changed[len(changed)-1])
			}
			changed = append(changed, rest)
		case "N":
			sub, dir, ok := strings.Cut(rest, "\t")
			if !ok || (sub != "ci" && sub != "install") {
				return nil, nil, fmt.Errorf("恢复的输出中有无效的 npm 记录 %q", rec)
			}
			npm = append(npm, npmDir{dir: dir, sub: sub})
		default:
			return nil, nil, fmt.Errorf("恢复的输出中有无效的记录 %q", rec)
		}
	}
	deps := dependencyDirs(changed)
	for i, d := range npm {
		if !slices.Contains(deps, d.dir) || (i > 0 && npm[i-1].dir >= d.dir) {
			return nil, nil, fmt.Errorf("恢复的输出中的 npm 目录 %q 不是改动的 package.json 或 lock 文件所在的目录，或者重复", d.dir)
		}
	}
	return changed, npm, nil
}

// installDeps runs npm d.sub as bearbot in the directory d.dir of the work tree; the directory is entered by the npm
// process itself after it became bearbot.
func (r *Repo) installDeps(ctx context.Context, d npmDir) (string, error) {
	_, err := proc.RunChecked(ctx, proc.RunSpec{
		Path:       r.node,
		Args:       []string{r.npm, d.sub},
		Env:        r.paths.ToolEnv(),
		Dir:        filepath.Join(r.paths.SnapshotWorkTree, d.dir),
		Credential: r.users.Credential(r.users.Bearbot),
		Timeout:    NpmTimeout,
	})
	if err != nil {
		return "", fmt.Errorf("在 %s 执行 npm %s 失败：%w", d.dir, d.sub, err)
	}
	return "npm " + d.sub, nil
}
