package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"bearbot/daemon/internal/config"
	"bearbot/daemon/internal/gateway"
	"bearbot/daemon/internal/incident"
	"bearbot/daemon/internal/proc"
	"bearbot/daemon/internal/rollback"
	"bearbot/daemon/internal/wire"
)

// stream writes the NDJSON answer of admin.sock (contracts 7): {"line"} records, then one {"done"} record.
type stream struct {
	w   http.ResponseWriter
	rc  *http.ResponseController
	enc *json.Encoder
	err error
}

func newStream(w http.ResponseWriter) *stream {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	return &stream{w: w, rc: http.NewResponseController(w), enc: json.NewEncoder(w)}
}

func (s *stream) write(v any) {
	if s.err != nil {
		return
	}
	if s.err = s.enc.Encode(v); s.err == nil {
		s.err = s.rc.Flush()
	}
}

func (s *stream) line(text string) { s.write(map[string]string{"line": text}) }

// text writes every line of t.
func (s *stream) text(t string) {
	for _, l := range strings.Split(strings.TrimRight(t, "\n"), "\n") {
		s.line(l)
	}
}

func (s *stream) done(err error) {
	if err != nil {
		s.write(map[string]any{"done": true, "ok": false, "error": err.Error()})
		return
	}
	s.write(map[string]any{"done": true, "ok": true})
}

// admin wraps an admin.sock handler: the body is decoded into a value of the handler's type before the answer
// starts, so a bad request ends with ok false.
func (s *Server) admin(h func(r *http.Request, out *stream) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out := newStream(w)
		err := h(r, out)
		if err != nil {
			s.Log.Info("admin.sock request failed", "path", r.URL.Path, "err", err)
		}
		out.done(err)
	}
}

// AdminHandler serves admin.sock.
func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/v1/status", s.admin(s.adminStatus))
	mux.HandleFunc("GET /admin/v1/logs", s.admin(s.adminLogs))
	mux.HandleFunc("POST /admin/v1/admin/token", s.admin(s.adminToken))
	mux.HandleFunc("POST /admin/v1/admin/list", s.admin(s.adminList))
	mux.HandleFunc("POST /admin/v1/admin/revoke", s.admin(s.adminRevoke))
	mux.HandleFunc("POST /admin/v1/restart", s.admin(func(r *http.Request, out *stream) error {
		if err := decode(r.Body, &struct{}{}); err != nil {
			return err
		}
		// The progress line only for a restart that can run; Restart checks again when the state changed meanwhile.
		if err := s.Sup.Require("restart", wire.Running); err != nil {
			return err
		}
		out.line("正在重启 agent，等待当前回合结束……")
		t, err := s.Sup.Restart()
		if err != nil {
			return err
		}
		out.text(t)
		return nil
	}))
	mux.HandleFunc("POST /admin/v1/incident/list", s.admin(func(r *http.Request, out *stream) error {
		if err := decode(r.Body, &struct{}{}); err != nil {
			return err
		}
		t, err := s.Sup.IncidentList()
		if err != nil {
			return err
		}
		out.text(t)
		return nil
	}))
	mux.HandleFunc("POST /admin/v1/incident/show", s.admin(func(r *http.Request, out *stream) error {
		var body struct {
			ID string `json:"id"`
		}
		if err := decode(r.Body, &body); err != nil {
			return err
		}
		t, err := s.Sup.IncidentShow(body.ID)
		if err != nil {
			return err
		}
		out.text(t)
		return nil
	}))
	mux.HandleFunc("POST /admin/v1/repair", s.admin(func(r *http.Request, out *stream) error {
		var body struct {
			Note string `json:"note"`
		}
		if err := decode(r.Body, &body); err != nil {
			return err
		}
		if err := s.Sup.Require("repair", wire.Maintenance); err != nil {
			return err
		}
		out.line("正在运行 recovery 修复，可能需要较长时间……")
		t, err := s.Sup.Repair(body.Note)
		if err != nil {
			return err
		}
		out.text(t)
		return nil
	}))
	mux.HandleFunc("POST /admin/v1/resume", s.admin(func(r *http.Request, out *stream) error {
		if err := decode(r.Body, &struct{}{}); err != nil {
			return err
		}
		t, err := s.Sup.Resume()
		if err != nil {
			return err
		}
		out.text(t)
		return nil
	}))
	mux.HandleFunc("POST /admin/v1/rollback", s.admin(func(r *http.Request, out *stream) error {
		var body struct {
			Snapshot string `json:"snapshot"`
		}
		if err := decode(r.Body, &body); err != nil {
			return err
		}
		t, err := s.Sup.Rollback(s.Ctx, rollback.Request{Snapshot: body.Snapshot})
		if err != nil {
			return err
		}
		out.text(t)
		return nil
	}))
	mux.HandleFunc("POST /admin/v1/snapshot/list", s.admin(func(r *http.Request, out *stream) error {
		if err := decode(r.Body, &struct{}{}); err != nil {
			return err
		}
		t, err := s.Sup.SnapshotList(s.Ctx)
		if err != nil {
			return err
		}
		out.text(t)
		return nil
	}))
	mux.HandleFunc("POST /admin/v1/snapshot/diff", s.admin(func(r *http.Request, out *stream) error {
		var body struct {
			Ref  string `json:"ref"`
			Ref2 string `json:"ref2"`
		}
		if err := decode(r.Body, &body); err != nil {
			return err
		}
		t, err := s.Sup.SnapshotDiff(s.Ctx, body.Ref, body.Ref2)
		if err != nil {
			return err
		}
		out.text(t)
		return nil
	}))
	mux.HandleFunc("/", s.admin(func(r *http.Request, out *stream) error {
		return fmt.Errorf("没有这个接口：%s %s", r.Method, r.URL.Path)
	}))
	return mux
}

func localTime(t time.Time) string { return t.Local().Format(time.DateTime) }

func isoLocal(s *string) string {
	if s == nil {
		return "无"
	}
	t, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		return *s
	}
	return localTime(t)
}

func (s *Server) adminStatus(r *http.Request, out *stream) error {
	gi := s.Gateway.Info()
	procText := "没有运行中的进程"
	if gi.PID != nil {
		procText = fmt.Sprintf("PID %d，启动于 %s", *gi.PID, localTime(*gi.StartedAt))
	}
	if gi.Connected {
		out.line(fmt.Sprintf("gateway：已连接（%s，自 %s 起连接；已启动 %d 次，连续意外退出 %d 次）", procText, localTime(*gi.ConnectedAt), gi.Starts, gi.Crashes))
	} else {
		out.line(fmt.Sprintf("gateway：没有连接（%s；已启动 %d 次，连续意外退出 %d 次）", procText, gi.Starts, gi.Crashes))
	}
	var platforms struct {
		Platforms []wire.PlatformStatus `json:"platforms"`
	}
	if err := s.Gateway.Call(s.Ctx, "platforms.status", struct{}{}, &platforms); err != nil {
		out.line("平台：无法取得（" + err.Error() + "）")
	} else {
		out.line("平台：")
		if len(platforms.Platforms) == 0 {
			out.line("  没有启用的平台")
		}
		for _, p := range platforms.Platforms {
			l := fmt.Sprintf("  %s：%s", p.Name, p.State)
			if p.Since != nil {
				l += "，自 " + isoLocal(p.Since)
			}
			if p.LastDisconnectAt != nil {
				l += "；最近断开 " + isoLocal(p.LastDisconnectAt)
			}
			if p.LastReconnectAt != nil {
				l += "；最近重连 " + isoLocal(p.LastReconnectAt)
			}
			if p.LastError != nil {
				l += fmt.Sprintf("；最近的错误（%s）：%s", isoLocal(p.LastErrorAt), *p.LastError)
			}
			out.line(l)
		}
	}
	info, err := s.Sup.Status(s.Ctx)
	if err != nil {
		return err
	}
	out.text(StatusText(info, time.Now()))
	return nil
}

// StatusText describes StatusInfo in Chinese for `bearbotd status`.
func StatusText(info *wire.StatusInfo, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "agent：%s", info.State)
	if info.Reason != nil {
		fmt.Fprintf(&b, "（%s）", *info.Reason)
	}
	b.WriteString("\n")
	if info.PID != nil {
		started := isoLocal(info.StartedAt)
		uptime := ""
		if t, err := time.Parse(time.RFC3339Nano, *info.StartedAt); err == nil {
			uptime = "，已运行 " + now.Sub(t).Round(time.Second).String()
		}
		fmt.Fprintf(&b, "  PID：%d，启动于 %s%s\n", *info.PID, started, uptime)
	} else {
		b.WriteString("  PID：没有运行中的 agent 进程\n")
	}
	fmt.Fprintf(&b, "  重启次数：%d，crash_window 内意外退出：%d 次\n", info.Restarts, info.CrashesInWindow)
	fmt.Fprintf(&b, "  会话：%s\n", deref(info.SessionID))
	if info.ProbationUntil != nil {
		fmt.Fprintf(&b, "  观察期：到 %s 为止\n", isoLocal(info.ProbationUntil))
	}
	fmt.Fprintf(&b, "  runningSnapshot：%s，last-good：%s\n", deref(info.RunningSnapshot), deref(info.LastGood))
	if pa := info.PendingApply; pa != nil {
		fmt.Fprintf(&b, "  待应用：%s（%s）\n", pa.Snapshot, pa.Summary)
	}
	if inc := info.Incident; inc != nil {
		fmt.Fprintf(&b, "  当前故障：%s（%s，已尝试修复 %d 次）\n", inc.ID, inc.Trigger, inc.Attempts)
	} else {
		b.WriteString("  当前故障：无\n")
	}
	if m := info.Maintenance; m != nil {
		id := "无"
		if m.IncidentID != nil {
			id = *m.IncidentID
		}
		fmt.Fprintf(&b, "  维护状态：%s（故障 %s，开始于 %s）\n", m.Reason, id, isoLocal(&m.Since))
	} else {
		b.WriteString("  维护状态：否\n")
	}
	if rb := info.Rollback; rb != nil {
		notified := "尚未通知 agent"
		if rb.Notified {
			notified = "已通知 agent"
		}
		fmt.Fprintf(&b, "  回滚记录：#%d，目标 %s，%s\n", rb.ID, rb.Target, notified)
	}
	if info.Console {
		b.WriteString("控制台：已启用\n")
	} else {
		b.WriteString("控制台：没有启用\n")
	}
	return b.String()
}

func deref(s *string) string {
	if s == nil {
		return "无"
	}
	return *s
}

func (s *Server) adminLogs(r *http.Request, out *stream) error {
	q := r.URL.Query()
	target := q.Get("target")
	n, err := lineCount(q.Get("n"))
	if err != nil {
		return err
	}
	follow := false
	switch q.Get("follow") {
	case "", "0":
	case "1":
		follow = true
	default:
		return badRequest("follow 必须是 0 或 1")
	}
	var path string
	var writer *proc.LogWriter
	switch {
	case target == "daemon":
		path, writer = s.Paths.DaemonLog, s.DaemonLog
	case target == "gateway":
		path, writer = s.Paths.GatewayLog, s.Gateway.Log()
	case target == "agent":
		path, writer = s.Paths.AgentStderrLog, s.Sup.StderrLog()
	case strings.HasPrefix(target, "recovery:"):
		id := strings.TrimPrefix(target, "recovery:")
		if !incident.ValidID(id) {
			return badRequest("无效的故障 id %q（格式为 inc-<14 位数字>）", id)
		}
		path, writer = s.Paths.RecoveryLog(id), s.Sup.RecoveryLog(id)
	default:
		return badRequest("日志必须是 daemon、gateway、agent 或 recovery:<故障id>，收到 %q", target)
	}
	var sub *proc.Subscription
	if follow && writer != nil {
		sub = writer.Subscribe()
		defer sub.Close()
	}
	lines, err := proc.TailFile(path, n)
	if err != nil {
		return err
	}
	for _, l := range lines {
		out.line(l)
	}
	if !follow {
		return nil
	}
	if sub == nil {
		out.line("（这个故障现在没有在修复，没有新的日志）")
		return nil
	}
	for {
		l, err := sub.Next(r.Context())
		if errors.Is(err, proc.ErrSubscriptionClosed) {
			return nil
		}
		if err != nil {
			return err
		}
		out.line(l)
		if out.err != nil {
			return out.err
		}
	}
}

func (s *Server) adminToken(r *http.Request, out *stream) error {
	var body struct {
		TTL string `json:"ttl"`
	}
	if err := decode(r.Body, &body); err != nil {
		return err
	}
	ttl := s.Config.Gateway.TokenTTL
	if body.TTL != "" {
		d, err := config.ParseDuration(body.TTL)
		if err != nil || d <= 0 {
			return badRequest("ttl %q 不是有效的时长（例如 10m、1h、1d）", body.TTL)
		}
		ttl = d
	}
	var res struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expiresAt"`
	}
	if err := s.Gateway.Call(s.Ctx, "admin.createToken", map[string]int64{"ttlMs": ttl.Milliseconds()}, &res); err != nil {
		return err
	}
	out.line(res.Token)
	out.line(fmt.Sprintf("有效期 %s，到 %s 为止。在 gateway 能收到消息的任意聊天中发送这段令牌（私聊机器人最可靠），发送者即成为 bearbot 管理员。", ttl, isoLocal(&res.ExpiresAt)))
	return nil
}

func (s *Server) adminList(r *http.Request, out *stream) error {
	if err := decode(r.Body, &struct{}{}); err != nil {
		return err
	}
	var res struct {
		Admins []wire.AdminInfo `json:"admins"`
	}
	if err := s.Gateway.Call(s.Ctx, "admin.list", struct{}{}, &res); err != nil {
		return err
	}
	if len(res.Admins) == 0 {
		out.line("还没有管理员：用 bearbotd admin token 生成令牌")
		return nil
	}
	out.line(fmt.Sprintf("管理员（%d 个）：", len(res.Admins)))
	for _, a := range res.Admins {
		l := "  " + a.Identity
		if a.DisplayName != nil {
			l += "（" + *a.DisplayName + "）"
		}
		if a.Builtin {
			l += "  内置，不能撤销"
		}
		l += "  通知会话串：" + deref(a.NotifyThread)
		if a.AddedBy != nil {
			l += "  添加者：" + *a.AddedBy
		}
		if a.AddedAt != nil {
			l += "  添加于 " + isoLocal(a.AddedAt)
		}
		out.line(l)
	}
	return nil
}

func (s *Server) adminRevoke(r *http.Request, out *stream) error {
	var body struct {
		Identity string `json:"identity"`
	}
	if err := decode(r.Body, &body); err != nil {
		return err
	}
	if body.Identity == "" {
		return badRequest("需要身份，例如 telegram:123456")
	}
	if err := s.Gateway.Call(s.Ctx, "admin.revoke", map[string]string{"identity": body.Identity}, nil); err != nil {
		var ce *gateway.CtlError
		if errors.As(err, &ce) {
			return errors.New(ce.Message)
		}
		return err
	}
	out.line("已撤销 " + body.Identity + " 的管理员身份")
	return nil
}
