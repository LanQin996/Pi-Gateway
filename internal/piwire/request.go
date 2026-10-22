package piwire

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// BuildOptions controls request normalisation.
type BuildOptions struct {
	// DefaultModel is used when the client omits a model.
	DefaultModel string
	// ModelMappings rewrites requested model ids.
	ModelMappings map[string]string
	// DefaultReasoningEffort/Summary are applied when the client omits reasoning.
	DefaultReasoningEffort  string
	DefaultReasoningSummary string
	// SessionID is the value sent as prompt_cache_key. Leave it empty when the
	// client did not ask for prompt caching: Pi only sends the field when a
	// session id actually exists. The gateway's internal session id (used for the
	// session headers and the upstream socket pool) is tracked separately and must
	// never be passed here, or a generated id would leak into the body.
	SessionID string
	// ExtraFields is retained for compatibility. Non-rejected client fields now
	// pass through by default; this cannot override route constraints.
	ExtraFields []string
}

// BuiltRequest is the normalised upstream request.
type BuiltRequest struct {
	Body      *OrderedMap
	JSON      []byte
	Model     string
	SessionID string
}

// BuildRequest normalises a Sign in with ChatGPT Responses payload. Core keys
// retain Pi's order; other client fields pass through unless the preview docs
// explicitly reject them. Continuation is validated separately by the gateway.
func BuildRequest(client map[string]any, o BuildOptions) (*BuiltRequest, error) {
	if client == nil {
		client = map[string]any{}
	}

	model := getString(client, "model")
	if model == "" {
		model = o.DefaultModel
	}
	if mapped, ok := o.ModelMappings[model]; ok && mapped != "" {
		model = mapped
	}
	if model == "" {
		return nil, fmt.Errorf("no model specified and no default model configured")
	}

	body := NewOrderedMap()
	body.Set("model", model)

	input := client["input"]
	switch value := input.(type) {
	case nil:
		input = []any{}
	case string:
		// Responses clients can use a string shorthand. Pi's ChatGPT route
		// expects an explicit user message with input_text content instead.
		input = []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": value},
		}}}
	}
	body.Set("input", input)
	body.Set("stream", true)

	if o.SessionID != "" {
		body.Set("prompt_cache_key", ClampPromptCacheKey(o.SessionID))
	}
	// ChatGPT rejects store:true for subscription accounts ("Store must be set to false").
	body.Set("store", false)

	appendRequestOptions(body, client, o)

	// previous_response_id is appended last, matching Pi's spread order. Unlike
	// Pi's own client (which lets the transport own continuation state), the
	// gateway is transparent: a client that explicitly asks to continue a response
	// must keep that field, so it is preserved rather than dropped.
	if v, ok := client["previous_response_id"]; ok && v != nil && v != "" {
		body.SetLast("previous_response_id", v)
	}

	appendRequestExtensions(body, client)
	EnforcePiShape(body)

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode upstream request: %w", err)
	}
	return &BuiltRequest{Body: body, JSON: raw, Model: model, SessionID: o.SessionID}, nil
}

// chatgptRejectedFields follows the preview limitations for this route:
// https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations
// previous_response_id is handled by connection-local continuation validation,
// rather than this transport-independent field filter.
var chatgptRejectedFields = []string{
	"background",
	"conversation",
	"max_output_tokens",
	"max_tool_calls",
	"metadata",
	"moderation",
	"multi_agent",
	"prompt",
	"prompt_cache_retention",
	"safety_identifier",
	"temperature",
	"top_logprobs",
	"top_p",
	"truncation",
	"user",
	// The WebSocket envelope is added by the transport, never by client fields.
	"type",
}

func isChatGPTRejectedField(field string) bool {
	for _, rejected := range chatgptRejectedFields {
		if field == rejected {
			return true
		}
	}
	return false
}

// EnforcePiShape re-applies the route invariants to a body that middleware may
// have rewritten: rejected fields are dropped, store is forced to false, stream to
// true, and previous_response_id is moved back to the end where Pi spreads it. It
// reports what it changed so the capture can note it.
//
// Fields not forbidden by the preview docs are preserved, including instructions,
// text options and parallel_tool_calls. A model may still reject an option.
func EnforcePiShape(body *OrderedMap) []string {
	if body == nil {
		return nil
	}
	var changes []string
	for _, field := range chatgptRejectedFields {
		if body.Has(field) {
			body.Delete(field)
			changes = append(changes, "dropped "+field)
		}
	}
	if v, ok := body.Get("store"); !ok || v != false {
		body.Set("store", false)
		changes = append(changes, "forced store=false")
	}
	if v, ok := body.Get("stream"); !ok || v != true {
		body.Set("stream", true)
		changes = append(changes, "forced stream=true")
	}
	if v, ok := body.Get("previous_response_id"); ok {
		body.Delete("previous_response_id")
		body.SetLast("previous_response_id", v)
	}
	return changes
}

// PreferredKeyOrder is the order Pi builds for the responses request body. Keys not
// listed follow in lexicographic order.
var PreferredKeyOrder = []string{
	"model", "input", "stream", "prompt_cache_key", "store",
	"service_tier", "tools", "tool_choice", "reasoning", "include", "previous_response_id",
}

// MarshalWithPreferredOrder serialises a body using Pi's field order, which keeps a
// rebuilt body (for example a WebSocket continuation delta) byte-shaped like Pi's.
func MarshalWithPreferredOrder(body map[string]any) ([]byte, error) {
	om := NewOrderedMap()
	for _, key := range PreferredKeyOrder {
		if v, ok := body[key]; ok {
			om.Set(key, v)
		}
	}
	var rest []string
	for key := range body {
		if !om.Has(key) {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	for _, key := range rest {
		om.Set(key, body[key])
	}
	return json.Marshal(om)
}

// buildReasoning mirrors Pi's reasoning handling.
func buildReasoning(client map[string]any, o BuildOptions) map[string]any {
	if raw, ok := client["reasoning"]; ok {
		switch t := raw.(type) {
		case map[string]any:
			effort := getString(t, "effort")
			out := make(map[string]any, len(t)+1)
			for key, value := range t {
				out[key] = value
			}
			if _, present := t["summary"]; !present && o.DefaultReasoningSummary != "" && effort != "" {
				out["summary"] = o.DefaultReasoningSummary
			}
			if len(out) == 0 {
				return nil
			}
			return out
		case nil:
			// Explicit null means "no reasoning block".
			return nil
		}
	}
	if o.DefaultReasoningEffort == "" {
		return nil
	}
	out := map[string]any{"effort": o.DefaultReasoningEffort}
	if o.DefaultReasoningSummary != "" {
		out["summary"] = o.DefaultReasoningSummary
	}
	return out
}

// DeriveSessionID picks the session identifier used for prompt_cache_key and the
// session-id / x-client-request-id headers.
//
// Preference order: the client's own prompt_cache_key (a legitimate part of the
// Responses API used for server-side caching), then an explicit session header,
// then a generated id. Header *values* may inform this, but client headers are
// never forwarded verbatim.
func DeriveSessionID(clientBody map[string]any, headerHints []string, generate func() string) string {
	if v := getString(clientBody, "prompt_cache_key"); strings.TrimSpace(v) != "" {
		return ClampPromptCacheKey(v)
	}
	for _, h := range headerHints {
		if strings.TrimSpace(h) != "" {
			return ClampPromptCacheKey(strings.TrimSpace(h))
		}
	}
	if generate != nil {
		return ClampPromptCacheKey(generate())
	}
	return ""
}

func getString(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func getNestedString(m map[string]any, outer, inner string) string {
	v, ok := m[outer]
	if !ok {
		return ""
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	return getString(obj, inner)
}

func appendRequestExtensions(body *OrderedMap, client map[string]any) {
	// Preserve extensions and model-specific options, including false, null and
	// nested values. Never restore a field already owned by normalisation.
	var extraKeys []string
	for field := range client {
		switch field {
		case "model", "input", "stream", "store", "prompt_cache_key", "service_tier", "tools", "tool_choice", "reasoning", "include", "previous_response_id", "type":
			continue
		}
		if !isChatGPTRejectedField(field) {
			extraKeys = append(extraKeys, field)
		}
	}
	sort.Strings(extraKeys)
	for _, field := range extraKeys {
		body.Set(field, client[field])
	}
}

func appendRequestOptions(body *OrderedMap, client map[string]any, o BuildOptions) {
	if v, ok := client["service_tier"]; ok && v != nil {
		body.Set("service_tier", v)
	}
	if tools, ok := client["tools"].([]any); ok && len(tools) > 0 {
		body.Set("tools", tools)
	}
	if v, ok := client["tool_choice"]; ok && v != nil {
		body.Set("tool_choice", v)
	}

	reasoning := buildReasoning(client, o)
	if reasoning != nil {
		body.Set("reasoning", reasoning)
	}
	// Pi requests encrypted reasoning whenever reasoning is used; an explicit client
	// include list is preserved.
	if v, ok := client["include"]; ok && v != nil {
		body.Set("include", v)
	} else if reasoning != nil {
		body.Set("include", []any{IncludeReasoningEncrypted})
	}

}
