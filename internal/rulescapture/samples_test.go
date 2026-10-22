package rulescapture

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"pi-gateway/internal/capture"
	"pi-gateway/internal/piwire"
	"pi-gateway/internal/rules"
	"pi-gateway/internal/store"
)

func frame(seq int, dir, kind, typ, raw string) store.Frame {
	return store.Frame{Seq: seq, Dir: dir, Kind: kind, Type: typ, Bytes: len(raw), Data: json.RawMessage(raw)}
}
func object(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err = json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
func emptyEngine(t *testing.T) *rules.Engine {
	t.Helper()
	e, err := rules.Compile(nil)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func fixtureRule(id, phase string, action rules.Action) rules.Rule {
	return rules.Rule{SchemaVersion: 1, ID: id, Name: id, Enabled: true, Phase: phase, When: rules.Condition{Op: "always"}, Actions: []rules.Action{action}, OnError: rules.OnErrorAbort}
}

func TestRequestUsesCheckpointOrClientNormalizationNeverUpstream(t *testing.T) {
	client := map[string]any{"model": "alias", "input": "hello", "prompt_cache_key": "explicit"}
	raw, _ := json.Marshal(client)
	rt := &store.Settings{DefaultModel: "fallback", ModelMappings: map[string]string{"alias": "mapped"}, ReasoningEffort: "high"}
	for _, kind := range []string{store.KindRequestBody, store.KindWSFrame} {
		t.Run(kind, func(t *testing.T) {
			payload := string(raw)
			if kind == store.KindWSFrame {
				payload = `{"type":"response.create","model":"alias","input":"hello","prompt_cache_key":"explicit"}`
			}
			c := &store.Capture{Model: "already-rewritten", ClientTransport: "ws", APIKeyID: 7, RequestBody: `{"model":"incorrect-upstream","input":[]}`, Outcome: store.OutcomeOK, ResponseFrames: []store.Frame{frame(2, "client_in", kind, "", payload)}}
			sample, err := Request(c, rt)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			reportTestFailure(t, sample.Source != "reconstructed" || sample.Input.Model != "mapped" || object(t, sample.Input.Body)["model"] != "mapped", func() {
				t.Fatalf("sample=%+v", sample)
			})
			expected, err := piwire.BuildRequest(client, piwire.BuildOptions{DefaultModel: rt.DefaultModel, ModelMappings: rt.ModelMappings, DefaultReasoningEffort: rt.ReasoningEffort, SessionID: "explicit"})
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			actual, _ := json.Marshal(sample.Input.Body)
			want, _ := json.Marshal(expected.Body)
			reportTestFailure(t, string(actual) != string(want), func() {
				t.Fatalf("normalization diverged: %s != %s", actual, want)
			})
			reportTestFailure(t, sample.Input.Context["api_key_id"] != int64(7), func() {
				t.Fatalf("identity=%v", sample.Input.Context)
			})
			c.Truncated = true
			{
				_, err := Request(c, rt)
				reportTestFailure(t, err != nil, func() {
					t.Fatalf("complete client frame rejected solely due to global truncation: %v", err)
				})
			}
			c.ResponseFrames = nil
			{
				_, err := Request(c, rt)
				reportTestFailure(t, err == nil, func() {
					t.Fatal("used final upstream request as original")
				})
			}
		})
	}
	recorder := capture.New(nil, capture.Options{})
	recorder.OnRuleInput(rules.PhaseRequest, &rules.Input{Body: map[string]any{"model": "checkpoint", "x": nil}, Model: "checkpoint", Context: map[string]any{"request_path": "/actual"}}, false)
	c := recorder.Finalize(store.OutcomeError, "")
	sample, err := Request(c, rt)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	reportTestFailure(t, sample.Source != "checkpoint" || sample.Input.Model != "checkpoint" || sample.Input.Context["request_path"] != "/actual", func() {
		t.Fatalf("sample=%+v", sample)
	})
	{
		v, exists := object(t, sample.Input.Body)["x"]
		reportTestFailure(t, !exists || v != nil, func() {
			t.Fatal("lost explicit null")
		})
	}
}

func TestTruncatedValidJSONPrefixAndEventIdentity(t *testing.T) {
	c := &store.Capture{Model: "m", Outcome: store.OutcomeOK, Truncated: true}
	raw := `{"type":"response.output_text.delta","delta":"original"}`
	for _, kind := range []string{store.KindSSEEvent, store.KindWSFrame} {
		f := frame(9, "in", kind, "response.output_text.delta", raw)
		sample, err := Event(c, f)
		if err != nil {
			t.Fatal(err)
		}
		if sample.Source != "recorded_event" || sample.EventType != "response.output_text.delta" || object(t, sample.Input.Body)["delta"] != "original" {
			t.Fatalf("sample=%+v", sample)
		}
		bad := f
		bad.Dir = "client_out"
		if _, err := Event(c, bad); err == nil {
			t.Fatal("accepted client output")
		}
		bad = f
		bad.Type = "response.completed"
		if _, err := Event(c, bad); err == nil {
			t.Fatal("accepted mismatched event type")
		}
		bad = f
		bad.Data = nil
		bad.Text = raw
		bad.Bytes = len(raw) + 100
		if _, err := Event(c, bad); err == nil {
			t.Fatal("accepted valid JSON prefix of a truncated event")
		}
	}
	c.ResponseFrames = []store.Frame{{Seq: 1, Dir: "client_in", Kind: store.KindRequestBody, Text: `{"model":"m"}`, Bytes: 200}}
	if _, err := Request(c, &store.Settings{}); err == nil {
		t.Fatal("accepted truncated client request")
	}
	c.ResponseFrames = []store.Frame{frame(1, "client_out", store.KindWSFrame, "response.completed", `{"type":"response.completed"}`)}
	if len(EventFrames(c)) != 0 {
		t.Fatal("client output masqueraded as upstream event")
	}
}

func TestEventMissingContextIsLazyAndNeverGuessed(t *testing.T) {
	c := &store.Capture{Model: "routed-model", Outcome: store.OutcomeOK}
	sample, err := Event(c, frame(4, "in", store.KindSSEEvent, "response.completed", `{"type":"response.completed","response":{"output":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	r := fixtureRule("test", rules.PhaseResponseEvent, rules.Action{ID: "set", Type: "json_set", Params: map[string]any{"path": "/marker", "value": true}})
	r.When = rules.Condition{Op: "eq", Source: "context", Path: "/model", Value: "routed-model"}
	engine, err := rules.Compile([]rules.Rule{r})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(context.Background(), r.Phase, sample.Input); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/original_model", "/request_path", "/request_method", "/account_id", "/api_key_id"} {
		r.When = rules.Condition{Op: "exists", Source: "context", Path: path}
		engine, err = rules.Compile([]rules.Rule{r})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Apply(context.Background(), r.Phase, sample.Input); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("missing %s silently guessed: %v", path, err)
		}
	}
	r.When = rules.Condition{Op: "any", Conditions: []rules.Condition{{Op: "always"}, {Op: "exists", Source: "context", Path: "/request_path"}}}
	engine, err = rules.Compile([]rules.Rule{r})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(context.Background(), r.Phase, sample.Input); err != nil {
		t.Fatalf("checked unexecuted missing context: %v", err)
	}
}

func aggregateCapture() *store.Capture {
	return &store.Capture{Model: "m", Outcome: store.OutcomeOK, Status: 200, ClientTransport: "http", ResponseFrames: []store.Frame{
		frame(1, "client_in", store.KindRequestBody, "", `{"model":"m","input":[],"stream":false}`),
		frame(2, "in", store.KindSSEEvent, "response.completed", `{"type":"response.completed","response":{"id":"r","status":"completed","output":[],"label":"original"}}`),
		frame(3, "client_out", store.KindHTTPResponse, "response", `{"id":"r","label":"already-body-transformed"}`),
	}}
}

func TestAggregateReconstructionUsesRealEventRulesAndPhaseEvidence(t *testing.T) {
	c := aggregateCapture()
	r := fixtureRule("event", rules.PhaseResponseEvent, rules.Action{ID: "set", Type: "json_set", Params: map[string]any{"path": "/response/label", "value": "event-stage"}})
	engine, err := rules.Compile([]rules.Rule{r})
	if err != nil {
		t.Fatal(err)
	}
	sample, err := ResponseBody(context.Background(), c, engine)
	if err != nil {
		t.Fatal(err)
	}
	if sample.Source != "reconstructed" || object(t, sample.Input.Body)["label"] != "event-stage" || sample.Input.EventType != "" {
		t.Fatalf("aggregate=%+v", sample)
	}
	cases := []struct {
		name   string
		change func(*store.Capture)
	}{
		{"truncated", func(c *store.Capture) { c.Truncated = true }},
		{"error", func(c *store.Capture) { c.Outcome = store.OutcomeError }},
		{"streaming", func(c *store.Capture) {
			c.ResponseFrames[0] = frame(1, "client_in", store.KindRequestBody, "", `{"model":"m","stream":true}`)
		}},
		{"ws", func(c *store.Capture) { c.ClientTransport = "ws" }},
		{"no-terminal", func(c *store.Capture) { c.ResponseFrames = c.ResponseFrames[:1] }},
		{"client-final-only", func(c *store.Capture) { c.ResponseFrames = append(c.ResponseFrames[:1], c.ResponseFrames[2]) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := aggregateCapture()
			tc.change(bad)
			if _, err := ResponseBody(context.Background(), bad, engine); err == nil {
				t.Fatal("guessed aggregate input")
			}
		})
	}
	r.Actions = []rules.Action{{ID: "drop", Type: "drop_event", Params: map[string]any{}}}
	engine, err = rules.Compile([]rules.Rule{r})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResponseBody(context.Background(), c, engine); err == nil {
		t.Fatal("used terminal dropped by current event rules")
	}
}

func TestBodyCheckpointIsExactAndContextIndependent(t *testing.T) {
	recorder := capture.New(nil, capture.Options{})
	recorder.OnRuleInput(rules.PhaseResponseBody, &rules.Input{Body: map[string]any{"label": "before-body-rules"}, Model: "m", ClientBody: map[string]any{"stream": false}, Context: map[string]any{"original_model": "old", "account_id": 3}}, false)
	c := recorder.Finalize(store.OutcomeError, "later response rule failed")
	c.Truncated = true
	sample, err := ResponseBody(context.Background(), c, emptyEngine(t))
	if err != nil {
		t.Fatal(err)
	}
	if sample.Source != "checkpoint" || object(t, sample.Input.Body)["label"] != "before-body-rules" || sample.Input.Context["original_model"] != "old" {
		t.Fatalf("sample=%+v", sample)
	}
	c.ResponseFrames[0].Data = nil
	if _, err := ResponseBody(context.Background(), c, emptyEngine(t)); err == nil {
		t.Fatal("used omitted checkpoint")
	}
}
