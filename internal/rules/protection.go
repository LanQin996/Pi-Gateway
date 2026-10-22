package rules

import (
	"fmt"
	"strings"
)

func protectedKey(k, phase string) bool {
	switch strings.ToLower(k) {
	case "authorization", "proxy-authorization", "api_key", "access_token", "refresh_token", "cookie", "set-cookie":
		return true
	}
	if phase != PhaseResponseEvent && phase != PhaseResponseBody {
		return false
	}
	switch k {
	case "id", "type", "usage", "response_id", "item_id", "call_id", "tool_call_id", "output_index", "content_index", "summary_index", "sequence_number", "previous_response_id":
		return true
	}
	return false
}
func protectedPath(path, phase string) bool {
	parts, e := pointerSegments(path)
	if e != nil {
		return false
	}
	for _, p := range parts {
		if protectedKey(p, phase) {
			return true
		}
	}
	if (phase == PhaseResponseEvent || phase == PhaseResponseBody) && len(parts) > 0 {
		if parts[0] == "status" || parts[0] == "error" || parts[0] == "incomplete_details" {
			return true
		}
		if parts[0] == "response" && len(parts) > 1 && contains([]string{"status", "error", "incomplete_details"}, parts[1]) {
			return true
		}
	}
	return false
}
func validateProtectedAction(a Action, phase, path string) error {
	p := a.Params
	check := func(value any, field string) error {
		s, ok := value.(string)
		if ok && protectedPath(s, phase) {
			return invalid(path+"/params/"+field, "cannot modify protected protocol, usage or credential field")
		}
		return nil
	}
	switch a.Type {
	case "json_set", "json_merge", "array_insert", "array_filter", "text_replace":
		if e := check(p["path"], "path"); e != nil {
			return e
		}
	case "json_transfer":
		if e := check(p["target_path"], "target_path"); e != nil {
			return e
		}
		if p["operation"] == "move" {
			if e := check(p["source_path"], "source_path"); e != nil {
				return e
			}
		}
	case "json_remove":
		for i, v := range p["paths"].([]any) {
			if e := check(v, fmt.Sprintf("paths/%d", i)); e != nil {
				return e
			}
		}
	}
	// Known literal assignments can also be rejected before execution. Ancestor replacements
	// still require runtime projection comparison because references depend on actual input.
	if (phase == PhaseResponseEvent || phase == PhaseResponseBody) && (a.Type == "json_set" || a.Type == "json_merge") && !isReference(p["value"]) {
		if containsProtectedLiteral(expressionLiteral(p["value"]), phase) {
			return invalid(path+"/params/value", "literal value contains protected response fields")
		}
	}
	return nil
}
func containsProtectedLiteral(v any, phase string) bool {
	if m, ok := object(v); ok {
		for k, x := range m {
			if protectedKey(k, phase) || containsProtectedLiteral(x, phase) {
				return true
			}
		}
	}
	if a, ok := v.([]any); ok {
		for _, x := range a {
			if containsProtectedLiteral(x, phase) {
				return true
			}
		}
	}
	return false
}
func protectedProjection(v any, phase string) map[string]any {
	out := map[string]any{}
	collectProtectedProjection(v, "", phase, out)
	return out
}
func collectProtectedProjection(node any, path, phase string, out map[string]any) {
	if m, ok := object(node); ok {
		collectProtectedObject(m, path, phase, out)
		return
	}
	if arr, ok := node.([]any); ok {
		for i, x := range arr {
			collectProtectedProjection(x, fmt.Sprintf("%s/%d", path, i), phase, out)
		}
	}
}
func collectProtectedObject(m map[string]any, path, phase string, out map[string]any) {
	for k, x := range m {
		p := joinPointer(path, k)
		if protectedKey(k, phase) || protectedPath(p, phase) || protectedToolName(m, k, phase) {
			out[p] = x
			continue
		}
		collectProtectedProjection(x, p, phase, out)
	}
}
func protectedToolName(m map[string]any, key, phase string) bool {
	if (phase != PhaseResponseEvent && phase != PhaseResponseBody) || key != "name" {
		return false
	}
	typ, _ := m["type"].(string)
	return strings.Contains(typ, "call") || strings.Contains(typ, "tool")
}
func checkProtected(before, after any, phase string) error {
	a := protectedProjection(before, phase)
	b := protectedProjection(after, phase)
	for _, k := range keys(a) {
		v := a[k]
		w, ok := b[k]
		if !ok || !jsonEqual(v, w) {
			return fmt.Errorf("protected field %s was changed or removed", k)
		}
	}
	for _, k := range keys(b) {
		if _, ok := a[k]; !ok {
			return fmt.Errorf("protected field %s was added", k)
		}
	}
	return nil
}
func droppableEvent(kind string, body any) bool {
	if !contains([]string{"response.output_text.delta", "response.refusal.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta"}, kind) {
		return false
	}
	if m, ok := object(body); ok {
		for _, k := range []string{"usage", "response", "error", "call_id", "tool_call_id", "function_call", "tool_calls"} {
			if _, present := m[k]; present {
				return false
			}
		}
	}
	return true
}
