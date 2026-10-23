package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

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
	if err := s.inheritCodexBridge(p); err != nil {
		return err
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
	body := bridgeObject(p.Built.Body)
	recovered := false
	if s.cfg.Upstream.CodexHistoryRecovery {
		p.CodexRecovery = s.codexRecovery
		p.CodexRecoveryScope = codexRecoveryScope(p)
		var stats codexbridge.RecoveryStats
		var err error
		body, stats, err = p.CodexRecovery.Recover(body, p.CodexBridge, p.CodexRecoveryScope)
		if err != nil {
			return codexBridgeAPIError(err, http.StatusBadRequest)
		}
		recovered = stats.Recovered+stats.Unavailable > 0
		if recovered {
			p.recordCodexRecovery(stats)
		}
	}
	history := body
	body, changed, err := codexbridge.Forward(body, p.CodexBridge)
	if err != nil {
		return codexBridgeAPIError(err, http.StatusBadRequest)
	}
	// Recovery is copy-on-write too; Forward may be unchanged for an alias-only
	// replay even though its encrypted input was repaired above.
	if !changed && !recovered {
		p.CodexRecovery.Remember(history, p.CodexBridge, p.CodexRecoveryScope)
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
	p.CodexRecovery.Remember(history, p.CodexBridge, p.CodexRecoveryScope)
	p.recordCodexBridge("request", "", rules.PhaseRequest)
	return nil
}

// Use the full explicit hint, not a truncated wire key or a shared default.
// No hint means no cross-request plaintext caching.
func codexRecoveryScope(p *prepared) string {
	if p.Key == nil {
		return ""
	}
	hint, _ := p.ClientBody["prompt_cache_key"].(string)
	if strings.TrimSpace(hint) == "" {
		for _, name := range []string{"session-id", "session_id", "x-session-id"} {
			if value := p.ClientHeaders.Get(name); strings.TrimSpace(value) != "" {
				hint = value
				break
			}
		}
	}
	if strings.TrimSpace(hint) == "" {
		return ""
	}
	// Policy publication invalidates older plaintext, so recovery cannot undo a
	// newly introduced request/response redaction rule.
	raw, _ := json.Marshal([]any{p.Key.ID, hint, p.RuleVersion})
	return string(raw)
}

func (p *prepared) recordCodexRecovery(stats codexbridge.RecoveryStats) {
	if p.Recorder == nil {
		return
	}
	p.Recorder.OnRuleTrace(p.RuleVersion, map[string]any{
		"source_kind": "gateway", "rule_id": "gateway:codex_history_recovery",
		"rule_name": "Codex encrypted history recovery", "phase": rules.PhaseRequest,
		"status": "changed", "matched": true, "action_type": "protocol_recovery",
		"action_id": "codex_history_recovery", "action_index": 0,
		"recovered_items": stats.Recovered, "unavailable_items": stats.Unavailable,
	})
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
	cloned := *event
	cloned.Raw, cloned.Data = raw, body
	p.recordCodexBridge("response", event.Type, rules.PhaseResponseEvent)
	return &cloned, nil
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

func (s *Server) inheritCodexBridge(p *prepared) *apiError {
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
	return nil
}
