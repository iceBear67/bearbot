package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"bearbot/daemon/internal/agent"
	"bearbot/daemon/internal/state"
	"bearbot/daemon/internal/wire"
)

var (
	agentOnly     = agent.AgentStates
	agentOrRepair = append(slices.Clone(agent.AgentStates), wire.Repairing)
	repairOnly    = []wire.AgentState{wire.Repairing}
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(data, '\n'))
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status := statusOf(err)
	if status == http.StatusInternalServerError {
		s.Log.Error("agent.sock request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	} else {
		s.Log.Info("agent.sock request rejected", "method", r.Method, "path", r.URL.Path, "status", status, "err", err)
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// gate accepts the request only in the given states (contracts 6: 409 "当前状态为 <state>，不接受这个请求").
func (s *Server) gate(states []wire.AgentState, h func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.Sup.Require("", states...); err != nil {
			s.writeError(w, r, err)
			return
		}
		if err := h(w, r); err != nil {
			s.writeError(w, r, err)
		}
	}
}

// AgentHandler serves agent.sock.
func (s *Server) AgentHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/bridge/ready", s.gate(agentOnly, s.bridgeReady))
	mux.HandleFunc("GET /v1/bridge/events", s.gate(agentOnly, s.bridgeEvents))
	mux.HandleFunc("GET /v1/status", s.gate(agentOrRepair, s.status))
	mux.HandleFunc("GET /v1/logs", s.gate(agentOrRepair, s.logs))
	mux.HandleFunc("POST /v1/check", s.gate(agentOrRepair, s.check))
	mux.HandleFunc("POST /v1/apply", s.gate(agentOnly, s.apply))
	mux.HandleFunc("POST /v1/snapshot", s.gate(agentOrRepair, s.snapshot))
	mux.HandleFunc("POST /v1/classifier/turn", s.gate(agentOnly, s.classifierTurn))
	mux.HandleFunc("POST /v1/classifier/decisions", s.gate(agentOnly, s.classifierDecision))
	mux.HandleFunc("POST /v1/rollback/ack", s.gate(agentOnly, s.rollbackAck))
	mux.HandleFunc("POST /v1/recovery/report", s.gate(repairOnly, s.recoveryReport))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("没有这个接口：%s %s", r.Method, r.URL.Path)})
	})
	return mux
}

func (s *Server) bridgeReady(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Tools    []string `json:"tools"`
		Commands []string `json:"commands"`
	}
	if err := decode(r.Body, &body); err != nil {
		return err
	}
	if body.Tools == nil || body.Commands == nil {
		return badRequest("tools 与 commands 必须是数组")
	}
	res, err := s.Sup.Ready(body.Tools, body.Commands)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

func (s *Server) bridgeEvents(w http.ResponseWriter, r *http.Request) error {
	sub, err := s.Sup.Subscribe()
	if err != nil {
		return err
	}
	defer s.Sup.Unsubscribe(sub)
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return nil
	}
	enc := json.NewEncoder(w)
	for {
		ev, ack, ok := sub.Next(r.Context())
		if !ok {
			return nil
		}
		err := enc.Encode(ev)
		if err == nil {
			err = rc.Flush()
		}
		ack(err)
		if err != nil {
			s.Log.Info("bridge events connection ended", "err", err)
			return nil
		}
	}
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) error {
	info, err := s.Sup.Status(s.Ctx)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, info)
	return nil
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	source, kind := q.Get("source"), q.Get("kind")
	if st := s.Sup.CurrentState(); st != wire.Repairing && source != "agent" {
		return &agent.StateError{State: st, Op: "读取 source=" + source + " 的日志（agent 进程只能读取 source=agent）"}
	}
	n, err := lineCount(q.Get("n"))
	if err != nil {
		return err
	}
	lines, err := s.Sup.Logs(source, kind, n)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
	return nil
}

func lineCount(v string) (int, error) {
	if v == "" {
		return agent.LogsDefault, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, badRequest("n 必须是正整数，收到 %q", v)
	}
	return min(n, agent.LogsMax), nil
}

func (s *Server) check(w http.ResponseWriter, r *http.Request) error {
	var body struct{}
	if err := decode(r.Body, &body); err != nil {
		return err
	}
	rep, err := s.Sup.Check(s.Ctx)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, rep)
	return nil
}

func (s *Server) apply(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Summary string        `json:"summary"`
		Origin  *state.Origin `json:"origin"`
	}
	if err := decode(r.Body, &body); err != nil {
		return err
	}
	if body.Origin == nil {
		return badRequest("缺少 origin")
	}
	res, err := s.Sup.Apply(s.Ctx, body.Summary, *body.Origin)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Action string   `json:"action"`
		Ref    string   `json:"ref"`
		Ref2   string   `json:"ref2"`
		Paths  []string `json:"paths"`
	}
	if err := decode(r.Body, &body); err != nil {
		return err
	}
	switch body.Action {
	case "list":
		text, err := s.Sup.SnapshotList(s.Ctx)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, wire.TextResult{Text: text})
	case "diff":
		text, err := s.Sup.SnapshotDiff(s.Ctx, body.Ref, body.Ref2)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, wire.TextResult{Text: text})
	case "restore":
		res, err := s.Sup.SnapshotRestore(s.Ctx, body.Ref, body.Paths)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, res)
	default:
		return badRequest("action 必须是 list、diff 或 restore，收到 %q", body.Action)
	}
	return nil
}

func (s *Server) classifierTurn(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Phase   string   `json:"phase"`
		TurnID  string   `json:"turnId"`
		Senders []string `json:"senders"`
	}
	if err := decode(r.Body, &body); err != nil {
		return err
	}
	switch body.Phase {
	case "start":
		res, err := s.Sup.ClassifierTurnStart(body.TurnID, body.Senders)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, res)
	case "senders":
		if err := s.Sup.ClassifierTurnSenders(body.TurnID, body.Senders); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, struct{}{})
	case "end":
		res, err := s.Sup.ClassifierTurnEnd(body.TurnID)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, res)
	default:
		return badRequest("phase 必须是 start、senders 或 end，收到 %q", body.Phase)
	}
	return nil
}

func (s *Server) classifierDecision(w http.ResponseWriter, r *http.Request) error {
	var d wire.ClassifierDecision
	if err := decode(r.Body, &d); err != nil {
		return err
	}
	if err := s.Sup.RecordDecision(d); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, struct{}{})
	return nil
}

func (s *Server) rollbackAck(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		ID *int `json:"id"`
	}
	if err := decode(r.Body, &body); err != nil {
		return err
	}
	if body.ID == nil {
		return badRequest("缺少 id")
	}
	if err := s.Sup.RollbackAck(*body.ID); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, struct{}{})
	return nil
}

func (s *Server) recoveryReport(w http.ResponseWriter, r *http.Request) error {
	var rep wire.RecoveryReport
	if err := decode(r.Body, &rep); err != nil {
		return err
	}
	if err := s.Sup.RecoveryReport(rep); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]bool{"accepted": true})
	return nil
}
