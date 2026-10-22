package capture

import (
	"encoding/json"
	"math"
	"strings"
	"sync/atomic"
	"testing"

	"pi-gateway/internal/store"
)

func traceFixture() map[string]any {
	return map[string]any{
		"rule_id": "rule-1", "rule_name": "historical name", "revision": 2,
		"priority": 10, "phase": "request", "action_id": "action-1", "action_type": "set_json",
		"action_index": 0, "matched": true, "status": "changed",
		"changes": []any{map[string]any{"path": "/value", "operation": "replace", "before_exists": true, "after_exists": true, "before": nil, "after": "new"}},
	}
}

func decodeTrace(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestRuleTraceIndependentAndDetached(t *testing.T) {
	rec := New(nil, Options{})
	trace := traceFixture()
	rec.OnRuleTrace(11, trace)
	trace["rule_name"] = "renamed"
	trace["changes"].([]any)[0].(map[string]any)["after"] = "later"
	got := rec.Finalize(store.OutcomeOK, "")
	if got.RulesVersion != 11 || len(got.RuleTraces) != 1 || len(got.ResponseFrames) != 0 || got.RequestBytes != 0 || got.RequestBody != "" || got.Status != 0 {
		t.Fatalf("trace fabricated traffic or lost metadata: %+v", got)
	}
	stored := decodeTrace(t, got.RuleTraces[0])
	if stored["rule_name"] != "historical name" || stored["rules_version"] != float64(11) {
		t.Fatalf("historical metadata changed: %+v", stored)
	}
	change := stored["changes"].([]any)[0].(map[string]any)
	if change["before_exists"] != true || change["before"] != nil || change["after"] != "new" {
		t.Fatalf("before/after semantics changed: %+v", change)
	}
	if got.Truncated || got.RulesTraceTruncated {
		t.Fatal("small trace unexpectedly truncated")
	}
}

func TestRuleTraceBoundedWithoutStarvingWire(t *testing.T) {
	for _, maxBytes := range []int64{8 << 20, 8192, 128, 1} {
		rec := New(nil, Options{MaxBytesPerRecord: maxBytes})
		for i := 0; i < 3000; i++ {
			rec.OnRuleTrace(9, traceFixture())
		}
		reportTestFailure(t, rec.traceBytes > int(min(maxBytes/2, maxRuleTraceBytes)) || rec.seenBytes > maxBytes, func() {
			t.Fatalf("trace exceeded reservation for %d: trace=%d seen=%d", maxBytes, rec.traceBytes, rec.seenBytes)
		})
		reportTestFailure(t, len(rec.cap.RuleTraces)+rec.cap.RulesTraceOmitted != 3000, func() {
			t.Fatal("omitted trace count is not exact")
		})
		reportTestFailure(t, rec.truncated, func() {
			t.Fatal("trace-only clipping must not claim wire bytes were clipped")
		})
		reportTestFailure(t, !rec.cap.RulesTraceTruncated, func() {
			t.Fatal("trace overflow not marked")
		})
		available := int(rec.remainingBytesLocked())
		rec.OnRequestBody([]byte(strings.Repeat("x", available)), false)
		reportTestFailure(t, len(rec.cap.RequestBody) != available || rec.seenBytes != maxBytes, func() {
			t.Fatal("wire could not use the remaining record allowance")
		})
		for _, raw := range rec.cap.RuleTraces {
			reportTestFailure(t, !json.Valid(raw), func() {
				t.Fatal("partial JSON was retained")
			})
		}
	}
}

func TestRuleTracePreservesLargeValuesAndRedactsSecrets(t *testing.T) {
	const secret = "secret-never-store-this-credential"
	rec := New(nil, Options{})
	trace := traceFixture()
	trace["changes"] = []any{
		map[string]any{"path": "/authorization", "before": secret, "after": secret},
		map[string]any{"path": "/config", "before": map[string]any{"refresh_token": secret}, "after": []any{map[string]any{"name": "Cookie", "value": secret}}},
		map[string]any{"path": "/text", "before": "old", "after": strings.Repeat("x", 8<<20)},
	}
	rec.OnRuleTrace(1, trace)
	got := rec.Finalize(store.OutcomeOK, "")
	encoded, _ := json.Marshal(got.RuleTraces)
	if strings.Contains(string(encoded), secret) {
		t.Fatal("trace leaked a credential")
	}
	if len(encoded) < 8<<20 || got.RulesTraceTruncated || got.Truncated {
		t.Fatal("large trace snapshot lost content")
	}
	if len(got.RuleTraces) != 1 || decodeTrace(t, got.RuleTraces[0])["rule_id"] != "rule-1" {
		t.Fatal("oversized snapshot displaced lightweight rule metadata")
	}
}

func TestRuleTraceSafeInvalidAndCyclicInputs(t *testing.T) {
	rec := New(nil, Options{})
	for _, value := range []any{nil, true, func() {}, json.RawMessage(`{"bad":`), map[string]any{"bad": math.NaN()}} {
		rec.OnRuleTrace(1, value)
	}
	if rec.cap.RulesTraceOmitted != 5 || len(rec.cap.RuleTraces) != 0 {
		t.Fatal("invalid trace must be omitted and counted")
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	rec.OnRuleTrace(1, cycle)
	if len(rec.cap.RuleTraces) != 1 || len(rec.cap.RuleTraces[0]) > 4096 {
		t.Fatal("cycle was not safely bounded")
	}
	var calls int32
	rec.OnRuleTrace(1, map[string]any{"value": marshalProbe{calls: &calls}})
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("trace invoked an uncontrolled custom marshaler")
	}
}

func TestRuleTraceFrameCapAndVersion(t *testing.T) {
	rec := New(nil, Options{})
	rec.maxFrames = 2
	rec.OnRuleTrace(3, traceFixture())
	rec.OnRuleTrace(3, traceFixture())
	rec.OnRuleTrace(3, traceFixture())
	rec.OnRuleTrace(4, traceFixture())
	rec.OmitRuleTraces(3, 7)
	if len(rec.cap.RuleTraces) != 2 || rec.cap.RulesTraceOmitted != 9 || rec.cap.RulesVersion != 3 || rec.TraceBudget() != 0 {
		t.Fatalf("trace count/version limits failed: %+v", rec.cap)
	}
	rec.OnFrame("in", store.KindSSEEvent, "", []byte("one"), nil)
	rec.OnFrame(DirClientOut, store.KindSSEEvent, "", []byte("two"), nil)
	if len(rec.cap.ResponseFrames) != 2 {
		t.Fatal("trace consumed wire frame slots")
	}
}

func TestRuleEventAssociatesOnlyActualFrames(t *testing.T) {
	rec := New(nil, Options{})
	rec.OnClientRequest(nil, []byte(`{"model":"old"}`))
	rec.OnFrame("in", store.KindSSEEvent, "delta", []byte(`{"delta":"a"}`), nil)
	rec.SetRuleEvent("event-1")
	rec.OnRuleTrace(4, traceFixture())
	rec.OnClientResponseHeaders(200, nil)
	rec.OnFrame(DirClientOut, store.KindSSEEvent, "delta", []byte(`{"delta":"changed"}`), nil)
	// The next event is deliberately dropped. No client frame may be invented.
	rec.OnFrame("in", store.KindSSEEvent, "delta", []byte(`{"delta":"drop"}`), nil)
	rec.SetRuleEvent("event-2")
	rec.OnFrame("in", store.KindSSEEvent, "delta", []byte(`{"delta":"next"}`), nil)
	rec.OnFrame(DirClientOut, store.KindSSEEvent, "delta", []byte(`{"delta":"next"}`), nil)
	got := rec.Finalize(store.OutcomeOK, "")
	if len(got.ResponseFrames) != 7 {
		t.Fatalf("fabricated/missing frame: %+v", got.ResponseFrames)
	}
	for index, expected := range []string{"", "event-1", "", "event-1", "event-2", "", ""} {
		if got.ResponseFrames[index].RuleEventID != expected {
			t.Fatalf("frame %d ID=%q want %q", index, got.ResponseFrames[index].RuleEventID, expected)
		}
	}
	if trace := decodeTrace(t, got.RuleTraces[0]); trace["event_id"] != "event-1" || trace["upstream_seq"] != float64(1) {
		t.Fatal("trace association or original upstream sequence missing")
	}
	if got.RequestBytes != 0 || got.RequestBody != "" {
		t.Fatal("client input impersonated actual upstream traffic")
	}
}

func TestRuleEventTruncatedFramesCannotRelabelPreviousEvent(t *testing.T) {
	rec := New(nil, Options{})
	rec.maxFrames = 1
	rec.OnFrame("in", store.KindWSFrame, "delta", []byte(`{"delta":1}`), nil)
	rec.SetRuleEvent("first")
	rec.OnFrame("in", store.KindWSFrame, "delta", []byte(`{"delta":2}`), nil)
	rec.SetRuleEvent("second")
	if rec.cap.ResponseFrames[0].RuleEventID != "first" {
		t.Fatal("missing frame reassigned a previously captured event")
	}
}

func TestRuleTraceInheritsEngineTruncation(t *testing.T) {
	for _, trace := range []any{
		map[string]any{"rule_id": "r", "omitted_changes": 2},
		map[string]any{"rule_id": "r", "changes": []any{map[string]any{"truncated": true}}},
		map[string]any{"rule_id": "r", "trace_truncated": true},
	} {
		rec := New(nil, Options{})
		rec.OnRuleTrace(7, trace)
		got := rec.Finalize(store.OutcomeOK, "")
		if !got.RulesTraceTruncated || got.Truncated || got.RulesTraceOmitted != 0 || len(got.RuleTraces) != 1 {
			t.Fatalf("engine snapshot truncation was lost: %+v", got)
		}
	}
}

func TestRuleTraceOmissionsAndJSONNumbers(t *testing.T) {
	rec := New(nil, Options{})
	rec.OmitRuleTraces(12, 0)
	if rec.cap.RulesVersion != 12 || rec.cap.RulesTraceTruncated {
		t.Fatal("zero omissions should record version without inventing truncation")
	}
	rec.OnRuleTrace(12, map[string]any{"rule_id": "number", "value": json.Number("42")})
	if decodeTrace(t, rec.cap.RuleTraces[0])["value"] != float64(42) {
		t.Fatal("JSON number became a string")
	}
	rec.OmitRuleTraces(12, 4)
	rec.OmitRuleTraces(12, -9)
	if rec.cap.RulesTraceOmitted != 4 || len(rec.cap.ResponseFrames) != 0 {
		t.Fatal("omission count fabricated a frame or accepted negative counts")
	}
	rec.OmitRuleTraces(12, int(^uint(0)>>1))
	rec.OnRuleTrace(13, nil)
	if rec.cap.RulesTraceOmitted < 0 {
		t.Fatal("omission count overflowed")
	}
}

func TestRuleEventAssociationBudget(t *testing.T) {
	rec := New(nil, Options{MaxBytesPerRecord: 128})
	rec.OnFrame("in", store.KindWSFrame, "", []byte("x"), nil)
	rec.SetRuleEvent(strings.Repeat("e", 65)) // More than the trace allowance.
	if rec.cap.ResponseFrames[0].RuleEventID != "" || !rec.cap.RulesTraceTruncated {
		t.Fatal("association exceeded trace budget")
	}
	if rec.seenBytes != 1 {
		t.Fatal("rejected association charged nonexistent bytes")
	}
	rec.SetRuleEvent("ok")
	before := rec.seenBytes
	rec.SetRuleEvent("ok")
	if rec.seenBytes != before {
		t.Fatal("same event scope was charged twice")
	}
	rec.SetRuleEvent(strings.Repeat("x", 257))
	if rec.ruleEventID != "" || rec.cap.ResponseFrames[0].RuleEventID != "ok" {
		t.Fatal("oversized ID was cropped or relabeled a real frame")
	}
}

func TestSetRouteDoesNotFabricateTraffic(t *testing.T) {
	rec := New(nil, Options{})
	rec.SetRoute(12, "account", "model", "session", "websocket")
	got := rec.Finalize(store.OutcomeError, "blocked before upstream")
	if got.AccountID != 12 || got.Model != "model" || got.SessionID != "session" || got.UpstreamTransport != "websocket" {
		t.Fatalf("route metadata not completed: %+v", got)
	}
	if got.Status != 0 || got.RequestBody != "" || got.RequestBytes != 0 || len(got.ResponseFrames) != 0 {
		t.Fatal("routing metadata created fictitious wire traffic")
	}
}
