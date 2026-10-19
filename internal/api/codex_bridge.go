package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"pi-gateway/internal/codexbridge"
	"pi-gateway/internal/piwire"
	"pi-gateway/internal/rules"
	"pi-gateway/internal/session"
	"pi-gateway/internal/upstream"
)

// bridgeObject copies the ordered root, leaving nested copy-on-write handling to
// the protocol adapter. RawRequestBody and ClientBody remain diagnostic originals.
func bridgeObject(body *piwire.OrderedMap) map[string]any {
	object := make(map[string]any, len(body.Keys()))
	for _, key := range body.Keys() {
		object[key], _ = body.Get(key)
	}
	return object
}

func (s *Server) detectCodexBridge(p *prepared) *apiError {
	if !s.cfg.Upstream.CodexMultiAgentCompat {
		return nil
	}
	profile, err := codexbridge.Detect(bridgeObject(p.Built.Body))
	if err != nil {
		return codexBridgeAPIError(err, http.StatusBadRequest)
	}
	p.CodexBridge, err = codexbridge.Merge(p.CodexBridge, profile)
	if err != nil {
		return codexBridgeAPIError(err, http.StatusBadRequest)
	}
	return nil
}

func (s *Server) prepareCodexBridge(p *prepared) *apiError {
	if p.Continuation != nil {
		parent := p.Continuation.CodexBridge
		if !parent.Valid() || (!s.cfg.Upstream.CodexMultiAgentCompat && parent.Enabled()) {
			return continuationAPIError(session.ErrInvalid)
		}
		// The upstream chain cannot change its protocol domain halfway through.
		if p.CodexBridge.Enabled() && !parent.Enabled() {
			return continuationAPIError(session.ErrInvalid)
		}
		merged, err := codexbridge.Merge(parent, p.CodexBridge)
		if err != nil || merged != parent {
			return continuationAPIError(session.ErrInvalid)
		}
		p.CodexBridge = parent
	}
	// Finalize rules may have changed the visible tool table. Re-check it after
	// profile inheritance, while retaining identities stripped by compact.
	before := p.CodexBridge
	if apiErr := s.detectCodexBridge(p); apiErr != nil {
		return apiErr
	}
	if p.Continuation != nil && p.CodexBridge != before {
		return continuationAPIError(session.ErrInvalid)
	}
	if !p.CodexBridge.Enabled() {
		return nil
	}
	body, changed, err := codexbridge.Forward(bridgeObject(p.Built.Body), p.CodexBridge)
	if err != nil {
		return codexBridgeAPIError(err, http.StatusBadRequest)
	}
	if !changed {
		return nil
	}
	ordered, err := ruleOrderedBody(body, p.Built.Body)
	if err != nil {
		return ruleAPIError("could not construct Codex compatibility request")
	}
	raw, err := json.Marshal(ordered)
	if err != nil {
		return ruleAPIError("could not serialize Codex compatibility request")
	}
	p.Built.Body, p.Built.JSON = ordered, raw
	p.recordCodexBridge("request", "", rules.PhaseRequest)
	return nil
}

func codexBridgeAPIError(err error, status int) *apiError {
	code, message := "codex_bridge_protocol_error", "Codex multi-agent protocol compatibility failed."
	var bridgeErr *codexbridge.Error
	if errors.As(err, &bridgeErr) {
		code, message = bridgeErr.Code, bridgeErr.Message
	}
	kind := "invalid_request_error"
	if status >= 500 {
		kind = "upstream_error"
	}
	return &apiError{Status: status, Type: kind, Code: code, Message: message}
}

func (p *prepared) restoreCodexEvent(event *upstream.Event) (*upstream.Event, error) {
	if event == nil || !p.CodexBridge.Enabled() {
		return event, nil
	}
	body, changed, err := codexbridge.RestoreEvent(event.Data, p.CodexBridge)
	if err != nil {
		return nil, codexBridgeAPIError(err, http.StatusBadGateway)
	}
	if !changed {
		return event, nil
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, codexBridgeAPIError(err, http.StatusBadGateway)
	}
	copy := *event
	copy.Raw, copy.Data = raw, body
	p.recordCodexBridge("response", event.Type, rules.PhaseResponseEvent)
	return &copy, nil
}

func (p *prepared) restoreCodexBody(body map[string]any) (map[string]any, error) {
	if !p.CodexBridge.Enabled() {
		return body, nil
	}
	restored, changed, err := codexbridge.RestoreBody(body, p.CodexBridge)
	if err != nil {
		return nil, codexBridgeAPIError(err, http.StatusBadGateway)
	}
	if changed {
		p.recordCodexBridge("response", "response", rules.PhaseResponseBody)
	}
	return restored, nil
}

// Record only bounded protocol provenance, not a second copy of task text.
func (p *prepared) recordCodexBridge(direction, eventType, phase string) {
	if p.Recorder == nil {
		return
	}
	p.Recorder.OnRuleTrace(p.RuleVersion, map[string]any{
		"source_kind": "gateway", "rule_id": "gateway:codex_multi_agent_compat",
		"rule_name": "Codex multi-agent compatibility", "phase": phase,
		"status": "changed", "matched": true, "direction": direction,
		"event_type": eventType, "action_id": "codex_multi_agent_compat",
		"action_type": "protocol_bridge", "action_index": 0,
	})
}
