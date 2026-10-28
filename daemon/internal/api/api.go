// Package api serves agent.sock (contracts 6) and admin.sock (contracts 7) over HTTP on unix sockets, and handles
// the requests the gateway sends on gateway-ctl.sock (contracts 4.2). agent.sock accepts each endpoint only in the
// states of contracts 6.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"bearbot/daemon/internal/agent"
	"bearbot/daemon/internal/config"
	"bearbot/daemon/internal/gateway"
	"bearbot/daemon/internal/layout"
	"bearbot/daemon/internal/proc"
)

// Server holds what the three APIs need.
type Server struct {
	Sup       *agent.Supervisor
	Gateway   *gateway.Supervisor
	Paths     layout.Paths
	Config    *config.Config
	Log       *slog.Logger
	DaemonLog *proc.LogWriter
	// Ctx is the daemon's lifetime: operations run with it, not with the request, so a lost connection does not
	// interrupt them (DESIGN 3).
	Ctx context.Context
}

// ListenUnix creates a unix socket at path (a stale socket file is removed first) with the given mode and owner;
// the owner is not changed in rootless mode.
func ListenUnix(path string, mode os.FileMode, uid, gid uint32, users *layout.Users) (*net.UnixListener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("删除旧的 %s 失败：%w", path, err)
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("创建 %s 失败：%w", path, err)
	}
	l.SetUnlinkOnClose(true)
	if err := os.Chmod(path, mode); err != nil {
		l.Close()
		return nil, fmt.Errorf("修改 %s 的权限失败：%w", path, err)
	}
	if err := users.ChownIDs(path, uid, gid); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// NewHTTPServer returns an HTTP server without write timeouts (event and log streams stay open).
func NewHTTPServer(h http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}

// requestError is a bad request body or query (HTTP 400).
type requestError struct{ msg string }

func (e *requestError) Error() string { return e.msg }

func badRequest(format string, args ...any) error { return &requestError{fmt.Sprintf(format, args...)} }

// decode reads a JSON body into v; unknown fields are rejected. An empty body is read as {}.
func decode(r io.Reader, v any) error {
	data, err := io.ReadAll(io.LimitReader(r, 16<<20))
	if err != nil {
		return badRequest("读取请求失败：%v", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("请求不是有效的 JSON：%v", err)
	}
	return nil
}

func statusOf(err error) int {
	var se *agent.StateError
	var ue *agent.UserError
	var ce *agent.ConflictError
	var re *requestError
	switch {
	case errors.As(err, &se), errors.As(err, &ce):
		return http.StatusConflict
	case errors.As(err, &ue), errors.As(err, &re):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
