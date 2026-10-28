package incident

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"bearbot/daemon/internal/check"
	"bearbot/daemon/internal/layout"
	"bearbot/daemon/internal/proc"
	"bearbot/daemon/internal/state"
	"bearbot/daemon/internal/wire"
)

// Record statuses (incident.json "status").
const (
	StatusRepairing   = "repairing"
	StatusProbation   = "probation"
	StatusMaintenance = "maintenance"
	StatusClosed      = "closed"
)

// Resolution kinds: how the agent got back to running.
const (
	ResolvedByRecovery = "recovery"
	ResolvedByResume   = "resume"
	ResolvedByRollback = "rollback"
	// ResolvedBySuperseded means a new incident opened before probation ended.
	ResolvedBySuperseded = "superseded"
)

// Repair is one run of the repair flow (the automatic one or `repair [说明]`).
type Repair struct {
	StartedAt time.Time `json:"startedAt"`
	Note      string    `json:"note"`
	// MaintenanceReason is the reason of the maintenance state the run started from, when it continues an open
	// incident (for example a rollback that failed after the incident); empty otherwise.
	MaintenanceReason string `json:"maintenanceReason,omitempty"`
	// Result describes how the run ended; empty while it runs.
	Result string `json:"result"`
}

// CheckSummary is the part of a check report kept with an attempt.
type CheckSummary struct {
	ID         string `json:"id"`
	Passed     bool   `json:"passed"`
	ReportPath string `json:"reportPath"`
	// Problems lists why the check failed.
	Problems string `json:"problems"`
}

// StartSummary is the outcome of the agent start after a passed check.
type StartSummary struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// Attempt is one `fixed` report of recovery and its verification.
type Attempt struct {
	N          int                 `json:"n"`
	Snapshot   string              `json:"snapshot"`
	Commit     string              `json:"commit"`
	ReportedAt time.Time           `json:"reportedAt"`
	Report     wire.RecoveryReport `json:"report"`
	Check      *CheckSummary       `json:"check"`
	Start      *StartSummary       `json:"start"`
}

// Resolution tells how the incident was resolved.
type Resolution struct {
	Kind string `json:"kind"`
	// Attempt is the attempt number for ResolvedByRecovery.
	Attempt int    `json:"attempt"`
	Detail  string `json:"detail"`
}

// Record is the content of incidents/<id>/incident.json.
type Record struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	Trigger   string    `json:"trigger"`
	Detail    string    `json:"detail"`
	// Exit is the exit status of the process that failed; nil when it did not exit (for example a location check
	// failure) or for an incident opened by the administrator.
	Exit       *proc.ExitStatus `json:"exit"`
	StderrTail []string         `json:"stderrTail"`
	RPCEvents  []string         `json:"rpcEvents"`
	// LastApply is the most recent apply (snapshot, summary, requester, thread).
	LastApply *state.Applied `json:"lastApply"`
	// Origin is the origin of the apply that caused the incident: LastApply's when the files that failed were that
	// apply's snapshot, otherwise both fields are null.
	Origin   state.Origin `json:"origin"`
	LastGood *string      `json:"lastGood"`
	DiffStat string       `json:"diffStat"`
	// Before is the snapshot incident/<id>/before.
	Before            string      `json:"before"`
	Repairs           []Repair    `json:"repairs"`
	Attempts          []Attempt   `json:"attempts"`
	Status            string      `json:"status"`
	MaintenanceReason string      `json:"maintenanceReason"`
	Resolution        *Resolution `json:"resolution"`
	ClosedAt          *time.Time  `json:"closedAt"`
}

var idPattern = regexp.MustCompile(`^inc-[0-9]{14}$`)

// ValidID reports whether id has the form inc-<14 digits>.
func ValidID(id string) bool { return idPattern.MatchString(id) }

func recordPath(p layout.Paths, id string) string {
	return filepath.Join(p.IncidentDir(id), "incident.json")
}

// Load reads incidents/<id>/incident.json.
func Load(p layout.Paths, id string) (*Record, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("无效的故障 id %q（格式为 inc-<14 位数字>）", id)
	}
	data, err := os.ReadFile(recordPath(p, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("故障 %s 不存在", id)
	}
	if err != nil {
		return nil, fmt.Errorf("读取故障记录 %s 失败：%w", id, err)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("无法解析 %s：%w", recordPath(p, id), err)
	}
	return &rec, nil
}

// Save writes incident.json (temporary file + rename, mode 0644) and report.md.
func (r *Record) Save(p layout.Paths) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(recordPath(p, r.ID), append(data, '\n')); err != nil {
		return err
	}
	return writeFile(filepath.Join(p.IncidentDir(r.ID), "report.md"), []byte(r.ReportMarkdown()))
}

func writeFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("写入 %s 失败：%w", path, err)
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("写入 %s 失败：%w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Chmod(0o644); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("写入 %s 失败：%w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("写入 %s 失败：%w", path, err)
	}
	return nil
}

// SaveAttempt writes incidents/<id>/attempts/<n>.md.
func (r *Record) SaveAttempt(p layout.Paths, a Attempt) error {
	dir := filepath.Join(p.IncidentDir(r.ID), "attempts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, fmt.Sprintf("%d.md", a.N)), []byte(a.Markdown(r.ID)))
}

// setAttempt replaces or appends the attempt with the same number.
func (r *Record) setAttempt(a Attempt) {
	for i := range r.Attempts {
		if r.Attempts[i].N == a.N {
			r.Attempts[i] = a
			return
		}
	}
	r.Attempts = append(r.Attempts, a)
}

func (r *Record) attempt(n int) *Attempt {
	for i := range r.Attempts {
		if r.Attempts[i].N == n {
			return &r.Attempts[i]
		}
	}
	return nil
}

// StatusText is the Chinese name of a record status.
func StatusText(s string) string {
	switch s {
	case StatusRepairing:
		return "修复中"
	case StatusProbation:
		return "观察期"
	case StatusMaintenance:
		return "维护状态"
	case StatusClosed:
		return "已关闭"
	default:
		return s
	}
}

func exitText(e *proc.ExitStatus) string {
	if e == nil {
		return "无（进程没有退出或没有进程）"
	}
	return e.String()
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "无"
	}
	return s
}

func originText(o state.Origin) string {
	thread, requester := "无", "无"
	if o.Thread != nil {
		thread = *o.Thread
	}
	if o.Requester != nil {
		requester = *o.Requester
	}
	return fmt.Sprintf("发起者 %s，会话串 %s", requester, thread)
}

func lastApplyText(a *state.Applied) string {
	if a == nil {
		return "无"
	}
	return fmt.Sprintf("快照 %s，摘要 %s，%s", a.Snapshot, a.Summary, originText(a.Origin))
}

func resolutionText(r *Resolution) string {
	if r == nil {
		return "尚未解决"
	}
	switch r.Kind {
	case ResolvedByRecovery:
		return fmt.Sprintf("recovery 第 %d 次修复", r.Attempt)
	case ResolvedByResume:
		return "管理员执行 resume，不做改动恢复运行"
	case ResolvedByRollback:
		return "管理员回滚：" + r.Detail
	case ResolvedBySuperseded:
		return "修复后的版本没有度过观察期：" + r.Detail
	default:
		return r.Kind + "：" + r.Detail
	}
}

// Markdown renders the attempt for attempts/<n>.md.
func (a Attempt) Markdown(id string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 故障 %s 第 %d 次修复\n\n", id, a.N)
	fmt.Fprintf(&b, "- 报告时间：%s\n", a.ReportedAt.Local().Format(time.DateTime))
	fmt.Fprintf(&b, "- 快照：%s（%s）\n\n", a.Snapshot, a.Commit)
	b.WriteString("## recovery 的报告\n\n")
	fmt.Fprintf(&b, "- 状态：%s\n", a.Report.Status)
	fmt.Fprintf(&b, "- 改动的文件：%s\n\n", orNone(strings.Join(a.Report.ChangedFiles, "、")))
	fmt.Fprintf(&b, "### 摘要\n\n%s\n\n### 经验\n\n%s\n\n", strings.TrimSpace(a.Report.Summary), orNone(strings.TrimSpace(a.Report.Lesson)))
	b.WriteString("## 验证\n\n")
	b.WriteString(a.verificationText())
	return b.String()
}

func (a Attempt) verificationText() string {
	var b strings.Builder
	switch {
	case a.Check == nil:
		b.WriteString("- 检查：还没有执行\n")
	case a.Check.Passed:
		fmt.Fprintf(&b, "- 检查：通过（%s，报告 %s）\n", a.Check.ID, a.Check.ReportPath)
	default:
		fmt.Fprintf(&b, "- 检查：未通过（%s，报告 %s）\n\n%s\n", a.Check.ID, a.Check.ReportPath, a.Check.Problems)
	}
	switch {
	case a.Start == nil:
	case a.Start.OK:
		b.WriteString("- 启动：agent 进程启动成功，进入观察期\n")
	default:
		fmt.Fprintf(&b, "- 启动：agent 进程启动失败\n\n%s\n", a.Start.Error)
	}
	return b.String()
}

// ReportMarkdown renders report.md.
func (r *Record) ReportMarkdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 故障 %s\n\n", r.ID)
	fmt.Fprintf(&b, "- 状态：%s\n", StatusText(r.Status))
	fmt.Fprintf(&b, "- 建立时间：%s\n", r.CreatedAt.Local().Format(time.DateTime))
	if r.ClosedAt != nil {
		fmt.Fprintf(&b, "- 关闭时间：%s\n", r.ClosedAt.Local().Format(time.DateTime))
	}
	fmt.Fprintf(&b, "- 触发：%s\n", r.Trigger)
	fmt.Fprintf(&b, "- 退出码 / 信号：%s\n", exitText(r.Exit))
	fmt.Fprintf(&b, "- 最近一次应用：%s\n", lastApplyText(r.LastApply))
	fmt.Fprintf(&b, "- 引起故障的应用：%s\n", originText(r.Origin))
	fmt.Fprintf(&b, "- 故障时的快照：%s\n", r.Before)
	fmt.Fprintf(&b, "- 结果：%s\n", resolutionText(r.Resolution))
	if r.Status == StatusMaintenance && r.MaintenanceReason != "" {
		fmt.Fprintf(&b, "- 进入维护状态的原因：%s\n", r.MaintenanceReason)
	}
	fmt.Fprintf(&b, "\n## 详情\n\n%s\n", orNone(r.Detail))
	if r.DiffStat != "" {
		lastGood := "init"
		if r.LastGood != nil {
			lastGood = *r.LastGood
		}
		fmt.Fprintf(&b, "\n## 相对 last-good（%s）的改动\n\n```\n%s\n```\n", lastGood, r.DiffStat)
	}
	if len(r.Repairs) > 0 {
		b.WriteString("\n## 修复过程\n\n")
		for i, rep := range r.Repairs {
			note := ""
			if rep.Note != "" {
				note = "，管理员说明：" + rep.Note
			}
			fmt.Fprintf(&b, "%d. %s 开始%s；%s\n", i+1, rep.StartedAt.Local().Format(time.DateTime), note, orNone(rep.Result))
		}
	}
	for _, a := range r.Attempts {
		fmt.Fprintf(&b, "\n## 第 %d 次修复（%s）\n\n", a.N, a.Snapshot)
		fmt.Fprintf(&b, "摘要：%s\n\n改动的文件：%s\n\n经验：%s\n\n", strings.TrimSpace(a.Report.Summary), orNone(strings.Join(a.Report.ChangedFiles, "、")), orNone(a.Report.Lesson))
		b.WriteString(a.verificationText())
	}
	if len(r.StderrTail) > 0 {
		fmt.Fprintf(&b, "\n## stderr 最后 %d 行\n\n```\n%s\n```\n", len(r.StderrTail), strings.Join(r.StderrTail, "\n"))
	}
	return b.String()
}

// checkProblems describes why a check failed, for attempts and prompts.
func checkProblems(rep *check.Report) string {
	var b strings.Builder
	if !rep.Location.Passed {
		fmt.Fprintf(&b, "位置检查未通过：%s\n", rep.Location.Message)
	}
	if rep.Typecheck.Ran && !rep.Typecheck.Passed {
		b.WriteString("类型检查未通过：\n")
		limit := min(len(rep.Typecheck.Errors), 30)
		for _, e := range rep.Typecheck.Errors[:limit] {
			pos := ""
			if e.File != nil {
				pos = fmt.Sprintf("%s(%d,%d): ", *e.File, *e.Line, *e.Column)
			}
			fmt.Fprintf(&b, "  %s%s %s\n", pos, e.Code, e.Message)
		}
		if len(rep.Typecheck.Errors) > limit {
			fmt.Fprintf(&b, "  ……共 %d 个错误\n", len(rep.Typecheck.Errors))
		}
		if len(rep.Typecheck.Errors) == 0 {
			fmt.Fprintf(&b, "%s\n", rep.Typecheck.Output)
		}
	}
	if rep.Trial.Ran && !rep.Trial.Passed {
		exit := proc.ExitStatus{Code: rep.Trial.ExitCode, Signal: rep.Trial.Signal}
		fmt.Fprintf(&b, "试运行未通过（%s", exit)
		if rep.Trial.TimedOut {
			b.WriteString("，没有在 start_timeout 内响应 get_state")
		}
		b.WriteString("）\n")
		for _, x := range rep.Trial.ExtensionErrors {
			fmt.Fprintf(&b, "  extension_error：%s（%s）：%s\n", x.ExtensionPath, x.Event, x.Error)
		}
		if tail := lastLines(rep.Trial.Stderr, 40); tail != "" {
			fmt.Fprintf(&b, "  stderr 最后的内容：\n%s\n", tail)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// ListText is the text of `incident list` (newest first).
func ListText(p layout.Paths) (string, error) {
	entries, err := os.ReadDir(p.IncidentsDir)
	if err != nil {
		return "", fmt.Errorf("读取 %s 失败：%w", p.IncidentsDir, err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && ValidID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	if len(ids) == 0 {
		return "还没有故障记录\n", nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	var b strings.Builder
	fmt.Fprintf(&b, "故障记录（共 %d 条，最新的在前）：\n", len(ids))
	for _, id := range ids {
		rec, err := Load(p, id)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "  %s  %s  %s  %s  修复 %d 次  %s\n", rec.ID, rec.CreatedAt.Local().Format(time.DateTime), rec.Trigger, StatusText(rec.Status), len(rec.Attempts), resolutionText(rec.Resolution))
	}
	return b.String(), nil
}

// ShowText is the text of `incident show <id>`: report.md plus the paths of the files.
func ShowText(p layout.Paths, id string) (string, error) {
	rec, err := Load(p, id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(rec.ReportMarkdown())
	if len(rec.RPCEvents) > 0 {
		fmt.Fprintf(&b, "\n## rpc 事件最后 %d 条\n\n```\n%s\n```\n", len(rec.RPCEvents), strings.Join(rec.RPCEvents, "\n"))
	}
	fmt.Fprintf(&b, "\n文件：%s、%s、%s\n", recordPath(p, id), filepath.Join(p.IncidentDir(id), "report.md"), filepath.Join(p.IncidentDir(id), "attempts"))
	fmt.Fprintf(&b, "recovery 日志：%s\n", p.RecoveryLog(id))
	return b.String(), nil
}
