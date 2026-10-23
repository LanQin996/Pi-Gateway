package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"pi-gateway/internal/middleware"
	"pi-gateway/internal/piwire"
	"pi-gateway/internal/rules"
	"pi-gateway/internal/rulesruntime"
	"pi-gateway/internal/upstream"
)

func (s *Server) applyRequestRules(ctx context.Context, p *prepared) *apiError {
	engine, version := p.RuleEngine, p.RuleVersion
	var err error
	if engine == nil {
		engine, version, err = s.ruleService.Load(ctx)
	}
	if err != nil {
		if errors.Is(err, rulesruntime.ErrMigration) {
			// A failed conversion must not disable the legacy policy. Keep it live
			// until the operator repairs it through the compatibility endpoint.
			return s.applyLegacyRequestRules(ctx, p)
		}
		s.logger.Warn("loading rules failed", "error", err)
		return ruleAPIError("could not load request rules")
	}
	p.RuleEngine, p.RuleVersion = engine, version
	p.recordGatewayDifference("normalize", "Request normalization", p.RawRequestBody, p.Built.JSON)
	input := p.ruleInput(p.Built.Body, "")
	if p.Recorder != nil {
		p.Recorder.OnRuleInput(rules.PhaseRequest, input, false)
	}
	result, err := engine.Apply(ctx, rules.PhaseRequest, input)
	p.recordRuleResult(result, "", "", nil)
	if err != nil {
		s.logger.Warn("request rule failed", "error", err)
		return ruleAPIError("request rule evaluation failed")
	}
	body, err := ruleOrderedBody(result.Body, p.Built.Body)
	if err != nil {
		return ruleAPIError("request rules must produce a JSON object")
	}
	p.Built.Body, p.Built.Model = body, result.Model
	if value, exists := body.Get("model"); exists {
		model, ok := value.(string)
		if !ok || strings.TrimSpace(model) == "" {
			return ruleAPIError("request rules produced an invalid model")
		}
		p.Built.Model = model
	}
	p.RuleContext["model"] = p.Built.Model
	if p.Recorder != nil {
		p.Recorder.SetRoute(0, "", p.Built.Model, p.SessionID, "")
	}
	if result.Blocked {
		status := result.Status
		if status < 400 || status > 499 {
			status = http.StatusForbidden
		}
		return &apiError{Status: status, Message: result.Reason, Type: "invalid_request_error", Code: "blocked_by_rule"}
	}
	return nil
}

func (s *Server) applyLegacyRequestRules(ctx context.Context, p *prepared) *apiError {
	p.recordGatewayDifference("normalize", "Request normalization", p.RawRequestBody, p.Built.JSON)
	chain, err := s.buildChain(ctx)
	if err != nil {
		return ruleAPIError("legacy rule configuration requires repair")
	}
	request := &middleware.Request{Body: p.Built.Body, Model: p.Built.Model,
		SessionID: p.SessionID, APIKeyID: p.Key.ID,
		Meta: map[string]any{"client_body": p.ClientBody, "client_transport": p.RuleContext["client_protocol"]}}
	p.MW, err = chain.Apply(ctx, request)
	p.Built.Body, p.Built.Model = request.Body, request.Model
	if p.Recorder != nil {
		status := "legacy"
		if err != nil {
			status = "error"
		} else if p.MW.Dropped {
			status = "blocked"
		}
		p.Recorder.OnRuleTrace(0, map[string]any{"source_kind": "legacy", "rule_id": "legacy:unmigrated", "rule_name": "Legacy middleware (migration requires repair)", "phase": rules.PhaseRequest, "status": status, "notes": p.MW.Changes})
	}
	if err != nil {
		return ruleAPIError("legacy rule evaluation failed")
	}
	if p.MW.Dropped {
		return &apiError{Status: http.StatusForbidden, Message: p.MW.Reason, Type: "invalid_request_error", Code: "blocked_by_rule"}
	}
	return nil
}

func ruleAPIError(message string) *apiError {
	return &apiError{Status: http.StatusInternalServerError, Message: message, Type: "server_error", Code: "rule_error"}
}

func (p *prepared) ruleInput(body any, eventType string) *rules.Input {
	facts := make(map[string]any, len(p.RuleContext)+1)
	for key, value := range p.RuleContext {
		facts[key] = value
	}
	if eventType != "" {
		facts["event_type"] = eventType
	}
	input := &rules.Input{Body: body, ClientBody: p.ClientBody, Context: facts, Model: p.Built.Model, EventType: eventType}
	if p.Recorder != nil {
		input.Trace = true
		input.TraceMaxBytes = max(1, p.Recorder.TraceBudget())
	}
	return input
}

func (p *prepared) recordRuleResult(result rules.Result, eventID, eventType string, sequence any) {
	if p.Recorder == nil {
		return
	}
	for _, trace := range result.Traces {
		trace.EventID, trace.EventType, trace.Sequence = eventID, eventType, sequence
		p.Recorder.OnRuleTrace(p.RuleVersion, trace)
	}
	if result.TraceOmitted > 0 {
		p.Recorder.OmitRuleTraces(p.RuleVersion, result.TraceOmitted)
	}
}

// transformResponseEvent is shared by SSE, aggregated HTTP and downstream WS.
// The upstream sink/session bookkeeping sees the original event before this call.
func (p *prepared) transformResponseEvent(ctx context.Context, event *upstream.Event) (*upstream.Event, error) {
	// Rules see the same native tool identities as the client. Re-validate after
	// rules too: a rule must not turn plaintext tool arguments into ciphertext or
	// leak the reserved upstream alias. Both passes are immutable and idempotent.
	restored, err := p.restoreCodexEvent(event)
	if err != nil {
		return nil, err
	}
	transformed, err := p.applyResponseEventRules(ctx, restored)
	if err != nil {
		return nil, err
	}
	final, err := p.restoreCodexEvent(transformed)
	// Aggregated responses still pass through response-body policy. Do not
	// retain event arguments that a later body rule may redact or drop.
	if err == nil && final != nil && p.WantsStream {
		p.CodexRecovery.Remember(final.Data, p.CodexBridge, p.CodexRecoveryScope)
	}
	return final, err
}

func (p *prepared) applyResponseEventRules(ctx context.Context, event *upstream.Event) (*upstream.Event, error) {
	if event == nil {
		return nil, nil
	}
	p.RuleEventSeq++
	eventID := strconv.FormatInt(p.RuleEventSeq, 10)
	if p.Recorder != nil {
		p.Recorder.SetRuleEvent(eventID)
	}
	if p.RuleEngine == nil {
		return event, nil
	}
	if p.RuleEventSeq == 1 && p.Recorder != nil {
		p.Recorder.OnRuleInput(rules.PhaseResponseEvent, p.ruleInput(nil, ""), true)
	}
	result, err := p.RuleEngine.Apply(ctx, rules.PhaseResponseEvent, p.ruleInput(event.Data, event.Type))
	p.recordRuleResult(result, eventID, event.Type, event.Data["sequence_number"])
	if err != nil {
		return nil, ruleAPIError("response rule evaluation failed")
	}
	if result.Dropped {
		return nil, nil
	}
	if !result.Changed {
		return event, nil
	}
	raw, data, err := ruleResponseObject(result.Body)
	if err != nil {
		return nil, ruleAPIError("response rules must produce a JSON object")
	}
	cloned := *event
	cloned.Raw, cloned.Data = raw, data
	return &cloned, nil
}

func (p *prepared) transformResponseBody(ctx context.Context, body map[string]any) (map[string]any, error) {
	restored, err := p.restoreCodexBody(body)
	if err != nil {
		return nil, err
	}
	transformed, err := p.applyResponseBodyRules(ctx, restored)
	if err != nil {
		return nil, err
	}
	final, err := p.restoreCodexBody(transformed)
	if err == nil {
		p.CodexRecovery.Remember(final, p.CodexBridge, p.CodexRecoveryScope)
	}
	return final, err
}

func (p *prepared) applyResponseBodyRules(ctx context.Context, body map[string]any) (map[string]any, error) {
	if p.RuleEngine == nil {
		return body, nil
	}
	input := p.ruleInput(body, "")
	if p.Recorder != nil {
		p.Recorder.OnRuleInput(rules.PhaseResponseBody, input, false)
	}
	result, err := p.RuleEngine.Apply(ctx, rules.PhaseResponseBody, input)
	p.recordRuleResult(result, strconv.FormatInt(p.RuleEventSeq, 10), "response", nil)
	if err != nil {
		return nil, ruleAPIError("response body rule evaluation failed")
	}
	if !result.Changed {
		return body, nil
	}
	_, output, err := ruleResponseObject(result.Body)
	if err != nil {
		return nil, ruleAPIError("response body rules must produce a JSON object")
	}
	return output, nil
}

func ruleResponseObject(body any) ([]byte, map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, nil, fmt.Errorf("expected a JSON object")
	}
	return raw, object, nil
}

func ruleOrderedBody(body any, previous *piwire.OrderedMap) (*piwire.OrderedMap, error) {
	if ordered, ok := body.(*piwire.OrderedMap); ok && ordered != nil {
		return ordered, nil
	}
	object, ok := body.(map[string]any)
	if !ok || object == nil {
		return nil, fmt.Errorf("expected a JSON object")
	}
	ordered := piwire.NewOrderedMap()
	keys := piwire.PreferredKeyOrder
	if previous != nil {
		keys = previous.Keys()
	}
	for _, key := range keys {
		if value, exists := object[key]; exists {
			ordered.Set(key, value)
		}
	}
	remaining := make([]string, 0)
	for key := range object {
		if !ordered.Has(key) {
			remaining = append(remaining, key)
		}
	}
	sort.Strings(remaining)
	for _, key := range remaining {
		ordered.Set(key, object[key])
	}
	return ordered, nil
}

// Gateway transformations are reported separately from user rules. This bounded
// top-level diff is explanatory; it never drives execution or guesses lineage.
func (p *prepared) recordGatewayDifference(id, name string, before, after []byte) {
	if p.Recorder == nil || bytes.Equal(before, after) {
		return
	}
	trace := map[string]any{"source_kind": "gateway", "rule_id": "gateway:" + id,
		"rule_name": name, "phase": rules.PhaseRequest, "status": "changed", "matched": true,
		"action_id": id, "action_type": id, "action_index": 0}
	budget := p.Recorder.TraceBudget()
	if budget > 1<<20 {
		budget = 1 << 20
	}
	if budget <= 0 || len(before) > budget || len(after) > budget {
		trace["status"], trace["omitted_changes"] = "unavailable", 1
		p.Recorder.OnRuleTrace(p.RuleVersion, trace)
		return
	}
	var left, right map[string]any
	if json.Unmarshal(before, &left) != nil || json.Unmarshal(after, &right) != nil {
		return
	}
	changes, omitted := gatewayChanges(left, right)
	if len(changes) == 0 && omitted == 0 {
		return
	}
	trace["changes"], trace["omitted_changes"] = changes, omitted
	p.Recorder.OnRuleTrace(p.RuleVersion, trace)
}

func boundedRuleValue(value any) (any, bool) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, true
	}
	if len(raw) <= rules.MaxPayloadBytes {
		return value, false
	}
	return map[string]any{"preview": strings.ToValidUTF8(string(raw[:rules.MaxPayloadBytes]), "\uFFFD"), "bytes": len(raw)}, true
}

func gatewayChanges(left, right map[string]any) ([]rules.Change, int) {
	keys := sortedGatewayKeys(left, right)
	changes := make([]rules.Change, 0)
	omitted := 0
	for _, key := range keys {
		old, had := left[key]
		next, has := right[key]
		if had == has && reflect.DeepEqual(old, next) {
			continue
		}
		if len(changes) >= 64 {
			omitted++
			continue
		}
		op := "replace"
		if !had {
			op = "add"
		} else if !has {
			op = "remove"
		}
		old, oldTruncated := boundedRuleValue(old)
		next, nextTruncated := boundedRuleValue(next)
		changes = append(changes, rules.Change{Path: "/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(key),
			Operation: op, BeforeExists: had, AfterExists: has, Before: old, After: next, Truncated: oldTruncated || nextTruncated})
	}
	return changes, omitted
}

func sortedGatewayKeys(left, right map[string]any) []string {
	keys, seen := []string{}, map[string]bool{}
	for key := range left {
		keys = append(keys, key)
		seen[key] = true
	}
	for key := range right {
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	return keys
}
