package capture

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"

	"pi-gateway/internal/store"
)

const (
	maxRuleTraceBytes  = 64 << 20
	maxRuleTraces      = 2048
	maxTraceValueBytes = 64 << 20
	traceOmittedValue  = "[omitted]"
)

const redactedValue = "[redacted]"

// SetRoute completes an early recorder only after account selection succeeds.
// It never fabricates upstream traffic, a response status, or request byte counts.
func (r *Recorder) SetRoute(accountID int64, accountName, model, sessionID, transport string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cap.AccountID, r.cap.AccountName = accountID, accountName
	r.cap.Model, r.cap.SessionID, r.cap.UpstreamTransport = model, sessionID, transport
}

func rulePayloadKind(kind string) bool {
	switch kind {
	case store.KindSSEEvent, store.KindWSFrame, store.KindHTTPResponse, "http_error_body":
		return true
	}
	return false
}

// SetRuleEvent annotates the most recently observed upstream payload and actual
// subsequent client payloads. Call after upstream observation and before delivery;
// a dropped event produces no fabricated client frame. Empty IDs clear the scope.
func (r *Recorder) SetRuleEvent(eventID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ruleEventID = ""
	if eventID == "" {
		return
	}
	// Never crop identifiers: a prefix collision would invent an association.
	if len(eventID) > 256 {
		r.cap.RulesTraceTruncated = true
		return
	}
	r.ruleEventID = strings.Clone(eventID)
	if i := r.latestInbound; i >= 0 && i < len(r.cap.ResponseFrames) {
		frame := &r.cap.ResponseFrames[i]
		if frame.RuleEventID == "" {
			if r.traceRemainingLocked() < len(eventID) {
				r.cap.RulesTraceTruncated = true
				return
			}
			frame.RuleEventID = r.ruleEventID
			r.chargeTraceLocked(len(eventID))
		} else if frame.RuleEventID != eventID {
			// A previously associated upstream event cannot become a different one.
			r.ruleEventID = ""
			r.cap.RulesTraceTruncated = true
		}
	}
}

// TraceBudget is the currently available byte budget, not an execution limit.
// Reserve half the record for wire payloads; trace values use the remaining budget.
func (r *Recorder) TraceBudget() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cap.RuleTraces) >= r.traceCountLimitLocked() {
		return 0
	}
	return r.traceRemainingLocked()
}

func (r *Recorder) traceCountLimitLocked() int {
	if r.maxFrames > 0 {
		return min(maxRuleTraces, r.maxFrames)
	}
	return maxRuleTraces
}

func (r *Recorder) traceLimitLocked() int {
	return int(min(int64(maxRuleTraceBytes), r.maxBytes/2))
}

func (r *Recorder) traceRemainingLocked() int {
	return max(0, min(r.traceLimitLocked()-r.traceBytes, int(r.remainingBytesLocked())))
}

func (r *Recorder) chargeTraceLocked(n int) {
	r.traceBytes += n
	r.seenBytes += int64(n)
}

// OnRuleTrace retains a detached, bounded JSON object independently of wire frames.
// Reflection bounds traversal before JSON encoding, so malicious/oversized values
// and custom MarshalJSON implementations cannot allocate unbounded trace buffers.
// Failures affect tracing only, never rule execution or the recorded wire outcome.
func (r *Recorder) OnRuleTrace(version int64, trace any) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cap.RulesVersion == 0 {
		r.cap.RulesVersion = version
	}
	if r.cap.RulesVersion != version || r.traceRemainingLocked() < 64 || len(r.cap.RuleTraces) >= r.traceCountLimitLocked() {
		r.omitTraceLocked()
		return
	}
	budget := traceValueBudget{nodes: max(1024, r.traceRemainingLocked()), bytes: r.traceRemainingLocked()}
	value := budget.copy(reflect.ValueOf(trace), 0)
	object, ok := value.(map[string]any)
	if !ok || budget.invalid {
		r.omitTraceLocked()
		return
	}
	r.decorateTraceLocked(object, version, budget.truncated)
	encoded, err := json.Marshal(object)
	if err != nil {
		r.omitTraceLocked()
		return
	}
	// Preserve a final quarter of the trace allowance for lightweight metadata.
	if len(encoded) > r.traceRemainingLocked() || r.traceBytes+len(encoded) > r.traceLimitLocked()*3/4 {
		if changes, ok := object["changes"].([]any); ok && len(changes) > 0 {
			previous := traceOmittedCount(object["omitted_changes"])
			object["omitted_changes"] = previous + int64(len(changes))
			delete(object, "changes")
		}
		object["trace_truncated"] = true
		r.cap.RulesTraceTruncated = true
		encoded, err = json.Marshal(object)
	}
	if err != nil || len(encoded)+1 > r.traceRemainingLocked() {
		r.omitTraceLocked()
		return
	}
	r.chargeTraceLocked(len(encoded) + 1)
	r.cap.RuleTraces = append(r.cap.RuleTraces, json.RawMessage(encoded))
}

// OmitRuleTraces accounts for execution-side omissions without retaining payloads
// or inserting fake wire frames. A zero count still records the ruleset version.
func (r *Recorder) OmitRuleTraces(version int64, count int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cap.RulesVersion == 0 {
		r.cap.RulesVersion = version
	}
	if count > 0 {
		r.cap.RulesTraceTruncated = true
		maxInt := int(^uint(0) >> 1)
		r.cap.RulesTraceOmitted += min(count, maxInt-r.cap.RulesTraceOmitted)
	}
}

func traceOmittedCount(value any) int64 {
	switch n := value.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		if n > 0 {
			return int64(n)
		}
	case json.Number:
		if parsed, err := n.Int64(); err == nil {
			return parsed
		}
	}
	return 0
}

type traceValueBudget struct {
	nodes, bytes       int
	truncated, invalid bool
}

func (b *traceValueBudget) copy(v reflect.Value, depth int) any {
	if !v.IsValid() {
		return nil
	}
	if b.nodes <= 0 || b.bytes <= 0 || depth > 256 {
		b.truncated = true
		return traceOmittedValue
	}
	b.nodes--
	if v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		return b.copy(v.Elem(), depth+1)
	}
	if v.Type() == reflect.TypeOf(json.Number("")) {
		return b.copyNumber(v, depth)
	}
	if v.Type() == reflect.TypeOf(json.RawMessage{}) {
		return b.copyRawJSON(v, depth)
	}
	switch v.Kind() {
	case reflect.Bool:
		return v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint()
	case reflect.Float32, reflect.Float64:
		return b.copyFloat(v)
	case reflect.String:
		s := v.String()
		limit := min(b.bytes, maxTraceValueBytes)
		if len(s) > limit {
			b.truncated = true
			s = s[:limit]
		}
		b.bytes -= len(s)
		return strings.Clone(s)
	case reflect.Slice, reflect.Array:
		return b.copySequence(v, depth)
	case reflect.Map:
		return b.copyMap(v, depth)
	case reflect.Struct:
		return b.copyStruct(v, depth)
	default:
		b.invalid = true
		return nil
	}
}

func traceSecretKey(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	switch key {
	case "authorization", "proxy_authorization", "api_key", "x_api_key", "openai_api_key", "cookie", "set_cookie", "access_token", "refresh_token", "id_token", "password", "secret", "client_secret":
		return true
	}
	return false
}

// Changes can carry secret fields as JSON Pointer paths instead of map keys.
// Header arrays similarly use a name/value pair rather than the header as a key.
func redactTraceObject(value any) {
	switch v := value.(type) {
	case map[string]any:
		if name, ok := v["name"].(string); ok && traceSecretKey(name) {
			v["value"] = redactedValue
		}
		redactTracePath(v)
		for _, child := range v {
			redactTraceObject(child)
		}
	case []any:
		for _, child := range v {
			redactTraceObject(child)
		}
	}
}

func (b *traceValueBudget) copySequence(v reflect.Value, depth int) any {
	if v.Kind() == reflect.Slice && v.IsNil() {
		return nil
	}
	out := make([]any, 0, min(v.Len(), 128))
	for i := 0; i < v.Len(); i++ {
		if i >= 128 || b.nodes <= 0 || b.bytes <= 0 {
			b.truncated = true
			break
		}
		out = append(out, b.copy(v.Index(i), depth+1))
	}
	return out
}

func (b *traceValueBudget) copyMap(v reflect.Value, depth int) any {
	if v.Type().Key().Kind() != reflect.String {
		b.invalid = true
		return nil
	}
	out := map[string]any{}
	b.copyPriorityFields(v, depth, out)
	iter := v.MapRange()
	for iter.Next() {
		b.nodes--
		if b.nodes <= 0 || b.bytes <= 0 || len(out) >= 128 {
			b.truncated = true
			break
		}
		key := iter.Key().String()
		if _, copied := out[key]; copied {
			continue
		}
		if len(key) > 256 {
			b.truncated = true
			continue
		}
		b.bytes -= len(key)
		if traceSecretKey(key) {
			out[key] = redactedValue
		} else {
			out[key] = b.copy(iter.Value(), depth+1)
		}
	}
	return out
}

func (b *traceValueBudget) copyStruct(v reflect.Value, depth int) any {
	out := map[string]any{}
	for i := 0; i < v.NumField(); i++ {
		if i >= 128 || b.nodes <= 0 || b.bytes <= 0 {
			b.truncated = true
			break
		}
		b.nodes--
		key, include := traceStructField(v, i)
		if !include {
			continue
		}
		if traceSecretKey(key) {
			out[key] = redactedValue
		} else {
			out[key] = b.copy(v.Field(i), depth+1)
		}
	}
	return out
}

func (b *traceValueBudget) copyNumber(v reflect.Value, depth int) any {
	n := json.Number(v.String())
	if len(n) > min(b.bytes, maxTraceValueBytes) {
		b.truncated = true
		return traceOmittedValue
	}
	if !json.Valid([]byte(n)) {
		b.invalid = true
		return nil
	}
	b.bytes -= len(n)
	return json.Number(strings.Clone(string(n)))

}

func (b *traceValueBudget) copyRawJSON(v reflect.Value, depth int) any {
	if v.Len() > min(b.bytes, 16<<10) {
		b.truncated = true
		return traceOmittedValue
	}
	var value any
	if err := json.Unmarshal(v.Bytes(), &value); err != nil {
		b.invalid = true
		return nil
	}
	return b.copy(reflect.ValueOf(value), depth+1)

}

func (b *traceValueBudget) copyPriorityFields(v reflect.Value, depth int, out map[string]any) {
	if depth <= 2 {
		// Gateway traces arrive as maps. Preserve provenance before potentially
		// large snapshots, independent of Go's randomized map iteration order.
		for _, key := range []string{"rule_id", "rule_name", "revision", "priority", "phase", "action_id", "action_type", "action_index", "matched", "status", "rolled_back", "error", "duration_ns", "event_id", "event_type", "sequence", "source_kind", "source", "rules_version", "trace_truncated", "omitted_changes"} {
			field := v.MapIndex(reflect.ValueOf(key).Convert(v.Type().Key()))
			if field.IsValid() {
				out[key] = b.copy(field, depth+1)
			}
		}
	}
}

func redactTracePath(v map[string]any) {
	if path, ok := v["path"].(string); ok {
		for _, part := range strings.Split(path, "/") {
			if traceSecretKey(strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")) {
				v["before"], v["after"] = redactedValue, redactedValue
				break
			}
		}
	}
}

func (r *Recorder) decorateTraceLocked(object map[string]any, version int64, truncated bool) {
	object["rules_version"] = version
	if r.ruleEventID != "" && object["event_id"] == nil {
		object["event_id"] = r.ruleEventID
	}
	if i := r.latestInbound; i >= 0 && i < len(r.cap.ResponseFrames) && object["event_id"] == r.cap.ResponseFrames[i].RuleEventID && r.cap.ResponseFrames[i].RuleEventID != "" {
		object["upstream_seq"] = r.cap.ResponseFrames[i].Seq
	}
	redactTraceObject(object)
	// The engine may already have bounded individual snapshots or change lists.
	// Surface those limits even if the recorder itself did not need to clip.
	if object["trace_truncated"] == true || traceOmittedCount(object["omitted_changes"]) > 0 {
		r.cap.RulesTraceTruncated = true
	}
	if traceChangesTruncated(object) {
		r.cap.RulesTraceTruncated = true
	}
	if truncated {
		r.markTraceTruncatedLocked(object)
	}
}

func traceChangesTruncated(object map[string]any) bool {
	if changes, ok := object["changes"].([]any); ok {
		for _, change := range changes {
			if c, ok := change.(map[string]any); ok && c["truncated"] == true {
				return true
			}
		}
	}
	return false
}

func traceStructField(v reflect.Value, i int) (string, bool) {
	field := v.Type().Field(i)
	if field.PkgPath != "" {
		return "", false
	}
	tag := strings.Split(field.Tag.Get("json"), ",")
	key := tag[0]
	if key == "-" {
		return "", false
	}
	if key == "" {
		key = field.Name
	}
	if len(tag) > 1 && tag[1] == "omitempty" && v.Field(i).IsZero() {
		return "", false
	}
	return key, true
}

func (b *traceValueBudget) copyFloat(v reflect.Value) any {
	if math.IsNaN(v.Float()) || math.IsInf(v.Float(), 0) {
		b.invalid = true
		return nil
	}
	return v.Float()
}

func (r *Recorder) omitTraceLocked() {
	r.cap.RulesTraceTruncated = true
	if r.cap.RulesTraceOmitted < int(^uint(0)>>1) {
		r.cap.RulesTraceOmitted++
	}
}

func (r *Recorder) markTraceTruncatedLocked(object map[string]any) {
	object["trace_truncated"] = true
	r.cap.RulesTraceTruncated = true
	if changes, ok := object["changes"].([]any); ok {
		for _, change := range changes {
			if c, ok := change.(map[string]any); ok {
				c["truncated"] = true
			}
		}
	}
}
