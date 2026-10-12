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
	if err := rulesruntime.New(h.store).Ensure(ctx); err != nil {
		t.Fatalf("initialize rules: %v", err)
	}
	snapshot, err := h.store.LoadRuleSet(ctx)
	if err != nil {
		t.Fatal(err)
	}
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
		if err != nil {
			t.Fatal(err)
		}
		change := store.RuleChange{Kind: "create", ID: definition.ID, Rule: raw, Source: "user"}
		if row := existing[definition.ID]; row != nil {
			change.Kind, change.ExpectedRevision = "update", row.Revision
		}
		changes = append(changes, change)
	}
	published, err := h.store.PublishRules(ctx, &snapshot.Version, changes, rulesruntime.ValidateSnapshot)
	if err != nil {
		t.Fatalf("publish rules: %v", err)
	}
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
		calls.Add(1)
		if transport == "auto-fallback-sse" && websocket.IsWebSocketUpgrade(r) {
			// Only a pre-send 5xx handshake rejection permits safe auto fallback.
			http.Error(w, "WebSocket temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		if transport == "sse" || transport == "auto-fallback-sse" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read fake upstream request: %v", err)
				return
			}
			observed.set(r, body)
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(unifiedRulesTimeout))
			w.Header().Set("Content-Type", "text/event-stream")
			for _, raw := range events {
				if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
					return // Runtime-error tests intentionally abort the upstream.
				}
				w.(http.Flusher).Flush()
			}
			return
		}
		observed.set(r, nil)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("fake upstream upgrade: %v", err)
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(unifiedRulesTimeout))
		_ = conn.SetWriteDeadline(time.Now().Add(unifiedRulesTimeout))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("fake upstream request frame: %v", err)
			return
		}
		observed.setWS(raw)
		for _, event := range events {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
				return
			}
		}
	}), configuredTransport)
	h.server.Client().Timeout = unifiedRulesTimeout
	t.Cleanup(h.dataPlane.Close)
	return h, observed, calls
}

func unifiedObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		t.Fatalf("expected JSON object: %s (%v)", raw, err)
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
		if err != nil {
			t.Fatalf("parse client SSE: %v", err)
		}
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
	if err := conn.WriteMessage(websocket.TextMessage, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	var events [][]byte
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read downstream WS event: %v", err)
		}
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
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream status=%d body=%s", response.StatusCode, body)
	}
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
	if record.ResponseID != "resp_rules_1" || record.PromptTokens != 11 || record.CompletionTokens != 7 || record.TotalTokens != 18 {
		t.Fatalf("raw capture identity/usage changed: id=%q tokens=%d/%d/%d", record.ResponseID, record.PromptTokens, record.CompletionTokens, record.TotalTokens)
	}
	usage := waitForUsageRecord(t, h.store)
	if usage.Outcome != "succeeded" || usage.AccountID != h.accountID || usage.Model != "gpt-5.5" {
		t.Fatalf("usage attribution/outcome changed: %+v", usage)
	}
	for _, metric := range []struct {
		name string
		got  *int64
		want int64
	}{{"input", usage.InputTokens, 11}, {"output", usage.OutputTokens, 7}, {"total", usage.TotalTokens, 18}, {"cached", usage.CachedTokens, 4}, {"reasoning", usage.ReasoningTokens, 3}} {
		if metric.got == nil || *metric.got != metric.want {
			t.Errorf("%s usage=%v want=%d", metric.name, metric.got, metric.want)
		}
	}
}

func unifiedAssertRawFrames(t *testing.T, record *store.Capture, transport string, expected []string) []store.Frame {
	t.Helper()
	kind := store.KindSSEEvent
	if transport != "sse" {
		kind = store.KindWSFrame
	}
	frames := captureFrames(record, "in", kind)
	if len(frames) != len(expected) {
		t.Fatalf("raw upstream frames=%d want=%d", len(frames), len(expected))
	}
	for i, frame := range frames {
		if !equalCaptureJSON(captureFrameBytes(t, frame), []byte(expected[i])) {
			t.Errorf("upstream frame %d changed: %s want=%s", i, captureFrameBytes(t, frame), expected[i])
		}
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
	if len(frames) != len(events) {
		t.Fatalf("client capture frames=%d actual=%d (duplicate or missing delivery)", len(frames), len(events))
	}
	for i, frame := range frames {
		if !equalCaptureJSON(captureFrameBytes(t, frame), events[i]) {
			t.Errorf("client frame %d disagrees with delivered bytes: %s != %s", i, captureFrameBytes(t, frame), events[i])
		}
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
	if response.StatusCode != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d body=%s", response.StatusCode, calls.Load(), body)
	}
	_, sent := observed.get()
	payload := unifiedObject(t, sent)
	metadata, _ := payload["text"].(map[string]any)
	if payload["model"] != "gpt-5.5" || metadata["order"] != "final" {
		t.Fatalf("priority/multiple actions did not reach upstream: %s", sent)
	}
	if !reflect.DeepEqual(metadata["copied"], map[string]any{"nested": []any{true, nil, float64(7)}}) || !reflect.DeepEqual(metadata["literal"], map[string]any{"$ref": "not-an-expression"}) {
		t.Fatalf("native JSON/$ref/$literal changed: %#v", metadata)
	}
	if !reflect.DeepEqual(payload["reasoning"], map[string]any{"effort": "high", "summary": "concise"}) {
		t.Fatalf("mixed action missing: %#v", payload["reasoning"])
	}
	if record.RulesVersion != version || record.Model != "gpt-5.5" {
		t.Fatalf("capture version/model=%d/%q", record.RulesVersion, record.Model)
	}
	var execution []string
	for _, raw := range record.RuleTraces {
		trace := unifiedObject(t, raw)
		if trace["phase"] == rules.PhaseRequest && trace["action_id"] != nil && len(strings.Split(fmt.Sprint(trace["action_path"]), "/")) <= 4 && trace["source_kind"] != "gateway" {
			execution = append(execution, fmt.Sprint(trace["rule_id"], "/", trace["action_id"]))
		}
	}
	wantOrder := []string{"first/model", "first/instructions", "first/copy-client", "first/literal", "first/reasoning", "second/replace", "third/replace"}
	if !reflect.DeepEqual(execution, wantOrder) {
		t.Fatalf("actual execution=%v want=%v", execution, wantOrder)
	}
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
			if response.StatusCode != http.StatusOK {
				t.Fatalf("disabled blocker ran: status=%d body=%s", response.StatusCode, body)
			}
			_, raw := observed.get()
			want := "later"
			if stop {
				want = "first"
			}
			metadata, _ := unifiedObject(t, raw)["text"].(map[string]any)
			if metadata["order"] != want {
				t.Fatalf("stop=%t upstream=%s want text.order=%q", stop, raw, want)
			}
			if len(unifiedTraces(t, record, "disabled")) != 0 || (stop && len(unifiedTraces(t, record, "later")) != 0) {
				t.Fatalf("disabled/stopped rule was executed: %s", record.RuleTraces)
			}
		})
	}
}

func TestUnifiedRulesBlockedBeforeAccountAndUsage(t *testing.T) {
	for _, downstream := range []string{"sse", "ws"} {
		t.Run(downstream, func(t *testing.T) {
			h, _, calls := unifiedHarness(t, "sse", unifiedRulesEvents)
			block := unifiedRule("block", rules.PhaseRequest, 1, unifiedAction("reject", "reject_request", map[string]any{"status": 429, "message": "blocked by integration policy"}))
			publishUnifiedRules(t, h, block)
			// A deliberate tripwire: a blocked request must not call any account
			// method. The real manager is unnecessary until after request rules.
			h.dataPlane.accounts = nil
			var record *store.Capture
			var errorBody []byte
			request := `{"model":"gpt-5.5","input":"blocked payload","stream":true,"marker":"original"}`
			if downstream == "ws" {
				request = `{"type":"response.create","model":"gpt-5.5","input":"blocked payload","marker":"original"}`
				events, captured := unifiedWSResponse(t, h, request)
				record, errorBody = captured, events[0]
				if len(events) != 1 {
					t.Fatalf("blocked WS delivered %d events", len(events))
				}
				unifiedAssertClientFrames(t, record, "ws", events)
				frames := captureFrames(record, capture.DirClientIn, store.KindWSFrame)
				if len(frames) != 1 || !equalCaptureJSON(captureFrameBytes(t, frames[0]), []byte(request)) {
					t.Fatalf("original WS envelope lost: %+v", frames)
				}
			} else {
				response, body, captured := capturedHTTPResponse(t, h, request)
				record, errorBody = captured, body
				if response.StatusCode != http.StatusTooManyRequests {
					t.Fatalf("blocked HTTP status=%d body=%s", response.StatusCode, body)
				}
				assertCapturedJSONBody(t, record, body, "error")
				frames := captureFrames(record, capture.DirClientIn, store.KindRequestBody)
				if len(frames) != 1 || !equalCaptureJSON(captureFrameBytes(t, frames[0]), []byte(request)) {
					t.Fatalf("original HTTP body lost: %+v", frames)
				}
			}
			problem, _ := unifiedObject(t, errorBody)["error"].(map[string]any)
			if problem["code"] != "blocked_by_rule" || problem["message"] != "blocked by integration policy" {
				t.Fatalf("wrong rule rejection: %s", errorBody)
			}
			if calls.Load() != 0 || record.AccountID != 0 || record.AccountName != "" || record.Outcome != store.OutcomeError || record.Error == "" {
				t.Fatalf("blocked request touched routing or lost diagnostics: calls=%d capture=%+v", calls.Load(), record)
			}
			if record.Status != 0 || record.URL != "" || record.RequestBody != "" || record.RequestBytes != 0 || record.ResponseID != "" || record.TotalTokens != 0 {
				t.Fatalf("blocked request invented upstream wire/usage: %+v", record)
			}
			for _, frame := range record.ResponseFrames {
				if frame.Dir == "in" || frame.Dir == "out" {
					t.Errorf("blocked request invented upstream frame: %+v", frame)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), unifiedRulesTimeout)
			defer cancel()
			usage, total, err := h.store.ListUsageRecords(ctx, store.UsageFilter{Limit: 10})
			if err != nil || total != 0 || len(usage) != 0 {
				t.Fatalf("blocked request created usage: total=%d rows=%+v err=%v", total, usage, err)
			}
			traces := unifiedTraces(t, record, "block")
			if len(traces) != 1 || traces[0]["status"] != "blocked" || traces[0]["action_id"] != "reject" {
				t.Fatalf("missing blocked rule provenance: %+v", traces)
			}
		})
	}
}

func TestUnifiedRulesResponseProtocolMatrix(t *testing.T) {
	for _, transport := range []string{"sse", "websocket"} {
		for _, downstream := range []string{"sse", "ws"} {
			t.Run(downstream+"_from_"+transport, func(t *testing.T) {
				h, _, calls := unifiedHarness(t, transport, unifiedRulesEvents)
				rule := unifiedEventRule("replace-delta", "response.output_text.delta", unifiedReplace("replace", "/delta", "hello", "hello!"))
				protocol := "sse"
				if transport != "sse" {
					protocol = "ws"
				}
				// Context exposes the observed wire protocol, not the configured
				// routing mode (websocket, websocket-cached, or auto).
				rule.When = rules.Condition{Op: "all", Conditions: []rules.Condition{
					rule.When,
					{Op: "eq", Source: "context", Path: "/upstream_protocol", Value: protocol},
				}}
				version := publishUnifiedRules(t, h, rule)
				events, record := unifiedStreamResponse(t, h, downstream)
				if calls.Load() != 1 || len(events) != 3 || record.Outcome != store.OutcomeOK || record.RulesVersion != version {
					t.Fatalf("protocol result: calls=%d events=%d outcome=%s version=%d", calls.Load(), len(events), record.Outcome, record.RulesVersion)
				}
				if delta := unifiedObject(t, events[1]); delta["delta"] != "hello!" || delta["item_id"] != "msg_rules_1" || delta["response_id"] != "resp_rules_1" {
					t.Fatalf("delta rewrite missing, duplicated, or identity changed: %s", events[1])
				}
				for _, i := range []int{0, 2} {
					if string(events[i]) != unifiedRulesEvents[i] {
						t.Errorf("unchanged event %d was re-encoded: %s", i, events[i])
					}
				}
				rawFrames := unifiedAssertRawFrames(t, record, transport, unifiedRulesEvents)
				clientFrames := unifiedAssertClientFrames(t, record, downstream, events)
				for i := range clientFrames {
					if rawFrames[i].RuleEventID == "" || clientFrames[i].RuleEventID != rawFrames[i].RuleEventID {
						t.Errorf("event %d missing explicit upstream/client correlation: %q/%q", i, rawFrames[i].RuleEventID, clientFrames[i].RuleEventID)
					}
				}
				traces := unifiedTraces(t, record, "replace-delta")
				changed := 0
				for _, trace := range traces {
					if trace["status"] == "changed" {
						changed++
						if trace["event_id"] != rawFrames[1].RuleEventID || trace["event_type"] != "response.output_text.delta" || trace["sequence"] != float64(2) || trace["action_id"] != "replace" {
							t.Errorf("trace not explicitly associated with its event: %+v", trace)
						}
					}
				}
				if changed != 1 {
					t.Fatalf("text_replace executed %d times, want exactly once: %+v", changed, traces)
				}
				unifiedAssertUsage(t, h, record)
			})
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
			if response.StatusCode != http.StatusOK {
				t.Fatalf("aggregate status=%d body=%s", response.StatusCode, body)
			}
			want := unifiedObject(t, []byte(unifiedRulesEvents[2]))["response"].(map[string]any)
			want["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] = "final-body"
			if !reflect.DeepEqual(unifiedObject(t, body), want) {
				t.Fatalf("aggregate body/identity/usage changed unexpectedly: %s want=%+v", body, want)
			}
			assertCapturedJSONBody(t, record, body, "response")
			rawFrames := unifiedAssertRawFrames(t, record, transport, unifiedRulesEvents)
			frames := captureFrames(record, capture.DirClientOut, store.KindHTTPResponse)
			if frames[0].RuleEventID == "" || frames[0].RuleEventID != rawFrames[2].RuleEventID {
				t.Fatalf("aggregate response lost terminal association: %+v", frames)
			}
			traces := unifiedTraces(t, record, "body")
			if len(traces) != 1 || traces[0]["status"] != "changed" || traces[0]["event_id"] != rawFrames[2].RuleEventID {
				t.Fatalf("response_body did not execute exactly once after event rules: %+v", traces)
			}
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
				if len(events) != 2 || !equalCaptureJSON(events[0], []byte(unifiedRulesEvents[0])) || !equalCaptureJSON(events[1], []byte(unifiedRulesEvents[2])) {
					t.Fatalf("drop must remove only delta and preserve terminal: %s", events)
				}
				raw := unifiedAssertRawFrames(t, record, transport, unifiedRulesEvents)
				client := unifiedAssertClientFrames(t, record, downstream, events)
				if raw[1].RuleEventID == "" || client[1].RuleEventID != raw[2].RuleEventID || client[1].RuleEventID == raw[1].RuleEventID {
					t.Fatalf("drop corrupted terminal correlation: raw=%+v client=%+v", raw, client)
				}
				dropped := 0
				for _, trace := range unifiedTraces(t, record, "drop-delta") {
					if trace["status"] == "dropped" && trace["event_id"] == raw[1].RuleEventID {
						dropped++
					}
				}
				if dropped != 1 {
					t.Fatalf("missing dropped-event trace: %s", record.RuleTraces)
				}
				unifiedAssertUsage(t, h, record)
			})
		}
	}
}

func TestUnifiedRulesRuntimeRollbackAndErrors(t *testing.T) {
	for _, phase := range []string{rules.PhaseRequest, rules.PhaseResponseEvent, rules.PhaseResponseBody} {
		for _, policy := range []string{rules.OnErrorAbort, rules.OnErrorSkipRule} {
			t.Run(phase+"/"+policy, func(t *testing.T) {
				h, observed, calls := unifiedHarness(t, "sse", unifiedRulesEvents)
				path, eventType := "/model", ""
				switch phase {
				case rules.PhaseResponseEvent:
					path, eventType = "/delta", "response.output_text.delta"
				case rules.PhaseResponseBody:
					path = "/output/0/content/0/text"
				}
				failing := unifiedRule("failing", phase, 10,
					unifiedSet("temporary", path, "must-not-leak"),
					unifiedReplace("fail", "/missing-text", "hello", "bad"))
				failing.OnError = policy
				if eventType != "" {
					failing.When = rules.Condition{Op: "eq", Source: "context", Path: "/event_type", Value: eventType}
				}
				publishUnifiedRules(t, h, failing)
				stream := phase == rules.PhaseResponseEvent
				response, body, record := capturedHTTPResponse(t, h, fmt.Sprintf(`{"model":"gpt-5.5","instructions":"original","input":[],"stream":%t}`, stream))
				if bytes.Contains(body, []byte("must-not-leak")) {
					t.Fatalf("failed rule partially committed to client: %s", body)
				}
				traces := unifiedTraces(t, record, "failing")
				rolledBack, failures := 0, 0
				for _, trace := range traces {
					if trace["action_id"] == "temporary" && trace["status"] == "rolled_back" && trace["rolled_back"] == true {
						rolledBack++
						if changes, ok := trace["changes"].([]any); ok && len(changes) != 0 {
							t.Errorf("rolled-back change falsely attributed: %+v", trace)
						}
					}
					if trace["action_id"] == "fail" && trace["rolled_back"] == true && trace["error"] != "" {
						failures++
					}
				}
				if rolledBack != 1 || failures != 1 {
					t.Fatalf("atomic rollback/error traces missing: %+v", traces)
				}
				if policy == rules.OnErrorSkipRule {
					if response.StatusCode != http.StatusOK || record.Outcome != store.OutcomeOK || calls.Load() != 1 {
						t.Fatalf("skip_rule did not continue: status=%d outcome=%s body=%s", response.StatusCode, record.Outcome, body)
					}
					if phase == rules.PhaseRequest {
						_, sent := observed.get()
						if unifiedObject(t, sent)["model"] != "gpt-5.5" || record.Model != "gpt-5.5" {
							t.Fatalf("partial request mutation reached upstream: %s", sent)
						}
					} else if phase == rules.PhaseResponseEvent {
						events := unifiedSSEEvents(t, body)
						if len(events) != 3 || unifiedObject(t, events[1])["delta"] != "hello" {
							t.Fatalf("skip_rule failed to preserve original delta: %s", body)
						}
					} else if !bytes.Contains(body, []byte(`"text":"hello"`)) {
						t.Fatalf("body rollback lost original text: %s", body)
					}
					return
				}
				if record.Outcome != store.OutcomeError {
					t.Fatalf("abort capture outcome=%s", record.Outcome)
				}
				if stream {
					events := unifiedSSEEvents(t, body)
					if response.StatusCode != http.StatusOK || len(events) != 2 || unifiedObject(t, events[0])["type"] != "response.created" {
						t.Fatalf("committed stream must retain first event then one error: status=%d body=%s", response.StatusCode, body)
					}
					unifiedAssertRuleError(t, events[1])
					assertCapturedSSEBody(t, record, body, 2)
				} else {
					if response.StatusCode != http.StatusInternalServerError {
						t.Fatalf("rule abort status=%d want=500 body=%s", response.StatusCode, body)
					}
					unifiedAssertRuleError(t, body)
					assertCapturedJSONBody(t, record, body, "error")
				}
				if phase == rules.PhaseRequest {
					if calls.Load() != 0 || record.AccountID != 0 || record.RequestBytes != 0 {
						t.Fatalf("request rule failure touched upstream: calls=%d capture=%+v", calls.Load(), record)
					}
					ctx, cancel := context.WithTimeout(context.Background(), unifiedRulesTimeout)
					defer cancel()
					_, total, err := h.store.ListUsageRecords(ctx, store.UsageFilter{Limit: 10})
					if err != nil || total != 0 {
						t.Fatalf("request failure invented usage: %d err=%v", total, err)
					}
				}
			})
		}
	}
}

func unifiedAssertRuleError(t *testing.T, raw []byte) {
	t.Helper()
	payload := unifiedObject(t, raw)
	problem, ok := payload["error"].(map[string]any)
	if !ok || problem["code"] != "rule_error" || problem["type"] != "server_error" || problem["message"] == "" {
		t.Fatalf("wrong rule runtime error: %s", raw)
	}
}

func TestUnifiedRulesResponseProtectionAndErrorDeduplication(t *testing.T) {
	for _, downstream := range []string{"sse", "ws"} {
		for _, failure := range []string{"ancestor-replacement", "drop-terminal", "drop-error"} {
			t.Run(downstream+"/"+failure, func(t *testing.T) {
				events := unifiedRulesEvents
				kind := "response.completed"
				action := unifiedSet("protected", "/response", map[string]any{"replacement": "must-not-leak"})
				if failure == "drop-terminal" {
					action = unifiedAction("protected", "drop_event", nil)
				}
				if failure == "drop-error" {
					kind = "error"
					action = unifiedAction("protected", "drop_event", nil)
					events = []string{unifiedRulesEvents[0], `{"type":"error","error":{"type":"server_error","code":"upstream_failed","message":"upstream failure"}}`}
				}
				h, _, _ := unifiedHarness(t, "sse", events)
				rule := unifiedEventRule("protection", kind, unifiedSet("temporary", "/uncommitted", "must-not-leak"), action)
				publishUnifiedRules(t, h, rule)
				delivered, record := unifiedStreamResponse(t, h, downstream)
				wantCount := len(events)
				if len(delivered) != wantCount || record.Outcome != store.OutcomeError {
					t.Fatalf("protection error lost/duplicated: delivered=%s outcome=%s", delivered, record.Outcome)
				}
				unifiedAssertRuleError(t, delivered[len(delivered)-1])
				errorCount := 0
				for _, raw := range delivered {
					payload := unifiedObject(t, raw)
					if payload["type"] == "error" {
						errorCount++
					}
					if bytes.Contains(raw, []byte("must-not-leak")) || payload["type"] == "response.completed" {
						t.Errorf("failed protected event leaked partial result: %s", raw)
					}
				}
				if errorCount != 1 {
					t.Fatalf("delivered %d errors, want one", errorCount)
				}
				unifiedAssertRawFrames(t, record, "sse", events)
				unifiedAssertClientFrames(t, record, downstream, delivered)
				rolledBack, protected := false, false
				for _, trace := range unifiedTraces(t, record, "protection") {
					if trace["action_id"] == "temporary" && trace["status"] == "rolled_back" && trace["rolled_back"] == true {
						rolledBack = true
					}
					if trace["action_id"] == "protected" && trace["status"] == "error" && trace["rolled_back"] == true {
						protected = true
					}
				}
				if !rolledBack || !protected {
					t.Fatalf("protection failed without atomic rollback: %s", record.RuleTraces)
				}
				if failure != "drop-error" && (record.ResponseID != "resp_rules_1" || record.TotalTokens != 18) {
					t.Fatalf("response protection polluted prior raw metering: %+v", record)
				}
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
		call := calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(unifiedRulesTimeout))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", unifiedRulesEvents[0])
		w.(http.Flusher).Flush()
		if call == 1 {
			startOnce.Do(func() { close(started) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			case <-time.After(unifiedRulesTimeout):
				t.Error("timed out waiting to release in-flight rule stream")
				return
			}
		}
		for _, event := range unifiedRulesEvents[1:] {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
			w.(http.Flusher).Flush()
		}
	}), "sse")
	h.server.Client().Timeout = unifiedRulesTimeout
	rule := unifiedEventRule("versioned", "response.output_text.delta", unifiedReplace("replace", "/delta", "hello", "old-version"))
	oldVersion := publishUnifiedRules(t, h, rule)
	ctx, cancel := context.WithTimeout(context.Background(), 2*unifiedRulesTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":[],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+h.key)
	request.Header.Set("Content-Type", "application/json")
	response, err := h.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first upstream request never started")
	}
	// Reading a real client event establishes that this request already loaded
	// and used the old snapshot; publication is not racing request preparation.
	reader := upstream.ParseSSE(response.Body)
	if event, err := reader.Next(); err != nil || event.Type != "response.created" {
		t.Fatalf("first event=%+v err=%v", event, err)
	}
	rule.Actions[0] = unifiedReplace("replace", "/delta", "hello", "new-version")
	newVersion := publishUnifiedRules(t, h, rule)
	if newVersion <= oldVersion {
		t.Fatalf("publication version did not advance: %d -> %d", oldVersion, newVersion)
	}
	releaseOnce.Do(func() { close(release) })
	if event, err := reader.Next(); err != nil || event.Data["delta"] != "old-version" {
		t.Fatalf("in-flight event mixed snapshots: event=%+v err=%v", event, err)
	}
	if event, err := reader.Next(); err != nil || event.Type != "response.completed" {
		t.Fatalf("old stream terminal=%+v err=%v", event, err)
	}
	if _, err := reader.Next(); err != io.EOF {
		t.Fatalf("old stream emitted extra event: %v", err)
	}
	_ = response.Body.Close()
	firstCapture := waitForResponseCapture(t, h)
	if firstCapture.RulesVersion != oldVersion {
		t.Fatalf("in-flight capture version=%d want=%d", firstCapture.RulesVersion, oldVersion)
	}
	// The second request is intentionally made through HTTP, not Service.Load.
	request2, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":[],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	request2.Header = request.Header.Clone()
	response2, err := h.server.Client().Do(request2)
	if err != nil {
		t.Fatal(err)
	}
	body2, readErr := io.ReadAll(response2.Body)
	_ = response2.Body.Close()
	if readErr != nil || response2.StatusCode != http.StatusOK {
		t.Fatalf("new request status=%d body=%s err=%v", response2.StatusCode, body2, readErr)
	}
	events := unifiedSSEEvents(t, body2)
	if len(events) != 3 || unifiedObject(t, events[1])["delta"] != "new-version" || calls.Load() != 2 {
		t.Fatalf("new request did not load latest published snapshot: events=%s calls=%d", events, calls.Load())
	}
	deadline := time.NewTimer(unifiedRulesTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		records, total, err := h.store.ListCaptures(ctx, store.CaptureFilter{Limit: 3, IncludePayloads: true})
		if err != nil {
			t.Fatal(err)
		}
		if total > 2 {
			t.Fatalf("two requests created %d captures", total)
		}
		if len(records) == 2 {
			versions := map[int64]int{}
			for _, record := range records {
				versions[record.RulesVersion]++
				for _, trace := range unifiedTraces(t, record, "versioned") {
					if trace["status"] == "changed" {
						wantRevision := float64(1)
						if record.RulesVersion == newVersion {
							wantRevision = 2
						}
						if trace["revision"] != wantRevision {
							t.Errorf("capture and trace revisions mixed: version=%d trace=%+v", record.RulesVersion, trace)
						}
					}
				}
			}
			if versions[oldVersion] != 1 || versions[newVersion] != 1 {
				t.Fatalf("capture versions=%v", versions)
			}
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
	if response.StatusCode != http.StatusOK {
		t.Fatalf("shape request failed: %d %s", response.StatusCode, body)
	}
	_, raw := observed.get()
	payload := unifiedObject(t, raw)
	if payload["store"] != false || payload["stream"] != true {
		t.Fatalf("user rules overrode required upstream Pi shape: %s", raw)
	}
	userTraces := unifiedTraces(t, record, "user-shape")
	protocolTraces := unifiedTraces(t, record, "protocol-request-finalize")
	if len(userTraces) != 2 || len(protocolTraces) < 3 {
		t.Fatalf("missing protocol rule provenance")
	}
	gateway := protocolTraces[len(protocolTraces)-1]
	if gateway["phase"] != rules.PhaseRequestFinalize || gateway["source"] != "rule" {
		t.Fatalf("protocol change is not a rule: %v", gateway)
	}
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
	if !seen["/store"] || !seen["/stream"] {
		t.Fatalf("gateway trace does not explain overwritten user values: %+v", gateway)
	}
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
			t.Run(fmt.Sprintf("%s/capture=%t", tc.transport, captureEnabled), func(t *testing.T) {
				h, _, calls := unifiedHarness(t, tc.transport, unifiedRulesEvents)
				ctx, cancel := context.WithTimeout(context.Background(), unifiedRulesSetupTimeout)
				defer cancel()
				settings := h.dataPlane.settings.Get()
				settings.CaptureEnabled = captureEnabled
				if err := h.dataPlane.settings.Set(ctx, settings); err != nil {
					t.Fatal(err)
				}
				rule := unifiedEventRule("replace-delta", "response.output_text.delta", unifiedReplace("replace", "/delta", "hello", "hello!"))
				rule.When = rules.Condition{Op: "all", Conditions: []rules.Condition{
					rule.When,
					{Op: "eq", Source: "context", Path: "/upstream_protocol", Value: tc.protocol},
				}}
				version := publishUnifiedRules(t, h, rule)
				status, body := gatewayHTTP(t, h, map[string]any{"model": "gpt-5.5", "input": "hello", "stream": true})
				events := unifiedSSEEvents(t, body)
				if status != http.StatusOK || len(events) != 3 || calls.Load() != tc.calls {
					t.Fatalf("route=%s status=%d events=%d calls=%d want=%d body=%s", tc.transport, status, len(events), calls.Load(), tc.calls, body)
				}
				if unifiedObject(t, events[1])["delta"] != "hello!" {
					t.Fatalf("route %s did not expose actual %q protocol or applied the rule repeatedly: %s", tc.transport, tc.protocol, events[1])
				}
				if string(events[0]) != unifiedRulesEvents[0] || string(events[2]) != unifiedRulesEvents[2] {
					t.Fatalf("cached/auto route modified untouched events: %s", body)
				}
				if captureEnabled {
					record := waitForResponseCapture(t, h)
					if record.RulesVersion != version || record.Outcome != store.OutcomeOK {
						t.Fatalf("cached/auto capture version/outcome=%d/%s", record.RulesVersion, record.Outcome)
					}
					raw := unifiedAssertRawFrames(t, record, tc.protocol, unifiedRulesEvents)
					client := unifiedAssertClientFrames(t, record, "sse", events)
					if raw[1].RuleEventID == "" || raw[1].RuleEventID != client[1].RuleEventID {
						t.Fatalf("cached/fallback protocol lost event association: %q/%q", raw[1].RuleEventID, client[1].RuleEventID)
					}
					changed := 0
					for _, trace := range unifiedTraces(t, record, "replace-delta") {
						if trace["status"] == "changed" {
							changed++
						}
					}
					if changed != 1 {
						t.Fatalf("cached/auto rewrite count=%d want=1", changed)
					}
					unifiedAssertUsage(t, h, record)
				} else {
					// A final usage row proves finish ran before asserting absence.
					usage := waitForUsageRecord(t, h.store)
					if usage.Outcome != "succeeded" || usage.TotalTokens == nil || *usage.TotalTokens != 18 {
						t.Fatalf("capture-disabled protocol rewrite changed usage: %+v", usage)
					}
					records, total, err := h.store.ListCaptures(ctx, store.CaptureFilter{Limit: 2, IncludePayloads: true})
					if err != nil || total != 0 || len(records) != 0 {
						t.Fatalf("capture-disabled request persisted capture: total=%d err=%v", total, err)
					}
				}
			})
		}
	}
}

func TestUnifiedRulesBlockedCaptureDisabledOrNotPersisted(t *testing.T) {
	for _, mode := range []string{"disabled", "not-persisted"} {
		for _, downstream := range []string{"sse", "ws"} {
			t.Run(mode+"/"+downstream, func(t *testing.T) {
				h, _, calls := unifiedHarness(t, "sse", unifiedRulesEvents)
				ctx, cancel := context.WithTimeout(context.Background(), unifiedRulesSetupTimeout)
				defer cancel()
				if mode == "disabled" {
					settings := h.dataPlane.settings.Get()
					settings.CaptureEnabled = false
					if err := h.dataPlane.settings.Set(ctx, settings); err != nil {
						t.Fatal(err)
					}
				} else {
					h.dataPlane.cfg.Capture.Persist = false
				}
				publishUnifiedRules(t, h, unifiedRule("block", rules.PhaseRequest, 1,
					unifiedAction("reject", "reject_request", map[string]any{"status": 403, "message": "blocked without persistence"})))
				h.dataPlane.accounts = nil // No account-selection call is allowed.
				var body []byte
				if downstream == "sse" {
					status, raw := gatewayHTTP(t, h, map[string]any{"model": "gpt-5.5", "input": "blocked", "stream": true})
					body = raw
					if status != http.StatusForbidden {
						t.Fatalf("block status=%d body=%s", status, body)
					}
				} else {
					conn := unifiedDialWS(t, h)
					if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.5","input":"blocked"}`)); err != nil {
						t.Fatal(err)
					}
					_, raw, err := conn.ReadMessage()
					if err != nil {
						t.Fatal(err)
					}
					body = raw
					_ = conn.Close()
				}
				problem, _ := unifiedObject(t, body)["error"].(map[string]any)
				if problem["code"] != "blocked_by_rule" || problem["message"] != "blocked without persistence" {
					t.Fatalf("capture setting bypassed rule rejection: %s", body)
				}
				// Join HTTP handlers and wait for any hijacked WS exchange to finish;
				// absence assertions must not race the deferred persistence path.
				h.server.Close()
				gatewayAwait(t, func() bool {
					h.dataPlane.lifecycle.mu.Lock()
					defer h.dataPlane.lifecycle.mu.Unlock()
					return len(h.dataPlane.lifecycle.active) == 0
				}, "blocked downstream did not finish")
				records, captureCount, err := h.store.ListCaptures(ctx, store.CaptureFilter{Limit: 2, IncludePayloads: true})
				if err != nil || captureCount != 0 || len(records) != 0 {
					t.Fatalf("%s block persisted captures: total=%d records=%+v err=%v", mode, captureCount, records, err)
				}
				usage, usageCount, err := h.store.ListUsageRecords(ctx, store.UsageFilter{Limit: 2})
				if err != nil || usageCount != 0 || len(usage) != 0 || calls.Load() != 0 {
					t.Fatalf("%s block accessed upstream or created usage: calls=%d count=%d rows=%+v err=%v", mode, calls.Load(), usageCount, usage, err)
				}
			})
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
	if firstOutput["arguments"] != `{"query":"client-only"}` || firstOutput["id"] != "fc_rules" || firstOutput["call_id"] != "call_rules" {
		t.Fatalf("client tool text was not rewritten without changing associations: %+v", firstOutput)
	}
	if firstCapture.ResponseID != firstID || firstCapture.RulesVersion != version || firstCapture.TotalTokens != 4 {
		t.Fatalf("first cached response lost raw identity/usage: %+v", firstCapture)
	}
	result := map[string]any{"type": "function_call_output", "call_id": "call_rules", "output": "result"}
	// Submit full history containing the ORIGINAL upstream output. If response
	// rewriting mutated the pooled baseline, this cannot reduce to a two-item delta.
	secondInput := []any{gatewayUser("first"), originalCall, result, gatewayUser("second")}
	secondEvent := gatewayExchange(t, conn, gatewayCreate(secondInput, "", ""))
	secondID := gatewayResponseID(t, secondEvent)
	secondTurn := gatewayObservedTurn(t, backend)
	if secondID == firstID || secondTurn.connection != firstTurn.connection || backend.connections.Load() != 1 || backend.httpCalls.Load() != 0 {
		t.Fatalf("continuation did not reuse the original WS exactly once: first=%+v second=%+v connections=%d HTTP=%d", firstTurn, secondTurn, backend.connections.Load(), backend.httpCalls.Load())
	}
	if secondTurn.body["previous_response_id"] != firstID {
		t.Fatalf("response rewrite polluted baseline parent optimization: %+v", secondTurn.body)
	}
	delta, _ := secondTurn.body["input"].([]any)
	if len(delta) != 2 || !reflect.DeepEqual(delta[0], result) {
		t.Fatalf("original baseline did not produce tool-result/new-input delta: %+v", secondTurn.body)
	}
	secondOutput := secondEvent["response"].(map[string]any)["output"].([]any)[0].(map[string]any)
	if secondOutput["arguments"] != `{"query":"client-only"}` {
		t.Fatalf("reused WS did not expose actual ws protocol to response rules: %+v", secondOutput)
	}
	ctx, cancel := context.WithTimeout(context.Background(), unifiedRulesTimeout)
	defer cancel()
	var captures []*store.Capture
	gatewayAwait(t, func() bool {
		var total int
		var err error
		captures, total, err = h.store.ListCaptures(ctx, store.CaptureFilter{Limit: 3, IncludePayloads: true})
		if err != nil || total > 2 {
			t.Fatalf("continuation capture count=%d err=%v", total, err)
		}
		return len(captures) == 2
	}, "second cached response did not finish its capture")
	for _, record := range captures {
		raw := captureFrames(record, "in", store.KindWSFrame)
		client := captureFrames(record, capture.DirClientOut, store.KindWSFrame)
		if len(raw) != 2 || len(client) != 2 || record.Outcome != store.OutcomeOK || record.RulesVersion != version || record.TotalTokens != 4 {
			t.Fatalf("cached continuation lost events or usage: %+v", record)
		}
		rawOutput := unifiedObject(t, captureFrameBytes(t, raw[1]))["response"].(map[string]any)["output"].([]any)[0].(map[string]any)
		clientOutput := unifiedObject(t, captureFrameBytes(t, client[1]))["response"].(map[string]any)["output"].([]any)[0].(map[string]any)
		if rawOutput["arguments"] != `{"query":"hello"}` || clientOutput["arguments"] != `{"query":"client-only"}` || raw[1].RuleEventID == "" || raw[1].RuleEventID != client[1].RuleEventID {
			t.Fatalf("cached response original/client payload or event correlation changed: raw=%+v client=%+v", raw, client)
		}
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
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.StatusCode, body)
			}
			headers, raw := observed.get()
			if headers.Get("User-Agent") != "custom-rule-client" || headers.Get("X-Stainless-Package-Version") != "test-version" {
				t.Fatalf("metadata rules not applied: %#v", headers)
			}
			if headers.Get("Authorization") == "" {
				t.Fatal("gateway credential missing")
			}
			if transport == "websocket" {
				observed.mu.Lock()
				raw = append([]byte(nil), observed.wsFrame...)
				observed.mu.Unlock()
			}
			payload := unifiedObject(t, raw)
			input := payload["input"].([]any)
			content := input[0].(map[string]any)["content"].([]any)
			if content[0].(map[string]any)["text"] != "changed before normalization" {
				t.Fatalf("client phase lost before construction: %s", raw)
			}
			if len(unifiedTraces(t, record, "metadata-change")) == 0 {
				t.Fatal("missing metadata provenance")
			}
		})
	}
}
