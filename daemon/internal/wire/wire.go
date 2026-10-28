// Package wire holds the JSON types of the daemon's protocols that several packages share: the agent states of
// gateway-ctl.sock and agent.sock (contracts 4 and 6), StatusInfo, ReadyResult, the bridge events, the classifier
// decision record and the recovery report.
package wire

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"bearbot/daemon/internal/pirpc"
	"bearbot/daemon/internal/state"
)

// AgentState is the agent state as sent to the gateway and to bearbot-im (contracts 4 AgentWireState).
type AgentState string

// Agent states of DESIGN 5.4.
const (
	Stopped     AgentState = "stopped"
	Starting    AgentState = "starting"
	Running     AgentState = "running"
	Draining    AgentState = "draining"
	Restarting  AgentState = "restarting"
	Incident    AgentState = "incident"
	Repairing   AgentState = "repairing"
	Maintenance AgentState = "maintenance"
	RollingBack AgentState = "rolling_back"
)

// AgentProcessExists reports whether s is one of the states in which the agent process exists (contracts 6:
// starting, running, draining, restarting).
func (s AgentState) AgentProcessExists() bool {
	switch s {
	case Starting, Running, Draining, Restarting:
		return true
	default:
		return false
	}
}

// Time formats t as ISO 8601 with milliseconds and the local offset.
func Time(t time.Time) string { return t.Local().Format("2006-01-02T15:04:05.000Z07:00") }

// TimePtr formats t, or returns nil for nil.
func TimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := Time(*t)
	return &s
}

// PendingApplyInfo is StatusInfo.pendingApply.
type PendingApplyInfo struct {
	Snapshot string `json:"snapshot"`
	Summary  string `json:"summary"`
}

// IncidentInfo is StatusInfo.incident.
type IncidentInfo struct {
	ID       string `json:"id"`
	Trigger  string `json:"trigger"`
	Attempts int    `json:"attempts"`
}

// MaintenanceInfo is StatusInfo.maintenance.
type MaintenanceInfo struct {
	Reason     string  `json:"reason"`
	IncidentID *string `json:"incidentId"`
	Since      string  `json:"since"`
}

// RollbackInfo is StatusInfo.rollback.
type RollbackInfo struct {
	ID       int    `json:"id"`
	Target   string `json:"target"`
	Notified bool   `json:"notified"`
}

// StatusInfo is the answer of gateway-ctl `status` and agent.sock GET /v1/status (contracts 4).
type StatusInfo struct {
	State           AgentState        `json:"state"`
	Reason          *string           `json:"reason"`
	PID             *int              `json:"pid"`
	StartedAt       *string           `json:"startedAt"`
	Restarts        int               `json:"restarts"`
	CrashesInWindow int               `json:"crashesInWindow"`
	ProbationUntil  *string           `json:"probationUntil"`
	SessionID       *string           `json:"sessionId"`
	RunningSnapshot *string           `json:"runningSnapshot"`
	LastGood        *string           `json:"lastGood"`
	PendingApply    *PendingApplyInfo `json:"pendingApply"`
	Incident        *IncidentInfo     `json:"incident"`
	Maintenance     *MaintenanceInfo  `json:"maintenance"`
	Rollback        *RollbackInfo     `json:"rollback"`
	Console         bool              `json:"console"`
}

// ReadyResult is the answer of POST /v1/bridge/ready (contracts 6). Absent fields have nothing to deliver.
type ReadyResult struct {
	LastExit          *state.LastExit          `json:"lastExit,omitempty"`
	Applied           *state.Applied           `json:"applied,omitempty"`
	Incident          *state.IncidentNotice    `json:"incident,omitempty"`
	Rollback          *state.Rollback          `json:"rollback,omitempty"`
	ClassifierRestore *state.ClassifierRestore `json:"classifierRestore,omitempty"`
}

// BridgeEvent is one record of GET /v1/bridge/events (contracts 6).
type BridgeEvent struct {
	Type string
	// ExtensionError is set for Type "extension_error".
	ExtensionError pirpc.ExtensionError
	// State and Reason are set for Type "state".
	State  AgentState
	Reason *string
	// Incident is set for Type "incident".
	Incident *state.IncidentNotice
}

// Bridge event types.
const (
	EventExtensionError = "extension_error"
	EventState          = "state"
	EventIncident       = "incident"
)

// MarshalJSON writes the record of the event's type.
func (e BridgeEvent) MarshalJSON() ([]byte, error) {
	switch e.Type {
	case EventExtensionError:
		return json.Marshal(struct {
			Type          string `json:"type"`
			ExtensionPath string `json:"extensionPath"`
			Event         string `json:"event"`
			Error         string `json:"error"`
		}{e.Type, e.ExtensionError.ExtensionPath, e.ExtensionError.Event, e.ExtensionError.Error})
	case EventState:
		return json.Marshal(struct {
			Type   string     `json:"type"`
			State  AgentState `json:"state"`
			Reason *string    `json:"reason"`
		}{e.Type, e.State, e.Reason})
	case EventIncident:
		return json.Marshal(struct {
			Type     string                `json:"type"`
			Incident *state.IncidentNotice `json:"incident"`
		}{e.Type, e.Incident})
	default:
		return nil, fmt.Errorf("unknown bridge event type %q", e.Type)
	}
}

// ClassifierDecision is one classifier decision of bearbot-im (contracts 6).
type ClassifierDecision struct {
	Time        string `json:"time"`
	TurnID      string `json:"turnId"`
	Tool        string `json:"tool"`
	ArgsSummary string `json:"argsSummary"`
	Decision    string `json:"decision"`
	Reason      string `json:"reason"`
	Basis       string `json:"basis"`
	DurationMs  int64  `json:"durationMs"`
}

// Validate checks the enumerated fields.
func (d ClassifierDecision) Validate() error {
	if d.Time == "" || d.TurnID == "" || d.Tool == "" {
		return fmt.Errorf("time、turnId、tool 不能为空")
	}
	switch d.Decision {
	case "allow", "deny":
	default:
		return fmt.Errorf("decision 必须是 allow 或 deny，收到 %q", d.Decision)
	}
	switch d.Basis {
	case "admin-rule", "builtin", "model", "cache", "classifier-error", "rules-file":
	default:
		return fmt.Errorf("basis %q 无效", d.Basis)
	}
	if d.DurationMs < 0 {
		return fmt.Errorf("durationMs 不能为负数")
	}
	return nil
}

// DecisionRecord is the "decision" line of logs/agent/classifier.jsonl (contracts 8.2).
type DecisionRecord struct {
	Type string `json:"type"`
	ClassifierDecision
}

// Recovery report statuses.
const (
	ReportFixed     = "fixed"
	ReportCannotFix = "cannot_fix"
)

// RecoveryReport is the body of POST /v1/recovery/report (contracts 6).
type RecoveryReport struct {
	Status       string   `json:"status"`
	Summary      string   `json:"summary"`
	ChangedFiles []string `json:"changed_files"`
	Lesson       string   `json:"lesson"`
}

// Validate checks the report.
func (r RecoveryReport) Validate() error {
	if r.Status != ReportFixed && r.Status != ReportCannotFix {
		return fmt.Errorf("status 必须是 fixed 或 cannot_fix，收到 %q", r.Status)
	}
	if r.Summary == "" {
		return fmt.Errorf("summary 不能为空")
	}
	// The first line of the summary goes into the commit message of the attempt snapshot (a command line argument).
	if strings.ContainsRune(r.Summary, 0) {
		return fmt.Errorf("summary 不能包含 NUL 字符")
	}
	if r.ChangedFiles == nil {
		return fmt.Errorf("changed_files 必须是数组")
	}
	return nil
}

// PlatformStatus is one entry of gateway-ctl `platforms.status` (contracts 4).
type PlatformStatus struct {
	Name             string  `json:"name"`
	State            string  `json:"state"`
	Since            *string `json:"since"`
	LastDisconnectAt *string `json:"lastDisconnectAt"`
	LastReconnectAt  *string `json:"lastReconnectAt"`
	LastError        *string `json:"lastError"`
	LastErrorAt      *string `json:"lastErrorAt"`
}

// AdminInfo is one entry of gateway-ctl `admin.list` (contracts 4).
type AdminInfo struct {
	Identity     string  `json:"identity"`
	DisplayName  *string `json:"displayName"`
	NotifyThread *string `json:"notifyThread"`
	AddedBy      *string `json:"addedBy"`
	AddedAt      *string `json:"addedAt"`
	Builtin      bool    `json:"builtin"`
}

// TextResult is the {text} answer of most gateway-ctl requests.
type TextResult struct {
	Text string `json:"text"`
}
