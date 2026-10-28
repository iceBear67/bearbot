package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestLoadMissing(t *testing.T) {
	s, exists, err := Load(filepath.Join(t.TempDir(), "state.json"))
	if err != nil || exists || s.Version != Version || s.SessionID != "" || s.Maintenance != nil {
		t.Fatalf("Load = %+v, %v, %v", s, exists, err)
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	at := time.Date(2026, 10, 27, 8, 30, 0, 0, time.UTC)
	s := New()
	s.SessionID = "bearbot-20261027083000-1a2b3c4d"
	s.SessionFile = "/bearbot/profile/agent/sessions/--bearbot-workspace--/x.jsonl"
	s.RunningSnapshot = strings.Repeat("a", 40)
	s.LastGoodPending = true
	s.ProbationUntil = Ptr(at.Add(10 * time.Minute))
	s.Maintenance = &Maintenance{Reason: "recovery 无法修复", IncidentID: Ptr("inc-20261027083000"), Since: at}
	s.Crashes = []time.Time{at, at.Add(time.Minute)}
	s.PendingApply = &PendingApply{CheckID: "chk-20261027083000-abcd", Snapshot: "apply/3", Summary: "加了一个工具", Origin: Origin{Thread: Ptr("telegram:-100"), Requester: Ptr("telegram:1")}}
	code := 1
	s.Deliver.LastExit = &LastExit{Code: &code, StderrTail: "boom", At: at}
	s.Deliver.ClassifierRestore = &ClassifierRestore{Restored: true, Detail: "已恢复"}
	s.Rollback = &Rollback{ID: 2, Target: "apply/3^", Saved: "rollback/2/before", Reverted: []Reverted{{Ref: "apply/3", Summary: "x"}}, Requesters: nil, Thread: nil}
	s.OpenIncident = &OpenIncident{ID: "inc-20261027083000", Trigger: "启动失败", Attempts: 1, State: IncidentRepairing, LastApply: &Applied{Snapshot: "apply/3", Summary: "x"}}
	s.ClassifierTurn = &ClassifierTurn{TurnID: "t1", StartedAt: at, Senders: []string{"telegram:1"}}
	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}
	loaded, exists, err := Load(path)
	if err != nil || !exists {
		t.Fatal(err)
	}
	a, _ := json.Marshal(s)
	b, _ := json.Marshal(loaded)
	if string(a) != string(b) {
		t.Fatalf("round trip differs:\n%s\n%s", a, b)
	}
	raw, _ := os.ReadFile(path)
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	if generic["rollback"].(map[string]any)["requesters"] != nil || generic["rollback"].(map[string]any)["thread"] != nil {
		t.Fatalf("CLI rollback must have null requesters and thread: %s", raw)
	}
	if generic["deliver"].(map[string]any)["applied"] != nil {
		t.Fatalf("absent deliver item must be null: %s", raw)
	}
	if generic["deliver"].(map[string]any)["lastExit"].(map[string]any)["signal"] != nil {
		t.Fatalf("signal must be null: %s", raw)
	}
	if generic["probationUntil"] != "2026-10-27T08:40:00Z" {
		t.Fatalf("probationUntil must be an RFC 3339 time: %s", raw)
	}

	// Overwrite keeps the file valid.
	s.SessionID = "bearbot-20261027090000-00000000"
	s.Rollback = nil
	s.ProbationUntil = nil
	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}
	loaded, _, err = Load(path)
	if err != nil || loaded.SessionID != s.SessionID || loaded.Rollback != nil || loaded.ProbationUntil != nil {
		t.Fatalf("after overwrite: %+v, %v", loaded, err)
	}
	raw, _ = os.ReadFile(path)
	if !strings.Contains(string(raw), `"probationUntil": null`) {
		t.Fatalf("no probation must be written as null: %s", raw)
	}
}

// TestLoadWithoutProbationUntil: a state.json written before probationUntil existed has no such field; it loads
// with no probation end.
func TestLoadWithoutProbationUntil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version": 1, "lastGoodPending": true, "deliver": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _, err := Load(path)
	if err != nil || !s.LastGoodPending || s.ProbationUntil != nil {
		t.Fatalf("Load = %+v, %v", s, err)
	}
}

func TestContractExample(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	example := `{
  "version": 1,
  "sessionId": "bearbot-20261027083000-1a2b3c4d",
  "sessionFile": "/bearbot/profile/agent/sessions/--bearbot-workspace--/x.jsonl",
  "runningSnapshot": "0123456789012345678901234567890123456789",
  "lastGoodPending": true,
  "probationUntil": "2026-10-27T08:40:00.123+08:00",
  "maintenance": { "reason": "...", "incidentId": "inc-20261027083000", "since": "2026-10-27T08:30:00Z" },
  "crashes": ["2026-10-27T08:30:00Z", "2026-10-27T08:31:00+08:00"],
  "pendingApply": { "checkId": "chk-x", "snapshot": "apply/3", "summary": "...", "origin": { "thread": "t", "requester": "r" } },
  "deliver": { "lastExit": null, "applied": null, "incident": null, "classifierRestore": null },
  "rollback": null,
  "openIncident": { "id": "inc-20261027083000", "trigger": "...", "attempts": 1, "state": "repairing", "lastApply": { "snapshot": "apply/3", "summary": "...", "origin": {"thread": "t", "requester": "r"} } },
  "classifierTurn": { "turnId": "t1", "startedAt": "2026-10-27T08:30:00Z", "senders": ["telegram:1"] }
}`
	if err := os.WriteFile(path, []byte(example), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.OpenIncident.State != IncidentRepairing || *s.PendingApply.Origin.Requester != "r" || len(s.Crashes) != 2 || s.ClassifierTurn.Senders[0] != "telegram:1" ||
		!s.ProbationUntil.Equal(time.Date(2026, 10, 27, 0, 40, 0, 123e6, time.UTC)) {
		t.Fatalf("state = %+v", s)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"version":       `{"version": 2}`,
		"no version":    `{"sessionId": "x"}`,
		"unknown field": `{"version": 1, "surprise": true}`,
		"bad json":      `{"version": 1`,
	}
	for name, content := range cases {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Load(path); err == nil {
			t.Errorf("%s: Load succeeded", name)
		}
	}
}

func TestNewSessionID(t *testing.T) {
	id := NewSessionID(time.Date(2026, 10, 27, 16, 30, 0, 0, time.FixedZone("CST", 8*3600)))
	if !regexp.MustCompile(`^bearbot-20261027083000-[0-9a-f]{8}$`).MatchString(id) {
		t.Fatalf("id = %s", id)
	}
	if NewSessionID(time.Now()) == NewSessionID(time.Now()) {
		t.Fatal("ids repeat")
	}
}
