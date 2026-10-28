// Package config parses and validates bearbot.toml (DESIGN 12).
//
// Every key of DESIGN 12 has the default shown there, except the [agent.env] and [recovery.env] maps, which default
// to empty maps (DESIGN gives their entries as examples). Unknown keys are an error. String values may reference
// process environment variables as "${NAME}"; a missing variable is an error that names it. The sections of disabled
// platforms are not expanded, because their values are never used.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// Config is the validated content of bearbot.toml. Durations are already parsed.
type Config struct {
	Daemon      Daemon
	Agent       Agent
	Gateway     Gateway
	Platforms   Platforms
	Bridge      Bridge
	Classifier  Classifier
	Recovery    Recovery
	Maintenance Maintenance
}

// Daemon is the [daemon] section.
type Daemon struct {
	// LogLevel is one of "debug", "info", "warn", "error".
	LogLevel      string
	ShutdownGrace time.Duration
	TermGrace     time.Duration
}

// Agent is the [agent] section.
type Agent struct {
	StartTimeout      time.Duration
	ReadyTimeout      time.Duration
	HeartbeatInterval time.Duration
	RPCTimeout        time.Duration
	// CrashLimit is the number of unexpected exits within CrashWindow that opens an incident.
	CrashLimit   int
	CrashWindow  time.Duration
	Probation    time.Duration
	DrainTimeout time.Duration
	// Env holds the expanded [agent.env] entries.
	Env map[string]string
}

// Gateway is the [gateway] section.
type Gateway struct {
	UserName          string
	CrashLimit        int
	TokenTTL          time.Duration
	ReplayWindow      time.Duration
	MessageRetention  time.Duration
	AttachmentTimeout time.Duration
	ImageMaxBytes     int64
	MaintenanceNotice string
	HTTP              GatewayHTTP
}

// GatewayHTTP is the [gateway.http] section.
type GatewayHTTP struct {
	// Listen is empty when the HTTP service is disabled.
	Listen string
}

// Platforms holds the [platforms.*] sections.
type Platforms struct {
	Telegram Telegram
	QQ       QQ
	WeCom    WeCom
	Console  Console
}

// Telegram is the [platforms.telegram] section.
type Telegram struct {
	Enabled  bool
	BotToken string
}

// QQ is the [platforms.qq] section.
type QQ struct {
	Enabled      bool
	AppID        string
	ClientSecret string
	Sandbox      bool
}

// WeCom is the [platforms.wecom] section. Enabled is always false after validation: the adapter is not implemented
// in this version.
type WeCom struct {
	Enabled bool
	BotID   string
	Secret  string
}

// Console is the [platforms.console] section.
type Console struct {
	Enabled bool
}

// Bridge is the [bridge] section.
type Bridge struct {
	ObserveLimit  int
	HoldWarnAfter time.Duration
	ErrorNotice   string
}

// Classifier is the [classifier] section.
type Classifier struct {
	Enabled bool
	// Model is "<provider>/<model id>"; the model id may itself contain "/".
	Model         string
	Timeout       time.Duration
	RulesMaxBytes int64
}

// ModelParts splits Model into provider and model id at the first "/".
func (c Classifier) ModelParts() (provider, modelID string) {
	provider, modelID, _ = strings.Cut(c.Model, "/")
	return provider, modelID
}

// Recovery is the [recovery] section.
type Recovery struct {
	Timeout  time.Duration
	Attempts int
	// Env holds the expanded [recovery.env] entries.
	Env map[string]string
}

// Maintenance is the [maintenance] section.
type Maintenance struct {
	// OnEnter and OnExit are the entries of on_enter and on_exit as written in bearbot.toml, after expansion.
	OnEnter     []string
	OnExit      []string
	HookTimeout time.Duration
}

type rawConfig struct {
	Daemon      rawDaemon      `toml:"daemon"`
	Agent       rawAgent       `toml:"agent"`
	Gateway     rawGateway     `toml:"gateway"`
	Platforms   rawPlatforms   `toml:"platforms"`
	Bridge      rawBridge      `toml:"bridge"`
	Classifier  rawClassifier  `toml:"classifier"`
	Recovery    rawRecovery    `toml:"recovery"`
	Maintenance rawMaintenance `toml:"maintenance"`
}

type rawDaemon struct {
	LogLevel      string `toml:"log_level"`
	ShutdownGrace string `toml:"shutdown_grace"`
	TermGrace     string `toml:"term_grace"`
}

type rawAgent struct {
	StartTimeout      string            `toml:"start_timeout"`
	ReadyTimeout      string            `toml:"ready_timeout"`
	HeartbeatInterval string            `toml:"heartbeat_interval"`
	RPCTimeout        string            `toml:"rpc_timeout"`
	CrashLimit        int               `toml:"crash_limit"`
	CrashWindow       string            `toml:"crash_window"`
	Probation         string            `toml:"probation"`
	DrainTimeout      string            `toml:"drain_timeout"`
	Env               map[string]string `toml:"env"`
}

type rawGateway struct {
	UserName          string         `toml:"user_name"`
	CrashLimit        int            `toml:"crash_limit"`
	TokenTTL          string         `toml:"token_ttl"`
	ReplayWindow      string         `toml:"replay_window"`
	MessageRetention  string         `toml:"message_retention"`
	AttachmentTimeout string         `toml:"attachment_timeout"`
	ImageMaxBytes     int64          `toml:"image_max_bytes"`
	MaintenanceNotice string         `toml:"maintenance_notice"`
	HTTP              rawGatewayHTTP `toml:"http"`
}

type rawGatewayHTTP struct {
	Listen string `toml:"listen"`
}

type rawPlatforms struct {
	Telegram rawTelegram `toml:"telegram"`
	QQ       rawQQ       `toml:"qq"`
	WeCom    rawWeCom    `toml:"wecom"`
	Console  rawConsole  `toml:"console"`
}

type rawTelegram struct {
	Enabled  bool   `toml:"enabled"`
	BotToken string `toml:"bot_token"`
}

type rawQQ struct {
	Enabled      bool   `toml:"enabled"`
	AppID        string `toml:"app_id"`
	ClientSecret string `toml:"client_secret"`
	Sandbox      bool   `toml:"sandbox"`
}

type rawWeCom struct {
	Enabled bool   `toml:"enabled"`
	BotID   string `toml:"bot_id"`
	Secret  string `toml:"secret"`
}

type rawConsole struct {
	Enabled bool `toml:"enabled"`
}

type rawBridge struct {
	ObserveLimit  int    `toml:"observe_limit"`
	HoldWarnAfter string `toml:"hold_warn_after"`
	ErrorNotice   string `toml:"error_notice"`
}

type rawClassifier struct {
	Enabled       bool   `toml:"enabled"`
	Model         string `toml:"model"`
	Timeout       string `toml:"timeout"`
	RulesMaxBytes int64  `toml:"rules_max_bytes"`
}

type rawRecovery struct {
	Timeout  string            `toml:"timeout"`
	Attempts int               `toml:"attempts"`
	Env      map[string]string `toml:"env"`
}

type rawMaintenance struct {
	OnEnter     []string `toml:"on_enter"`
	OnExit      []string `toml:"on_exit"`
	HookTimeout string   `toml:"hook_timeout"`
}

// defaults returns the values of DESIGN 12.
func defaults() rawConfig {
	return rawConfig{
		Daemon: rawDaemon{LogLevel: "info", ShutdownGrace: "90s", TermGrace: "10s"},
		Agent: rawAgent{
			StartTimeout:      "60s",
			ReadyTimeout:      "30s",
			HeartbeatInterval: "30s",
			RPCTimeout:        "30s",
			CrashLimit:        3,
			CrashWindow:       "10m",
			Probation:         "10m",
			DrainTimeout:      "30m",
		},
		Gateway: rawGateway{
			UserName:          "bearbot",
			CrashLimit:        5,
			TokenTTL:          "10m",
			ReplayWindow:      "10m",
			MessageRetention:  "30d",
			AttachmentTimeout: "60s",
			ImageMaxBytes:     5242880,
			MaintenanceNotice: "bearbot 正在修复，暂时无法回复。",
		},
		Platforms: rawPlatforms{
			Telegram: rawTelegram{Enabled: true, BotToken: "${TELEGRAM_BOT_TOKEN}"},
			QQ:       rawQQ{Enabled: false, AppID: "${QQ_BOT_APP_ID}", ClientSecret: "${QQ_BOT_CLIENT_SECRET}"},
			WeCom:    rawWeCom{Enabled: false, BotID: "${WECOM_BOT_ID}", Secret: "${WECOM_BOT_SECRET}"},
			Console:  rawConsole{Enabled: true},
		},
		Bridge: rawBridge{ObserveLimit: 30, HoldWarnAfter: "10m", ErrorNotice: "处理时出现错误，已通知管理员。"},
		Classifier: rawClassifier{
			Enabled:       true,
			Model:         "anthropic/claude-haiku-4-5-20251001",
			Timeout:       "15s",
			RulesMaxBytes: 32768,
		},
		Recovery:    rawRecovery{Timeout: "30m", Attempts: 2},
		Maintenance: rawMaintenance{OnEnter: []string{}, OnExit: []string{}, HookTimeout: "30s"},
	}
}

// Load reads and validates the configuration file at path. Variables are looked up in the process environment.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s 失败：%w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("配置文件 %s：%w", path, err)
	}
	return cfg, nil
}

// Parse decodes and validates a bearbot.toml document. Variables are looked up in the process environment.
func Parse(data []byte) (*Config, error) {
	raw := defaults()
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, describeDecodeError(err)
	}
	if err := expandAll(&raw); err != nil {
		return nil, err
	}
	return build(&raw)
}

func describeDecodeError(err error) error {
	var strict *toml.StrictMissingError
	if errors.As(err, &strict) {
		keys := make([]string, 0, len(strict.Errors))
		for i := range strict.Errors {
			e := &strict.Errors[i]
			row, _ := e.Position()
			keys = append(keys, fmt.Sprintf("%s（第 %d 行）", strings.Join(e.Key(), "."), row))
		}
		return fmt.Errorf("未知的配置项：%s", strings.Join(keys, "，"))
	}
	var decErr *toml.DecodeError
	if errors.As(err, &decErr) {
		row, col := decErr.Position()
		detail := strings.TrimPrefix(decErr.Error(), "toml: ")
		if key := strings.Join(decErr.Key(), "."); key != "" {
			return fmt.Errorf("第 %d 行第 %d 列（%s）无法解析：%s", row, col, key, detail)
		}
		return fmt.Errorf("第 %d 行第 %d 列无法解析：%s", row, col, detail)
	}
	return fmt.Errorf("无法解析：%w", err)
}

var (
	refPattern     = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Expand replaces every "${NAME}" in value with the value of the environment variable NAME. key names the
// configuration key in error messages. A "${" that does not start a valid reference is an error.
func Expand(key, value string) (string, error) {
	var out strings.Builder
	rest := value
	for {
		i := strings.Index(rest, "${")
		if i < 0 {
			out.WriteString(rest)
			return out.String(), nil
		}
		out.WriteString(rest[:i])
		loc := refPattern.FindStringSubmatchIndex(rest[i:])
		if loc == nil || loc[0] != 0 {
			return "", fmt.Errorf("%s：无效的变量引用 %q（格式为 ${变量名}）", key, rest[i:])
		}
		name := rest[i+loc[2] : i+loc[3]]
		v, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("%s 引用的环境变量 %s 不存在", key, name)
		}
		out.WriteString(v)
		rest = rest[i+loc[1]:]
	}
}

func expandAll(raw *rawConfig) error {
	var firstErr error
	str := func(key string, p *string) {
		if firstErr != nil {
			return
		}
		v, err := Expand(key, *p)
		if err != nil {
			firstErr = err
			return
		}
		*p = v
	}
	envMap := func(section string, m map[string]string) {
		names := make([]string, 0, len(m))
		for k := range m {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if firstErr != nil {
				return
			}
			if !envNamePattern.MatchString(k) {
				firstErr = fmt.Errorf("[%s] 中的 %q 不是有效的环境变量名", section, k)
				return
			}
			v := m[k]
			str("["+section+"] "+k, &v)
			m[k] = v
		}
	}
	list := func(key string, l []string) {
		for i := range l {
			str(fmt.Sprintf("%s[%d]", key, i), &l[i])
		}
	}

	str("[daemon] log_level", &raw.Daemon.LogLevel)
	str("[daemon] shutdown_grace", &raw.Daemon.ShutdownGrace)
	str("[daemon] term_grace", &raw.Daemon.TermGrace)

	str("[agent] start_timeout", &raw.Agent.StartTimeout)
	str("[agent] ready_timeout", &raw.Agent.ReadyTimeout)
	str("[agent] heartbeat_interval", &raw.Agent.HeartbeatInterval)
	str("[agent] rpc_timeout", &raw.Agent.RPCTimeout)
	str("[agent] crash_window", &raw.Agent.CrashWindow)
	str("[agent] probation", &raw.Agent.Probation)
	str("[agent] drain_timeout", &raw.Agent.DrainTimeout)
	envMap("agent.env", raw.Agent.Env)

	str("[gateway] user_name", &raw.Gateway.UserName)
	str("[gateway] token_ttl", &raw.Gateway.TokenTTL)
	str("[gateway] replay_window", &raw.Gateway.ReplayWindow)
	str("[gateway] message_retention", &raw.Gateway.MessageRetention)
	str("[gateway] attachment_timeout", &raw.Gateway.AttachmentTimeout)
	str("[gateway] maintenance_notice", &raw.Gateway.MaintenanceNotice)
	str("[gateway.http] listen", &raw.Gateway.HTTP.Listen)

	if raw.Platforms.Telegram.Enabled {
		str("[platforms.telegram] bot_token", &raw.Platforms.Telegram.BotToken)
	}
	if raw.Platforms.QQ.Enabled {
		str("[platforms.qq] app_id", &raw.Platforms.QQ.AppID)
		str("[platforms.qq] client_secret", &raw.Platforms.QQ.ClientSecret)
	}

	str("[bridge] hold_warn_after", &raw.Bridge.HoldWarnAfter)
	str("[bridge] error_notice", &raw.Bridge.ErrorNotice)

	str("[classifier] model", &raw.Classifier.Model)
	str("[classifier] timeout", &raw.Classifier.Timeout)

	str("[recovery] timeout", &raw.Recovery.Timeout)
	envMap("recovery.env", raw.Recovery.Env)

	list("[maintenance] on_enter", raw.Maintenance.OnEnter)
	list("[maintenance] on_exit", raw.Maintenance.OnExit)
	str("[maintenance] hook_timeout", &raw.Maintenance.HookTimeout)
	return firstErr
}

var dayPattern = regexp.MustCompile(`^([0-9]+)d(.*)$`)

// listenPattern is the gateway's rule for [gateway.http] listen: an optional "<host>:" and the port.
var listenPattern = regexp.MustCompile(`^(?:.*:)?([0-9]+)$`)

// ParseDuration parses a Go duration (time.ParseDuration) that may start with a whole number of days, for example
// "30d" or "1d12h".
func ParseDuration(s string) (time.Duration, error) {
	m := dayPattern.FindStringSubmatch(s)
	if m == nil {
		return time.ParseDuration(s)
	}
	days, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number of days in %q: %w", s, err)
	}
	const day = 24 * time.Hour
	if days > int64(1<<63-1)/int64(day) {
		return 0, fmt.Errorf("duration %q is too large", s)
	}
	d := time.Duration(days) * day
	if m[2] == "" {
		return d, nil
	}
	if strings.HasPrefix(m[2], "-") || strings.HasPrefix(m[2], "+") {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	rest, err := time.ParseDuration(m[2])
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	return d + rest, nil
}

func build(raw *rawConfig) (*Config, error) {
	var firstErr error
	fail := func(format string, args ...any) {
		if firstErr == nil {
			firstErr = fmt.Errorf(format, args...)
		}
	}
	dur := func(key, s string) time.Duration {
		d, err := ParseDuration(s)
		if err != nil {
			fail("%s = %q 不是有效的时长（例如 30s、10m、1h、30d）", key, s)
			return 0
		}
		if d <= 0 {
			fail("%s 必须大于 0", key)
		}
		return d
	}
	// durMs is dur for a duration that is passed on in whole milliseconds (gateway.json, the bearbot-im
	// environment); the receivers require at least 1.
	durMs := func(key, s string) time.Duration {
		d := dur(key, s)
		if d > 0 && d < time.Millisecond {
			fail("%s 必须不小于 1ms", key)
		}
		return d
	}
	atLeast := func(key string, v, min int64) {
		if v < min {
			fail("%s 必须不小于 %d，当前为 %d", key, min, v)
		}
	}
	nonEmpty := func(key, v string) {
		if strings.TrimSpace(v) == "" {
			fail("%s 不能为空", key)
		}
	}

	cfg := &Config{}

	switch raw.Daemon.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		fail("[daemon] log_level = %q 无效，可选 debug、info、warn、error", raw.Daemon.LogLevel)
	}
	cfg.Daemon = Daemon{
		LogLevel:      raw.Daemon.LogLevel,
		ShutdownGrace: dur("[daemon] shutdown_grace", raw.Daemon.ShutdownGrace),
		TermGrace:     dur("[daemon] term_grace", raw.Daemon.TermGrace),
	}

	atLeast("[agent] crash_limit", int64(raw.Agent.CrashLimit), 1)
	cfg.Agent = Agent{
		StartTimeout:      dur("[agent] start_timeout", raw.Agent.StartTimeout),
		ReadyTimeout:      dur("[agent] ready_timeout", raw.Agent.ReadyTimeout),
		HeartbeatInterval: dur("[agent] heartbeat_interval", raw.Agent.HeartbeatInterval),
		RPCTimeout:        dur("[agent] rpc_timeout", raw.Agent.RPCTimeout),
		CrashLimit:        raw.Agent.CrashLimit,
		CrashWindow:       dur("[agent] crash_window", raw.Agent.CrashWindow),
		Probation:         dur("[agent] probation", raw.Agent.Probation),
		DrainTimeout:      dur("[agent] drain_timeout", raw.Agent.DrainTimeout),
		Env:               copyMap(raw.Agent.Env),
	}

	nonEmpty("[gateway] user_name", raw.Gateway.UserName)
	atLeast("[gateway] crash_limit", int64(raw.Gateway.CrashLimit), 1)
	atLeast("[gateway] image_max_bytes", raw.Gateway.ImageMaxBytes, 1)
	nonEmpty("[gateway] maintenance_notice", raw.Gateway.MaintenanceNotice)
	if listen := raw.Gateway.HTTP.Listen; listen != "" {
		// The gateway's rule: host:port, :port or port.
		m := listenPattern.FindStringSubmatch(listen)
		port := 0
		if m != nil {
			port, _ = strconv.Atoi(m[1])
		}
		if m == nil || port < 1 || port > 65535 {
			fail("[gateway.http] listen = %q 无效：应当写成 host:port、:port 或 port，端口为 1～65535", listen)
		}
	}
	cfg.Gateway = Gateway{
		UserName:          raw.Gateway.UserName,
		CrashLimit:        raw.Gateway.CrashLimit,
		TokenTTL:          durMs("[gateway] token_ttl", raw.Gateway.TokenTTL),
		ReplayWindow:      durMs("[gateway] replay_window", raw.Gateway.ReplayWindow),
		MessageRetention:  durMs("[gateway] message_retention", raw.Gateway.MessageRetention),
		AttachmentTimeout: durMs("[gateway] attachment_timeout", raw.Gateway.AttachmentTimeout),
		ImageMaxBytes:     raw.Gateway.ImageMaxBytes,
		MaintenanceNotice: raw.Gateway.MaintenanceNotice,
		HTTP:              GatewayHTTP{Listen: raw.Gateway.HTTP.Listen},
	}

	if raw.Platforms.Telegram.Enabled {
		nonEmpty("[platforms.telegram] bot_token", raw.Platforms.Telegram.BotToken)
	}
	if raw.Platforms.QQ.Enabled {
		nonEmpty("[platforms.qq] app_id", raw.Platforms.QQ.AppID)
		nonEmpty("[platforms.qq] client_secret", raw.Platforms.QQ.ClientSecret)
	}
	if raw.Platforms.WeCom.Enabled {
		fail("[platforms.wecom] enabled = true 不受支持：这个版本没有实现企业微信适配器，请设为 false")
	}
	cfg.Platforms = Platforms{
		Telegram: Telegram{Enabled: raw.Platforms.Telegram.Enabled, BotToken: raw.Platforms.Telegram.BotToken},
		QQ: QQ{
			Enabled:      raw.Platforms.QQ.Enabled,
			AppID:        raw.Platforms.QQ.AppID,
			ClientSecret: raw.Platforms.QQ.ClientSecret,
			Sandbox:      raw.Platforms.QQ.Sandbox,
		},
		WeCom:   WeCom{Enabled: false, BotID: raw.Platforms.WeCom.BotID, Secret: raw.Platforms.WeCom.Secret},
		Console: Console{Enabled: raw.Platforms.Console.Enabled},
	}
	if !raw.Platforms.Telegram.Enabled {
		cfg.Platforms.Telegram.BotToken = ""
	}
	if !raw.Platforms.QQ.Enabled {
		cfg.Platforms.QQ.AppID = ""
		cfg.Platforms.QQ.ClientSecret = ""
	}
	cfg.Platforms.WeCom.BotID = ""
	cfg.Platforms.WeCom.Secret = ""

	atLeast("[bridge] observe_limit", int64(raw.Bridge.ObserveLimit), 0)
	nonEmpty("[bridge] error_notice", raw.Bridge.ErrorNotice)
	cfg.Bridge = Bridge{
		ObserveLimit:  raw.Bridge.ObserveLimit,
		HoldWarnAfter: durMs("[bridge] hold_warn_after", raw.Bridge.HoldWarnAfter),
		ErrorNotice:   raw.Bridge.ErrorNotice,
	}

	provider, modelID, ok := strings.Cut(raw.Classifier.Model, "/")
	if !ok || provider == "" || modelID == "" {
		fail("[classifier] model = %q 无效，格式为 <provider>/<model id>", raw.Classifier.Model)
	}
	atLeast("[classifier] rules_max_bytes", raw.Classifier.RulesMaxBytes, 1)
	cfg.Classifier = Classifier{
		Enabled:       raw.Classifier.Enabled,
		Model:         raw.Classifier.Model,
		Timeout:       durMs("[classifier] timeout", raw.Classifier.Timeout),
		RulesMaxBytes: raw.Classifier.RulesMaxBytes,
	}

	atLeast("[recovery] attempts", int64(raw.Recovery.Attempts), 1)
	cfg.Recovery = Recovery{
		Timeout:  dur("[recovery] timeout", raw.Recovery.Timeout),
		Attempts: raw.Recovery.Attempts,
		Env:      copyMap(raw.Recovery.Env),
	}

	for i, c := range raw.Maintenance.OnEnter {
		nonEmpty(fmt.Sprintf("[maintenance] on_enter[%d]", i), c)
	}
	for i, c := range raw.Maintenance.OnExit {
		nonEmpty(fmt.Sprintf("[maintenance] on_exit[%d]", i), c)
	}
	cfg.Maintenance = Maintenance{
		OnEnter:     append([]string{}, raw.Maintenance.OnEnter...),
		OnExit:      append([]string{}, raw.Maintenance.OnExit...),
		HookTimeout: dur("[maintenance] hook_timeout", raw.Maintenance.HookTimeout),
	}

	if firstErr != nil {
		return nil, firstErr
	}
	return cfg, nil
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
