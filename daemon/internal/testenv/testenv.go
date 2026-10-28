// Package testenv builds the rootless test environment of contracts section 10: a root prefix R under /tmp with
// symlinks into the repository, the three mount point directories, a bearbot.toml written from a template, and
// helpers to build, start and stop bearbotd and to call its HTTP APIs over the unix sockets.
package testenv

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"bearbot/daemon/internal/layout"
)

// RepoRoot walks up from the working directory to the directory that contains DESIGN.md.
func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "DESIGN.md")); err == nil {
			return dir, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no DESIGN.md in the working directory or any parent")
		}
		dir = parent
	}
}

// Env is one test root prefix.
type Env struct {
	// Root is R, a directory created with os.MkdirTemp("/tmp", "bb").
	Root string
	// Repo is the repository root.
	Repo  string
	Paths layout.Paths
	// ConfigPath is R/bearbot/config/bearbot.toml.
	ConfigPath string
}

// symlinks of contracts 10: path under R -> target. Targets starting with "@" are relative to the repository.
var symlinks = [][2]string{
	{"/opt/bearbot/gateway", "@gateway"},
	{"/opt/bearbot/extensions", "@extensions"},
	{"/opt/bearbot/sdk", "@sdk"},
	{"/opt/bearbot/skills", "@skills"},
	{"/opt/bearbot/recovery-profile", "@recovery-profile"},
	{"/opt/bearbot/toolchain", "@"},
	{"/usr/local/lib/node_modules/@earendil-works/pi-coding-agent", "@node_modules/@earendil-works/pi-coding-agent"},
	{"/usr/local/bin/pi", "@node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js"},
	{"/usr/local/bin/python3.14", "/usr/bin/python3.14"},
	{"/usr/local/bin/uv", "/usr/bin/uv"},
	{"/usr/local/bin/uvx", "/usr/bin/uvx"},
}

// New creates the root prefix with the symlinks, the directories R/bearbot/{profile,workspace,config} and
// R/home/bearbot, and writes DefaultConfig. R is removed when the test ends.
func New(t testing.TB) *Env {
	t.Helper()
	repo, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "bb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove %s: %v", root, err)
		}
	})
	for _, l := range symlinks {
		link := filepath.Join(root, l[0])
		target := l[1]
		if target[0] == '@' {
			target = filepath.Join(repo, target[1:])
		} else if _, err := os.Stat(target); err != nil {
			t.Fatalf("the test environment needs %s: %v", target, err)
		}
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"/bearbot/profile", "/bearbot/workspace", "/bearbot/config", "/home/bearbot"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := layout.NewPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	e := &Env{Root: root, Repo: repo, Paths: paths, ConfigPath: paths.DefaultConfigFile}
	e.WriteConfig(t, DefaultConfig())
	return e
}

// Users returns the rootless users (every child runs as the test's own user).
func (e *Env) Users(t testing.TB) *layout.Users {
	t.Helper()
	u, err := layout.LookupUsers(true)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// RequireGateway fails the test when the gateway was not built.
func (e *Env) RequireGateway(t testing.TB) {
	t.Helper()
	if _, err := os.Stat(e.Paths.GatewayMain); err != nil {
		t.Fatalf("%s 不存在：请先在仓库根目录执行 npm run build -w gateway（%v）", filepath.Join(e.Repo, "gateway/dist/main.js"), err)
	}
}

// RequireRepoFile fails the test when a file of another component (relative to the repository) is missing.
func (e *Env) RequireRepoFile(t testing.TB, rel string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(e.Repo, rel)); err != nil {
		t.Fatalf("这个测试需要仓库中的 %s：%v", rel, err)
	}
}

// WriteFile writes a file under R (rel is an absolute DESIGN.md path such as "/bearbot/workspace/x"), creating
// parent directories.
func (e *Env) WriteFile(t testing.TB, rel string, content string) string {
	t.Helper()
	path := filepath.Join(e.Root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// String describes the environment for failure messages.
func (e *Env) String() string { return fmt.Sprintf("testenv R=%s repo=%s", e.Root, e.Repo) }
