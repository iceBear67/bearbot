package recovery

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bearbot/daemon/internal/config"
	"bearbot/daemon/internal/layout"
	"bearbot/daemon/internal/proc"
	"bearbot/daemon/internal/testenv"
)

type fixture struct {
	env   *testenv.Env
	users *layout.Users
	cfg   *config.Config
	node  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	e := testenv.New(t)
	e.RequireRepoFile(t, "extensions/bearbot-recovery/index.ts")
	e.RequireRepoFile(t, "recovery-profile/settings.json")
	u := e.Users(t)
	if err := os.MkdirAll(filepath.Dir(e.Paths.RunDir), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range e.Paths.Dirs() {
		if _, err := layout.EnsureDir(d, u); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load(e.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{env: e, users: u, cfg: cfg, node: node}
}

func TestPrepareProfile(t *testing.T) {
	f := newFixture(t)
	p := f.env.Paths
	if err := PrepareProfile(p, f.users); err != nil {
		t.Fatal(err)
	}
	settings, err := os.ReadFile(filepath.Join(p.RecoveryAgentDir, "settings.json"))
	if err != nil || !strings.Contains(string(settings), `"defaultProvider": "anthropic"`) {
		t.Fatalf("template settings = %s %v", settings, err)
	}
	info, err := os.Stat(p.RecoveryAgentDir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("recovery agent dir: %v %v", info, err)
	}
	if provider, err := Provider(p); err != nil || provider != "anthropic" {
		t.Fatalf("provider %q %v", provider, err)
	}

	// Overrides replace the template; files left by a previous run are removed.
	if err := os.WriteFile(filepath.Join(p.RecoveryAgentDir, "stale.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := testenv.Model{BaseURL: "http://127.0.0.1:9/v1", APIKey: "k", ID: "m1"}
	f.env.InstallModel(t, m)
	if err := os.WriteFile(filepath.Join(p.RecoveryOverrideDir, "auth.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareProfile(p, f.users); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(p.RecoveryAgentDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		info, err := e.Info()
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", e.Name(), info.Mode(), err)
		}
	}
	if strings.Join(names, ",") != "auth.json,models.json,settings.json" {
		t.Fatalf("files %v", names)
	}
	if provider, err := Provider(p); err != nil || provider != testenv.Provider {
		t.Fatalf("provider %q %v", provider, err)
	}

	// RemoveProfile removes the profile with the credentials; removing it again is no error.
	for range 2 {
		if err := RemoveProfile(p); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(p.RecoveryAgentDir); !os.IsNotExist(err) {
			t.Fatalf("%s after RemoveProfile: %v", p.RecoveryAgentDir, err)
		}
	}
}

func TestCheckCredentials(t *testing.T) {
	f := newFixture(t)
	p := f.env.Paths
	ctx := context.Background()
	if err := PrepareProfile(p, f.users); err != nil {
		t.Fatal(err)
	}
	err := CheckCredentials(ctx, p, f.users, f.cfg, f.node, "inc-20261027083000")
	if err == nil || !strings.Contains(err.Error(), "没有 anthropic 的可用凭据") {
		t.Fatalf("without credentials: %v", err)
	}
	// [recovery.env] provides the key.
	c := testenv.DefaultConfig()
	c.RecoveryEnv = map[string]string{"ANTHROPIC_API_KEY": "sk-ant-test"}
	f.env.WriteConfig(t, c)
	cfg, err := config.Load(f.env.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckCredentials(ctx, p, f.users, cfg, f.node, "inc-20261027083000"); err != nil {
		t.Fatalf("with [recovery.env]: %v", err)
	}
	// models.json with an API key in the override directory.
	f.env.InstallModel(t, testenv.Model{BaseURL: "http://127.0.0.1:9/v1", APIKey: "k", ID: "m1"})
	if err := PrepareProfile(p, f.users); err != nil {
		t.Fatal(err)
	}
	if err := CheckCredentials(ctx, p, f.users, f.cfg, f.node, "inc-20261027083000"); err != nil {
		t.Fatalf("with models.json: %v", err)
	}
}

// TestProcess starts the real recovery process with bearbot-recovery: get_state answers, a prompt without
// credentials is refused, Close stops it.
func TestProcess(t *testing.T) {
	f := newFixture(t)
	p := f.env.Paths
	if err := PrepareProfile(p, f.users); err != nil {
		t.Fatal(err)
	}
	id := "inc-20261027083000"
	logw := proc.NewLogWriter(p.RecoveryLog(id), 100)
	defer logw.Close()
	ctx := context.Background()
	rp, err := Start(ctx, Options{Paths: p, Users: f.users, Config: f.cfg, Node: f.node, IncidentID: id, Log: logw, Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	err = rp.Prompt(ctx, "故障 "+id)
	if err == nil || !strings.Contains(err.Error(), "No API key") {
		t.Fatalf("prompt without credentials: %v", err)
	}
	start := time.Now()
	if err := rp.Close(); err != nil {
		t.Fatal(err)
	}
	if !rp.Exit().Success() || time.Since(start) > 5*time.Second {
		t.Fatalf("exit %s after %v", rp.Exit(), time.Since(start))
	}
	log, err := os.ReadFile(p.RecoveryLog(id))
	if err != nil || !strings.Contains(string(log), "recovery 进程启动于") || !strings.Contains(string(log), "daemon 发送提示") {
		t.Fatalf("recovery log = %s %v", log, err)
	}
}
