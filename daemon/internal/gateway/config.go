package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"bearbot/daemon/internal/config"
	"bearbot/daemon/internal/layout"
)

// Console file descriptors in the gateway: ExtraFiles[0] and [1] (contracts 2.4).
const (
	ConsoleInputFd  = 3
	ConsoleOutputFd = 4
)

type telegramJSON struct {
	BotToken string `json:"botToken"`
}

type qqJSON struct {
	AppID        string `json:"appId"`
	ClientSecret string `json:"clientSecret"`
	Sandbox      bool   `json:"sandbox"`
}

type platformsJSON struct {
	Telegram *telegramJSON `json:"telegram,omitempty"`
	QQ       *qqJSON       `json:"qq,omitempty"`
}

type consoleJSON struct {
	InputFd  int `json:"inputFd"`
	OutputFd int `json:"outputFd"`
}

type httpJSON struct {
	Listen string `json:"listen"`
}

// File is the content of gateway.json (contracts 3).
type File struct {
	LogLevel                 string        `json:"logLevel"`
	UserName                 string        `json:"userName"`
	GatewaySocket            string        `json:"gatewaySocket"`
	CtlSocket                string        `json:"ctlSocket"`
	SocketGroupGid           uint32        `json:"socketGroupGid"`
	Database                 string        `json:"database"`
	ConsoleFilesDir          string        `json:"consoleFilesDir"`
	WorkspaceDir             string        `json:"workspaceDir"`
	TokenTTLMs               int64         `json:"tokenTtlMs"`
	ReplayWindowMs           int64         `json:"replayWindowMs"`
	MessageRetentionMs       int64         `json:"messageRetentionMs"`
	AttachmentTimeoutMs      int64         `json:"attachmentTimeoutMs"`
	ImageMaxBytes            int64         `json:"imageMaxBytes"`
	MaintenanceNotice        string        `json:"maintenanceNotice"`
	ClassifierEnabledDefault bool          `json:"classifierEnabledDefault"`
	HTTP                     httpJSON      `json:"http"`
	Platforms                platformsJSON `json:"platforms"`
	Console                  *consoleJSON  `json:"console,omitempty"`
}

// BuildFile returns gateway.json for cfg; the console entry is present only when console is true.
func BuildFile(p layout.Paths, u *layout.Users, cfg *config.Config, console bool) File {
	f := File{
		LogLevel:                 cfg.Daemon.LogLevel,
		UserName:                 cfg.Gateway.UserName,
		GatewaySocket:            p.GatewaySock,
		CtlSocket:                p.GatewayCtlSock,
		SocketGroupGid:           u.BearbotGroup,
		Database:                 p.GatewayDB,
		ConsoleFilesDir:          p.ConsoleFilesDir,
		WorkspaceDir:             p.WorkspaceDir,
		TokenTTLMs:               cfg.Gateway.TokenTTL.Milliseconds(),
		ReplayWindowMs:           cfg.Gateway.ReplayWindow.Milliseconds(),
		MessageRetentionMs:       cfg.Gateway.MessageRetention.Milliseconds(),
		AttachmentTimeoutMs:      cfg.Gateway.AttachmentTimeout.Milliseconds(),
		ImageMaxBytes:            cfg.Gateway.ImageMaxBytes,
		MaintenanceNotice:        cfg.Gateway.MaintenanceNotice,
		ClassifierEnabledDefault: cfg.Classifier.Enabled,
		HTTP:                     httpJSON{Listen: cfg.Gateway.HTTP.Listen},
	}
	if cfg.Platforms.Telegram.Enabled {
		f.Platforms.Telegram = &telegramJSON{BotToken: cfg.Platforms.Telegram.BotToken}
	}
	if cfg.Platforms.QQ.Enabled {
		f.Platforms.QQ = &qqJSON{AppID: cfg.Platforms.QQ.AppID, ClientSecret: cfg.Platforms.QQ.ClientSecret, Sandbox: cfg.Platforms.QQ.Sandbox}
	}
	if console {
		f.Console = &consoleJSON{InputFd: ConsoleInputFd, OutputFd: ConsoleOutputFd}
	}
	return f
}

// WriteFile writes gateway.json at p.GatewayConfig: owner bearbot-gw, mode 0600, through a temporary file and rename.
func WriteFile(p layout.Paths, u *layout.Users, f File) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p.GatewayConfig), ".gateway-*.json")
	if err != nil {
		return fmt.Errorf("写入 %s 失败：%w", p.GatewayConfig, err)
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("写入 %s 失败：%w", p.GatewayConfig, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("写入 %s 失败：%w", p.GatewayConfig, err)
	}
	if err := u.Chown(name, u.Gateway); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, p.GatewayConfig); err != nil {
		os.Remove(name)
		return fmt.Errorf("写入 %s 失败：%w", p.GatewayConfig, err)
	}
	return nil
}
