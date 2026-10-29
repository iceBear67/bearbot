// Package classifierturn keeps the CLASSIFIER.md turn record of DESIGN 7.10: at the start of a member turn the
// daemon (root) records the type and the content of CLASSIFIER.md; at the end it compares and, when the file was a
// regular file or missing at the start, restores the start state. A restore writes a temporary file in the workspace
// and replaces the directory entry with rename(2), so a hard link or symlink put in place of CLASSIFIER.md never
// leads to another file being written. Notifications and logging are left to the caller.
package classifierturn

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	udiff "github.com/aymanbagabas/go-udiff"
	"golang.org/x/sys/unix"

	"bearbot/daemon/internal/layout"
)

// FileType is the type of CLASSIFIER.md found by lstat.
type FileType string

// File types.
const (
	TypeMissing   FileType = "missing"
	TypeRegular   FileType = "regular"
	TypeSymlink   FileType = "symlink"
	TypeDirectory FileType = "directory"
	TypeOther     FileType = "other"
)

// FileState is what the daemon knows about CLASSIFIER.md at one moment.
type FileState struct {
	Type FileType `json:"type"`
	// Size is the content length of a regular file, the target length of a symlink, otherwise the lstat size.
	Size int64 `json:"size"`
	// Nlink is the hard link count of a regular file.
	Nlink uint64 `json:"nlink"`
	// LinkTarget is the target of a symlink.
	LinkTarget string `json:"linkTarget,omitempty"`
	// Content is the content of a regular file (stored in current.content, not in current.json): all of it, or the
	// first bytes when Truncated.
	Content []byte `json:"-"`
	// Truncated is true when a read with a byte limit stopped before the end of the file; Size is the real size then.
	Truncated bool `json:"-"`
}

// Summary describes the state in one line for logs and notifications.
func (s FileState) Summary() string {
	switch s.Type {
	case TypeMissing:
		return "不存在"
	case TypeRegular:
		sum := sha256.Sum256(s.Content)
		if s.Truncated {
			return fmt.Sprintf("普通文件，%d 字节，只读取了前 %d 字节，这部分的 sha256 %s", s.Size, len(s.Content), hex.EncodeToString(sum[:])[:12])
		}
		return fmt.Sprintf("普通文件，%d 字节，sha256 %s", s.Size, hex.EncodeToString(sum[:])[:12])
	case TypeSymlink:
		return "符号链接，指向 " + s.LinkTarget
	case TypeDirectory:
		return "目录"
	default:
		return "特殊文件（不是普通文件、目录或符号链接）"
	}
}

// Record is the start record of the open turn (daemon/classifier-turn/current.json + current.content).
// Senders are the senders of the turn's trigger messages: one for a turn of chat messages, none for a prompt() turn.
type Record struct {
	TurnID    string    `json:"turnId"`
	StartedAt time.Time `json:"startedAt"`
	Senders   []string  `json:"senders"`
	File      FileState `json:"file"`
}

// StartResult is the response of POST /v1/classifier/turn {phase:"start"} (contracts 6).
type StartResult struct {
	Exists  bool    `json:"exists"`
	Regular bool    `json:"regular"`
	Size    int64   `json:"size"`
	Content *string `json:"content,omitempty"`
}

// RestoreRecord is the "restore" line of logs/agent/classifier.jsonl (contracts 8.2). Before is the state found at
// the end of the turn, After the state after the daemon acted (equal to Before when nothing was restored).
type RestoreRecord struct {
	Type          string    `json:"type"`
	Time          time.Time `json:"time"`
	TurnID        string    `json:"turnId"`
	BeforeType    FileType  `json:"beforeType"`
	AfterType     FileType  `json:"afterType"`
	BeforeSummary string    `json:"beforeSummary"`
	AfterSummary  string    `json:"afterSummary"`
	Senders       []string  `json:"senders"`
}

// EndResult is the outcome of the end comparison.
type EndResult struct {
	// Changed is true when CLASSIFIER.md differs from the start record.
	Changed bool
	// Restored is true when the start state was written back.
	Restored bool
	// Detail describes the change in Chinese with the unified diff of the reverted content (at most DiffLimit
	// characters); empty when nothing changed.
	Detail string
	// Record is the classifier.jsonl restore line; nil when nothing changed.
	Record *RestoreRecord
	// Turn is the start record that was closed.
	Turn *Record
}

// DiffLimit is the maximal length (in characters) of the diff in Detail (DESIGN 7.10).
const DiffLimit = 4000

// ErrNoTurn means there is no open turn record.
var ErrNoTurn = errors.New("没有未结束的 CLASSIFIER.md 回合记录")

// Recorder reads, compares and restores CLASSIFIER.md.
type Recorder struct {
	paths layout.Paths
	users *layout.Users
}

// New returns a Recorder for paths.ClassifierMD with the record in paths.ClassifierTurnDir (which must exist).
func New(paths layout.Paths, users *layout.Users) *Recorder {
	return &Recorder{paths: paths, users: users}
}

func (r *Recorder) recordPath() string {
	return filepath.Join(r.paths.ClassifierTurnDir, "current.json")
}

func (r *Recorder) contentPath() string {
	return filepath.Join(r.paths.ClassifierTurnDir, "current.content")
}

// Capture reads the state of CLASSIFIER.md: lstat, then a regular file is opened with O_NOFOLLOW|O_NONBLOCK and read;
// the type comes from fstat of the opened file, so a file swapped in between is never read through a link and a FIFO
// never blocks. A regular file is read completely when limit is negative, otherwise at most limit bytes (Truncated
// tells whether the file is longer).
func (r *Recorder) Capture(limit int64) (FileState, error) {
	path := r.paths.ClassifierMD
	for range 5 {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return FileState{Type: TypeMissing}, nil
		}
		if err != nil {
			return FileState{}, fmt.Errorf("无法读取 %s 的状态：%w", path, err)
		}
		mode := info.Mode()
		switch {
		case mode&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.EINVAL) {
				continue
			}
			if err != nil {
				return FileState{}, fmt.Errorf("无法读取符号链接 %s：%w", path, err)
			}
			return FileState{Type: TypeSymlink, Size: int64(len(target)), LinkTarget: target}, nil
		case mode.IsDir():
			return FileState{Type: TypeDirectory, Size: info.Size()}, nil
		case mode.IsRegular():
			st, retry, err := readRegular(path, limit)
			if err != nil {
				return FileState{}, err
			}
			if retry {
				continue
			}
			return st, nil
		default:
			return FileState{Type: TypeOther, Size: info.Size()}, nil
		}
	}
	return FileState{}, fmt.Errorf("%s 在读取期间不断变化", path)
}

// readRegular opens and reads a file that lstat reported as regular, completely when limit is negative, otherwise at
// most limit bytes. retry is true when the entry changed between lstat and open.
func readRegular(path string, limit int64) (FileState, bool, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOENT) {
			return FileState{}, true, nil
		}
		return FileState{}, false, fmt.Errorf("无法打开 %s：%w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return FileState{}, false, fmt.Errorf("无法读取 %s 的状态：%w", path, err)
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
	case unix.S_IFDIR:
		return FileState{Type: TypeDirectory, Size: st.Size}, false, nil
	default:
		return FileState{Type: TypeOther, Size: st.Size}, false, nil
	}
	if limit < 0 {
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(f); err != nil {
			return FileState{}, false, fmt.Errorf("读取 %s 失败：%w", path, err)
		}
		content := buf.Bytes()
		return FileState{Type: TypeRegular, Size: int64(len(content)), Nlink: uint64(st.Nlink), Content: content}, false, nil
	}
	content, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return FileState{}, false, fmt.Errorf("读取 %s 失败：%w", path, err)
	}
	fs := FileState{Type: TypeRegular, Size: int64(len(content)), Nlink: uint64(st.Nlink), Content: content}
	if int64(len(content)) > limit {
		// The file may have grown while it was read.
		fs.Content, fs.Truncated, fs.Size = content[:limit], true, max(st.Size, limit+1)
	}
	return fs, false, nil
}

// Pending returns the open turn record, or nil when there is none.
func (r *Recorder) Pending() (*Record, error) {
	data, err := os.ReadFile(r.recordPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("无法解析 %s：%w", r.recordPath(), err)
	}
	if rec.File.Type == TypeRegular {
		content, err := os.ReadFile(r.contentPath())
		if err != nil {
			return nil, fmt.Errorf("回合记录缺少 CLASSIFIER.md 的内容：%w", err)
		}
		if int64(len(content)) != rec.File.Size {
			return nil, fmt.Errorf("回合记录中 CLASSIFIER.md 的内容长度为 %d，记录的大小为 %d", len(content), rec.File.Size)
		}
		rec.File.Content = content
	}
	return &rec, nil
}

// Start records the state of CLASSIFIER.md for the member turn turnID (DESIGN 7.10 "回合开始"); a regular file is
// read completely, whatever its size. An open record is an error: the previous turn has to be ended first. The
// answer has the content only when the file is not larger than rulesMaxBytes ([classifier] rules_max_bytes):
// bearbot-im uses the content only then.
func (r *Recorder) Start(turnID string, senders []string, rulesMaxBytes int64) (*StartResult, *Record, error) {
	if turnID == "" {
		return nil, nil, errors.New("turnId 不能为空")
	}
	if pending, err := r.Pending(); err != nil {
		return nil, nil, err
	} else if pending != nil {
		return nil, nil, fmt.Errorf("回合 %s 的 CLASSIFIER.md 记录还没有结束", pending.TurnID)
	}
	st, err := r.Capture(-1)
	if err != nil {
		return nil, nil, err
	}
	rec := &Record{TurnID: turnID, StartedAt: time.Now(), Senders: append([]string{}, senders...), File: st}
	if st.Type == TypeRegular {
		if err := writeAtomic(r.contentPath(), st.Content); err != nil {
			return nil, nil, err
		}
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return nil, nil, err
	}
	if err := writeAtomic(r.recordPath(), data); err != nil {
		return nil, nil, err
	}
	res := &StartResult{Exists: st.Type != TypeMissing, Regular: st.Type == TypeRegular, Size: st.Size}
	if st.Type == TypeRegular && st.Size <= rulesMaxBytes {
		content := string(st.Content)
		res.Content = &content
	}
	return res, rec, nil
}

func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func changed(start, end FileState) bool {
	if start.Type != end.Type {
		return true
	}
	switch start.Type {
	case TypeRegular:
		return !bytes.Equal(start.Content, end.Content) || start.Nlink != end.Nlink
	case TypeSymlink:
		return start.LinkTarget != end.LinkTarget
	default:
		return false
	}
}

// End compares CLASSIFIER.md with the open record and restores the start state when it was a regular file or
// missing (DESIGN 7.10 "回合结束"); then the record is removed. turnID must match the record; empty accepts any
// record (the daemon's own comparison after an exit or at startup). Without a record it returns ErrNoTurn. The end
// state is read up to the start size plus 4*DiffLimit bytes: enough to compare and to show the diff, and a file the
// turn made huge (for example a sparse file) is never read into memory.
func (r *Recorder) End(turnID string) (*EndResult, error) {
	rec, err := r.Pending()
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, ErrNoTurn
	}
	if turnID != "" && turnID != rec.TurnID {
		return nil, fmt.Errorf("未结束的回合是 %s，不是 %s", rec.TurnID, turnID)
	}
	limit := 4 * int64(DiffLimit)
	if rec.File.Type == TypeRegular {
		limit += rec.File.Size
	}
	end, err := r.Capture(limit)
	if err != nil {
		return nil, err
	}
	res := &EndResult{Turn: rec}
	if changed(rec.File, end) {
		res.Changed = true
		after := end
		if rec.File.Type == TypeRegular || rec.File.Type == TypeMissing {
			if err := r.restore(rec.File, end); err != nil {
				return nil, err
			}
			res.Restored = true
			if after, err = r.Capture(limit); err != nil {
				return nil, err
			}
		}
		res.Detail = detail(rec, end, after, res.Restored)
		res.Record = &RestoreRecord{
			Type:          "restore",
			Time:          time.Now(),
			TurnID:        rec.TurnID,
			BeforeType:    end.Type,
			AfterType:     after.Type,
			BeforeSummary: end.Summary(),
			AfterSummary:  after.Summary(),
			Senders:       rec.Senders,
		}
	}
	if err := os.Remove(r.recordPath()); err != nil {
		return nil, err
	}
	if err := os.Remove(r.contentPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return res, nil
}

func (r *Recorder) restore(start, end FileState) error {
	path := r.paths.ClassifierMD
	if end.Type == TypeDirectory {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("删除目录 %s 失败：%w", path, err)
		}
	}
	if start.Type == TypeMissing {
		if end.Type != TypeDirectory {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("删除 %s 失败：%w", path, err)
			}
		}
		return syncDir(r.paths.WorkspaceDir)
	}
	f, err := os.CreateTemp(r.paths.WorkspaceDir, ".CLASSIFIER.md.restore-*")
	if err != nil {
		return fmt.Errorf("在 %s 中创建临时文件失败：%w", r.paths.WorkspaceDir, err)
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("恢复 %s 失败：%w", path, err)
	}
	if _, err := f.Write(start.Content); err != nil {
		return fail(err)
	}
	if err := f.Chmod(0o664); err != nil {
		return fail(err)
	}
	if !r.users.Rootless {
		if err := f.Chown(int(r.users.Bearbot.UID), int(r.users.Bearbot.GID)); err != nil {
			return fail(err)
		}
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("恢复 %s 失败：%w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("恢复 %s 失败：%w", path, err)
	}
	return syncDir(r.paths.WorkspaceDir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func textOf(s FileState) string {
	if s.Type == TypeRegular {
		return string(s.Content)
	}
	return ""
}

func truncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit]) + fmt.Sprintf("\n…（差异超过 %d 字符，后面的部分已截断）\n", limit)
}

func detail(rec *Record, end, after FileState, restored bool) string {
	var b strings.Builder
	if restored {
		fmt.Fprintf(&b, "CLASSIFIER.md 在 member 回合 %s 中被改动，daemon 已把它恢复为回合开始时的状态。\n", rec.TurnID)
	} else {
		fmt.Fprintf(&b, "CLASSIFIER.md 在 member 回合 %s 中有变化。回合开始时它不是普通文件，daemon 没有恢复，只报告这次变化。\n", rec.TurnID)
	}
	if len(rec.Senders) > 0 {
		fmt.Fprintf(&b, "本轮的触发消息发送者：%s\n", strings.Join(rec.Senders, "、"))
	}
	fmt.Fprintf(&b, "回合开始时：%s\n", rec.File.Summary())
	fmt.Fprintf(&b, "回合结束时：%s\n", end.Summary())
	if end.Truncated {
		fmt.Fprintf(&b, "回合结束时文件有 %d 字节，daemon 只读取了前 %d 字节用于比较和生成差异。\n", end.Size, len(end.Content))
	}
	if restored {
		fmt.Fprintf(&b, "恢复之后：%s\n", after.Summary())
	}
	diff := udiff.Unified("回合开始时", "回合结束时", textOf(rec.File), textOf(end))
	if diff != "" {
		if restored {
			b.WriteString("被撤销的改动：\n")
		} else {
			b.WriteString("内容差异：\n")
		}
		b.WriteString(truncateRunes(diff, DiffLimit))
	} else if end.Type == TypeRegular && rec.File.Type == TypeRegular {
		fmt.Fprintf(&b, "内容相同，硬链接数从 %d 变为 %d（文件被链接到了其他路径）。\n", rec.File.Nlink, end.Nlink)
	}
	return b.String()
}
