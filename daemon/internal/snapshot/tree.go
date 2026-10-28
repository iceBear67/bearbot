package snapshot

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

// TrackedPaths are the paths, relative to the work tree, that every snapshot stores (DESIGN 5.9): the workspace
// documents and the restart effective paths. Every file below them is stored file by file, apart from what leftOut
// names; no .gitignore, .gitattributes or nested .git in the work tree changes that.
var TrackedPaths = slices.Concat(DocumentPaths, RestartEffectivePaths)

// Modes of tree entries. A snapshot holds only files, executable files and symlinks; a directory with its own .git
// is stored file by file, so a gitlink (modeGitlink) never enters a snapshot.
const (
	modeFile    = "100644"
	modeExec    = "100755"
	modeSymlink = "120000"
	modeGitlink = "160000"
)

// Attributes is the content of info/attributes, which every git command of the snapshots also uses as
// core.attributesFile. info/attributes takes precedence over the .gitattributes files of the work tree; these
// settings turn off everything that would make a blob differ from the file (end of line conversion, filters, ident,
// encodings). diff is reset to unspecified, so `git diff --stat` still counts the lines of text files.
const Attributes = "* -text -eol -filter -ident -working-tree-encoding -merge !diff\n"

var objectName = regexp.MustCompile(`^[0-9a-f]{40}$`)

// entry is one tree entry: mode, blob and path relative to the work tree.
type entry struct {
	mode string
	sha  string
	path string
}

// record returns the entry as a record of `git update-index -z --index-info`, which is also the record of the scan
// output: "<mode> <sha>\t<path>\0".
func (e entry) record() string { return e.mode + " " + e.sha + "\t" + e.path + "\x00" }

// leftOut reports whether the directory entry name is not stored, with everything below it when it is a directory
// (isDir: a real directory, not a symlink): .git (a directory or a file; git cannot store a path component that
// equals .git in any case), node_modules directories, *.db and *.log.
func leftOut(name string, isDir bool) bool {
	return strings.EqualFold(name, ".git") || (isDir && name == "node_modules") ||
		strings.HasSuffix(name, ".db") || strings.HasSuffix(name, ".log")
}

// below reports whether rel is root or a path inside root.
func below(rel, root string) bool { return rel == root || strings.HasPrefix(rel, root+"/") }

func belowAny(rel string, roots []string) bool {
	return slices.ContainsFunc(roots, func(root string) bool { return below(rel, root) })
}

// checkEntryPath checks a path received from the helper: it is below one of roots, has no empty, "." or ".."
// component, and no component that the scan leaves out (every component but the last is a directory).
func checkEntryPath(rel string, roots []string) error {
	if !belowAny(rel, roots) {
		return fmt.Errorf("路径 %q 不在快照保存的路径中", rel)
	}
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("无效的路径 %q", rel)
		}
		if leftOut(part, i < len(parts)-1) {
			return fmt.Errorf("路径 %q 属于快照不保存的内容（.git、node_modules、*.db、*.log）", rel)
		}
	}
	return nil
}

// parseEntries reads the entry list of the scan and checks every record: mode, object name, path (checkEntryPath)
// and that no path occurs twice.
func parseEntries(data []byte, roots []string) ([]entry, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if data[len(data)-1] != 0 {
		return nil, errors.New("快照内容列表没有以 NUL 结束")
	}
	seen := map[string]bool{}
	var out []entry
	for _, rec := range strings.Split(string(data[:len(data)-1]), "\x00") {
		meta, path, ok := strings.Cut(rec, "\t")
		mode, sha, ok2 := strings.Cut(meta, " ")
		if !ok || !ok2 {
			return nil, fmt.Errorf("无法解析快照内容列表中的记录 %q", rec)
		}
		if mode != modeFile && mode != modeExec && mode != modeSymlink {
			return nil, fmt.Errorf("快照内容列表中 %q 的类型 %q 无效", path, mode)
		}
		if !objectName.MatchString(sha) {
			return nil, fmt.Errorf("快照内容列表中 %q 的对象名 %q 无效", path, sha)
		}
		if err := checkEntryPath(path, roots); err != nil {
			return nil, err
		}
		if seen[path] {
			return nil, fmt.Errorf("快照内容列表中 %q 出现了两次", path)
		}
		seen[path] = true
		out = append(out, entry{mode: mode, sha: sha, path: path})
	}
	return out, nil
}

// listedRow is one record of `git ls-tree -z` or `git ls-files -s -z`: the fields before the tab and the path.
type listedRow struct {
	fields []string
	path   string
}

// parseListing reads NUL terminated "<field> <field> ...\t<path>" records with exactly fields fields.
func parseListing(data []byte, fields int) ([]listedRow, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if data[len(data)-1] != 0 {
		return nil, fmt.Errorf("unexpected end of git output %q", data)
	}
	var rows []listedRow
	for _, rec := range strings.Split(string(data[:len(data)-1]), "\x00") {
		meta, path, ok := strings.Cut(rec, "\t")
		f := strings.Split(meta, " ")
		if !ok || len(f) != fields {
			return nil, fmt.Errorf("unexpected git output record %q", rec)
		}
		rows = append(rows, listedRow{fields: f, path: path})
	}
	return rows, nil
}

// sameEntries compares what git has (got, by path) with the entries that were handed to it; git leaves out some
// paths without failing (a component that NTFS treats as .git, for example), so every difference is an error.
func sameEntries(want []entry, got map[string]entry, what string) error {
	for _, e := range want {
		g, ok := got[e.path]
		if !ok {
			return fmt.Errorf("git 没有把 %s 写入%s（git 不接受这个路径）", e.path, what)
		}
		if g != e {
			return fmt.Errorf("%s中的 %s 是 %s %s，应为 %s %s", what, e.path, g.mode, g.sha, e.mode, e.sha)
		}
	}
	if len(got) != len(want) {
		for path := range got {
			if !slices.ContainsFunc(want, func(e entry) bool { return e.path == path }) {
				return fmt.Errorf("%s中多出了 %s", what, path)
			}
		}
	}
	return nil
}

// objectTypes returns the type of every object of shas, "missing" for one that does not exist, with
// `git cat-file --batch-check` run through run (which adds the repository).
func objectTypes(shas []string, run func(stdin io.Reader, args ...string) ([]byte, error)) (map[string]string, error) {
	unique := slices.Compact(slices.Sorted(slices.Values(shas)))
	if len(unique) == 0 {
		return nil, nil
	}
	out, err := run(strings.NewReader(strings.Join(unique, "\n")+"\n"), "cat-file", "--batch-check=%(objectname) %(objecttype)", "--buffer")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != len(unique) {
		return nil, fmt.Errorf("git cat-file 返回了 %d 行，应为 %d 行", len(lines), len(unique))
	}
	types := map[string]string{}
	for i, line := range lines {
		sha, typ, ok := strings.Cut(line, " ")
		if !ok || sha != unique[i] {
			return nil, fmt.Errorf("git cat-file 对 %s 返回了 %q", unique[i], line)
		}
		types[sha] = typ
	}
	return types, nil
}

// quoteC quotes path in the C style that `git hash-object --stdin-paths` unquotes, so any byte but NUL can occur in
// it (a newline or a trailing carriage return included).
func quoteC(path string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(path); i++ {
		c := path[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, "\\%03o", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
