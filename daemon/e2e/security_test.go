package e2e

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// jitiCacheDir is os.tmpdir()/jiti of the children: they get no TMPDIR.
const jitiCacheDir = "/tmp/jiti"

// plantJitiEntry writes a jiti file cache entry for the extension file path whose code writes marker to stderr and
// throws. jiti 2.7 names the entry <dir>-<name>.<md5(path)[:8]>.mjs and accepts it when it ends with
// " /* v9-<md5(source)[:16]> */\n". The entry is removed when the test ends.
func (b *bed) plantJitiEntry(path, marker string) {
	b.t.Helper()
	source := b.readFile(path)
	name := filepath.Base(filepath.Dir(path)) + "-" + strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)) + "." + md5hex(path)[:8] + ".mjs"
	entry := filepath.Join(jitiCacheDir, name)
	if err := os.MkdirAll(jitiCacheDir, 0o777); err != nil {
		b.t.Fatal(err)
	}
	content := `process.stderr.write("` + marker + `\n"); throw new Error("poisoned jiti cache entry");` + " /* v9-" + md5hex(source)[:16] + " */\n"
	if err := os.WriteFile(entry, []byte(content), 0o644); err != nil {
		b.t.Fatal(err)
	}
	b.t.Cleanup(func() {
		if err := os.Remove(entry); err != nil {
			b.t.Errorf("remove %s: %v", entry, err)
		}
	})
}

// requirePoisonUsed checks that the planted entry is valid: pi with jiti's file cache on runs it in place of the
// extension file.
func (b *bed) requirePoisonUsed(path, marker string) {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", b.env.Paths.PiBin, "--mode", "rpc", "--no-session", "-ne", "-e", path)
	cmd.Env = []string{"HOME=" + b.env.Paths.BearbotHome, "PATH=" + os.Getenv("PATH"), "PI_CODING_AGENT_DIR=" + b.t.TempDir(), "PI_SKIP_VERSION_CHECK=1"}
	cmd.Dir = b.env.Paths.BearbotHome
	cmd.Stdin = strings.NewReader(`{"type":"get_state","id":"probe"}` + "\n")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Run()
	if !strings.Contains(stderr.String(), marker) {
		b.t.Fatalf("the planted jiti cache entry was not used by pi with the file cache on, so this test checks nothing (has the cache format of jiti changed?); stderr:\n%s", stderr.String())
	}
}

// TestReplacedCodeIsNotRun (DESIGN 4.6, 10.1, 15): bearbot can write jiti's file cache in /tmp/jiti and the profile's
// bin directory, the first entry of the child PATH. Neither replaces the code the daemon starts: the agent loads its
// extensions from the files (JITI_FS_CACHE=false), and pi, tsc and the trial run with the node of the daemon's PATH.
func TestReplacedCodeIsNotRun(t *testing.T) {
	b := newBed(t, nil)
	ext := b.ext("real.ts", commandExtension("e2e-real"))
	marker := "JITI-CACHE-POISON-" + filepath.Base(b.env.Root)
	b.plantJitiEntry(ext, marker)
	b.requirePoisonUsed(ext, marker)
	nodeRan := filepath.Join(b.env.Root, "profile-node-ran")
	writeExecutable(t, filepath.Join(b.env.Paths.ProfileBin, "node"), "#!/bin/sh\ntouch "+nodeRan+"\nexit 1\n")

	b.start()
	b.waitState("running", 90*time.Second)
	if ready := b.lastLine(b.env.Paths.DaemonLog, "msg=bridge/ready"); !strings.Contains(ready, "e2e-real") {
		t.Fatalf("the extension's own code did not run: %s", ready)
	}
	var rep checkReport
	if code := b.call("POST", "/v1/check", map[string]any{}, &rep); code != 200 || !rep.Passed {
		t.Fatalf("check: %d %+v", code, rep)
	}
	if strings.Contains(b.readFile(b.env.Paths.AgentStderrLog), marker) || strings.Contains(b.readFile(filepath.Join(filepath.Dir(rep.ReportPath), "trial-stderr.log")), marker) {
		t.Fatal("the agent or the trial ran the planted jiti cache entry")
	}
	if fileExists(nodeRan) {
		t.Fatalf("the node in %s ran", b.env.Paths.ProfileBin)
	}
}
