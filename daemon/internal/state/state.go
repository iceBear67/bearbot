// Package state owns <DaemonDir>/state.json (contracts 8.1): the struct, loading with a version check, and atomic
// saving (temporary file + rename, mode 0600). Nil pointers and empty strings mean "none". The agent supervisor
// serializes all writes; this package does no locking.
package state

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Version is the only state.json version this daemon reads and writes.
const Version = 1

// Origin is where an apply came from: the thread and the requester identity, each possibly null.
type Origin struct {
	Thread    *string `json:"thread"`
	Requester *string `json:"requester"`
}

// Maintenance is the persisted maintenance mark (DESIGN 5.8).
type Maintenance struct {
	Reason string `json:"reason"`
	// IncidentID is null when the maintenance state came from a failed rollback.
	IncidentID *string   `json:"incidentId"`
	Since      time.Time `json:"since"`
}

// PendingApply is the apply that waits for the restart (DESIGN 5.6 apply step 3).
type PendingApply struct {
	CheckID  string `json:"checkId"`
	Snapshot string `json:"snapshot"`
	Summary  string `json:"summary"`
	Origin   Origin `json:"origin"`
}

// LastExit is ReadyResult.lastExit (contracts 6).
type LastExit struct {
	Code       *int      `json:"code"`
	Signal     *string   `json:"signal"`
	StderrTail string    `json:"stderrTail"`
	At         time.Time `json:"at"`
}

// Applied is ReadyResult.applied (contracts 6) and the last apply of an incident.
type Applied struct {
	Snapshot string `json:"snapshot"`
	Summary  string `json:"summary"`
	Origin   Origin `json:"origin"`
}

// IncidentNotice is ReadyResult.incident and the payload of the "incident" bridge event (contracts 6).
type IncidentNotice struct {
	ID           string   `json:"id"`
	Trigger      string   `json:"trigger"`
	Summary      string   `json:"summary"`
	ChangedFiles []string `json:"changedFiles"`
	Lesson       string   `json:"lesson"`
	Origin       Origin   `json:"origin"`
}

// ClassifierRestore is ReadyResult.classifierRestore (contracts 6).
type ClassifierRestore struct {
	Restored bool   `json:"restored"`
	Detail   string `json:"detail"`
}

// Deliver holds the one-time items of the next bridge/ready (contracts 6 delivery rules).
type Deliver struct {
	LastExit          *LastExit          `json:"lastExit"`
	Applied           *Applied           `json:"applied"`
	Incident          *IncidentNotice    `json:"incident"`
	ClassifierRestore *ClassifierRestore `json:"classifierRestore"`
}

// Reverted is one apply snapshot undone by a rollback.
type Reverted struct {
	Ref     string `json:"ref"`
	Summary string `json:"summary"`
}

// Rollback is the unacknowledged rollback record (DESIGN 5.12, contracts 6 RollbackRecord).
type Rollback struct {
	ID     int    `json:"id"`
	Target string `json:"target"`
	// Saved is "rollback/<n>/before".
	Saved    string     `json:"saved"`
	Reverted []Reverted `json:"reverted"`
	// Requesters is null when the rollback came from the command line.
	Requesters []string `json:"requesters"`
	Thread     *string  `json:"thread"`
	Notified   bool     `json:"notified"`
}

// Incident states of OpenIncident.State.
const (
	IncidentRepairing   = "repairing"
	IncidentProbation   = "probation"
	IncidentMaintenance = "maintenance"
)

// OpenIncident is the incident that is not closed yet.
type OpenIncident struct {
	ID       string `json:"id"`
	Trigger  string `json:"trigger"`
	Attempts int    `json:"attempts"`
	// State is IncidentRepairing, IncidentProbation or IncidentMaintenance.
	State     string   `json:"state"`
	LastApply *Applied `json:"lastApply"`
}

// ClassifierTurn records that a member turn started and has not ended (DESIGN 7.10).
type ClassifierTurn struct {
	TurnID    string    `json:"turnId"`
	StartedAt time.Time `json:"startedAt"`
	Senders   []string  `json:"senders"`
}

// State is the content of state.json.
type State struct {
	Version     int    `json:"version"`
	SessionID   string `json:"sessionId,omitempty"`
	SessionFile string `json:"sessionFile,omitempty"`
	// RunningSnapshot is the 40-hex commit that matches the current files.
	RunningSnapshot string `json:"runningSnapshot,omitempty"`
	// LastGoodPending is true from a start that begins a probation until that probation ended. ProbationUntil is the
	// end of the running probation: the ready time of the start that began it plus [agent] probation; it is nil
	// while LastGoodPending is true and that start was not ready yet.
	LastGoodPending bool            `json:"lastGoodPending"`
	ProbationUntil  *time.Time      `json:"probationUntil"`
	Maintenance     *Maintenance    `json:"maintenance"`
	Crashes         []time.Time     `json:"crashes"`
	PendingApply    *PendingApply   `json:"pendingApply"`
	Deliver         Deliver         `json:"deliver"`
	Rollback        *Rollback       `json:"rollback"`
	OpenIncident    *OpenIncident   `json:"openIncident"`
	ClassifierTurn  *ClassifierTurn `json:"classifierTurn"`
}

// New returns an empty state of the current version.
func New() *State { return &State{Version: Version} }

// Load reads the state file at path. A missing file gives New() and exists = false. A file of another version,
// with unknown fields or invalid JSON is an error.
func Load(path string) (s *State, exists bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return New(), false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("读取 %s 失败：%w", path, err)
	}
	var probe struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, true, fmt.Errorf("%s 不是有效的 JSON：%w", path, err)
	}
	if probe.Version == nil {
		return nil, true, fmt.Errorf("%s 中没有 version", path)
	}
	if *probe.Version != Version {
		return nil, true, fmt.Errorf("%s 的版本为 %d，这个 bearbotd 只支持版本 %d", path, *probe.Version, Version)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	st := &State{}
	if err := dec.Decode(st); err != nil {
		return nil, true, fmt.Errorf("无法解析 %s：%w", path, err)
	}
	return st, true, nil
}

// Save writes s to path atomically: a temporary file in the same directory with mode 0600, fsync, rename, fsync of
// the directory. Version is set to the current version.
func Save(path string, s *State) error {
	s.Version = Version
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return fmt.Errorf("写入 %s 失败：%w", path, err)
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("写入 %s 失败：%w", path, err)
	}
	if err := f.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := f.Write(data); err != nil {
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
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("写入 %s 失败：%w", path, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("写入 %s 失败：%w", path, err)
	}
	return nil
}

// NewSessionID returns a session id "bearbot-<UTC yyyymmddhhmmss>-<8 hex>" (DESIGN 5.3).
func NewSessionID(now time.Time) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "bearbot-" + now.UTC().Format("20060102150405") + "-" + hex.EncodeToString(b[:])
}

// Ptr returns a pointer to v, for the nullable fields.
func Ptr[T any](v T) *T { return &v }
