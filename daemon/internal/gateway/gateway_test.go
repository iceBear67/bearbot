package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bearbot/daemon/internal/config"
	"bearbot/daemon/internal/layout"
	"bearbot/daemon/internal/testenv"
)

func TestConsoleInput(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if ok, kind, err := ConsoleInput(r); err != nil || !ok || kind != "管道或 FIFO" {
		t.Fatalf("pipe: %v %q %v", ok, kind, err)
	}
	file := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(file, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if ok, kind, err := ConsoleInput(f); err != nil || !ok || kind != "普通文件" {
		t.Fatalf("regular file: %v %q %v", ok, kind, err)
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if ok, kind, err := ConsoleInput(null); err != nil || ok || !strings.Contains(kind, "字符设备") {
		t.Fatalf("/dev/null: %v %q %v", ok, kind, err)
	}
}

func TestPromptUnavailable(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	done := make(chan error)
	go func() { done <- PromptUnavailable(r, &out) }()
	io.WriteString(w, "hello\n/bb status\nlast line without newline")
	w.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r.Close()
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("output = %q", out.String())
	}
	for _, l := range lines {
		if l != UnavailableNotice {
			t.Fatalf("line %q", l)
		}
	}
}

func TestGatewayFile(t *testing.T) {
	e := testenv.New(t)
	u := e.Users(t)
	if _, err := layout.EnsureDir(layout.DirSpec{Path: filepath.Dir(e.Paths.RunDir), Owner: layout.OwnerRoot, Mode: 0o755}, u); err != nil {
		t.Fatal(err)
	}
	for _, d := range e.Paths.Dirs() {
		if d.Path == e.Paths.RunDir {
			if _, err := layout.EnsureDir(d, u); err != nil {
				t.Fatal(err)
			}
		}
	}
	c := testenv.DefaultConfig()
	c.TelegramEnabled = true
	c.TelegramBotToken = "123:abc"
	c.TokenTTL = "5m"
	e.WriteConfig(t, c)
	cfg, err := config.Load(e.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(e.Paths, u, BuildFile(e.Paths, u, cfg, true)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(e.Paths.GatewayConfig)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("gateway.json: %v %v", info, err)
	}
	data, err := os.ReadFile(e.Paths.GatewayConfig)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"logLevel":                 "debug",
		"userName":                 "bearbot",
		"gatewaySocket":            e.Paths.GatewaySock,
		"ctlSocket":                e.Paths.GatewayCtlSock,
		"socketGroupGid":           float64(u.BearbotGroup),
		"database":                 e.Paths.GatewayDB,
		"consoleFilesDir":          e.Paths.ConsoleFilesDir,
		"workspaceDir":             e.Paths.WorkspaceDir,
		"tokenTtlMs":               float64(300000),
		"replayWindowMs":           float64(600000),
		"messageRetentionMs":       float64(2592000000),
		"attachmentTimeoutMs":      float64(60000),
		"imageMaxBytes":            float64(5242880),
		"maintenanceNotice":        "bearbot 正在修复，暂时无法回复。",
		"classifierEnabledDefault": true,
		"http":                     map[string]any{"listen": ""},
		"platforms":                map[string]any{"telegram": map[string]any{"botToken": "123:abc"}},
		"console":                  map[string]any{"inputFd": float64(3), "outputFd": float64(4)},
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("gateway.json\n got  %s\n want %s", gotJSON, wantJSON)
	}

	if err := WriteFile(e.Paths, u, BuildFile(e.Paths, u, cfg, false)); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(e.Paths.GatewayConfig)
	if strings.Contains(string(data), `"console"`) {
		t.Fatalf("console present without the console: %s", data)
	}
}

// TestStopBeforeStart (DESIGN 5.2): Stop called before Run started the gateway process (a SIGTERM during the
// startup of bearbotd): Run starts no process and returns, and Stop returns.
func TestStopBeforeStart(t *testing.T) {
	e := testenv.New(t)
	e.RequireGateway(t)
	cfg, err := config.Load(e.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(Options{Paths: e.Paths, Users: e.Users(t), Config: cfg, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- g.Stop(time.Second) }()
	for !g.stopping() {
		time.Sleep(time.Millisecond)
	}
	ran := make(chan struct{})
	go func() {
		g.Run(context.Background())
		close(ran)
	}()
	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after Stop")
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return")
	}
	if info := g.Info(); info.Starts != 0 || info.PID != nil {
		t.Fatalf("a gateway process was started after Stop: %+v", info)
	}
}
