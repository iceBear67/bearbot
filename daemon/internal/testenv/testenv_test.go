package testenv

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bearbot/daemon/internal/config"
)

func TestNewLayout(t *testing.T) {
	e := New(t)
	if !strings.HasPrefix(e.Root, "/tmp/bb") {
		t.Fatalf("root = %s", e.Root)
	}
	if len(e.Paths.GatewayCtlSock) >= 108 {
		t.Fatalf("socket path too long: %s", e.Paths.GatewayCtlSock)
	}
	for _, l := range symlinks {
		target, err := os.Readlink(filepath.Join(e.Root, l[0]))
		if err != nil {
			t.Fatal(err)
		}
		want := l[1]
		if want[0] == '@' {
			want = filepath.Join(e.Repo, want[1:])
		}
		if target != want {
			t.Errorf("%s -> %s, want %s", l[0], target, want)
		}
	}
	for _, p := range []string{e.Paths.PiBin, e.Paths.Tsc, e.Paths.Python, e.Paths.Uv, filepath.Join(e.Paths.PiPackage, "package.json"), filepath.Join(e.Paths.ToolchainTypes, "node")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s does not resolve: %v", p, err)
		}
	}
	for _, d := range []string{e.Paths.ProfileDir, e.Paths.WorkspaceDir, e.Paths.ConfigDir, e.Paths.BearbotHome} {
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			t.Errorf("%s: %v", d, err)
		}
	}
	// An environment created in a subtest is removed when the subtest ends (its cleanups ran when t.Run returned).
	var root string
	t.Run("removed after test", func(t *testing.T) {
		root = New(t).Root
		if _, err := os.Stat(root); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s still exists after the test: %v", root, err)
	}
}

func TestConfigTemplateRoundTrip(t *testing.T) {
	e := New(t)
	cfg, err := config.Load(e.ConfigPath)
	if err != nil {
		t.Fatalf("default test config does not load: %v", err)
	}
	if cfg.Agent.Probation != 5*time.Second || cfg.Platforms.Telegram.Enabled || !cfg.Platforms.Console.Enabled || cfg.Daemon.TermGrace != 2*time.Second {
		t.Fatalf("config = %+v", cfg)
	}

	c := DefaultConfig()
	c.Probation = "1s"
	c.AgentEnv = map[string]string{"ANTHROPIC_API_KEY": "sk-\"quoted\"\\x", "B": "${HOME}"}
	c.RecoveryEnv = map[string]string{"K": "v"}
	c.OnEnter = []string{"/hooks/a.sh", "/hooks/b \"c\".sh"}
	c.ErrorNotice = "多行\n文字\t制表"
	c.ClassifierModel = "testprovider/some/model"
	e.WriteConfig(t, c)
	cfg, err = config.Load(e.ConfigPath)
	if err != nil {
		data, _ := os.ReadFile(e.ConfigPath)
		t.Fatalf("Load: %v\n%s", err, data)
	}
	if cfg.Agent.Probation != time.Second {
		t.Errorf("probation = %v", cfg.Agent.Probation)
	}
	if cfg.Agent.Env["ANTHROPIC_API_KEY"] != "sk-\"quoted\"\\x" || cfg.Agent.Env["B"] != os.Getenv("HOME") {
		t.Errorf("agent env = %q", cfg.Agent.Env)
	}
	if cfg.Recovery.Env["K"] != "v" {
		t.Errorf("recovery env = %q", cfg.Recovery.Env)
	}
	if len(cfg.Maintenance.OnEnter) != 2 || cfg.Maintenance.OnEnter[1] != "/hooks/b \"c\".sh" {
		t.Errorf("on_enter = %q", cfg.Maintenance.OnEnter)
	}
	if cfg.Bridge.ErrorNotice != "多行\n文字\t制表" {
		t.Errorf("error notice = %q", cfg.Bridge.ErrorNotice)
	}
	if cfg.Classifier.Model != "testprovider/some/model" {
		t.Errorf("model = %q", cfg.Classifier.Model)
	}
}

func TestModelFiles(t *testing.T) {
	e := New(t)
	m := Model{BaseURL: "http://127.0.0.1:9/v1", APIKey: "key", ID: "m1"}
	e.InstallModel(t, m)
	for _, dir := range []string{e.Paths.AgentDir, e.Paths.RecoveryOverrideDir} {
		data, err := os.ReadFile(filepath.Join(dir, "models.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"testprovider"`) || !strings.Contains(string(data), `"openai-completions"`) {
			t.Errorf("models.json = %s", data)
		}
		data, err = os.ReadFile(filepath.Join(dir, "settings.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"defaultModel": "m1"`) || !strings.Contains(string(data), `"defaultProjectTrust": "never"`) {
			t.Errorf("settings.json = %s", data)
		}
	}
	if m.Ref() != "testprovider/m1" {
		t.Errorf("Ref = %s", m.Ref())
	}
}

func TestRepoRoot(t *testing.T) {
	repo, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "daemon", "go.mod")); err != nil {
		t.Fatalf("repo root %s: %v", repo, err)
	}
}
