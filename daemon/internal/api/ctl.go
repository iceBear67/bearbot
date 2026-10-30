package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"bearbot/daemon/internal/gateway"
	"bearbot/daemon/internal/rollback"
	"bearbot/daemon/internal/wire"
)

// HandleCtl handles the requests of the gateway on gateway-ctl.sock (contracts 4.2). It implements gateway.Handler.
func (s *Server) HandleCtl(ctx context.Context, method string, params json.RawMessage) (any, error) {
	text := func(t string, err error) (any, error) {
		if err != nil {
			return nil, err
		}
		return wire.TextResult{Text: t}, nil
	}
	switch method {
	case "status":
		if err := ctlParams(params, &struct{}{}); err != nil {
			return nil, err
		}
		return s.Sup.Status(ctx)
	case "session":
		var p struct {
			Action       string  `json:"action"`
			Instructions *string `json:"instructions"`
		}
		if err := ctlParams(params, &p); err != nil {
			return nil, err
		}
		switch p.Action {
		case "new", "abort", "compact":
		default:
			return nil, &gateway.CtlError{Code: gateway.CodeInvalidParams, Message: fmt.Sprintf("action 必须是 new、abort 或 compact，收到 %q", p.Action)}
		}
		instructions := ""
		if p.Instructions != nil {
			instructions = *p.Instructions
		}
		return text(s.Sup.Session(ctx, p.Action, instructions))
	case "restart":
		if err := ctlParams(params, &struct{}{}); err != nil {
			return nil, err
		}
		return text(s.Sup.Restart())
	case "incident.list":
		if err := ctlParams(params, &struct{}{}); err != nil {
			return nil, err
		}
		return text(s.Sup.IncidentList())
	case "incident.get":
		var p struct {
			ID string `json:"id"`
		}
		if err := ctlParams(params, &p); err != nil {
			return nil, err
		}
		return text(s.Sup.IncidentShow(p.ID))
	case "repair":
		var p struct {
			Note  *string `json:"note"`
			Force *bool   `json:"force"`
		}
		if err := ctlParams(params, &p); err != nil {
			return nil, err
		}
		note := ""
		if p.Note != nil {
			note = *p.Note
		}
		return text(s.Sup.Repair(note, p.Force != nil && *p.Force))
	case "resume":
		if err := ctlParams(params, &struct{}{}); err != nil {
			return nil, err
		}
		return text(s.Sup.Resume())
	case "rollback":
		var p struct {
			Snapshot  *string `json:"snapshot"`
			Requester string  `json:"requester"`
			Thread    string  `json:"thread"`
		}
		if err := ctlParams(params, &p); err != nil {
			return nil, err
		}
		if p.Requester == "" || p.Thread == "" {
			return nil, &gateway.CtlError{Code: gateway.CodeInvalidParams, Message: "requester 与 thread 不能为空"}
		}
		req := rollback.Request{Requester: &p.Requester, Thread: &p.Thread}
		if p.Snapshot != nil {
			req.Snapshot = *p.Snapshot
		}
		return text(s.Sup.Rollback(ctx, req))
	case "snapshot.list":
		if err := ctlParams(params, &struct{}{}); err != nil {
			return nil, err
		}
		return text(s.Sup.SnapshotList(ctx))
	case "snapshot.diff":
		var p struct {
			Ref  string  `json:"ref"`
			Ref2 *string `json:"ref2"`
		}
		if err := ctlParams(params, &p); err != nil {
			return nil, err
		}
		ref2 := ""
		if p.Ref2 != nil {
			ref2 = *p.Ref2
		}
		return text(s.Sup.SnapshotDiff(ctx, p.Ref, ref2))
	default:
		return nil, &gateway.CtlError{Code: gateway.CodeMethodNotFound, Message: "Method not found: " + method}
	}
}

// ctlParams decodes params strictly; problems are answered with -32602.
func ctlParams(params json.RawMessage, v any) error {
	if len(bytes.TrimSpace(params)) == 0 || string(bytes.TrimSpace(params)) == "null" {
		params = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(params))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &gateway.CtlError{Code: gateway.CodeInvalidParams, Message: "参数无效：" + err.Error()}
	}
	return nil
}
