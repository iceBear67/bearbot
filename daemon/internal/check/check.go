// Package check runs the check of DESIGN 5.6 (bearbot_check, recovery_check, and the first two steps of an apply):
// the location check, the type check with TypeScript (contracts 9.2), and the trial start of pi (contracts 2.2 and
// 9.1 item 10). Each run gets a directory checks/<id>/ with tsconfig.json, tsc.out, trial-stderr.log and report.json.
package check

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"bearbot/daemon/internal/config"
	"bearbot/daemon/internal/layout"
	"bearbot/daemon/internal/pirpc"
	"bearbot/daemon/internal/proc"
)

// Location is the result of the location check (contracts 6 CheckReport.location).
type Location struct {
	Passed     bool     `json:"passed"`
	Violations []string `json:"violations"`
	Message    string   `json:"message"`
}

// TypeError is one TypeScript diagnostic. File, Line and Column are null for diagnostics without a position.
type TypeError struct {
	File    *string `json:"file"`
	Line    *int    `json:"line"`
	Column  *int    `json:"column"`
	Code    string  `json:"code"`
	Message string  `json:"message"`
}

// Typecheck is the result of the type check (contracts 6 CheckReport.typecheck).
type Typecheck struct {
	Ran    bool        `json:"ran"`
	Passed bool        `json:"passed"`
	Errors []TypeError `json:"errors"`
	Output string      `json:"output"`
}

// Trial is the result of the trial start (contracts 6 CheckReport.trial).
type Trial struct {
	Ran             bool                   `json:"ran"`
	Passed          bool                   `json:"passed"`
	ExitCode        *int                   `json:"exitCode"`
	Signal          *string                `json:"signal"`
	TimedOut        bool                   `json:"timedOut"`
	Stderr          string                 `json:"stderr"`
	ExtensionErrors []pirpc.ExtensionError `json:"extensionErrors"`
	Warnings        []string               `json:"warnings"`
}

// Report is CheckReport of contracts 6.
type Report struct {
	ID         string    `json:"id"`
	Passed     bool      `json:"passed"`
	Location   Location  `json:"location"`
	Typecheck  Typecheck `json:"typecheck"`
	Trial      Trial     `json:"trial"`
	ReportPath string    `json:"reportPath"`
}

const (
	// DefaultTypecheckTimeout bounds tsc when Checker.TypecheckTimeout is zero.
	DefaultTypecheckTimeout = 5 * time.Minute
	// WarningWindow is how long the trial keeps reading stdout after the get_state answer (contracts 9.1 item 10).
	WarningWindow = 3 * time.Second
	// MCPWarningPrefix starts the notify message of the built-in MCP extension about failed servers.
	MCPWarningPrefix = "MCP servers need attention:"
	// StderrTailLines is the number of trial stderr lines in the report (the whole stderr is in trial-stderr.log).
	StderrTailLines = 200
	// OutputLimit is the number of characters of tsc output in the report (the whole output is in tsc.out).
	OutputLimit = 20000
)

// NewID returns a check id "chk-<UTC yyyymmddhhmmss>-<4 hex>".
func NewID(now time.Time) string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "chk-" + now.UTC().Format("20060102150405") + "-" + hex.EncodeToString(b[:])
}

// locationDirs are profile directories that must be empty, locationFiles profile files that must not exist
// (DESIGN 5.6 step 1; the context file names follow pi's lookup of AGENTS.md, pi-extension-api.md 12 item 12).
var (
	locationDirs  = []string{"extensions", "skills", "prompts"}
	locationFiles = []string{"mcp.json", "AGENTS.md", "AGENTS.override.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD", "SYSTEM.md", "APPEND_SYSTEM.md"}
)

// CheckLocation runs the location check. It is also run before every agent start (DESIGN 5.3).
func CheckLocation(p layout.Paths) (Location, error) {
	loc := Location{Violations: []string{}}
	for _, name := range locationDirs {
		path := filepath.Join(p.AgentDir, name)
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return Location{}, fmt.Errorf("无法访问 %s：%w", path, err)
		}
		if !info.IsDir() {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			return Location{}, fmt.Errorf("无法读取 %s：%w", path, err)
		}
		names, err := f.Readdirnames(1)
		f.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return Location{}, fmt.Errorf("无法读取 %s：%w", path, err)
		}
		if len(names) > 0 {
			loc.Violations = append(loc.Violations, path+"/")
		}
	}
	for _, name := range locationFiles {
		path := filepath.Join(p.AgentDir, name)
		if _, err := os.Lstat(path); err == nil {
			loc.Violations = append(loc.Violations, path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return Location{}, fmt.Errorf("无法访问 %s：%w", path, err)
		}
	}
	if len(loc.Violations) == 0 {
		loc.Passed = true
		loc.Message = "位置检查通过：profile 中没有 pi 会加载的扩展、技能、提示模板、MCP 配置、记忆文件或系统提示文件。"
		return loc, nil
	}
	loc.Message = fmt.Sprintf("位置检查失败。以下内容位于 %s 中，pi 会加载它们，但它们不在快照与检查的范围内：%s。"+
		"请把扩展移到工作区的 .pi/extensions/，技能移到 .pi/skills/，提示模板移到 .pi/prompts/，mcp.json 移到工作区的 .pi/mcp.json，"+
		"SYSTEM.md、APPEND_SYSTEM.md 移到工作区的 .pi/，记忆写入工作区的 AGENTS.md，然后删除 profile 中的这些内容。",
		p.AgentDir, strings.Join(loc.Violations, "、"))
	return loc, nil
}

// findPackage applies Node's lookup rule: <dir>/node_modules/<name> for start and each parent directory.
func findPackage(start, name string) (string, error) {
	for dir := start; ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, "node_modules", name)
		info, err := os.Stat(filepath.Join(candidate, "package.json"))
		switch {
		case err == nil && info.Mode().IsRegular():
			return candidate, nil
		case err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR):
			return "", err
		}
		if dir == "/" {
			return "", fmt.Errorf("从 %s 向上找不到 node_modules/%s", start, name)
		}
	}
}

// Aliases returns the 22 module names pi gives to extensions mapped to their declaration files (contracts 9.2,
// toolchain.md 1.3). The nested packages are found with Node's lookup rule from realpath(PiPackage).
func Aliases(p layout.Paths) (map[string]string, error) {
	real, err := filepath.EvalSymlinks(p.PiPackage)
	if err != nil {
		return nil, fmt.Errorf("找不到 pi 包 %s：%w", p.PiPackage, err)
	}
	pkg := map[string]string{}
	for _, name := range []string{"typebox", "@earendil-works/pi-ai", "@earendil-works/pi-tui", "@earendil-works/pi-agent-core"} {
		dir, err := findPackage(real, name)
		if err != nil {
			return nil, err
		}
		pkg[name] = dir
	}
	codingAgent := filepath.Join(p.PiPackage, "dist/index.d.ts")
	agentCore := filepath.Join(pkg["@earendil-works/pi-agent-core"], "dist/index.d.ts")
	tui := filepath.Join(pkg["@earendil-works/pi-tui"], "dist/index.d.ts")
	aiCompat := filepath.Join(pkg["@earendil-works/pi-ai"], "dist/compat.d.ts")
	aiModels := filepath.Join(pkg["@earendil-works/pi-ai"], "dist/models.d.ts")
	aiOauth := filepath.Join(pkg["@earendil-works/pi-ai"], "dist/oauth.d.ts")
	aiProviders := filepath.Join(pkg["@earendil-works/pi-ai"], "dist/providers/all.d.ts")
	typebox := filepath.Join(pkg["typebox"], "build/index.d.mts")
	typeboxCompile := filepath.Join(pkg["typebox"], "build/compile/index.d.mts")
	typeboxValue := filepath.Join(pkg["typebox"], "build/value/index.d.mts")
	aliases := map[string]string{}
	for _, scope := range []string{"@earendil-works", "@mariozechner"} {
		aliases[scope+"/pi-coding-agent"] = codingAgent
		aliases[scope+"/pi-agent-core"] = agentCore
		aliases[scope+"/pi-tui"] = tui
		aliases[scope+"/pi-ai"] = aiCompat
		aliases[scope+"/pi-ai/compat"] = aiCompat
		aliases[scope+"/pi-ai/models"] = aiModels
		aliases[scope+"/pi-ai/oauth"] = aiOauth
		aliases[scope+"/pi-ai/providers/all"] = aiProviders
	}
	for _, name := range []string{"typebox", "@sinclair/typebox"} {
		aliases[name] = typebox
		aliases[name+"/compile"] = typeboxCompile
		aliases[name+"/value"] = typeboxValue
	}
	for name, file := range aliases {
		if _, err := os.Stat(file); err != nil {
			return nil, fmt.Errorf("模块 %s 的类型声明 %s 不存在：%w", name, file, err)
		}
	}
	return aliases, nil
}

// TSConfig returns the tsconfig.json of the type check (contracts 9.2).
func TSConfig(p layout.Paths) ([]byte, error) {
	aliases, err := Aliases(p)
	if err != nil {
		return nil, err
	}
	paths := map[string][]string{}
	for name, file := range aliases {
		paths[name] = []string{file}
	}
	extensions := filepath.Join(p.WorkspaceDir, ".pi", "extensions")
	cfg := map[string]any{
		"compilerOptions": map[string]any{
			"noEmit":                     true,
			"strict":                     true,
			"skipLibCheck":               true,
			"allowImportingTsExtensions": true,
			"module":                     "preserve",
			"moduleResolution":           "bundler",
			"target":                     "es2024",
			"lib":                        []string{"es2024"},
			"types":                      []string{"node"},
			"typeRoots":                  []string{p.ToolchainTypes},
			"paths":                      paths,
		},
		"files":   []string{p.SDKFile},
		"include": []string{extensions + "/**/*"},
		"exclude": []string{extensions + "/**/node_modules"},
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

var diagnosticLine = regexp.MustCompile(`^(?:(.+)\((\d+),(\d+)\): )?error (TS\d+): (.*)$`)

// ParseDiagnostics parses tsc output written with --pretty false. Lines starting with a space continue the message
// of the previous diagnostic. Relative file paths are made absolute against cwd.
func ParseDiagnostics(output, cwd string) []TypeError {
	errs := []TypeError{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if m := diagnosticLine.FindStringSubmatch(line); m != nil {
			te := TypeError{Code: m[4], Message: m[5]}
			if m[1] != "" {
				file := m[1]
				if !filepath.IsAbs(file) {
					file = filepath.Join(cwd, file)
				}
				l, _ := strconv.Atoi(m[2])
				c, _ := strconv.Atoi(m[3])
				te.File, te.Line, te.Column = &file, &l, &c
			}
			errs = append(errs, te)
			continue
		}
		if strings.HasPrefix(line, " ") && len(errs) > 0 {
			errs[len(errs)-1].Message += "\n" + strings.TrimLeft(line, " ")
		}
	}
	return errs
}

func limitText(s string, limit int, note string) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit]) + "\n…（" + note + "）\n"
}

// Checker runs checks. Its fields must be set; TypecheckTimeout may be zero for the default.
type Checker struct {
	Paths  layout.Paths
	Users  *layout.Users
	Config *config.Config
	// Node is node found in the daemon's PATH. tsc and pi are scripts with `#!/usr/bin/env node` and run as
	// `<Node> <script>`: the child PATH begins with directories bearbot can write.
	Node string
	// TypecheckTimeout bounds tsc.
	TypecheckTimeout time.Duration
}

func (c *Checker) newDir() (string, string, error) {
	for range 10 {
		id := NewID(time.Now())
		dir := c.Paths.CheckDir(id)
		err := os.Mkdir(dir, 0o755)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", "", fmt.Errorf("创建检查目录 %s 失败：%w", dir, err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			return "", "", err
		}
		return id, dir, nil
	}
	return "", "", errors.New("无法分配检查 id")
}

// Run runs the full check: location check, then (when it passed) the type check, then (when it passed) the trial
// start, and writes report.json. The error is only about the check machinery; a failed check is a Report with
// Passed false.
func (c *Checker) Run(ctx context.Context) (*Report, error) {
	id, dir, err := c.newDir()
	if err != nil {
		return nil, err
	}
	rep := &Report{
		ID:         id,
		ReportPath: filepath.Join(dir, "report.json"),
		Typecheck:  Typecheck{Errors: []TypeError{}},
		Trial:      Trial{ExtensionErrors: []pirpc.ExtensionError{}, Warnings: []string{}},
	}
	if rep.Location, err = CheckLocation(c.Paths); err != nil {
		return nil, err
	}
	if rep.Location.Passed {
		if rep.Typecheck, err = c.Typecheck(ctx, dir); err != nil {
			return nil, err
		}
		if rep.Typecheck.Passed {
			if rep.Trial, err = c.Trial(ctx, dir); err != nil {
				return nil, err
			}
		}
	}
	rep.Passed = rep.Location.Passed && rep.Typecheck.Passed && rep.Trial.Passed
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(rep.ReportPath, append(data, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("写入 %s 失败：%w", rep.ReportPath, err)
	}
	return rep, nil
}

// Typecheck writes dir/tsconfig.json and runs `<Node> <Tsc> -p <dir>/tsconfig.json --pretty false` as bearbot with
// cwd WorkspaceDir; the output goes to dir/tsc.out.
func (c *Checker) Typecheck(ctx context.Context, dir string) (Typecheck, error) {
	tc := Typecheck{Ran: true, Errors: []TypeError{}}
	tsconfig, err := TSConfig(c.Paths)
	if err != nil {
		return Typecheck{}, err
	}
	tsconfigPath := filepath.Join(dir, "tsconfig.json")
	if err := os.WriteFile(tsconfigPath, tsconfig, 0o644); err != nil {
		return Typecheck{}, err
	}
	timeout := c.TypecheckTimeout
	if timeout == 0 {
		timeout = DefaultTypecheckTimeout
	}
	res, err := proc.Run(ctx, proc.RunSpec{
		Path:       c.Node,
		Args:       []string{c.Paths.Tsc, "-p", tsconfigPath, "--pretty", "false"},
		Env:        c.Paths.ToolEnv(),
		Dir:        c.Paths.WorkspaceDir,
		Credential: c.Users.Credential(c.Users.Bearbot),
		Timeout:    timeout,
	})
	if err != nil {
		return Typecheck{}, fmt.Errorf("运行类型检查失败：%w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tsc.out"), res.Output, 0o644); err != nil {
		return Typecheck{}, err
	}
	tc.Output = limitText(string(res.Output), OutputLimit, "输出过长，完整内容见 "+filepath.Join(dir, "tsc.out"))
	switch {
	case res.TimedOut:
		tc.Output += fmt.Sprintf("\n类型检查超过 %s 没有结束，已终止。\n", timeout)
	case res.Exit.Success():
		tc.Passed = true
	default:
		tc.Errors = ParseDiagnostics(string(res.Stdout), c.Paths.WorkspaceDir)
		if strings.TrimSpace(string(res.Output)) == "" {
			tc.Output = fmt.Sprintf("tsc 以%s结束，没有输出。\n", res.Exit)
		}
	}
	return tc, nil
}

// trialEvents collects the trial's extension errors and MCP warnings until the warning window ends.
type trialEvents struct {
	mu       sync.Mutex
	closed   bool
	errors   []pirpc.ExtensionError
	warnings []string
}

func (t *trialEvents) on(ev pirpc.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if x, ok := ev.ExtensionError(); ok {
		t.errors = append(t.errors, x)
		return
	}
	if r, ok := ev.UIRequest(); ok && r.Method == "notify" && r.NotifyType == "warning" && strings.HasPrefix(r.Message, MCPWarningPrefix) {
		t.warnings = append(t.warnings, r.Message)
	}
}

func (t *trialEvents) close() ([]pirpc.ExtensionError, []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return append([]pirpc.ExtensionError{}, t.errors...), append([]string{}, t.warnings...)
}

// Trial starts pi like the agent process but with --no-session and BEARBOT_TRIAL=1 (contracts 2.2), sends get_state
// within [agent] start_timeout, keeps reading stdout for WarningWindow, and stops the process with the DESIGN 5.3
// procedure. The trial passes when pi answered, did not exit by itself, and wrote no extension_error until the end
// of the window. stderr goes to dir/trial-stderr.log.
func (c *Checker) Trial(ctx context.Context, dir string) (Trial, error) {
	tr := Trial{Ran: true, ExtensionErrors: []pirpc.ExtensionError{}, Warnings: []string{}}
	stderrFile, err := os.OpenFile(filepath.Join(dir, "trial-stderr.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return Trial{}, err
	}
	defer stderrFile.Close()
	ring := proc.NewRing(StderrTailLines)
	p, err := proc.Start(proc.Spec{
		Path:       c.Node,
		Args:       append([]string{c.Paths.PiBin}, c.Paths.TrialArgs()...),
		Env:        c.Paths.TrialEnv(c.Config),
		Dir:        c.Paths.WorkspaceDir,
		Credential: c.Users.Credential(c.Users.Bearbot),
		StdinPipe:  true,
		Stderr:     io.MultiWriter(stderrFile, ring),
	})
	if err != nil {
		return Trial{}, fmt.Errorf("启动试运行进程失败：%w", err)
	}
	events := &trialEvents{}
	client := pirpc.New(p.Stdin, p.Stdout, pirpc.Options{OnEvent: events.on, IDPrefix: "trial-"})

	answered := make(chan error, 1)
	callCtx, cancel := context.WithTimeout(ctx, c.Config.Agent.StartTimeout)
	go func() {
		_, err := client.GetState(callCtx, "boot")
		answered <- err
	}()
	exitedEarly := false
	select {
	case err := <-answered:
		switch {
		case err == nil:
		case errors.Is(err, context.DeadlineExceeded):
			tr.TimedOut = true
		case errors.Is(err, pirpc.ErrStreamEnded), errors.Is(err, pirpc.ErrClosed):
			exitedEarly = true
		default:
			cancel()
			return Trial{}, errors.Join(err, p.KillTree())
		}
		if err == nil {
			window := time.NewTimer(WarningWindow)
			select {
			case <-window.C:
			case <-p.Done():
				exitedEarly = true
			case <-ctx.Done():
				window.Stop()
				cancel()
				return Trial{}, errors.Join(ctx.Err(), p.KillTree())
			}
			window.Stop()
		}
	case <-p.Done():
		// pi exited before answering; a descendant may still hold stdout open.
		exitedEarly = true
		client.Close()
		<-answered
	}
	cancel()
	tr.ExtensionErrors, tr.Warnings = events.close()

	if p.Exited() {
		exitedEarly = true
	} else if err := p.Stop(c.Config.Daemon.TermGrace); err != nil {
		return Trial{}, err
	}
	// Let the stderr copy finish (it ends with the pipe; a descendant may keep it open).
	select {
	case <-p.OutputDone():
	case <-time.After(time.Second):
	}
	exit := p.Exit()
	tr.ExitCode, tr.Signal = exit.Code, exit.Signal
	tr.Stderr = strings.Join(ring.Lines(0), "\n")
	tr.Passed = !tr.TimedOut && !exitedEarly && len(tr.ExtensionErrors) == 0
	return tr, nil
}
