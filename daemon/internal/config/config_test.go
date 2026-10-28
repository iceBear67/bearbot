package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bearbot.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustLoad(t *testing.T, content string) *Config {
	t.Helper()
	cfg, err := Load(writeConfig(t, content))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func loadErr(t *testing.T, content string) string {
	t.Helper()
	_, err := Load(writeConfig(t, content))
	if err == nil {
		t.Fatalf("Load succeeded, want an error for:\n%s", content)
	}
	return err.Error()
}

const minimal = `
[platforms.telegram]
enabled = false
`

func TestDefaults(t *testing.T) {
	cfg := mustLoad(t, minimal)
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"daemon.log_level", cfg.Daemon.LogLevel, "info"},
		{"daemon.shutdown_grace", cfg.Daemon.ShutdownGrace, 90 * time.Second},
		{"daemon.term_grace", cfg.Daemon.TermGrace, 10 * time.Second},
		{"agent.start_timeout", cfg.Agent.StartTimeout, 60 * time.Second},
		{"agent.ready_timeout", cfg.Agent.ReadyTimeout, 30 * time.Second},
		{"agent.heartbeat_interval", cfg.Agent.HeartbeatInterval, 30 * time.Second},
		{"agent.rpc_timeout", cfg.Agent.RPCTimeout, 30 * time.Second},
		{"agent.crash_limit", cfg.Agent.CrashLimit, 3},
		{"agent.crash_window", cfg.Agent.CrashWindow, 10 * time.Minute},
		{"agent.probation", cfg.Agent.Probation, 10 * time.Minute},
		{"agent.drain_timeout", cfg.Agent.DrainTimeout, 30 * time.Minute},
		{"agent.env", len(cfg.Agent.Env), 0},
		{"gateway.user_name", cfg.Gateway.UserName, "bearbot"},
		{"gateway.crash_limit", cfg.Gateway.CrashLimit, 5},
		{"gateway.token_ttl", cfg.Gateway.TokenTTL, 10 * time.Minute},
		{"gateway.replay_window", cfg.Gateway.ReplayWindow, 10 * time.Minute},
		{"gateway.message_retention", cfg.Gateway.MessageRetention, 30 * 24 * time.Hour},
		{"gateway.attachment_timeout", cfg.Gateway.AttachmentTimeout, 60 * time.Second},
		{"gateway.image_max_bytes", cfg.Gateway.ImageMaxBytes, int64(5242880)},
		{"gateway.maintenance_notice", cfg.Gateway.MaintenanceNotice, "bearbot 正在修复，暂时无法回复。"},
		{"gateway.http.listen", cfg.Gateway.HTTP.Listen, ""},
		{"platforms.telegram.enabled", cfg.Platforms.Telegram.Enabled, false},
		{"platforms.qq.enabled", cfg.Platforms.QQ.Enabled, false},
		{"platforms.qq.sandbox", cfg.Platforms.QQ.Sandbox, false},
		{"platforms.wecom.enabled", cfg.Platforms.WeCom.Enabled, false},
		{"platforms.console.enabled", cfg.Platforms.Console.Enabled, true},
		{"bridge.observe_limit", cfg.Bridge.ObserveLimit, 30},
		{"bridge.hold_warn_after", cfg.Bridge.HoldWarnAfter, 10 * time.Minute},
		{"bridge.error_notice", cfg.Bridge.ErrorNotice, "处理时出现错误，已通知管理员。"},
		{"classifier.enabled", cfg.Classifier.Enabled, true},
		{"classifier.model", cfg.Classifier.Model, "anthropic/claude-haiku-4-5-20251001"},
		{"classifier.timeout", cfg.Classifier.Timeout, 15 * time.Second},
		{"classifier.rules_max_bytes", cfg.Classifier.RulesMaxBytes, int64(32768)},
		{"recovery.timeout", cfg.Recovery.Timeout, 30 * time.Minute},
		{"recovery.attempts", cfg.Recovery.Attempts, 2},
		{"recovery.env", len(cfg.Recovery.Env), 0},
		{"maintenance.on_enter", len(cfg.Maintenance.OnEnter), 0},
		{"maintenance.on_exit", len(cfg.Maintenance.OnExit), 0},
		{"maintenance.hook_timeout", cfg.Maintenance.HookTimeout, 30 * time.Second},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	provider, model := cfg.Classifier.ModelParts()
	if provider != "anthropic" || model != "claude-haiku-4-5-20251001" {
		t.Errorf("ModelParts = %q, %q", provider, model)
	}
}

func TestDefaultTelegramNeedsToken(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "123:abc")
	cfg := mustLoad(t, "")
	if !cfg.Platforms.Telegram.Enabled || cfg.Platforms.Telegram.BotToken != "123:abc" {
		t.Fatalf("telegram = %+v", cfg.Platforms.Telegram)
	}
}

func TestDesignExample(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-agent")
	t.Setenv("TELEGRAM_BOT_TOKEN", "123:abc")
	t.Setenv("BEARBOT_RECOVERY_API_KEY", "sk-recovery")
	cfg := mustLoad(t, `
[daemon]
log_level = "debug"
shutdown_grace = "90s"
term_grace = "10s"

[agent]
start_timeout = "60s"
ready_timeout = "30s"
heartbeat_interval = "30s"
rpc_timeout = "30s"
crash_limit = 3                 # comment
crash_window = "10m"
probation = "10m"
drain_timeout = "30m"

[agent.env]
ANTHROPIC_API_KEY = "${ANTHROPIC_API_KEY}"
PLAIN = "prefix-${ANTHROPIC_API_KEY}-suffix $NOT_A_REF"

[gateway]
user_name = "bearbot"
crash_limit = 5
token_ttl = "10m"
replay_window = "10m"
message_retention = "30d"
attachment_timeout = "60s"
image_max_bytes = 5242880
maintenance_notice = "bearbot 正在修复，暂时无法回复。"
[gateway.http]
listen = "127.0.0.1:8080"

[platforms.telegram]
enabled = true
bot_token = "${TELEGRAM_BOT_TOKEN}"

[platforms.qq]
enabled = false
app_id = "${QQ_BOT_APP_ID}"
client_secret = "${QQ_BOT_CLIENT_SECRET}"
sandbox = false

[platforms.wecom]
enabled = false
bot_id = "${WECOM_BOT_ID}"
secret = "${WECOM_BOT_SECRET}"

[platforms.console]
enabled = false

[bridge]
observe_limit = 10
hold_warn_after = "1d2h"
error_notice = "出错了"

[classifier]
enabled = false
model = "openrouter/anthropic/claude"
timeout = "15s"
rules_max_bytes = 100

[recovery]
timeout = "30m"
attempts = 4
[recovery.env]
ANTHROPIC_API_KEY = "${BEARBOT_RECOVERY_API_KEY}"

[maintenance]
on_enter = ["/bearbot/config/hooks/notify.sh"]
on_exit = ["/bearbot/config/hooks/a.sh", "/bearbot/config/hooks/b.sh"]
hook_timeout = "30s"
`)
	if cfg.Daemon.LogLevel != "debug" {
		t.Errorf("log_level = %q", cfg.Daemon.LogLevel)
	}
	if got := cfg.Agent.Env["ANTHROPIC_API_KEY"]; got != "sk-agent" {
		t.Errorf("agent env = %q", got)
	}
	if got := cfg.Agent.Env["PLAIN"]; got != "prefix-sk-agent-suffix $NOT_A_REF" {
		t.Errorf("agent env PLAIN = %q", got)
	}
	if cfg.Gateway.HTTP.Listen != "127.0.0.1:8080" {
		t.Errorf("listen = %q", cfg.Gateway.HTTP.Listen)
	}
	if cfg.Platforms.Telegram.BotToken != "123:abc" {
		t.Errorf("bot token = %q", cfg.Platforms.Telegram.BotToken)
	}
	if cfg.Platforms.QQ.AppID != "" || cfg.Platforms.QQ.ClientSecret != "" {
		t.Errorf("disabled QQ kept credentials: %+v", cfg.Platforms.QQ)
	}
	if cfg.Platforms.Console.Enabled {
		t.Errorf("console enabled")
	}
	if cfg.Bridge.ObserveLimit != 10 || cfg.Bridge.HoldWarnAfter != 26*time.Hour || cfg.Bridge.ErrorNotice != "出错了" {
		t.Errorf("bridge = %+v", cfg.Bridge)
	}
	if cfg.Classifier.Enabled || cfg.Classifier.RulesMaxBytes != 100 {
		t.Errorf("classifier = %+v", cfg.Classifier)
	}
	if p, m := cfg.Classifier.ModelParts(); p != "openrouter" || m != "anthropic/claude" {
		t.Errorf("model parts = %q %q", p, m)
	}
	if cfg.Recovery.Attempts != 4 || cfg.Recovery.Env["ANTHROPIC_API_KEY"] != "sk-recovery" {
		t.Errorf("recovery = %+v", cfg.Recovery)
	}
	if len(cfg.Maintenance.OnEnter) != 1 || cfg.Maintenance.OnEnter[0] != "/bearbot/config/hooks/notify.sh" {
		t.Errorf("on_enter = %v", cfg.Maintenance.OnEnter)
	}
	if len(cfg.Maintenance.OnExit) != 2 || cfg.Maintenance.OnExit[1] != "/bearbot/config/hooks/b.sh" {
		t.Errorf("on_exit = %v", cfg.Maintenance.OnExit)
	}
}

func TestExpansionEverywhere(t *testing.T) {
	t.Setenv("BB_PROBATION", "5s")
	t.Setenv("BB_HOOK", "/hooks/enter.sh")
	cfg := mustLoad(t, minimal+`
[agent]
probation = "${BB_PROBATION}"
[maintenance]
on_enter = ["${BB_HOOK}"]
`)
	if cfg.Agent.Probation != 5*time.Second {
		t.Errorf("probation = %v", cfg.Agent.Probation)
	}
	if cfg.Maintenance.OnEnter[0] != "/hooks/enter.sh" {
		t.Errorf("on_enter = %v", cfg.Maintenance.OnEnter)
	}
}

func TestDurations(t *testing.T) {
	cases := map[string]time.Duration{
		"30d":    30 * 24 * time.Hour,
		"1d12h":  36 * time.Hour,
		"0d1s":   time.Second,
		"90s":    90 * time.Second,
		"1h30m":  90 * time.Minute,
		"250ms":  250 * time.Millisecond,
		"2d30m":  48*time.Hour + 30*time.Minute,
		"10m":    10 * time.Minute,
		"1.5h":   90 * time.Minute,
		"100000": 0,
	}
	for in, want := range cases {
		got, err := ParseDuration(in)
		if want == 0 {
			if err == nil {
				t.Errorf("ParseDuration(%q) = %v, want error", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"d", "1d-1h", "1dx", "abc", "-1d", "1.5d"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("ParseDuration(%q) succeeded", bad)
		}
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{"unknown key", minimal + "[agent]\nstart_timout = \"1s\"\n", []string{"未知的配置项", "agent.start_timout", "第 5 行"}},
		{"unknown section", minimal + "[nonsense]\na = 1\n", []string{"未知的配置项", "nonsense"}},
		{"unknown platform", minimal + "[platforms.slack]\nenabled = true\n", []string{"未知的配置项", "platforms.slack"}},
		{"syntax", minimal + "[agent\n", []string{"第 4 行"}},
		{"type mismatch", minimal + "[agent]\ncrash_limit = \"3\"\n", []string{"第 5 行"}},
		{"missing var", minimal + "[agent.env]\nKEY = \"${BEARBOT_TEST_SURELY_MISSING_VAR}\"\n", []string{"[agent.env] KEY", "BEARBOT_TEST_SURELY_MISSING_VAR", "不存在"}},
		{"missing var recovery", minimal + "[recovery.env]\nKEY = \"x${BEARBOT_TEST_SURELY_MISSING_VAR2}\"\n", []string{"[recovery.env] KEY", "BEARBOT_TEST_SURELY_MISSING_VAR2"}},
		{"missing telegram var", "", []string{"[platforms.telegram] bot_token", "TELEGRAM_BOT_TOKEN"}},
		{"bad reference", minimal + "[gateway]\nuser_name = \"${1abc}\"\n", []string{"[gateway] user_name", "无效的变量引用"}},
		{"unterminated reference", minimal + "[gateway]\nuser_name = \"${abc\"\n", []string{"无效的变量引用"}},
		{"bad env name", minimal + "[agent.env]\n\"BAD-NAME\" = \"x\"\n", []string{"BAD-NAME", "环境变量名"}},
		{"env value type", minimal + "[agent.env]\nKEY = 1\n", []string{"第 5 行"}},
		{"bad duration", minimal + "[agent]\nprobation = \"ten minutes\"\n", []string{"[agent] probation", "不是有效的时长"}},
		{"zero duration", minimal + "[agent]\nrpc_timeout = \"0s\"\n", []string{"[agent] rpc_timeout", "必须大于 0"}},
		{"negative duration", minimal + "[daemon]\nterm_grace = \"-1s\"\n", []string{"[daemon] term_grace", "必须大于 0"}},
		{"crash limit", minimal + "[agent]\ncrash_limit = 0\n", []string{"[agent] crash_limit"}},
		{"attempts", minimal + "[recovery]\nattempts = 0\n", []string{"[recovery] attempts"}},
		{"log level", minimal + "[daemon]\nlog_level = \"verbose\"\n", []string{"log_level"}},
		{"model", minimal + "[classifier]\nmodel = \"haiku\"\n", []string{"[classifier] model"}},
		{"wecom", minimal + "[platforms.wecom]\nenabled = true\nbot_id = \"a\"\nsecret = \"b\"\n", []string{"[platforms.wecom]", "企业微信"}},
		{"telegram empty token", "[platforms.telegram]\nenabled = true\nbot_token = \"\"\n", []string{"bot_token", "不能为空"}},
		{"qq missing secret", minimal + "[platforms.qq]\nenabled = true\napp_id = \"1\"\nclient_secret = \"\"\n", []string{"client_secret"}},
		{"empty hook", minimal + "[maintenance]\non_enter = [\"\"]\n", []string{"on_enter[0]"}},
		{"empty notice", minimal + "[gateway]\nmaintenance_notice = \" \"\n", []string{"maintenance_notice"}},
		{"rules max bytes", minimal + "[classifier]\nrules_max_bytes = 0\n", []string{"rules_max_bytes"}},
		{"listen without port", minimal + "[gateway.http]\nlisten = \"localhost\"\n", []string{"[gateway.http] listen", "localhost", "host:port"}},
		{"listen port too large", minimal + "[gateway.http]\nlisten = \"0.0.0.0:99999\"\n", []string{"[gateway.http] listen", "1～65535"}},
		{"listen port zero", minimal + "[gateway.http]\nlisten = \":0\"\n", []string{"[gateway.http] listen"}},
		{"sub-millisecond gateway duration", minimal + "[gateway]\ntoken_ttl = \"500us\"\n", []string{"[gateway] token_ttl", "必须不小于 1ms"}},
		{"sub-millisecond classifier timeout", minimal + "[classifier]\ntimeout = \"500us\"\n", []string{"[classifier] timeout", "必须不小于 1ms"}},
		{"sub-millisecond hold warning", minimal + "[bridge]\nhold_warn_after = \"999us\"\n", []string{"[bridge] hold_warn_after", "必须不小于 1ms"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := loadErr(t, c.content)
			for _, w := range c.want {
				if !strings.Contains(msg, w) {
					t.Errorf("error %q does not contain %q", msg, w)
				}
			}
			if !strings.Contains(msg, "bearbot.toml") {
				t.Errorf("error %q does not name the file", msg)
			}
		})
	}
}

func TestDisabledPlatformsNotExpanded(t *testing.T) {
	cfg := mustLoad(t, `
[platforms.telegram]
enabled = false
bot_token = "${BEARBOT_TEST_SURELY_MISSING_VAR}"
[platforms.qq]
enabled = false
app_id = "${BEARBOT_TEST_SURELY_MISSING_VAR}"
`)
	if cfg.Platforms.Telegram.BotToken != "" {
		t.Errorf("bot token = %q", cfg.Platforms.Telegram.BotToken)
	}
}

func TestQQEnabled(t *testing.T) {
	t.Setenv("BB_QQ_ID", "102")
	t.Setenv("BB_QQ_SECRET", "s3cret")
	cfg := mustLoad(t, minimal+`
[platforms.qq]
enabled = true
app_id = "${BB_QQ_ID}"
client_secret = "${BB_QQ_SECRET}"
sandbox = true
`)
	if !cfg.Platforms.QQ.Enabled || cfg.Platforms.QQ.AppID != "102" || cfg.Platforms.QQ.ClientSecret != "s3cret" || !cfg.Platforms.QQ.Sandbox ||
		!cfg.Platforms.QQ.Markdown {
		t.Fatalf("qq = %+v", cfg.Platforms.QQ)
	}
}

func TestQQMarkdownOff(t *testing.T) {
	cfg := mustLoad(t, minimal+`
[platforms.qq]
enabled = true
app_id = "102"
client_secret = "s3cret"
markdown = false
`)
	if cfg.Platforms.QQ.Markdown {
		t.Fatalf("qq = %+v", cfg.Platforms.QQ)
	}
}

// TestListenForms: the forms the gateway accepts for [gateway.http] listen, and millisecond durations.
func TestListenForms(t *testing.T) {
	for _, listen := range []string{"8080", ":8080", "127.0.0.1:8080", "[::1]:8443", "0.0.0.0:65535"} {
		cfg := mustLoad(t, minimal+"[gateway.http]\nlisten = \""+listen+"\"\n")
		if cfg.Gateway.HTTP.Listen != listen {
			t.Errorf("listen = %q, want %q", cfg.Gateway.HTTP.Listen, listen)
		}
	}
	cfg := mustLoad(t, minimal+"[gateway]\ntoken_ttl = \"1ms\"\n[classifier]\ntimeout = \"1500us\"\n")
	if cfg.Gateway.TokenTTL != time.Millisecond || cfg.Classifier.Timeout != 1500*time.Microsecond {
		t.Fatalf("durations = %v %v", cfg.Gateway.TokenTTL, cfg.Classifier.Timeout)
	}
}

func TestMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err == nil || !strings.Contains(err.Error(), "读取配置文件") {
		t.Fatalf("err = %v", err)
	}
}
