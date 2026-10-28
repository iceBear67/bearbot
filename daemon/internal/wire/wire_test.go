package wire

import (
	"encoding/json"
	"testing"
	"time"

	"bearbot/daemon/internal/pirpc"
	"bearbot/daemon/internal/state"
)

func TestBridgeEventJSON(t *testing.T) {
	reason := "应用 apply/1"
	cases := []struct {
		ev   BridgeEvent
		want string
	}{
		{BridgeEvent{Type: EventExtensionError, ExtensionError: pirpc.ExtensionError{ExtensionPath: "<runtime>", Event: "send_user_message", Error: "boom"}},
			`{"type":"extension_error","extensionPath":"<runtime>","event":"send_user_message","error":"boom"}`},
		{BridgeEvent{Type: EventState, State: Draining, Reason: &reason}, `{"type":"state","state":"draining","reason":"应用 apply/1"}`},
		{BridgeEvent{Type: EventState, State: Running}, `{"type":"state","state":"running","reason":null}`},
		{BridgeEvent{Type: EventIncident, Incident: &state.IncidentNotice{ID: "inc-20261027083000", Trigger: "启动失败", Summary: "s", ChangedFiles: []string{"/a"}, Lesson: "l"}},
			`{"type":"incident","incident":{"id":"inc-20261027083000","trigger":"启动失败","summary":"s","changedFiles":["/a"],"lesson":"l","origin":{"thread":null,"requester":null}}}`},
	}
	for _, c := range cases {
		data, err := json.Marshal(c.ev)
		if err != nil {
			t.Fatal(err)
		}
		var got, want any
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(c.want), &want); err != nil {
			t.Fatal(err)
		}
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		if string(g) != string(w) {
			t.Errorf("got  %s\nwant %s", data, c.want)
		}
	}
	if _, err := json.Marshal(BridgeEvent{Type: "other"}); err == nil {
		t.Fatal("unknown type marshalled")
	}
}

func TestReadyResultOmitsEmptyFields(t *testing.T) {
	data, err := json.Marshal(ReadyResult{})
	if err != nil || string(data) != "{}" {
		t.Fatalf("empty ReadyResult = %s %v", data, err)
	}
	code := 1
	at := time.Date(2026, 10, 27, 8, 30, 0, 0, time.UTC)
	data, err = json.Marshal(ReadyResult{LastExit: &state.LastExit{Code: &code, StderrTail: "x", At: at}})
	if err != nil || string(data) != `{"lastExit":{"code":1,"signal":null,"stderrTail":"x","at":"2026-10-27T08:30:00Z"}}` {
		t.Fatalf("lastExit = %s %v", data, err)
	}
}

func TestAgentProcessExists(t *testing.T) {
	for _, s := range []AgentState{Starting, Running, Draining, Restarting} {
		if !s.AgentProcessExists() {
			t.Errorf("%s", s)
		}
	}
	for _, s := range []AgentState{Stopped, Incident, Repairing, Maintenance, RollingBack} {
		if s.AgentProcessExists() {
			t.Errorf("%s", s)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := ClassifierDecision{Time: "t", TurnID: "1", Tool: "bash", Decision: "deny", Basis: "model"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []ClassifierDecision{
		{TurnID: "1", Tool: "bash", Decision: "deny", Basis: "model"},
		{Time: "t", TurnID: "1", Tool: "bash", Decision: "maybe", Basis: "model"},
		{Time: "t", TurnID: "1", Tool: "bash", Decision: "allow", Basis: "guess"},
		{Time: "t", TurnID: "1", Tool: "bash", Decision: "allow", Basis: "cache", DurationMs: -1},
	} {
		if bad.Validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if err := (RecoveryReport{Status: ReportFixed, Summary: "s", ChangedFiles: []string{}}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []RecoveryReport{
		{Status: "done", Summary: "s", ChangedFiles: []string{}},
		{Status: ReportCannotFix, ChangedFiles: []string{}},
		{Status: ReportCannotFix, Summary: "s"},
		{Status: ReportFixed, Summary: "a\x00b", ChangedFiles: []string{}},
	} {
		if bad.Validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}
