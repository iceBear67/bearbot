package e2e

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"bearbot/daemon/internal/snapshot"
	"bearbot/daemon/internal/testenv"
)

// gitIn runs the system git in dir for the test itself, without the user's git configuration.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=e2e", "-c", "user.email=e2e@localhost", "-c", "commit.gpgSign=false", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// TestSnapshotStoresClonedPackage (DESIGN 5.9): an apply stores a package cloned into .pi/git file by file, although
// its own .gitignore and workspace/.gitignore hide it from git and it has its own .git; the rollback restores its
// files through the hidden helper subcommand of bearbotd and leaves its .git alone. The helper subcommand is not in
// the help text.
func TestSnapshotStoresClonedPackage(t *testing.T) {
	help, err := exec.Command(testenv.DaemonBinary(t), "--help").CombinedOutput()
	if err != nil || strings.Contains(string(help), snapshot.HelperCommand) {
		t.Fatalf("bearbotd --help: %v\n%s", err, help)
	}
	b := newBed(t, nil)
	b.start()
	b.waitState("running", 90*time.Second)

	rel := "workspace/.pi/git/github.com/e2e/pkg"
	pkg := b.env.Paths.SnapshotWorkTree + "/" + rel
	b.env.WriteFile(t, "/bearbot/workspace/.gitignore", ".pi/\n")
	b.env.WriteFile(t, "/bearbot/"+rel+"/README.md", "readme\n")
	b.env.WriteFile(t, "/bearbot/"+rel+"/.gitignore", "*\n")
	gitIn(t, pkg, "init", "-q")
	gitIn(t, pkg, "add", "-A", "--force")
	gitIn(t, pkg, "commit", "-q", "-m", "package")
	b.apply("克隆的包", "apply/1")

	files := b.gitFiles("apply/1")
	if !contains(files, rel+"/README.md") || !contains(files, rel+"/.gitignore") {
		t.Fatalf("apply/1 files %v", files)
	}
	for _, f := range files {
		if strings.Contains(f, "/.git/") {
			t.Fatalf("apply/1 stores %s", f)
		}
	}
	tree, err := exec.Command("git", "--git-dir="+b.env.Paths.SnapshotsGit, "ls-tree", "-r", "apply/1").Output()
	if err != nil || strings.Contains(string(tree), "160000") {
		t.Fatalf("apply/1 tree has a gitlink: %v\n%s", err, tree)
	}

	if err := os.Remove(pkg + "/README.md"); err != nil {
		t.Fatal(err)
	}
	b.env.WriteFile(t, "/bearbot/"+rel+"/extra.txt", "extra\n")
	out, stderr, code := b.cli("rollback", "apply/1")
	if code != 0 || !strings.Contains(out, "管理员通过命令行把 bot 回滚到了快照 apply/1") || !strings.Contains(out, rel+"/README.md") || !strings.Contains(out, rel+"/extra.txt") {
		t.Fatalf("rollback apply/1: %d\n%s\n%s", code, out, stderr)
	}
	b.waitState("running", 60*time.Second)
	if got := b.readFile(pkg + "/README.md"); got != "readme\n" {
		t.Fatalf("README.md after the rollback: %q", got)
	}
	if fileExists(pkg + "/extra.txt") {
		t.Fatal("extra.txt is still there")
	}
	if status := gitIn(t, pkg, "status", "--porcelain"); status != "" {
		t.Fatalf("the package does not match its own HEAD after the rollback:\n%s", status)
	}
}
