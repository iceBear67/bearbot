// Package recovery prepares the recovery profile (DESIGN 10.1) and runs the recovery process (DESIGN 10.2,
// contracts 2.3): pi in RPC mode with only the bearbot-recovery extension, in its own profile under the run
// directory, as bearbot.
package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bearbot/daemon/internal/config"
	"bearbot/daemon/internal/layout"
	"bearbot/daemon/internal/pirpc"
	"bearbot/daemon/internal/proc"
)

// OverrideFiles are the files of <ConfigDir>/recovery that replace the template files of the same name (DESIGN 10.1).
var OverrideFiles = []string{"settings.json", "models.json", "auth.json"}

// PrepareProfile empties and rebuilds RecoveryAgentDir: every regular file of the template directory, then the
// override files that exist, all owned by bearbot with mode 0600 (DESIGN 10.1).
func PrepareProfile(p layout.Paths, u *layout.Users) error {
	parent := filepath.Dir(p.RecoveryAgentDir)
	if _, err := layout.EnsureDir(layout.DirSpec{Path: parent, Owner: layout.OwnerRoot, Mode: 0o755}, u); err != nil {
		return err
	}
	if err := os.RemoveAll(p.RecoveryAgentDir); err != nil {
		return fmt.Errorf("清空 %s 失败：%w", p.RecoveryAgentDir, err)
	}
	if _, err := layout.EnsureDir(layout.DirSpec{Path: p.RecoveryAgentDir, Owner: layout.OwnerBearbot, Mode: 0o700}, u); err != nil {
		return err
	}
	entries, err := os.ReadDir(p.RecoveryProfileTemplate)
	if err != nil {
		return fmt.Errorf("读取 recovery profile 模板 %s 失败：%w", p.RecoveryProfileTemplate, err)
	}
	for _, e := range entries {
		src := filepath.Join(p.RecoveryProfileTemplate, e.Name())
		info, err := os.Stat(src)
		if err != nil {
			return fmt.Errorf("读取 %s 失败：%w", src, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := copyFile(src, filepath.Join(p.RecoveryAgentDir, e.Name()), u); err != nil {
			return err
		}
	}
	for _, name := range OverrideFiles {
		src := filepath.Join(p.RecoveryOverrideDir, name)
		info, err := os.Stat(src)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("读取 %s 失败：%w", src, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s 不是普通文件", src)
		}
		if err := copyFile(src, filepath.Join(p.RecoveryAgentDir, name), u); err != nil {
			return err
		}
	}
	return nil
}

// RemoveProfile removes RecoveryAgentDir: the recovery credentials exist only while a repair runs (DESIGN 15).
func RemoveProfile(p layout.Paths) error {
	if err := os.RemoveAll(p.RecoveryAgentDir); err != nil {
		return fmt.Errorf("删除 recovery profile %s 失败：%w", p.RecoveryAgentDir, err)
	}
	return nil
}

func copyFile(src, dst string, u *layout.Users) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("读取 %s 失败：%w", src, err)
	}
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("替换 %s 失败：%w", dst, err)
	}
	return layout.WriteNewFile(dst, data, 0o600, layout.OwnerBearbot, u)
}

// Provider returns defaultProvider of the prepared recovery profile's settings.json.
func Provider(p layout.Paths) (string, error) {
	path := filepath.Join(p.RecoveryAgentDir, "settings.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("recovery profile 中没有 settings.json（模板 %s 与覆盖目录 %s 中都没有）", p.RecoveryProfileTemplate, p.RecoveryOverrideDir)
	}
	if err != nil {
		return "", fmt.Errorf("读取 %s 失败：%w", path, err)
	}
	var settings struct {
		DefaultProvider string `json:"defaultProvider"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return "", fmt.Errorf("recovery profile 的 settings.json 不是有效的 JSON：%w", err)
	}
	if settings.DefaultProvider == "" {
		return "", errors.New("recovery profile 的 settings.json 中没有 defaultProvider")
	}
	return settings.DefaultProvider, nil
}

// CheckCredentials runs `<node> <PiBin> auth check --provider <defaultProvider> --json` as bearbot with the recovery
// environment, so it sees auth.json of the recovery profile, the [recovery.env] variables and models.json. A provider
// without usable credentials is an error that names it (DESIGN 5.7 "recovery 无法运行"). node is node found in the
// daemon's PATH.
func CheckCredentials(ctx context.Context, p layout.Paths, u *layout.Users, cfg *config.Config, node, incidentID string) error {
	provider, err := Provider(p)
	if err != nil {
		return err
	}
	res, err := proc.Run(ctx, u.RunSpec(u.Bearbot, node, []string{p.PiBin, "auth", "check", "--provider", provider, "--json"},
		p.RecoveryEnv(cfg, incidentID), p.RecoveryCwd(), cfg.Agent.StartTimeout))
	if err != nil {
		return fmt.Errorf("检查 recovery 的凭据失败：%w", err)
	}
	if res.OK() {
		return nil
	}
	if res.TimedOut {
		return fmt.Errorf("检查 recovery 的凭据超过 %s 没有结束", cfg.Agent.StartTimeout)
	}
	var answer struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(res.Stdout, &answer); err == nil && answer.Status != "" {
		return fmt.Errorf("没有 %s 的可用凭据（%s：%s）：auth.json、[recovery.env] 与 models.json 都没有提供", provider, answer.Status, answer.Reason)
	}
	return fmt.Errorf("检查 recovery 的凭据失败：%w", res.Err())
}

// TurnEnd describes how a recovery turn ended (agent_settled).
type TurnEnd struct {
	Aborted bool
	// StopReason and ErrorMessage come from the last assistant message of the run; empty when there was none.
	StopReason   string
	ErrorMessage string
}

// Failed reports whether the run ended with an error (DESIGN 5.7 step 3: quota, invalid credentials, …).
func (t TurnEnd) Failed() bool { return !t.Aborted && t.StopReason == "error" }

// Options describes a recovery process.
type Options struct {
	Paths  layout.Paths
	Users  *layout.Users
	Config *config.Config
	// Node is node found in the daemon's PATH; the process runs as `<Node> <PiBin> <args>`.
	Node       string
	IncidentID string
	// Log receives stderr and the logged RPC records (logs/recovery/<incident>.log).
	Log    *proc.LogWriter
	Logger *slog.Logger
}

// Process is a running recovery process.
type Process struct {
	opts   Options
	p      *proc.Process
	rpc    *pirpc.Client
	stderr *proc.Ring

	mu        sync.Mutex
	assistant *pirpc.AssistantEnd
	turns     chan TurnEnd
}

// StderrTailLines is the number of stderr lines kept in memory for messages.
const StderrTailLines = 200

// Start starts the recovery process and waits for the get_state answer within [agent] start_timeout.
func Start(ctx context.Context, opts Options) (*Process, error) {
	rp := &Process{opts: opts, stderr: proc.NewRing(StderrTailLines), turns: make(chan TurnEnd, 16)}
	fmt.Fprintf(opts.Log, "=== recovery 进程启动于 %s（故障 %s） ===\n", time.Now().Format(time.RFC3339), opts.IncidentID)
	p, err := proc.Start(proc.Spec{
		Path:       opts.Node,
		Args:       append([]string{opts.Paths.PiBin}, opts.Paths.RecoveryArgs(opts.IncidentID)...),
		Env:        opts.Paths.RecoveryEnv(opts.Config, opts.IncidentID),
		Dir:        opts.Paths.RecoveryCwd(),
		Credential: opts.Users.Credential(opts.Users.Bearbot),
		StdinPipe:  true,
		Stderr:     io.MultiWriter(opts.Log, rp.stderr),
	})
	if err != nil {
		return nil, fmt.Errorf("启动 recovery 进程失败：%w", err)
	}
	rp.p = p
	rp.rpc = pirpc.New(p.Stdin, p.Stdout, pirpc.Options{OnEvent: rp.onEvent, IDPrefix: "recovery-"})
	go func() {
		<-p.Done()
		rp.rpc.Close()
	}()
	callCtx, cancel := context.WithTimeout(ctx, opts.Config.Agent.StartTimeout)
	defer cancel()
	if _, err := rp.rpc.GetState(callCtx, "boot"); err != nil {
		reason := fmt.Sprintf("recovery 进程没有在 %s 内响应 get_state：%v", opts.Config.Agent.StartTimeout, err)
		if p.Exited() {
			reason = fmt.Sprintf("recovery 进程在启动完成前退出（%s）", p.Exit())
		}
		stopErr := rp.Close()
		tail := strings.Join(rp.StderrTail(30), "\n")
		if tail != "" {
			reason += "\nstderr 最后的内容：\n" + tail
		}
		return nil, errors.Join(errors.New(reason), stopErr)
	}
	return rp, nil
}

func (rp *Process) onEvent(ev pirpc.Event) {
	switch ev.Type {
	case "message_end", "tool_execution_start", "tool_execution_end":
		rp.writeRecord(ev)
	default:
		if ev.RPCLogged() {
			rp.writeRecord(ev)
		}
	}
	if end, ok := ev.AssistantMessageEnd(); ok {
		rp.mu.Lock()
		rp.assistant = &end
		rp.mu.Unlock()
	}
	if ev.Type == "agent_start" {
		rp.mu.Lock()
		rp.assistant = nil
		rp.mu.Unlock()
	}
	if aborted, ok := ev.AgentSettled(); ok {
		rp.mu.Lock()
		end := TurnEnd{Aborted: aborted}
		if rp.assistant != nil {
			end.StopReason, end.ErrorMessage = rp.assistant.StopReason, rp.assistant.ErrorMessage
		}
		rp.assistant = nil
		rp.mu.Unlock()
		select {
		case rp.turns <- end:
		default:
			rp.opts.Logger.Warn("recovery turn results are not consumed", "incident", rp.opts.IncidentID)
		}
	}
	if r, ok := ev.UIRequest(); ok {
		rp.opts.Logger.Info("recovery extension UI request", "method", r.Method, "message", r.Message, "autoCancelled", ev.AutoCancelled)
	}
}

func (rp *Process) writeRecord(ev pirpc.Event) {
	if _, err := rp.opts.Log.Write(append(ev.LogRecord(), '\n')); err != nil {
		rp.opts.Logger.Error("write recovery log", "err", err)
	}
}

// PID returns the process id.
func (rp *Process) PID() int { return rp.p.PID() }

// Done is closed when the process exited.
func (rp *Process) Done() <-chan struct{} { return rp.p.Done() }

// Exit returns the exit status; only valid after Done.
func (rp *Process) Exit() proc.ExitStatus { return rp.p.Exit() }

// Turns delivers the end of every run of the recovery session.
func (rp *Process) Turns() <-chan TurnEnd { return rp.turns }

// StderrTail returns the last n stderr lines.
func (rp *Process) StderrTail(n int) []string { return rp.stderr.Lines(n) }

// Prompt sends an RPC prompt and waits for the answer within [agent] rpc_timeout. Only the disposition "started"
// is a success: anything else means no run was started.
func (rp *Process) Prompt(ctx context.Context, text string) error {
	fmt.Fprintf(rp.opts.Log, "=== daemon 发送提示（%d 字符） ===\n%s\n=== 提示结束 ===\n", len([]rune(text)), text)
	callCtx, cancel := context.WithTimeout(ctx, rp.opts.Config.Agent.RPCTimeout)
	defer cancel()
	disposition, err := rp.rpc.Prompt(callCtx, text)
	if err != nil {
		return err
	}
	if disposition != "started" {
		return fmt.Errorf("pi 没有开始处理提示（disposition %s）", disposition)
	}
	return nil
}

// Abort sends abort within [agent] rpc_timeout.
func (rp *Process) Abort(ctx context.Context) error {
	callCtx, cancel := context.WithTimeout(ctx, rp.opts.Config.Agent.RPCTimeout)
	defer cancel()
	return rp.rpc.Abort(callCtx)
}

// Close stops the process with the stop procedure of DESIGN 5.3 and returns after it exited.
func (rp *Process) Close() error {
	if rp.p.Exited() {
		rp.rpc.Close()
		return nil
	}
	err := rp.p.Stop(rp.opts.Config.Daemon.TermGrace)
	rp.rpc.Close()
	fmt.Fprintf(rp.opts.Log, "=== recovery 进程已停止（%s） ===\n", rp.p.Exit())
	return err
}
