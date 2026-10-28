package testenv

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

// Config holds the values of the bearbot.toml written for tests. Durations are strings in bearbot.toml syntax.
// [platforms.wecom] is always disabled.
type Config struct {
	LogLevel      string
	ShutdownGrace string
	TermGrace     string

	StartTimeout      string
	ReadyTimeout      string
	HeartbeatInterval string
	RPCTimeout        string
	CrashLimit        int
	CrashWindow       string
	Probation         string
	DrainTimeout      string
	AgentEnv          map[string]string

	GatewayUserName   string
	GatewayCrashLimit int
	TokenTTL          string
	ReplayWindow      string
	MessageRetention  string
	AttachmentTimeout string
	ImageMaxBytes     int64
	MaintenanceNotice string
	HTTPListen        string

	TelegramEnabled  bool
	TelegramBotToken string
	QQEnabled        bool
	QQAppID          string
	QQClientSecret   string
	QQSandbox        bool
	ConsoleEnabled   bool

	ObserveLimit  int
	HoldWarnAfter string
	ErrorNotice   string

	ClassifierEnabled bool
	ClassifierModel   string
	ClassifierTimeout string
	RulesMaxBytes     int64

	RecoveryTimeout  string
	RecoveryAttempts int
	RecoveryEnv      map[string]string

	OnEnter     []string
	OnExit      []string
	HookTimeout string
}

// DefaultConfig returns the test defaults: shortened timeouts, no IM platforms, console enabled.
func DefaultConfig() Config {
	return Config{
		LogLevel:          "debug",
		ShutdownGrace:     "10s",
		TermGrace:         "2s",
		StartTimeout:      "60s",
		ReadyTimeout:      "30s",
		HeartbeatInterval: "5s",
		RPCTimeout:        "10s",
		CrashLimit:        3,
		CrashWindow:       "1m",
		Probation:         "5s",
		DrainTimeout:      "1m",
		AgentEnv:          map[string]string{},
		GatewayUserName:   "bearbot",
		GatewayCrashLimit: 5,
		TokenTTL:          "10m",
		ReplayWindow:      "10m",
		MessageRetention:  "30d",
		AttachmentTimeout: "60s",
		ImageMaxBytes:     5242880,
		MaintenanceNotice: "bearbot 正在修复，暂时无法回复。",
		HTTPListen:        "",
		ConsoleEnabled:    true,
		ObserveLimit:      30,
		HoldWarnAfter:     "10m",
		ErrorNotice:       "处理时出现错误，已通知管理员。",
		ClassifierEnabled: true,
		ClassifierModel:   "anthropic/claude-haiku-4-5-20251001",
		ClassifierTimeout: "15s",
		RulesMaxBytes:     32768,
		RecoveryTimeout:   "5m",
		RecoveryAttempts:  2,
		RecoveryEnv:       map[string]string{},
		OnEnter:           []string{},
		OnExit:            []string{},
		HookTimeout:       "10s",
	}
}

// tomlString quotes s as a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func tomlList(l []string) string {
	parts := make([]string, len(l))
	for i, s := range l {
		parts[i] = tomlString(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

var configTemplate = template.Must(template.New("bearbot.toml").Funcs(template.FuncMap{
	"q":    tomlString,
	"list": tomlList,
}).Parse(`[daemon]
log_level = {{q .LogLevel}}
shutdown_grace = {{q .ShutdownGrace}}
term_grace = {{q .TermGrace}}

[agent]
start_timeout = {{q .StartTimeout}}
ready_timeout = {{q .ReadyTimeout}}
heartbeat_interval = {{q .HeartbeatInterval}}
rpc_timeout = {{q .RPCTimeout}}
crash_limit = {{.CrashLimit}}
crash_window = {{q .CrashWindow}}
probation = {{q .Probation}}
drain_timeout = {{q .DrainTimeout}}

[agent.env]
{{range $k, $v := .AgentEnv}}{{$k}} = {{q $v}}
{{end}}
[gateway]
user_name = {{q .GatewayUserName}}
crash_limit = {{.GatewayCrashLimit}}
token_ttl = {{q .TokenTTL}}
replay_window = {{q .ReplayWindow}}
message_retention = {{q .MessageRetention}}
attachment_timeout = {{q .AttachmentTimeout}}
image_max_bytes = {{.ImageMaxBytes}}
maintenance_notice = {{q .MaintenanceNotice}}
[gateway.http]
listen = {{q .HTTPListen}}

[platforms.telegram]
enabled = {{.TelegramEnabled}}
bot_token = {{q .TelegramBotToken}}

[platforms.qq]
enabled = {{.QQEnabled}}
app_id = {{q .QQAppID}}
client_secret = {{q .QQClientSecret}}
sandbox = {{.QQSandbox}}

[platforms.wecom]
enabled = false

[platforms.console]
enabled = {{.ConsoleEnabled}}

[bridge]
observe_limit = {{.ObserveLimit}}
hold_warn_after = {{q .HoldWarnAfter}}
error_notice = {{q .ErrorNotice}}

[classifier]
enabled = {{.ClassifierEnabled}}
model = {{q .ClassifierModel}}
timeout = {{q .ClassifierTimeout}}
rules_max_bytes = {{.RulesMaxBytes}}

[recovery]
timeout = {{q .RecoveryTimeout}}
attempts = {{.RecoveryAttempts}}
[recovery.env]
{{range $k, $v := .RecoveryEnv}}{{$k}} = {{q $v}}
{{end}}
[maintenance]
on_enter = {{list .OnEnter}}
on_exit = {{list .OnExit}}
hook_timeout = {{q .HookTimeout}}
`))

// RenderConfig renders cfg as bearbot.toml.
func RenderConfig(cfg Config) ([]byte, error) {
	var buf bytes.Buffer
	if err := configTemplate.Execute(&buf, cfg); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// WriteConfig writes cfg to ConfigPath (mode 0600).
func (e *Env) WriteConfig(t testing.TB, cfg Config) {
	t.Helper()
	data, err := RenderConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.ConfigPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Model is the OpenAI compatible model service of contracts section 0.
type Model struct {
	BaseURL string
	APIKey  string
	ID      string
}

// Provider is the provider name the tests use in models.json.
const Provider = "testprovider"

// Ref returns "testprovider/<model>", the default model and classifier model of the tests.
func (m Model) Ref() string { return Provider + "/" + m.ID }

// ModelFromEnv reads BEARBOT_TEST_OPENAI_BASE_URL, BEARBOT_TEST_OPENAI_API_KEY and BEARBOT_TEST_OPENAI_MODEL.
func ModelFromEnv() (Model, bool) {
	m := Model{
		BaseURL: os.Getenv("BEARBOT_TEST_OPENAI_BASE_URL"),
		APIKey:  os.Getenv("BEARBOT_TEST_OPENAI_API_KEY"),
		ID:      os.Getenv("BEARBOT_TEST_OPENAI_MODEL"),
	}
	return m, m.BaseURL != "" && m.APIKey != "" && m.ID != ""
}

// RequireModel skips the test when the model service variables are not set.
func RequireModel(t testing.TB) Model {
	t.Helper()
	m, ok := ModelFromEnv()
	if !ok {
		t.Skip("需要模型服务：设置 BEARBOT_TEST_OPENAI_BASE_URL、BEARBOT_TEST_OPENAI_API_KEY、BEARBOT_TEST_OPENAI_MODEL 后运行")
	}
	return m
}

// ModelsJSON returns the models.json content with the provider testprovider and the single model m.
func ModelsJSON(m Model) []byte {
	data, err := json.MarshalIndent(map[string]any{
		"providers": map[string]any{
			Provider: map[string]any{
				"baseUrl": m.BaseURL,
				"api":     "openai-completions",
				"apiKey":  m.APIKey,
				"models":  []map[string]any{{"id": m.ID}},
			},
		},
	}, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(data, '\n')
}

// SettingsJSON returns a profile settings.json that keeps the daemon's defaults and selects m as default model.
func SettingsJSON(m Model) []byte {
	data, err := json.MarshalIndent(map[string]any{
		"quietStartup":        true,
		"defaultProjectTrust": "never",
		"defaultProvider":     Provider,
		"defaultModel":        m.ID,
	}, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(data, '\n')
}

// InstallModel writes models.json and settings.json for m into the agent profile and into the recovery override
// directory (R/bearbot/config/recovery), so that the agent and recovery use the test model.
func (e *Env) InstallModel(t testing.TB, m Model) {
	t.Helper()
	for _, dir := range []string{e.Paths.AgentDir, e.Paths.RecoveryOverrideDir} {
		if err := os.MkdirAll(dir, 0o775); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "models.json"), ModelsJSON(m), 0o664); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), SettingsJSON(m), 0o664); err != nil {
			t.Fatal(err)
		}
	}
}
