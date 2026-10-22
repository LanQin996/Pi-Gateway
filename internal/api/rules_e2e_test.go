package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"pi-gateway/internal/capture"
	"pi-gateway/internal/rules"
	"pi-gateway/internal/rulesruntime"
	"pi-gateway/internal/store"
	"pi-gateway/internal/upstream"
)

const unifiedRulesTimeout = 5 * time.Second

// Cold rule compilation and SQLite setup under -race need their own budget;
// stream I/O keeps the short timeout above.
const unifiedRulesSetupTimeout = 30 * time.Second

// Both fake transports deliver exactly the same protocol identities and usage.
// Deliberately retain noncanonical spacing to catch needless Raw re-encoding.
var unifiedRulesEvents = []string{
	`{ "type":"response.created", "sequence_number":1, "response":{"id":"resp_rules_1","status":"in_progress"} }`,
	`{ "type":"response.output_text.delta", "sequence_number":2, "response_id":"resp_rules_1", "item_id":"msg_rules_1", "output_index":0, "content_index":0, "delta":"hello" }`,
	`{ "type":"response.completed", "sequence_number":3, "response":{"id":"resp_rules_1","status":"completed","output":[{"id":"msg_rules_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":3}}} }`,
}

func unifiedRule(id, phase string, priority int, actions ...rules.Action) rules.Rule {
	return rules.Rule{SchemaVersion: rules.SchemaVersion, ID: id, Name: id, Enabled: true,
		Priority: priority, Phase: phase, When: rules.Condition{Op: "always"},
		Actions: actions, OnError: rules.OnErrorAbort}
}

func unifiedAction(id, kind string, params map[string]any) rules.Action {
	if params == nil {
		params = map[string]any{}
	}
	return rules.Action{ID: id, Type: kind, Params: params}
}

func unifiedSet(id, path string, value any) rules.Action {
	return unifiedAction(id, "json_set", map[string]any{"path": path, "value": value, "create_parents": true})
}

func unifiedReplace(id, path, from, to string) rules.Action {
	return unifiedAction(id, "text_replace", map[string]any{"path": path, "pattern": from, "replacement": to, "on_missing": "error"})
}

func unifiedEventRule(id, kind string, actions ...rules.Action) rules.Rule {
	rule := unifiedRule(id, rules.PhaseResponseEvent, 10, actions...)
	rule.When = rules.Condition{Op: "eq", Source: "context", Path: "/event_type", Value: kind}
	return rule
}

// Use the public initialization/publication boundary, not direct SQL fixtures or
// an injected Engine. Subsequent calls update existing IDs with their revisions.
func publishUnifiedRules(t *testing.T, h *testHarness, definitions ...rules.Rule) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), unifiedRulesSetupTimeout)
	defer cancel()
	{
		err := rulesruntime.New(h.store).Ensure(ctx)
		reportTestFailure(t, err != nil, func() {
			t.Fatalf("initialize rules: %v", err)
		})
	}
	snapshot, err := h.store.LoadRuleSet(ctx)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	wanted := make(map[string]bool, len(definitions))
	existing := make(map[string]*store.RuleRow, len(snapshot.Rules))
	for _, definition := range definitions {
		wanted[definition.ID] = true
	}
	var changes []store.RuleChange
	for _, row := range snapshot.Rules {
		existing[row.ID] = row
		if !wanted[row.ID] && row.Source != "protocol" && row.Source != "system" {
			changes = append(changes, store.RuleChange{Kind: "delete", ID: row.ID, ExpectedRevision: row.Revision})
		}
	}
	for _, definition := range definitions {
		raw, err := rulesruntime.EditableJSON(definition)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
		change := store.RuleChange{Kind: "create", ID: definition.ID, Rule: raw, Source: "user"}
		if row := existing[definition.ID]; row != nil {
			change.Kind, change.ExpectedRevision = "update", row.Revision
		}
		changes = append(changes, change)
	}
	published, err := h.store.PublishRules(ctx, &snapshot.Version, changes, rulesruntime.ValidateSnapshot)
	reportTestFailure(t, err != nil, func() {
		t.Fatalf("publish rules: %v", err)
	})
	return published.Version
}

func unifiedHarness(t *testing.T, transport string, events []string) (*testHarness, *upstreamCapture, *atomic.Int32) {
	t.Helper()
	observed := &upstreamCapture{}
	calls := &atomic.Int32{}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	configuredTransport := transport
	if transport == "auto-fallback-sse" {
		configuredTransport = "auto"
	}
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveUnifiedHarnessRequest(w, r, serveUnifiedHarnessRequestState{capturedT: &t, capturedTransport: &transport, capturedEvents: &events, capturedObserved: &observed, capturedCalls: &calls, capturedUpgrader: &upgrader})
	}), configuredTransport)
	h.server.Client().Timeout = unifiedRulesTimeout
	t.Cleanup(h.dataPlane.Close)
	return h, observed, calls
}

func unifiedObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var value map[string]any
	{
		err := json.Unmarshal(raw, &value)
		reportTestFailure(t, err != nil || value == nil, func() {
			t.Fatalf("expected JSON object: %s (%v)", raw, err)
		})
	}
	return value
}

func unifiedSSEEvents(t *testing.T, raw []byte) [][]byte {
	t.Helper()
	reader := upstream.ParseSSE(bytes.NewReader(raw))
	var events [][]byte
	for {
		event, err := reader.Next()
		if err == io.EOF {
			return events
		}
		reportTestFailure(t, err != nil, func() {
			t.Fatalf("parse client SSE: %v", err)
		})
		events = append(events, append([]byte(nil), event.Raw...))
	}
}

func unifiedDialWS(t *testing.T, h *testHarness) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), unifiedRulesTimeout)
	defer cancel()
	dialer := websocket.Dialer{HandshakeTimeout: unifiedRulesTimeout}
	conn, response, err := dialer.DialContext(ctx, "ws"+strings.TrimPrefix(h.server.URL, "http")+"/v1/responses", http.Header{"Authorization": {"Bearer " + h.key}})
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("downstream WS handshake: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(unifiedRulesTimeout))
	_ = conn.SetWriteDeadline(time.Now().Add(unifiedRulesTimeout))
	return conn
}

func unifiedWSResponse(t *testing.T, h *testHarness, raw string) ([][]byte, *store.Capture) {
	t.Helper()
	conn := unifiedDialWS(t, h)
	{
		err := conn.WriteMessage(websocket.TextMessage, []byte(raw))
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	var events [][]byte
	for {
		_, payload, err := conn.ReadMessage()
		reportTestFailure(t, err != nil, func() {
			t.Fatalf("read downstream WS event: %v", err)
		})
		events = append(events, payload)
		kind, _ := unifiedObject(t, payload)["type"].(string)
		if kind == "error" || kind == "response.completed" || kind == "response.failed" {
			break
		}
	}
	// Waiting for persisted finish before closing catches duplicated capture
	// frames and avoids turning a successful exchange into a cancelled request.
	record := waitForResponseCapture(t, h)
	_ = conn.Close()
	return events, record
}

func unifiedStreamResponse(t *testing.T, h *testHarness, downstream string) ([][]byte, *store.Capture) {
	t.Helper()
	if downstream == "ws" {
		return unifiedWSResponse(t, h, `{"type":"response.create","model":"gpt-5.5","input":"hello"}`)
	}
	response, body, record := capturedHTTPResponse(t, h, `{"model":"gpt-5.5","input":"hello","stream":true}`)
	reportTestFailure(t, response.StatusCode != http.StatusOK, func() {
		t.Fatalf("stream status=%d body=%s", response.StatusCode, body)
	})
	events := unifiedSSEEvents(t, body)
	assertCapturedSSEBody(t, record, body, len(events))
	return events, record
}

func unifiedTraces(t *testing.T, record *store.Capture, ruleID string) []map[string]any {
	t.Helper()
	var traces []map[string]any
	for _, raw := range record.RuleTraces {
		trace := unifiedObject(t, raw)
		if trace["rule_id"] == ruleID {
			traces = append(traces, trace)
		}
	}
	return traces
}

func unifiedAssertUsage(t *testing.T, h *testHarness, record *store.Capture) {
	t.Helper()
	reportTestFailure(t, record.ResponseID != "resp_rules_1" || record.PromptTokens != 11 || record.CompletionTokens != 7 || record.TotalTokens != 18, func() {
		t.Fatalf("raw capture identity/usage changed: id=%q tokens=%d/%d/%d", record.ResponseID, record.PromptTokens, record.CompletionTokens, record.TotalTokens)
	})
	usage := waitForUsageRecord(t, h.store)
	reportTestFailure(t, usage.Outcome != "succeeded" || usage.AccountID != h.accountID || usage.Model != "gpt-5.5", func() {
		t.Fatalf("usage attribution/outcome changed: %+v", usage)
	})
	for _, metric := range []struct {
		name string
		got  *int64
		want int64
	}{{"input", usage.InputTokens, 11}, {"output", usage.OutputTokens, 7}, {"total", usage.TotalTokens, 18}, {"cached", usage.CachedTokens, 4}, {"reasoning", usage.ReasoningTokens, 3}} {
		reportTestFailure(t, metric.got == nil || *metric.got != metric.want, func() {
			t.Errorf("%s usage=%v want=%d", metric.name, metric.got, metric.want)
		})
	}
}

func unifiedAssertRawFrames(t *testing.T, record *store.Capture, transport string, expected []string) []store.Frame {
	t.Helper()
	kind := store.KindSSEEvent
	if transport != "sse" {
		kind = store.KindWSFrame
	}
	frames := captureFrames(record, "in", kind)
	reportTestFailure(t, len(frames) != len(expected), func() {
		t.Fatalf("raw upstream frames=%d want=%d", len(frames), len(expected))
	})
	for i, frame := range frames {
		reportTestFailure(t, !equalCaptureJSON(captureFrameBytes(t, frame), []byte(expected[i])), func() {
			t.Errorf("upstream frame %d changed: %s want=%s", i, captureFrameBytes(t, frame), expected[i])
		})
	}
	return frames
}

func unifiedAssertClientFrames(t *testing.T, record *store.Capture, downstream string, events [][]byte) []store.Frame {
	t.Helper()
	kind := store.KindSSEEvent
	if downstream == "ws" {
		kind = store.KindWSFrame
	}
	frames := captureFrames(record, capture.DirClientOut, kind)
	reportTestFailure(t, len(frames) != len(events), func() {
		t.Fatalf("client capture frames=%d actual=%d (duplicate or missing delivery)", len(frames), len(events))
	})
	for i, frame := range frames {
		reportTestFailure(t, !equalCaptureJSON(captureFrameBytes(t, frame), events[i]), func() {
			t.Errorf("client frame %d disagrees with delivered bytes: %s != %s", i, captureFrameBytes(t, frame), events[i])
		})
	}
	return frames
}

func TestUnifiedRulesRequestOrderingAndMultipleActions(t *testing.T) {
	h, observed, calls := unifiedHarness(t, "sse", unifiedRulesEvents)
	first := unifiedRule("first", rules.PhaseRequest, 10,
		unifiedAction("model", "rewrite_model", map[string]any{"model": "gpt-5.5"}),
		unifiedSet("instructions", "/text/order", "seed"),
		unifiedSet("copy-client", "/text/copied", map[string]any{"$ref": map[string]any{"source": "client", "path": "/marker"}}),
		unifiedSet("literal", "/text/literal", map[string]any{"$literal": map[string]any{"$ref": "not-an-expression"}}),
		unifiedAction("reasoning", "set_reasoning", map[string]any{"effort": "high", "summary": "concise"}))
	first.When = rules.Condition{Op: "all", Conditions: []rules.Condition{
		{Op: "eq", Source: "context", Path: "/original_model", Value: "original-model"},
		{Op: "eq", Source: "client", Path: "/marker", Value: map[string]any{"$literal": map[string]any{"nested": []any{true, nil, 7}}}},
	}}
	second := unifiedRule("second", rules.PhaseRequest, 20, unifiedReplace("replace", "/text/order", "seed", "second"))
	second.When = rules.Condition{Op: "eq", Source: "current", Path: "/model", Value: map[string]any{"$ref": map[string]any{"source": "context", "path": "/model"}}}
	third := unifiedRule("third", rules.PhaseRequest, 30, unifiedReplace("replace", "/text/order", "second", "final"))
	// Publish in reverse execution order: insertion/name order must not win.
	version := publishUnifiedRules(t, h, third, second, first)
	request := `{"model":"original-model","input":"hello","stream":false,"marker":{"nested":[true,null,7]}}`
	response, body, record := capturedHTTPResponse(t, h, request)
	reportTestFailure(t, response.StatusCode != http.StatusOK || calls.Load() != 1, func() {
		t.Fatalf("status=%d calls=%d body=%s", response.StatusCode, calls.Load(), body)
	})
	_, sent := observed.get()
	payload := unifiedObject(t, sent)
	metadata, _ := payload["text"].(map[string]any)
	reportTestFailure(t, payload["model"] != "gpt-5.5" || metadata["order"] != "final", func() {
		t.Fatalf("priority/multiple actions did not reach upstream: %s", sent)
	})
	reportTestFailure(t, !reflect.DeepEqual(metadata["copied"], map[string]any{"nested": []any{true, nil, float64(7)}}) || !reflect.DeepEqual(metadata["literal"], map[string]any{"$ref": "not-an-expression"}), func() {
		t.Fatalf("native JSON/$ref/$literal changed: %#v", metadata)
	})
	reportTestFailure(t, !reflect.DeepEqual(payload["reasoning"], map[string]any{"effort": "high", "summary": "concise"}), func() {
		t.Fatalf("mixed action missing: %#v", payload["reasoning"])
	})
	reportTestFailure(t, record.RulesVersion != version || record.Model != "gpt-5.5", func() {
		t.Fatalf("capture version/model=%d/%q", record.RulesVersion, record.Model)
	})
	var execution []string
	for _, raw := range record.RuleTraces {
		trace := unifiedObject(t, raw)
		if trace["phase"] == rules.PhaseRequest && trace["action_id"] != nil && len(strings.Split(fmt.Sprint(trace["action_path"]), "/")) <= 4 && trace["source_kind"] != "gateway" {
			execution = append(execution, fmt.Sprint(trace["rule_id"], "/", trace["action_id"]))
		}
	}
	wantOrder := []string{"first/model", "first/instructions", "first/copy-client", "first/literal", "first/reasoning", "second/replace", "third/replace"}
	reportTestFailure(t, !reflect.DeepEqual(execution, wantOrder), func() {
		t.Fatalf("actual execution=%v want=%v", execution, wantOrder)
	})
	unifiedAssertUsage(t, h, record)
}

func TestUnifiedRulesDisabledAndStopAfterMatch(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprintf("stop=%t", stop), func(t *testing.T) {
			h, observed, _ := unifiedHarness(t, "sse", unifiedRulesEvents)
			disabled := unifiedRule("disabled", rules.PhaseRequest, 0, unifiedAction("deny", "reject_request", map[string]any{"message": "must not run"}))
			disabled.Enabled = false
			first := unifiedRule("first", rules.PhaseRequest, 10, unifiedSet("set", "/text/order", "first"))
			first.StopAfterMatch = stop
			later := unifiedRule("later", rules.PhaseRequest, 20, unifiedSet("set", "/text/order", "later"))
			publishUnifiedRules(t, h, later, disabled, first)
			response, body, record := capturedHTTPResponse(t, h, `{"model":"gpt-5.5","input":[],"stream":false}`)
			reportTestFailure(t, response.StatusCode != http.StatusOK, func() {
				t.Fatalf("disabled blocker ran: status=%d body=%s", response.StatusCode, body)
			})
			_, raw := observed.get()
			want := "later"
			if stop {
				want = "first"
			}
			metadata, _ := unifiedObject(t, raw)["text"].(map[string]any)
			reportTestFailure(t, metadata["order"] != want, func() {
				t.Fatalf("stop=%t upstream=%s want text.order=%q", stop, raw, want)
			})
			reportTestFailure(t, len(unifiedTraces(t, record, "disabled")) != 0 || (stop && len(unifiedTraces(t, record, "later")) != 0), func() {
				t.Fatalf("disabled/stopped rule was executed: %s", record.RuleTraces)
			})
		})
	}
}

func TestUnifiedRulesBlockedBeforeAccountAndUsage(t *testing.T) {
	for _, downstream := range []string{"sse", "ws"} {
		t.Run(downstream, func(t *testing.T) { runUnifiedRulesBlockedBeforeAccountAndUsageCase(t, &downstream) })
	}
}

func TestUnifiedRulesResponseProtocolMatrix(t *testing.T) {
	for _, transport := range []string{"sse", "websocket"} {
		for _, downstream := range []string{"sse", "ws"} {
			t.Run(downstream+"_from_"+transport, func(t *testing.T) { runUnifiedRulesResponseProtocolMatrixCase(t, &transport, &downstream) })
		}
	}
}

func TestUnifiedRulesAggregatedResponseBody(t *testing.T) {
	for _, transport := range []string{"sse", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			h, _, _ := unifiedHarness(t, transport, unifiedRulesEvents)
			eventRule := unifiedEventRule("event", "response.completed", unifiedReplace("event-text", "/response/output/0/content/0/text", "hello", "event"))
			bodyRule := unifiedRule("body", rules.PhaseResponseBody, 10, unifiedReplace("body-text", "/output/0/content/0/text", "event", "final-body"))
			publishUnifiedRules(t, h, eventRule, bodyRule)
			response, body, record := capturedHTTPResponse(t, h, `{"model":"gpt-5.5","input":"hello","stream":false}`)
			reportTestFailure(t, response.StatusCode != http.StatusOK, func() {
				t.Fatalf("aggregate status=%d body=%s", response.StatusCode, body)
			})
			want := unifiedObject(t, []byte(unifiedRulesEvents[2]))["response"].(map[string]any)
			want["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] = "final-body"
			reportTestFailure(t, !reflect.DeepEqual(unifiedObject(t, body), want), func() {
				t.Fatalf("aggregate body/identity/usage changed unexpectedly: %s want=%+v", body, want)
			})
			assertCapturedJSONBody(t, record, body, "response")
			rawFrames := unifiedAssertRawFrames(t, record, transport, unifiedRulesEvents)
			frames := captureFrames(record, capture.DirClientOut, store.KindHTTPResponse)
			reportTestFailure(t, frames[0].RuleEventID == "" || frames[0].RuleEventID != rawFrames[2].RuleEventID, func() {
				t.Fatalf("aggregate response lost terminal association: %+v", frames)
			})
			traces := unifiedTraces(t, record, "body")
			reportTestFailure(t, len(traces) != 1 || traces[0]["status"] != "changed" || traces[0]["event_id"] != rawFrames[2].RuleEventID, func() {
				t.Fatalf("response_body did not execute exactly once after event rules: %+v", traces)
			})
			unifiedAssertUsage(t, h, record)
		})
	}
}

func TestUnifiedRulesDropDeltaKeepsTerminal(t *testing.T) {
	for _, transport := range []string{"sse", "websocket"} {
		for _, downstream := range []string{"sse", "ws"} {
			t.Run(downstream+"_from_"+transport, func(t *testing.T) {
				h, _, _ := unifiedHarness(t, transport, unifiedRulesEvents)
				rule := unifiedEventRule("drop-delta", "response.output_text.delta", unifiedAction("drop", "drop_event", nil))
				publishUnifiedRules(t, h, rule)
				events, record := unifiedStreamResponse(t, h, downstream)
				reportTestFailure(t, len(events) != 2 || !equalCaptureJSON(events[0], []byte(unifiedRulesEvents[0])) || !equalCaptureJSON(events[1], []byte(unifiedRulesEvents[2])), func() {
					t.Fatalf("drop must remove only delta and preserve terminal: %s", events)
				})
				raw := unifiedAssertRawFrames(t, record, transport, unifiedRulesEvents)
				client := unifiedAssertClientFrames(t, record, downstream, events)
				reportTestFailure(t, raw[1].RuleEventID == "" || client[1].RuleEventID != raw[2].RuleEventID || client[1].RuleEventID == raw[1].RuleEventID, func() {
					t.Fatalf("drop corrupted terminal correlation: raw=%+v client=%+v", raw, client)
				})
				dropped := 0
				for _, trace := range unifiedTraces(t, record, "drop-delta") {
					if trace["status"] == "dropped" && trace["event_id"] == raw[1].RuleEventID {
						dropped++
					}
				}
				reportTestFailure(t, dropped != 1, func() {
					t.Fatalf("missing dropped-event trace: %s", record.RuleTraces)
				})
				unifiedAssertUsage(t, h, record)
			})
		}
	}
}

func TestUnifiedRulesRuntimeRollbackAndErrors(t *testing.T) {
	for _, phase := range []string{rules.PhaseRequest, rules.PhaseResponseEvent, rules.PhaseResponseBody} {
		for _, policy := range []string{rules.OnErrorAbort, rules.OnErrorSkipRule} {
			t.Run(phase+"/"+policy, func(t *testing.T) { runUnifiedRulesRuntimeRollbackAndErrorsCase(t, &phase, &policy) })
		}
	}
}

func unifiedAssertRuleError(t *testing.T, raw []byte) {
	t.Helper()
	payload := unifiedObject(t, raw)
	problem, ok := payload["error"].(map[string]any)
	reportTestFailure(t, !ok || problem["code"] != "rule_error" || problem["type"] != "server_error" || problem["message"] == "", func() {
		t.Fatalf("wrong rule runtime error: %s", raw)
	})
}

func TestUnifiedRulesResponseProtectionAndErrorDeduplication(t *testing.T) {
	for _, downstream := range []string{"sse", "ws"} {
		for _, failure := range []string{"ancestor-replacement", "drop-terminal", "drop-error"} {
			t.Run(downstream+"/"+failure, func(t *testing.T) {
				runUnifiedRulesResponseProtectionAndErrorDeduplicationCase(t, &downstream, &failure)
			})
		}
	}
}

func TestUnifiedRulesSnapshotPinnedDuringStream(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var startOnce, releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var calls atomic.Int32
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveUnifiedRulesSnapshotPinnedDuringStreamRequest(w, r, &t, &started, &release, &startOnce, &calls)
	}), "sse")
	h.server.Client().Timeout = unifiedRulesTimeout
	rule := unifiedEventRule("versioned", "response.output_text.delta", unifiedReplace("replace", "/delta", "hello", "old-version"))
	oldVersion := publishUnifiedRules(t, h, rule)
	ctx, cancel := context.WithTimeout(context.Background(), 2*unifiedRulesTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":[],"stream":true}`))
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	request.Header.Set("Authorization", "Bearer "+h.key)
	request.Header.Set("Content-Type", "application/json")
	response, err := h.server.Client().Do(request)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	defer response.Body.Close()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first upstream request never started")
	}
	// Reading a real client event establishes that this request already loaded
	// and used the old snapshot; publication is not racing request preparation.
	reader := upstream.ParseSSE(response.Body)
	{
		event, err := reader.Next()
		reportTestFailure(t, err != nil || event.Type != "response.created", func() {
			t.Fatalf("first event=%+v err=%v", event, err)
		})
	}
	rule.Actions[0] = unifiedReplace("replace", "/delta", "hello", "new-version")
	newVersion := publishUnifiedRules(t, h, rule)
	reportTestFailure(t, newVersion <= oldVersion, func() {
		t.Fatalf("publication version did not advance: %d -> %d", oldVersion, newVersion)
	})
	releaseOnce.Do(func() { close(release) })
	{
		event, err := reader.Next()
		reportTestFailure(t, err != nil || event.Data["delta"] != "old-version", func() {
			t.Fatalf("in-flight event mixed snapshots: event=%+v err=%v", event, err)
		})
	}
	{
		event, err := reader.Next()
		reportTestFailure(t, err != nil || event.Type != "response.completed", func() {
			t.Fatalf("old stream terminal=%+v err=%v", event, err)
		})
	}
	{
		_, err := reader.Next()
		reportTestFailure(t, err != io.EOF, func() {
			t.Fatalf("old stream emitted extra event: %v", err)
		})
	}
	_ = response.Body.Close()
	firstCapture := waitForResponseCapture(t, h)
	reportTestFailure(t, firstCapture.RulesVersion != oldVersion, func() {
		t.Fatalf("in-flight capture version=%d want=%d", firstCapture.RulesVersion, oldVersion)
	})
	// The second request is intentionally made through HTTP, not Service.Load.
	request2, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":[],"stream":true}`))
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	request2.Header = request.Header.Clone()
	response2, err := h.server.Client().Do(request2)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	body2, readErr := io.ReadAll(response2.Body)
	_ = response2.Body.Close()
	reportTestFailure(t, readErr != nil || response2.StatusCode != http.StatusOK, func() {
		t.Fatalf("new request status=%d body=%s err=%v", response2.StatusCode, body2, readErr)
	})
	events := unifiedSSEEvents(t, body2)
	reportTestFailure(t, len(events) != 3 || unifiedObject(t, events[1])["delta"] != "new-version" || calls.Load() != 2, func() {
		t.Fatalf("new request did not load latest published snapshot: events=%s calls=%d", events, calls.Load())
	})
	deadline := time.NewTimer(unifiedRulesTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		records, total, err := h.store.ListCaptures(ctx, store.CaptureFilter{Limit: 3, IncludePayloads: true})
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
		reportTestFailure(t, total > 2, func() {
			t.Fatalf("two requests created %d captures", total)
		})
		if len(records) == 2 {
			versions := map[int64]int{}
			func() { verifyUnifiedRulesSnapshotPinnedDuringStream(&t, &newVersion, &records, &versions) }()
			reportTestFailure(t, versions[oldVersion] != 1 || versions[newVersion] != 1, func() {
				t.Fatalf("capture versions=%v", versions)
			})
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("second capture did not finish")
		case <-ticker.C:
		}
	}
}

func TestUnifiedRulesGatewayPiShapeProvenance(t *testing.T) {
	h, observed, _ := unifiedHarness(t, "sse", unifiedRulesEvents)
	rule := unifiedRule("user-shape", rules.PhaseRequest, 10,
		unifiedSet("store", "/store", true), unifiedSet("stream", "/stream", false))
	publishUnifiedRules(t, h, rule)
	response, body, record := capturedHTTPResponse(t, h, `{"model":"gpt-5.5","input":[],"stream":false}`)
	reportTestFailure(t, response.StatusCode != http.StatusOK, func() {
		t.Fatalf("shape request failed: %d %s", response.StatusCode, body)
	})
	_, raw := observed.get()
	payload := unifiedObject(t, raw)
	reportTestFailure(t, payload["store"] != false || payload["stream"] != true, func() {
		t.Fatalf("user rules overrode required upstream Pi shape: %s", raw)
	})
	userTraces := unifiedTraces(t, record, "user-shape")
	protocolTraces := unifiedTraces(t, record, "protocol-request-finalize")
	reportTestFailure(t, len(userTraces) != 2 || len(protocolTraces) < 3, func() {
		t.Fatalf("missing protocol rule provenance")
	})
	gateway := protocolTraces[len(protocolTraces)-1]
	reportTestFailure(t, gateway["phase"] != rules.PhaseRequestFinalize || gateway["source"] != "rule", func() {
		t.Fatalf("protocol change is not a rule: %v", gateway)
	})
	changes := []any{}
	for _, trace := range protocolTraces {
		items, _ := trace["changes"].([]any)
		changes = append(changes, items...)
	}
	seen := map[string]bool{}
	for _, rawChange := range changes {
		change := rawChange.(map[string]any)
		path, _ := change["path"].(string)
		if path == "/store" && change["before"] == true && change["after"] == false {
			seen[path] = true
		}
		if path == "/stream" && change["before"] == false && change["after"] == true {
			seen[path] = true
		}
	}
	reportTestFailure(t, !seen["/store"] || !seen["/stream"], func() {
		t.Fatalf("gateway trace does not explain overwritten user values: %+v", gateway)
	})
}

func TestUnifiedRulesCachedAndAutoUpstreamProtocols(t *testing.T) {
	for _, tc := range []struct {
		transport string
		protocol  string
		calls     int32
	}{
		{"websocket-cached", "ws", 1},
		{"auto", "ws", 1},
		{"auto-fallback-sse", "sse", 2},
	} {
		for _, captureEnabled := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/capture=%t", tc.transport, captureEnabled), func(t *testing.T) { runUnifiedRulesCachedAndAutoUpstreamProtocolsCase(t, &tc, &captureEnabled) })
		}
	}
}

func TestUnifiedRulesBlockedCaptureDisabledOrNotPersisted(t *testing.T) {
	for _, mode := range []string{"disabled", "not-persisted"} {
		for _, downstream := range []string{"sse", "ws"} {
			t.Run(mode+"/"+downstream, func(t *testing.T) { runUnifiedRulesBlockedCaptureDisabledOrNotPersistedCase(t, &mode, &downstream) })
		}
	}
}

func TestUnifiedRulesCachedResponseDoesNotPolluteContinuationBaseline(t *testing.T) {
	backend := newGatewayContinuationBackend()
	originalCall := map[string]any{"type": "function_call", "id": "fc_rules", "call_id": "call_rules", "name": "lookup", "arguments": `{"query":"hello"}`}
	backend.output = []any{originalCall}
	backend.hook = func(conn *websocket.Conn, _ gatewayTurn) bool {
		_ = conn.SetReadDeadline(time.Now().Add(unifiedRulesTimeout))
		_ = conn.SetWriteDeadline(time.Now().Add(unifiedRulesTimeout))
		return true
	}
	h := newGatewayContinuationHarness(t, backend, nil)
	rule := unifiedEventRule("client-arguments", "response.completed", unifiedReplace("replace", "/response/output/0/arguments", "hello", "client-only"))
	rule.When = rules.Condition{Op: "all", Conditions: []rules.Condition{
		rule.When,
		{Op: "eq", Source: "context", Path: "/upstream_protocol", Value: "ws"},
	}}
	version := publishUnifiedRules(t, h, rule)
	conn := unifiedDialWS(t, h)
	firstInput := []any{gatewayUser("first")}
	firstEvent := gatewayExchange(t, conn, gatewayCreate(firstInput, "", ""))
	firstID := gatewayResponseID(t, firstEvent)
	firstTurn := gatewayObservedTurn(t, backend)
	firstCapture := waitForResponseCapture(t, h)
	firstOutput := firstEvent["response"].(map[string]any)["output"].([]any)[0].(map[string]any)
	reportTestFailure(t, firstOutput["arguments"] != `{"query":"client-only"}` || firstOutput["id"] != "fc_rules" || firstOutput["call_id"] != "call_rules", func() {
		t.Fatalf("client tool text was not rewritten without changing associations: %+v", firstOutput)
	})
	reportTestFailure(t, firstCapture.ResponseID != firstID || firstCapture.RulesVersion != version || firstCapture.TotalTokens != 4, func() {
		t.Fatalf("first cached response lost raw identity/usage: %+v", firstCapture)
	})
	result := map[string]any{"type": "function_call_output", "call_id": "call_rules", "output": "result"}
	// Submit full history containing the ORIGINAL upstream output. If response
	// rewriting mutated the pooled baseline, this cannot reduce to a two-item delta.
	secondInput := []any{gatewayUser("first"), originalCall, result, gatewayUser("second")}
	secondEvent := gatewayExchange(t, conn, gatewayCreate(secondInput, "", ""))
	secondID := gatewayResponseID(t, secondEvent)
	secondTurn := gatewayObservedTurn(t, backend)
	reportTestFailure(t, secondID == firstID || secondTurn.connection != firstTurn.connection || backend.connections.Load() != 1 || backend.httpCalls.Load() != 0, func() {
		t.Fatalf("continuation did not reuse the original WS exactly once: first=%+v second=%+v connections=%d HTTP=%d", firstTurn, secondTurn, backend.connections.Load(), backend.httpCalls.Load())
	})
	reportTestFailure(t, secondTurn.body["previous_response_id"] != firstID, func() {
		t.Fatalf("response rewrite polluted baseline parent optimization: %+v", secondTurn.body)
	})
	delta, _ := secondTurn.body["input"].([]any)
	reportTestFailure(t, len(delta) != 2 || !reflect.DeepEqual(delta[0], result), func() {
		t.Fatalf("original baseline did not produce tool-result/new-input delta: %+v", secondTurn.body)
	})
	secondOutput := secondEvent["response"].(map[string]any)["output"].([]any)[0].(map[string]any)
	reportTestFailure(t, secondOutput["arguments"] != `{"query":"client-only"}`, func() {
		t.Fatalf("reused WS did not expose actual ws protocol to response rules: %+v", secondOutput)
	})
	ctx, cancel := context.WithTimeout(context.Background(), unifiedRulesTimeout)
	defer cancel()
	var captures []*store.Capture
	gatewayAwait(t, func() bool {
		var total int
		var err error
		captures, total, err = h.store.ListCaptures(ctx, store.CaptureFilter{Limit: 3, IncludePayloads: true})
		reportTestFailure(t, err != nil || total > 2, func() {
			t.Fatalf("continuation capture count=%d err=%v", total, err)
		})
		return len(captures) == 2
	}, "second cached response did not finish its capture")
	for _, record := range captures {
		raw := captureFrames(record, "in", store.KindWSFrame)
		client := captureFrames(record, capture.DirClientOut, store.KindWSFrame)
		reportTestFailure(t, len(raw) != 2 || len(client) != 2 || record.Outcome != store.OutcomeOK || record.RulesVersion != version || record.TotalTokens != 4, func() {
			t.Fatalf("cached continuation lost events or usage: %+v", record)
		})
		rawOutput := unifiedObject(t, captureFrameBytes(t, raw[1]))["response"].(map[string]any)["output"].([]any)[0].(map[string]any)
		clientOutput := unifiedObject(t, captureFrameBytes(t, client[1]))["response"].(map[string]any)["output"].([]any)[0].(map[string]any)
		reportTestFailure(t, rawOutput["arguments"] != `{"query":"hello"}` || clientOutput["arguments"] != `{"query":"client-only"}` || raw[1].RuleEventID == "" || raw[1].RuleEventID != client[1].RuleEventID, func() {
			t.Fatalf("cached response original/client payload or event correlation changed: raw=%+v client=%+v", raw, client)
		})
	}
}

func TestV2ClientAndMetadataRulesReachBothTransports(t *testing.T) {
	for _, transport := range []string{"sse", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			h, observed, _ := unifiedHarness(t, transport, unifiedRulesEvents)
			publishUnifiedRules(t, h,
				unifiedRule("client-change", rules.PhaseClientRequest, 10, unifiedSet("input", "/input", "changed before normalization")),
				unifiedRule("metadata-change", rules.PhaseUpstreamHeaders, 10, unifiedSet("agent", "/user-agent", []any{"custom-rule-client"}), unifiedSet("sdk", "/x-stainless-package-version", []any{"test-version"})))
			response, body, record := capturedHTTPResponse(t, h, `{"model":"gpt-5.5","input":"original","stream":false}`)
			reportTestFailure(t, response.StatusCode != http.StatusOK, func() {
				t.Fatalf("status=%d body=%s", response.StatusCode, body)
			})
			headers, raw := observed.get()
			reportTestFailure(t, headers.Get("User-Agent") != "custom-rule-client" || headers.Get("X-Stainless-Package-Version") != "test-version", func() {
				t.Fatalf("metadata rules not applied: %#v", headers)
			})
			reportTestFailure(t, headers.Get("Authorization") == "", func() {
				t.Fatal("gateway credential missing")
			})
			if transport == "websocket" {
				observed.mu.Lock()
				raw = append([]byte(nil), observed.wsFrame...)
				observed.mu.Unlock()
			}
			payload := unifiedObject(t, raw)
			input := payload["input"].([]any)
			content := input[0].(map[string]any)["content"].([]any)
			reportTestFailure(t, content[0].(map[string]any)["text"] != "changed before normalization", func() {
				t.Fatalf("client phase lost before construction: %s", raw)
			})
			reportTestFailure(t, len(unifiedTraces(t, record, "metadata-change")) == 0, func() {
				t.Fatal("missing metadata provenance")
			})
		})
	}
}

// runUnifiedRulesBlockedBeforeAccountAndUsageCase exercises one isolated test phase with the original shared state.
func runUnifiedRulesBlockedBeforeAccountAndUsageCase(t *testing.T, capturedDownstream *string) {
	t.Helper()

	h, _, calls := unifiedHarness(t, "sse", unifiedRulesEvents)
	block := unifiedRule("block", rules.PhaseRequest, 1, unifiedAction("reject", "reject_request", map[string]any{"status": 429, "message": "blocked by integration policy"}))
	publishUnifiedRules(t, h, block)
	// A deliberate tripwire: a blocked request must not call any account
	// method. The real manager is unnecessary until after request rules.
	h.dataPlane.accounts = nil
	var record *store.Capture
	var errorBody []byte
	request := `{"model":"gpt-5.5","input":"blocked payload","stream":true,"marker":"original"}`
	if (*capturedDownstream) == "ws" {
		request = `{"type":"response.create","model":"gpt-5.5","input":"blocked payload","marker":"original"}`
		events, captured := unifiedWSResponse(t, h, request)
		record, errorBody = captured, events[0]
		reportTestFailure(t, len(events) != 1, func() {
			t.Fatalf("blocked WS delivered %d events", len(events))
		})
		unifiedAssertClientFrames(t, record, "ws", events)
		frames := captureFrames(record, capture.DirClientIn, store.KindWSFrame)
		reportTestFailure(t, len(frames) != 1 || !equalCaptureJSON(captureFrameBytes(t, frames[0]), []byte(request)), func() {
			t.Fatalf("original WS envelope lost: %+v", frames)
		})
	} else {
		response, body, captured := capturedHTTPResponse(t, h, request)
		record, errorBody = captured, body
		reportTestFailure(t, response.StatusCode != http.StatusTooManyRequests, func() {
			t.Fatalf("blocked HTTP status=%d body=%s", response.StatusCode, body)
		})
		assertCapturedJSONBody(t, record, body, "error")
		frames := captureFrames(record, capture.DirClientIn, store.KindRequestBody)
		reportTestFailure(t, len(frames) != 1 || !equalCaptureJSON(captureFrameBytes(t, frames[0]), []byte(request)), func() {
			t.Fatalf("original HTTP body lost: %+v", frames)
		})
	}
	problem, _ := unifiedObject(t, errorBody)["error"].(map[string]any)
	reportTestFailure(t, problem["code"] != "blocked_by_rule" || problem["message"] != "blocked by integration policy", func() {
		t.Fatalf("wrong rule rejection: %s", errorBody)
	})
	reportTestFailure(t, calls.Load() != 0 || record.AccountID != 0 || record.AccountName != "" || record.Outcome != store.OutcomeError || record.Error == "", func() {
		t.Fatalf("blocked request touched routing or lost diagnostics: calls=%d capture=%+v", calls.Load(), record)
	})
	reportTestFailure(t, record.Status != 0 || record.URL != "" || record.RequestBody != "" || record.RequestBytes != 0 || record.ResponseID != "" || record.TotalTokens != 0, func() {
		t.Fatalf("blocked request invented upstream wire/usage: %+v", record)
	})
	for _, frame := range record.ResponseFrames {
		reportTestFailure(t, frame.Dir == "in" || frame.Dir == "out", func() {
			t.Errorf("blocked request invented upstream frame: %+v", frame)
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), unifiedRulesTimeout)
	defer cancel()
	usage, total, err := h.store.ListUsageRecords(ctx, store.UsageFilter{Limit: 10})
	reportTestFailure(t, err != nil || total != 0 || len(usage) != 0, func() {
		t.Fatalf("blocked request created usage: total=%d rows=%+v err=%v", total, usage, err)
	})
	traces := unifiedTraces(t, record, "block")
	reportTestFailure(t, len(traces) != 1 || traces[0]["status"] != "blocked" || traces[0]["action_id"] != "reject", func() {
		t.Fatalf("missing blocked rule provenance: %+v", traces)
	})
}

// runUnifiedRulesResponseProtocolMatrixCase exercises one isolated test phase with the original shared state.
func runUnifiedRulesResponseProtocolMatrixCase(t *testing.T, capturedTransport *string, capturedDownstream *string) {
	t.Helper()

	h, _, calls := unifiedHarness(t, (*capturedTransport), unifiedRulesEvents)
	rule := unifiedEventRule("replace-delta", "response.output_text.delta", unifiedReplace("replace", "/delta", "hello", "hello!"))
	protocol := "sse"
	if (*capturedTransport) != "sse" {
		protocol = "ws"
	}
	// Context exposes the observed wire protocol, not the configured
	// routing mode (websocket, websocket-cached, or auto).
	rule.When = rules.Condition{Op: "all", Conditions: []rules.Condition{
		rule.When,
		{Op: "eq", Source: "context", Path: "/upstream_protocol", Value: protocol},
	}}
	version := publishUnifiedRules(t, h, rule)
	events, record := unifiedStreamResponse(t, h, (*capturedDownstream))
	reportTestFailure(t, calls.Load() != 1 || len(events) != 3 || record.Outcome != store.OutcomeOK || record.RulesVersion != version, func() {
		t.Fatalf("protocol result: calls=%d events=%d outcome=%s version=%d", calls.Load(), len(events), record.Outcome, record.RulesVersion)
	})
	{
		delta := unifiedObject(t, events[1])
		reportTestFailure(t, delta["delta"] != "hello!" || delta["item_id"] != "msg_rules_1" || delta["response_id"] != "resp_rules_1", func() {
			t.Fatalf("delta rewrite missing, duplicated, or identity changed: %s", events[1])
		})
	}
	for _, i := range []int{0, 2} {
		reportTestFailure(t, string(events[i]) != unifiedRulesEvents[i], func() {
			t.Errorf("unchanged event %d was re-encoded: %s", i, events[i])
		})
	}
	rawFrames := unifiedAssertRawFrames(t, record, (*capturedTransport), unifiedRulesEvents)
	clientFrames := unifiedAssertClientFrames(t, record, (*capturedDownstream), events)
	for i := range clientFrames {
		reportTestFailure(t, rawFrames[i].RuleEventID == "" || clientFrames[i].RuleEventID != rawFrames[i].RuleEventID, func() {
			t.Errorf("event %d missing explicit upstream/client correlation: %q/%q", i, rawFrames[i].RuleEventID, clientFrames[i].RuleEventID)
		})
	}
	traces := unifiedTraces(t, record, "replace-delta")
	changed := 0
	for _, trace := range traces {
		if trace["status"] == "changed" {
			changed++
			reportTestFailure(t, trace["event_id"] != rawFrames[1].RuleEventID || trace["event_type"] != "response.output_text.delta" || trace["sequence"] != float64(2) || trace["action_id"] != "replace", func() {
				t.Errorf("trace not explicitly associated with its event: %+v", trace)
			})
		}
	}
	reportTestFailure(t, changed != 1, func() {
		t.Fatalf("text_replace executed %d times, want exactly once: %+v", changed, traces)
	})
	unifiedAssertUsage(t, h, record)
}

// runUnifiedRulesRuntimeRollbackAndErrorsCase exercises one isolated test phase with the original shared state.
func runUnifiedRulesRuntimeRollbackAndErrorsCase(t *testing.T, capturedPhase *string, capturedPolicy *string) {
	t.Helper()

	h, observed, calls := unifiedHarness(t, "sse", unifiedRulesEvents)
	path, eventType := "/model", ""
	switch *capturedPhase {
	case rules.PhaseResponseEvent:
		path, eventType = "/delta", "response.output_text.delta"
	case rules.PhaseResponseBody:
		path = "/output/0/content/0/text"
	}
	failing := unifiedRule("failing", (*capturedPhase), 10,
		unifiedSet("temporary", path, "must-not-leak"),
		unifiedReplace("fail", "/missing-text", "hello", "bad"))
	failing.OnError = (*capturedPolicy)
	if eventType != "" {
		failing.When = rules.Condition{Op: "eq", Source: "context", Path: "/event_type", Value: eventType}
	}
	publishUnifiedRules(t, h, failing)
	stream := (*capturedPhase) == rules.PhaseResponseEvent
	response, body, record := capturedHTTPResponse(t, h, fmt.Sprintf(`{"model":"gpt-5.5","instructions":"original","input":[],"stream":%t}`, stream))
	reportTestFailure(t, bytes.Contains(body, []byte("must-not-leak")), func() {
		t.Fatalf("failed rule partially committed to client: %s", body)
	})
	traces := unifiedTraces(t, record, "failing")
	rolledBack, failures := 0, 0
	func() { verifyUnifiedRulesRuntimeRollbackAndErrors(&t, &traces, &rolledBack, &failures) }()
	reportTestFailure(t, rolledBack != 1 || failures != 1, func() {
		t.Fatalf("atomic rollback/error traces missing: %+v", traces)
	})
	if (*capturedPolicy) == rules.OnErrorSkipRule {
		reportTestFailure(t, response.StatusCode != http.StatusOK || record.Outcome != store.OutcomeOK || calls.Load() != 1, func() {
			t.Fatalf("skip_rule did not continue: status=%d outcome=%s body=%s", response.StatusCode, record.Outcome, body)
		})
		func() {
			verifyUnifiedRulesRuntimeRollbackAndErrors2(&t, &capturedPhase, &observed, &body, &record)
		}()
		return
	}
	reportTestFailure(t, record.Outcome != store.OutcomeError, func() {
		t.Fatalf("abort capture outcome=%s", record.Outcome)
	})
	if stream {
		events := unifiedSSEEvents(t, body)
		reportTestFailure(t, response.StatusCode != http.StatusOK || len(events) != 2 || unifiedObject(t, events[0])["type"] != "response.created", func() {
			t.Fatalf("committed stream must retain first event then one error: status=%d body=%s", response.StatusCode, body)
		})
		unifiedAssertRuleError(t, events[1])
		assertCapturedSSEBody(t, record, body, 2)
	} else {
		reportTestFailure(t, response.StatusCode != http.StatusInternalServerError, func() {
			t.Fatalf("rule abort status=%d want=500 body=%s", response.StatusCode, body)
		})
		unifiedAssertRuleError(t, body)
		assertCapturedJSONBody(t, record, body, "error")
	}
	if (*capturedPhase) == rules.PhaseRequest {
		reportTestFailure(t, calls.Load() != 0 || record.AccountID != 0 || record.RequestBytes != 0, func() {
			t.Fatalf("request rule failure touched upstream: calls=%d capture=%+v", calls.Load(), record)
		})
		ctx, cancel := context.WithTimeout(context.Background(), unifiedRulesTimeout)
		defer cancel()
		_, total, err := h.store.ListUsageRecords(ctx, store.UsageFilter{Limit: 10})
		reportTestFailure(t, err != nil || total != 0, func() {
			t.Fatalf("request failure invented usage: %d err=%v", total, err)
		})
	}
}

// runUnifiedRulesResponseProtectionAndErrorDeduplicationCase exercises one isolated test phase with the original shared state.
func runUnifiedRulesResponseProtectionAndErrorDeduplicationCase(t *testing.T, capturedDownstream *string, capturedFailure *string) {
	t.Helper()

	events := unifiedRulesEvents
	kind := "response.completed"
	action := unifiedSet("protected", "/response", map[string]any{"replacement": "must-not-leak"})
	if (*capturedFailure) == "drop-terminal" {
		action = unifiedAction("protected", "drop_event", nil)
	}
	if (*capturedFailure) == "drop-error" {
		kind = "error"
		action = unifiedAction("protected", "drop_event", nil)
		events = []string{unifiedRulesEvents[0], `{"type":"error","error":{"type":"server_error","code":"upstream_failed","message":"upstream failure"}}`}
	}
	h, _, _ := unifiedHarness(t, "sse", events)
	rule := unifiedEventRule("protection", kind, unifiedSet("temporary", "/uncommitted", "must-not-leak"), action)
	publishUnifiedRules(t, h, rule)
	delivered, record := unifiedStreamResponse(t, h, (*capturedDownstream))
	wantCount := len(events)
	reportTestFailure(t, len(delivered) != wantCount || record.Outcome != store.OutcomeError, func() {
		t.Fatalf("protection error lost/duplicated: delivered=%s outcome=%s", delivered, record.Outcome)
	})
	unifiedAssertRuleError(t, delivered[len(delivered)-1])
	errorCount := 0
	for _, raw := range delivered {
		payload := unifiedObject(t, raw)
		if payload["type"] == "error" {
			errorCount++
		}
		reportTestFailure(t, bytes.Contains(raw, []byte("must-not-leak")) || payload["type"] == "response.completed", func() {
			t.Errorf("failed protected event leaked partial result: %s", raw)
		})
	}
	reportTestFailure(t, errorCount != 1, func() {
		t.Fatalf("delivered %d errors, want one", errorCount)
	})
	unifiedAssertRawFrames(t, record, "sse", events)
	unifiedAssertClientFrames(t, record, (*capturedDownstream), delivered)
	rolledBack, protected := false, false
	func() {
		verifyUnifiedRulesResponseProtectionAndErrorDeduplication(&t, &record, &rolledBack, &protected)
	}()
	reportTestFailure(t, !rolledBack || !protected, func() {
		t.Fatalf("protection failed without atomic rollback: %s", record.RuleTraces)
	})
	reportTestFailure(t, (*capturedFailure) != "drop-error" && (record.ResponseID != "resp_rules_1" || record.TotalTokens != 18), func() {
		t.Fatalf("response protection polluted prior raw metering: %+v", record)
	})
}

// serveUnifiedRulesSnapshotPinnedDuringStreamRequest exercises one isolated test phase with the original shared state.
func serveUnifiedRulesSnapshotPinnedDuringStreamRequest(w http.ResponseWriter, r *http.Request, capturedT **testing.T, capturedStarted *chan struct{}, capturedRelease *chan struct{}, capturedStartOnce *sync.Once, capturedCalls *atomic.Int32) {
	(*capturedT).Helper()

	call := (*capturedCalls).Add(1)
	_, _ = io.Copy(io.Discard, r.Body)
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(unifiedRulesTimeout))
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: %s\n\n", unifiedRulesEvents[0])
	w.(http.Flusher).Flush()
	if call == 1 {
		(*capturedStartOnce).Do(func() { close((*capturedStarted)) })
		select {
		case <-(*capturedRelease):
		case <-r.Context().Done():
			return
		case <-time.After(unifiedRulesTimeout):
			(*capturedT).Error("timed out waiting to release in-flight rule stream")
			return
		}
	}
	for _, event := range unifiedRulesEvents[1:] {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		w.(http.Flusher).Flush()
	}
}

// runUnifiedRulesCachedAndAutoUpstreamProtocolsCase exercises one isolated test phase with the original shared state.
func runUnifiedRulesCachedAndAutoUpstreamProtocolsCase(t *testing.T, capturedTc *struct {
	transport string
	protocol  string
	calls     int32
}, capturedCaptureEnabled *bool) {
	t.Helper()

	h, _, calls := unifiedHarness(t, (*capturedTc).transport, unifiedRulesEvents)
	ctx, cancel := context.WithTimeout(context.Background(), unifiedRulesSetupTimeout)
	defer cancel()
	settings := h.dataPlane.settings.Get()
	settings.CaptureEnabled = (*capturedCaptureEnabled)
	{
		err := h.dataPlane.settings.Set(ctx, settings)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	rule := unifiedEventRule("replace-delta", "response.output_text.delta", unifiedReplace("replace", "/delta", "hello", "hello!"))
	rule.When = rules.Condition{Op: "all", Conditions: []rules.Condition{
		rule.When,
		{Op: "eq", Source: "context", Path: "/upstream_protocol", Value: (*capturedTc).protocol},
	}}
	version := publishUnifiedRules(t, h, rule)
	status, body := gatewayHTTP(t, h, map[string]any{"model": "gpt-5.5", "input": "hello", "stream": true})
	events := unifiedSSEEvents(t, body)
	reportTestFailure(t, status != http.StatusOK || len(events) != 3 || calls.Load() != (*capturedTc).calls, func() {
		t.Fatalf("route=%s status=%d events=%d calls=%d want=%d body=%s", (*capturedTc).transport, status, len(events), calls.Load(), (*capturedTc).calls, body)
	})
	reportTestFailure(t, unifiedObject(t, events[1])["delta"] != "hello!", func() {
		t.Fatalf("route %s did not expose actual %q protocol or applied the rule repeatedly: %s", (*capturedTc).transport, (*capturedTc).protocol, events[1])
	})
	reportTestFailure(t, string(events[0]) != unifiedRulesEvents[0] || string(events[2]) != unifiedRulesEvents[2], func() {
		t.Fatalf("cached/auto route modified untouched events: %s", body)
	})
	if *capturedCaptureEnabled {
		record := waitForResponseCapture(t, h)
		reportTestFailure(t, record.RulesVersion != version || record.Outcome != store.OutcomeOK, func() {
			t.Fatalf("cached/auto capture version/outcome=%d/%s", record.RulesVersion, record.Outcome)
		})
		raw := unifiedAssertRawFrames(t, record, (*capturedTc).protocol, unifiedRulesEvents)
		client := unifiedAssertClientFrames(t, record, "sse", events)
		reportTestFailure(t, raw[1].RuleEventID == "" || raw[1].RuleEventID != client[1].RuleEventID, func() {
			t.Fatalf("cached/fallback protocol lost event association: %q/%q", raw[1].RuleEventID, client[1].RuleEventID)
		})
		changed := 0
		for _, trace := range unifiedTraces(t, record, "replace-delta") {
			if trace["status"] == "changed" {
				changed++
			}
		}
		reportTestFailure(t, changed != 1, func() {
			t.Fatalf("cached/auto rewrite count=%d want=1", changed)
		})
		unifiedAssertUsage(t, h, record)
	} else {
		// A final usage row proves finish ran before asserting absence.
		usage := waitForUsageRecord(t, h.store)
		reportTestFailure(t, usage.Outcome != "succeeded" || usage.TotalTokens == nil || *usage.TotalTokens != 18, func() {
			t.Fatalf("capture-disabled protocol rewrite changed usage: %+v", usage)
		})
		records, total, err := h.store.ListCaptures(ctx, store.CaptureFilter{Limit: 2, IncludePayloads: true})
		reportTestFailure(t, err != nil || total != 0 || len(records) != 0, func() {
			t.Fatalf("capture-disabled request persisted capture: total=%d err=%v", total, err)
		})
	}
}

// runUnifiedRulesBlockedCaptureDisabledOrNotPersistedCase exercises one isolated test phase with the original shared state.
func runUnifiedRulesBlockedCaptureDisabledOrNotPersistedCase(t *testing.T, capturedMode *string, capturedDownstream *string) {
	t.Helper()

	h, _, calls := unifiedHarness(t, "sse", unifiedRulesEvents)
	ctx, cancel := context.WithTimeout(context.Background(), unifiedRulesSetupTimeout)
	defer cancel()
	if (*capturedMode) == "disabled" {
		settings := h.dataPlane.settings.Get()
		settings.CaptureEnabled = false
		{
			err := h.dataPlane.settings.Set(ctx, settings)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
	} else {
		h.dataPlane.cfg.Capture.Persist = false
	}
	publishUnifiedRules(t, h, unifiedRule("block", rules.PhaseRequest, 1,
		unifiedAction("reject", "reject_request", map[string]any{"status": 403, "message": "blocked without persistence"})))
	h.dataPlane.accounts = nil // No account-selection call is allowed.
	var body []byte
	if (*capturedDownstream) == "sse" {
		status, raw := gatewayHTTP(t, h, map[string]any{"model": "gpt-5.5", "input": "blocked", "stream": true})
		body = raw
		reportTestFailure(t, status != http.StatusForbidden, func() {
			t.Fatalf("block status=%d body=%s", status, body)
		})
	} else {
		conn := unifiedDialWS(t, h)
		{
			err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.5","input":"blocked"}`))
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		_, raw, err := conn.ReadMessage()
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
		body = raw
		_ = conn.Close()
	}
	problem, _ := unifiedObject(t, body)["error"].(map[string]any)
	reportTestFailure(t, problem["code"] != "blocked_by_rule" || problem["message"] != "blocked without persistence", func() {
		t.Fatalf("capture setting bypassed rule rejection: %s", body)
	})
	// Join HTTP handlers and wait for any hijacked WS exchange to finish;
	// absence assertions must not race the deferred persistence path.
	h.server.Close()
	gatewayAwait(t, func() bool {
		h.dataPlane.lifecycle.mu.Lock()
		defer h.dataPlane.lifecycle.mu.Unlock()
		return len(h.dataPlane.lifecycle.active) == 0
	}, "blocked downstream did not finish")
	records, captureCount, err := h.store.ListCaptures(ctx, store.CaptureFilter{Limit: 2, IncludePayloads: true})
	reportTestFailure(t, err != nil || captureCount != 0 || len(records) != 0, func() {
		t.Fatalf("%s block persisted captures: total=%d records=%+v err=%v", (*capturedMode), captureCount, records, err)
	})
	usage, usageCount, err := h.store.ListUsageRecords(ctx, store.UsageFilter{Limit: 2})
	reportTestFailure(t, err != nil || usageCount != 0 || len(usage) != 0 || calls.Load() != 0, func() {
		t.Fatalf("%s block accessed upstream or created usage: calls=%d count=%d rows=%+v err=%v", (*capturedMode), calls.Load(), usageCount, usage, err)
	})
}

type serveUnifiedHarnessRequestState struct {
	capturedT         **testing.T
	capturedTransport *string
	capturedEvents    *[]string
	capturedObserved  **upstreamCapture
	capturedCalls     **atomic.Int32
	capturedUpgrader  *websocket.Upgrader
}

// serveUnifiedHarnessRequest exercises one isolated test phase with the original shared state.
func serveUnifiedHarnessRequest(w http.ResponseWriter, r *http.Request, state serveUnifiedHarnessRequestState) {
	(*state.capturedCalls).Add(1)
	if (*state.capturedTransport) == "auto-fallback-sse" && websocket.IsWebSocketUpgrade(r) {
		// Only a pre-send 5xx handshake rejection permits safe auto fallback.
		http.Error(w, "WebSocket temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if (*state.capturedTransport) == "sse" || (*state.capturedTransport) == "auto-fallback-sse" {
		serveUnifiedSSEFixture(w, r, state)
		return
	}
	(*state.capturedObserved).set(r, nil)
	conn, err := (*state.capturedUpgrader).Upgrade(w, r, nil)
	if err != nil {
		(*state.capturedT).Errorf("fake upstream upgrade: %v", err)
		return
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(unifiedRulesTimeout))
	_ = conn.SetWriteDeadline(time.Now().Add(unifiedRulesTimeout))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		(*state.capturedT).Errorf("fake upstream request frame: %v", err)
		return
	}
	(*state.capturedObserved).setWS(raw)
	for _, event := range *state.capturedEvents {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
			return
		}
	}
}

// verifyUnifiedRulesRuntimeRollbackAndErrors exercises one isolated test phase with the original shared state.
func verifyUnifiedRulesRuntimeRollbackAndErrors(capturedT **testing.T, capturedTraces *[]map[string]any, capturedRolledBack *int, capturedFailures *int) {
	(*capturedT).Helper()

	for _, trace := range *capturedTraces {
		if trace["action_id"] == "temporary" && trace["status"] == "rolled_back" && trace["rolled_back"] == true {
			(*capturedRolledBack)++
			{
				changes, ok := trace["changes"].([]any)
				reportTestFailure((*capturedT), ok && len(changes) != 0, func() {
					(*capturedT).Errorf("rolled-back change falsely attributed: %+v", trace)
				})
			}
		}
		if trace["action_id"] == "fail" && trace["rolled_back"] == true && trace["error"] != "" {
			(*capturedFailures)++
		}
	}
}

// verifyUnifiedRulesResponseProtectionAndErrorDeduplication exercises one isolated test phase with the original shared state.
func verifyUnifiedRulesResponseProtectionAndErrorDeduplication(capturedT **testing.T, capturedRecord **store.Capture, capturedRolledBack *bool, capturedProtected *bool) {
	(*capturedT).Helper()

	for _, trace := range unifiedTraces((*capturedT), (*capturedRecord), "protection") {
		if trace["action_id"] == "temporary" && trace["status"] == "rolled_back" && trace["rolled_back"] == true {
			(*capturedRolledBack) = true
		}
		if trace["action_id"] == "protected" && trace["status"] == "error" && trace["rolled_back"] == true {
			(*capturedProtected) = true
		}
	}
}

// verifyUnifiedRulesSnapshotPinnedDuringStream exercises one isolated test phase with the original shared state.
func verifyUnifiedRulesSnapshotPinnedDuringStream(capturedT **testing.T, capturedNewVersion *int64, capturedRecords *[]*store.Capture, capturedVersions *map[int64]int) {
	(*capturedT).Helper()

	for _, record := range *capturedRecords {
		(*capturedVersions)[record.RulesVersion]++
		for _, trace := range unifiedTraces((*capturedT), record, "versioned") {
			if trace["status"] == "changed" {
				wantRevision := float64(1)
				if record.RulesVersion == (*capturedNewVersion) {
					wantRevision = 2
				}
				reportTestFailure((*capturedT), trace["revision"] != wantRevision, func() {
					(*capturedT).Errorf("capture and trace revisions mixed: version=%d trace=%+v", record.RulesVersion, trace)
				})
			}
		}
	}
}

// verifyUnifiedRulesRuntimeRollbackAndErrors2 exercises one isolated test phase with the original shared state.
func verifyUnifiedRulesRuntimeRollbackAndErrors2(capturedT **testing.T, capturedCapturedPhase **string, capturedObserved **upstreamCapture, capturedBody *[]byte, capturedRecord **store.Capture) {
	(*capturedT).Helper()

	if (*(*capturedCapturedPhase)) == rules.PhaseRequest {
		_, sent := (*capturedObserved).get()
		reportTestFailure((*capturedT), unifiedObject((*capturedT), sent)["model"] != "gpt-5.5" || (*capturedRecord).Model != "gpt-5.5", func() {
			(*capturedT).Fatalf("partial request mutation reached upstream: %s", sent)
		})
	} else if (*(*capturedCapturedPhase)) == rules.PhaseResponseEvent {
		events := unifiedSSEEvents((*capturedT), (*capturedBody))
		reportTestFailure((*capturedT), len(events) != 3 || unifiedObject((*capturedT), events[1])["delta"] != "hello", func() {
			(*capturedT).Fatalf("skip_rule failed to preserve original delta: %s", (*capturedBody))
		})
	} else {
		reportTestFailure((*capturedT), !bytes.Contains((*capturedBody), []byte(`"text":"hello"`)), func() {
			(*capturedT).Fatalf("body rollback lost original text: %s", (*capturedBody))
		})
	}
}

func serveUnifiedSSEFixture(w http.ResponseWriter, r *http.Request, state serveUnifiedHarnessRequestState) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		(*state.capturedT).Errorf("read fake upstream request: %v", err)
		return
	}
	(*state.capturedObserved).set(r, body)
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(unifiedRulesTimeout))
	w.Header().Set("Content-Type", "text/event-stream")
	for _, raw := range *state.capturedEvents {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return // Runtime-error tests intentionally abort the upstream.
		}
		w.(http.Flusher).Flush()
	}
}
