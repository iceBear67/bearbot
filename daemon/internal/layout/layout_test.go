package layout_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bearbot/daemon/internal/config"
	"bearbot/daemon/internal/layout"
	"bearbot/daemon/internal/proc"
	"bearbot/daemon/internal/testenv"
)

func TestProductionPaths(t *testing.T) {
	p, err := layout.NewPaths("/")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"Root":                    "/",
		"ProfileDir":              "/bearbot/profile",
		"AgentDir":                "/bearbot/profile/agent",
		"AgentSessionsDir":        "/bearbot/profile/agent/sessions/--bearbot-workspace--",
		"ProfileBin":              "/bearbot/profile/bin",
		"PythonVenv":              "/bearbot/profile/python",
		"SysDir":                  "/bearbot/profile/bearbot",
		"DaemonDir":               "/bearbot/profile/bearbot/daemon",
		"GatewayDataDir":          "/bearbot/profile/bearbot/gateway",
		"LogsDir":                 "/bearbot/profile/bearbot/logs",
		"IncidentsDir":            "/bearbot/profile/bearbot/incidents",
		"RecoverySessionsDir":     "/bearbot/profile/bearbot/recovery-sessions",
		"ChecksDir":               "/bearbot/profile/bearbot/checks",
		"SnapshotsGit":            "/bearbot/profile/bearbot/snapshots.git",
		"SnapshotWorkTree":        "/bearbot",
		"WorkspaceDir":            "/bearbot/workspace",
		"ConfigDir":               "/bearbot/config",
		"RecoveryOverrideDir":     "/bearbot/config/recovery",
		"RunDir":                  "/run/bearbot",
		"GatewaySock":             "/run/bearbot/gateway.sock",
		"GatewayCtlSock":          "/run/bearbot/gateway-ctl.sock",
		"AgentSock":               "/run/bearbot/agent.sock",
		"AdminSock":               "/run/bearbot/admin.sock",
		"GatewayConfig":           "/run/bearbot/gateway.json",
		"RecoveryAgentDir":        "/run/bearbot/recovery/agent",
		"InstallDir":              "/opt/bearbot",
		"GatewayMain":             "/opt/bearbot/gateway/dist/main.js",
		"BearbotIMExt":            "/opt/bearbot/extensions/bearbot-im",
		"RecoveryExt":             "/opt/bearbot/extensions/bearbot-recovery",
		"SDKFile":                 "/opt/bearbot/sdk/index.ts",
		"SkillsDir":               "/opt/bearbot/skills",
		"RecoverySkill":           "/opt/bearbot/skills/bearbot-recovery",
		"RecoveryProfileTemplate": "/opt/bearbot/recovery-profile",
		"Tsc":                     "/opt/bearbot/toolchain/node_modules/.bin/tsc",
		"ToolchainTypes":          "/opt/bearbot/toolchain/node_modules/@types",
		"PiBin":                   "/usr/local/bin/pi",
		"PiPackage":               "/usr/local/lib/node_modules/@earendil-works/pi-coding-agent",
		"Python":                  "/usr/local/bin/python3.14",
		"Uv":                      "/usr/local/bin/uv",
		"BearbotHome":             "/home/bearbot",
		"DefaultConfigFile":       "/bearbot/config/bearbot.toml",
		"StateFile":               "/bearbot/profile/bearbot/daemon/state.json",
		"ClassifierTurnDir":       "/bearbot/profile/bearbot/daemon/classifier-turn",
		"GatewayDB":               "/bearbot/profile/bearbot/gateway/gateway.db",
		"ConsoleFilesDir":         "/bearbot/profile/bearbot/gateway/console-files",
		"DaemonLog":               "/bearbot/profile/bearbot/logs/daemon.log",
		"GatewayLog":              "/bearbot/profile/bearbot/logs/gateway.log",
		"AgentStderrLog":          "/bearbot/profile/bearbot/logs/agent/stderr.log",
		"AgentRPCEventsLog":       "/bearbot/profile/bearbot/logs/agent/rpc-events.jsonl",
		"AgentClassifierLog":      "/bearbot/profile/bearbot/logs/agent/classifier.jsonl",
		"AgentExitsLog":           "/bearbot/profile/bearbot/logs/agent/exits.jsonl",
		"RecoveryLogDir":          "/bearbot/profile/bearbot/logs/recovery",
		"AgentSettingsFile":       "/bearbot/profile/agent/settings.json",
		"AgentModelsFile":         "/bearbot/profile/agent/models.json",
		"AgentsMD":                "/bearbot/workspace/AGENTS.md",
		"ClassifierMD":            "/bearbot/workspace/CLASSIFIER.md",
		"PythonBin":               "/bearbot/profile/python/bin/python",
	}
	got := map[string]string{
		"Root": p.Root, "ProfileDir": p.ProfileDir, "AgentDir": p.AgentDir, "AgentSessionsDir": p.AgentSessionsDir,
		"ProfileBin": p.ProfileBin, "PythonVenv": p.PythonVenv, "SysDir": p.SysDir, "DaemonDir": p.DaemonDir,
		"GatewayDataDir": p.GatewayDataDir, "LogsDir": p.LogsDir, "IncidentsDir": p.IncidentsDir,
		"RecoverySessionsDir": p.RecoverySessionsDir, "ChecksDir": p.ChecksDir, "SnapshotsGit": p.SnapshotsGit,
		"SnapshotWorkTree": p.SnapshotWorkTree, "WorkspaceDir": p.WorkspaceDir, "ConfigDir": p.ConfigDir,
		"RecoveryOverrideDir": p.RecoveryOverrideDir, "RunDir": p.RunDir, "GatewaySock": p.GatewaySock,
		"GatewayCtlSock": p.GatewayCtlSock, "AgentSock": p.AgentSock, "AdminSock": p.AdminSock,
		"GatewayConfig": p.GatewayConfig, "RecoveryAgentDir": p.RecoveryAgentDir, "InstallDir": p.InstallDir,
		"GatewayMain": p.GatewayMain, "BearbotIMExt": p.BearbotIMExt, "RecoveryExt": p.RecoveryExt,
		"SDKFile": p.SDKFile, "SkillsDir": p.SkillsDir, "RecoverySkill": p.RecoverySkill,
		"RecoveryProfileTemplate": p.RecoveryProfileTemplate, "Tsc": p.Tsc, "ToolchainTypes": p.ToolchainTypes,
		"PiBin": p.PiBin, "PiPackage": p.PiPackage, "Python": p.Python, "Uv": p.Uv, "BearbotHome": p.BearbotHome,
		"DefaultConfigFile": p.DefaultConfigFile, "StateFile": p.StateFile, "ClassifierTurnDir": p.ClassifierTurnDir,
		"GatewayDB": p.GatewayDB, "ConsoleFilesDir": p.ConsoleFilesDir, "DaemonLog": p.DaemonLog,
		"GatewayLog": p.GatewayLog, "AgentStderrLog": p.AgentStderrLog, "AgentRPCEventsLog": p.AgentRPCEventsLog,
		"AgentClassifierLog": p.AgentClassifierLog, "AgentExitsLog": p.AgentExitsLog,
		"RecoveryLogDir": p.RecoveryLogDir, "AgentSettingsFile": p.AgentSettingsFile,
		"AgentModelsFile": p.AgentModelsFile, "AgentsMD": p.AgentsMD, "ClassifierMD": p.ClassifierMD,
		"PythonBin": p.PythonBin,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
	if path := p.ChildPATH(); path != "/bearbot/profile/bin:/bearbot/profile/python/bin:/usr/local/bin:/usr/bin:/bin" {
		t.Errorf("ChildPATH = %q", path)
	}
	if p.RecoveryLog("inc-20261027083000") != "/bearbot/profile/bearbot/logs/recovery/inc-20261027083000.log" {
		t.Errorf("RecoveryLog = %q", p.RecoveryLog("inc-20261027083000"))
	}
	if _, err := layout.NewPaths("relative"); err == nil {
		t.Error("relative root accepted")
	}
}

func TestRootPrefix(t *testing.T) {
	p, err := layout.NewPaths("/tmp/bb123")
	if err != nil {
		t.Fatal(err)
	}
	if p.AgentSessionsDir != "/tmp/bb123/bearbot/profile/agent/sessions/--tmp-bb123-bearbot-workspace--" {
		t.Errorf("AgentSessionsDir = %q", p.AgentSessionsDir)
	}
	want := "/tmp/bb123/bearbot/profile/bin:/tmp/bb123/bearbot/profile/python/bin:/tmp/bb123/usr/local/bin:/usr/local/bin:/usr/bin:/bin"
	if path := p.ChildPATH(); path != want {
		t.Errorf("ChildPATH = %q", path)
	}
	if rel, err := p.WorkTreeRel("/tmp/bb123/bearbot/workspace/.pi/x.ts"); err != nil || rel != "workspace/.pi/x.ts" {
		t.Errorf("WorkTreeRel = %q, %v", rel, err)
	}
	if _, err := p.WorkTreeRel("/tmp/bb123/other"); err == nil {
		t.Error("path outside the work tree accepted")
	}
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	e := testenv.New(t)
	c := testenv.DefaultConfig()
	c.AgentEnv = map[string]string{"ZED": "z", "ANTHROPIC_API_KEY": "sk-1"}
	c.RecoveryEnv = map[string]string{"ANTHROPIC_API_KEY": "sk-2"}
	c.ClassifierTimeout = "15s"
	c.HoldWarnAfter = "10m"
	c.ObserveLimit = 30
	c.RulesMaxBytes = 32768
	c.ClassifierModel = "anthropic/claude-haiku-4-5-20251001"
	e.WriteConfig(t, c)
	cfg, err := config.Load(e.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestAgentEnvAndArgs(t *testing.T) {
	cfg := testConfig(t)
	t.Setenv("TZ", "Asia/Shanghai")
	t.Setenv("LANG", "zh_CN.UTF-8")
	p, _ := layout.NewPaths("/")
	want := []string{
		"HOME=/home/bearbot",
		"PATH=/bearbot/profile/bin:/bearbot/profile/python/bin:/usr/local/bin:/usr/bin:/bin",
		"VIRTUAL_ENV=/bearbot/profile/python",
		"UV_LINK_MODE=copy",
		"PI_CODING_AGENT_DIR=/bearbot/profile/agent",
		"PI_SKIP_VERSION_CHECK=1",
		"JITI_FS_CACHE=false",
		"BEARBOT_WORKSPACE_DIR=/bearbot/workspace",
		"BEARBOT_GATEWAY_SOCKET=/run/bearbot/gateway.sock",
		"BEARBOT_AGENT_SOCKET=/run/bearbot/agent.sock",
		"BEARBOT_CLASSIFIER_MODEL=anthropic/claude-haiku-4-5-20251001",
		"BEARBOT_CLASSIFIER_TIMEOUT=15000",
		"BEARBOT_CLASSIFIER_RULES_MAX_BYTES=32768",
		"BEARBOT_OBSERVE_LIMIT=30",
		"BEARBOT_HOLD_WARN_AFTER=600000",
		"BEARBOT_ERROR_NOTICE=处理时出现错误，已通知管理员。",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=safe.directory",
		"GIT_CONFIG_VALUE_0=/bearbot/profile/bearbot/snapshots.git",
		"TZ=Asia/Shanghai",
		"LANG=zh_CN.UTF-8",
		"ANTHROPIC_API_KEY=sk-1",
		"ZED=z",
	}
	if got := p.AgentEnv(cfg); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("AgentEnv =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := p.TrialEnv(cfg); got[len(got)-1] != "BEARBOT_TRIAL=1" || len(got) != len(want)+1 {
		t.Errorf("TrialEnv = %q", got)
	}
	wantRecovery := []string{
		"HOME=/home/bearbot",
		"PATH=/bearbot/profile/bin:/bearbot/profile/python/bin:/usr/local/bin:/usr/bin:/bin",
		"VIRTUAL_ENV=/bearbot/profile/python",
		"UV_LINK_MODE=copy",
		"PI_SKIP_VERSION_CHECK=1",
		"JITI_FS_CACHE=false",
		"PI_CODING_AGENT_DIR=/run/bearbot/recovery/agent",
		"BEARBOT_AGENT_SOCKET=/run/bearbot/agent.sock",
		"BEARBOT_INCIDENT_ID=inc-20261027083000",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=safe.directory",
		"GIT_CONFIG_VALUE_0=/bearbot/profile/bearbot/snapshots.git",
		"TZ=Asia/Shanghai",
		"LANG=zh_CN.UTF-8",
		"ANTHROPIC_API_KEY=sk-2",
	}
	if got := p.RecoveryEnv(cfg, "inc-20261027083000"); strings.Join(got, "\n") != strings.Join(wantRecovery, "\n") {
		t.Errorf("RecoveryEnv = %q", got)
	}
	if got := strings.Join(p.GatewayEnv(), " "); got != "PATH=/bearbot/profile/bin:/bearbot/profile/python/bin:/usr/local/bin:/usr/bin:/bin HOME=/bearbot/profile/bearbot/gateway NODE_ENV=production TZ=Asia/Shanghai LANG=zh_CN.UTF-8" {
		t.Errorf("GatewayEnv = %q", got)
	}
	if got := strings.Join(p.AgentArgs("bearbot-20261027083000-1a2b3c4d"), " "); got != "--mode rpc --session-id bearbot-20261027083000-1a2b3c4d --session-dir /bearbot/profile/agent/sessions/--bearbot-workspace-- --tools +grep,+find,+ls -e /opt/bearbot/extensions/bearbot-im" {
		t.Errorf("AgentArgs = %q", got)
	}
	if got := strings.Join(p.TrialArgs(), " "); got != "--mode rpc --no-session --tools +grep,+find,+ls -e /opt/bearbot/extensions/bearbot-im" {
		t.Errorf("TrialArgs = %q", got)
	}
	if got := strings.Join(p.RecoveryArgs("inc-20261027083000"), " "); got != "--mode rpc -ne -e /opt/bearbot/extensions/bearbot-recovery --tools +grep,+find,+ls -ns --skill /opt/bearbot/skills/bearbot-recovery -np -nc -na --session-dir /bearbot/profile/bearbot/recovery-sessions --session-id inc-20261027083000" {
		t.Errorf("RecoveryArgs = %q", got)
	}
	if got := strings.Join(p.GatewayArgs(), " "); got != "/opt/bearbot/gateway/dist/main.js --config /run/bearbot/gateway.json" {
		t.Errorf("GatewayArgs = %q", got)
	}
	if p.GatewayCwd() != "/opt/bearbot/gateway" || p.RecoveryCwd() != "/bearbot" {
		t.Errorf("cwd = %q %q", p.GatewayCwd(), p.RecoveryCwd())
	}
}

func TestEnvWithoutTZ(t *testing.T) {
	cfg := testConfig(t)
	t.Setenv("TZ", "")
	os.Unsetenv("TZ")
	t.Setenv("LANG", "")
	os.Unsetenv("LANG")
	p, _ := layout.NewPaths("/")
	for _, kv := range p.AgentEnv(cfg) {
		if strings.HasPrefix(kv, "TZ=") || strings.HasPrefix(kv, "LANG=") {
			t.Errorf("unset variable passed: %s", kv)
		}
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}

func TestInit(t *testing.T) {
	e := testenv.New(t)
	u := e.Users(t)
	ctx := context.Background()
	res, err := layout.Init(ctx, e.Paths, u)
	if err != nil {
		t.Fatal(err)
	}
	if !res.WroteSettings || !res.WroteTemplates || !res.CreatedVenv || res.PythonVersion == "" {
		t.Fatalf("result = %+v", res)
	}
	for _, d := range e.Paths.Dirs() {
		m := mode(t, d.Path)
		if !m.IsDir() || m&(os.ModePerm|os.ModeSetgid|os.ModeSticky) != d.Mode {
			t.Errorf("%s mode %v, want %v", d.Path, m, d.Mode)
		}
	}
	if m := mode(t, e.Paths.WorkspaceDir); m&os.ModeSetgid == 0 || m.Perm() != 0o770 {
		t.Errorf("workspace mode %v", m)
	}
	if m := mode(t, e.Paths.RunDir); m&os.ModeSticky == 0 {
		t.Errorf("run dir mode %v", m)
	}
	if m := mode(t, e.Paths.PythonVenv); m.Perm() != 0o775 {
		t.Errorf("venv mode %v", m)
	}
	settings, err := os.ReadFile(e.Paths.AgentSettingsFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(settings) != "{\n  \"quietStartup\": true,\n  \"defaultProjectTrust\": \"never\"\n}\n" {
		t.Errorf("settings.json = %q", settings)
	}
	if m := mode(t, e.Paths.AgentSettingsFile); m.Perm() != 0o664 {
		t.Errorf("settings mode %v", m)
	}
	agents, _ := os.ReadFile(e.Paths.AgentsMD)
	if !strings.HasPrefix(string(agents), "# 记忆\n\n## 用途\n") || !strings.Contains(string(agents), "<!-- 犯过的错误与修复方式 -->\n") {
		t.Errorf("AGENTS.md = %q", agents)
	}
	classifier, _ := os.ReadFile(e.Paths.ClassifierMD)
	if !strings.HasPrefix(string(classifier), "# classifier 补充规则\n") || !strings.HasSuffix(string(classifier), "## 禁止成员进行的操作\n") {
		t.Errorf("CLASSIFIER.md = %q", classifier)
	}
	if _, err := os.Stat(filepath.Join(e.Paths.PythonVenv, "bin", "pip")); err != nil {
		t.Errorf("venv without pip: %v", err)
	}

	// Second start: nothing is rewritten, user edits stay.
	if err := os.WriteFile(e.Paths.AgentSettingsFile, []byte("{\"x\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(e.Paths.ClassifierMD); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e.Paths.LogsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	res, err = layout.Init(ctx, e.Paths, u)
	if err != nil {
		t.Fatal(err)
	}
	if res.WroteSettings || res.WroteTemplates || res.CreatedVenv || len(res.CreatedDirs) != 0 {
		t.Fatalf("second result = %+v", res)
	}
	if data, _ := os.ReadFile(e.Paths.AgentSettingsFile); string(data) != "{\"x\":1}\n" {
		t.Errorf("settings rewritten: %q", data)
	}
	if _, err := os.Stat(e.Paths.ClassifierMD); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("template written into a non-empty workspace")
	}
	if m := mode(t, e.Paths.LogsDir); m.Perm() != 0o755 {
		t.Errorf("logs mode not corrected: %v", m)
	}
}

func TestInitErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("missing mount point", func(t *testing.T) {
		e := testenv.New(t)
		if err := os.Remove(e.ConfigPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(e.Paths.ConfigDir); err != nil {
			t.Fatal(err)
		}
		_, err := layout.Init(ctx, e.Paths, e.Users(t))
		if err == nil || !strings.Contains(err.Error(), "挂载点") || !strings.Contains(err.Error(), e.Paths.ConfigDir) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("file instead of directory", func(t *testing.T) {
		e := testenv.New(t)
		if err := os.MkdirAll(e.Paths.SysDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(e.Paths.LogsDir, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := layout.Init(ctx, e.Paths, e.Users(t))
		if err == nil || !strings.Contains(err.Error(), e.Paths.LogsDir) || !strings.Contains(err.Error(), "普通文件") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("symlink instead of directory", func(t *testing.T) {
		e := testenv.New(t)
		if err := os.Symlink("/tmp", e.Paths.AgentDir); err != nil {
			t.Fatal(err)
		}
		_, err := layout.Init(ctx, e.Paths, e.Users(t))
		if err == nil || !strings.Contains(err.Error(), "符号链接") {
			t.Fatalf("err = %v", err)
		}
	})
}

// otherPython finds an interpreter whose version differs from the image's /usr/bin/python3.14, among the uv managed
// installations; when there is none, uv installs CPython 3.13 into a temporary directory.
func otherPython(t *testing.T, e *testenv.Env) string {
	t.Helper()
	ctx := context.Background()
	env := append(os.Environ(), "UV_PYTHON_DOWNLOADS=never")
	for _, v := range []string{"3.13", "3.12", "3.11", "3.15"} {
		res, err := proc.Run(ctx, proc.RunSpec{Path: e.Paths.Uv, Args: []string{"python", "find", "--managed-python", v}, Env: env, Dir: "/", Timeout: 30 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if res.OK() {
			return strings.TrimSpace(string(res.Stdout))
		}
	}
	dir := t.TempDir()
	if _, err := proc.RunChecked(ctx, proc.RunSpec{Path: e.Paths.Uv, Args: []string{"python", "install", "--install-dir", dir, "3.13"}, Env: os.Environ(), Dir: "/", Timeout: 10 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	res, err := proc.RunChecked(ctx, proc.RunSpec{Path: e.Paths.Uv, Args: []string{"python", "find", "3.13"}, Env: append(os.Environ(), "UV_PYTHON_INSTALL_DIR="+dir), Dir: "/", Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(res.Stdout))
}

func TestPythonVersionChangeAndRebuild(t *testing.T) {
	e := testenv.New(t)
	u := e.Users(t)
	ctx := context.Background()
	if _, err := layout.Init(ctx, e.Paths, u); err != nil {
		t.Fatal(err)
	}
	// The agent installs a package into the persistent environment.
	if _, err := proc.RunChecked(ctx, proc.RunSpec{
		Path:    e.Paths.Uv,
		Args:    []string{"pip", "install", "six==1.17.0"},
		Env:     append(e.Paths.ToolEnv(), "VIRTUAL_ENV="+e.Paths.PythonVenv, "UV_LINK_MODE=copy"),
		Dir:     e.Paths.BearbotHome,
		Timeout: 10 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	oldVersion, err := layout.CheckPython(ctx, e.Paths, u)
	if err != nil {
		t.Fatal(err)
	}

	// The image now ships another interpreter behind /usr/local/bin/python3.14.
	other := otherPython(t, e)
	if err := os.Remove(e.Paths.Python); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, e.Paths.Python); err != nil {
		t.Fatal(err)
	}
	_, err = layout.Init(ctx, e.Paths, u)
	var mismatch *layout.PythonMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("Init after the interpreter change: %v", err)
	}
	if !strings.Contains(err.Error(), layout.RebuildCommand) || !strings.Contains(err.Error(), "版本") {
		t.Fatalf("message = %q", err)
	}

	var out bytes.Buffer
	if err := layout.PythonRebuild(ctx, e.Paths, u, &out); err != nil {
		t.Fatalf("rebuild: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "six==1.17.0") {
		t.Errorf("rebuild output = %s", out.String())
	}
	backups, _ := filepath.Glob(filepath.Join(e.Paths.ProfileDir, "python.*.old"))
	if len(backups) != 1 || !strings.Contains(backups[0], "python.3.14") {
		t.Fatalf("backups = %v", backups)
	}
	newVersion, err := layout.CheckPython(ctx, e.Paths, u)
	if err != nil {
		t.Fatalf("check after rebuild: %v", err)
	}
	if newVersion == oldVersion {
		t.Fatalf("version did not change: %s", newVersion)
	}
	res, err := proc.RunChecked(ctx, proc.RunSpec{Path: e.Paths.PythonBin, Args: []string{"-c", "import six; print(six.__version__)"}, Env: e.Paths.ToolEnv(), Dir: "/", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(res.Stdout)) != "1.17.0" {
		t.Fatalf("six = %q", res.Stdout)
	}
	if _, err := layout.Init(ctx, e.Paths, u); err != nil {
		t.Fatalf("Init after rebuild: %v", err)
	}

	// A second rebuild with the same old version must not overwrite the first backup.
	if err := os.MkdirAll(filepath.Join(e.Paths.ProfileDir, "python."+venvVersion(t, e.Paths.PythonVenv)+".old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := layout.PythonRebuild(ctx, e.Paths, u, &out); err == nil || !strings.Contains(err.Error(), "已经存在") {
		t.Fatalf("rebuild over an existing backup: %v", err)
	}
}

func venvVersion(t *testing.T, venv string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(venv, "pyvenv.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == "version_info" {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("no version_info in %s", data)
	return ""
}

func TestBrokenVenv(t *testing.T) {
	e := testenv.New(t)
	u := e.Users(t)
	ctx := context.Background()
	if _, err := layout.Init(ctx, e.Paths, u); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(e.Paths.PythonBin); err != nil {
		t.Fatal(err)
	}
	_, err := layout.Init(ctx, e.Paths, u)
	var mismatch *layout.PythonMismatchError
	if !errors.As(err, &mismatch) || !strings.Contains(err.Error(), "无法执行虚拟环境") {
		t.Fatalf("err = %v", err)
	}
}
