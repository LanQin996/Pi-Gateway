package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"pi-gateway/internal/capture"
	"pi-gateway/internal/rules"
	"pi-gateway/internal/rulesruntime"
	"pi-gateway/internal/store"
)

func simRule(id, phase string, actions ...rules.Action) rules.Rule {
	return rules.Rule{SchemaVersion: 1, ID: id, Name: id, Enabled: true, Phase: phase, When: rules.Condition{Op: "always"}, Actions: append([]rules.Action{}, actions...), OnError: rules.OnErrorAbort}
}
func simAction(id, kind, path string, value any) rules.Action {
	return rules.Action{ID: id, Type: kind, Params: map[string]any{"path": path, "value": value}}
}
func simFrame(seq int, dir, kind, typ, body string) store.Frame {
	return store.Frame{Seq: seq, Dir: dir, Kind: kind, Type: typ, Bytes: len(body), Data: json.RawMessage(body)}
}
func saveSimCapture(t *testing.T, st *store.Store, c *store.Capture) *store.Capture {
	t.Helper()
	if c.Outcome == "" {
		c.Outcome = store.OutcomeOK
	}
	{
		err := st.InsertCapture(context.Background(), c, 0)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	return c
}
func simJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	return string(raw)
}
func simPost(t *testing.T, mux http.Handler, payload map[string]any, status int) captureSimulationResponse {
	t.Helper()
	raw := ruleHTTP(t, mux, "POST", "/api/rules/simulate-capture", simJSON(t, payload), status)
	var out captureSimulationResponse
	{
		err := json.Unmarshal([]byte(simJSON(t, raw)), &out)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	return out
}
func simBody(t *testing.T, value any) map[string]any {
	t.Helper()
	var out map[string]any
	{
		err := json.Unmarshal([]byte(simJSON(t, value)), &out)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	return out
}
func simStoredRule(t *testing.T, st *store.Store, r rules.Rule) rules.Rule {
	t.Helper()
	version, err := st.RuleSetVersion(context.Background())
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	raw, err := rulesruntime.EditableJSON(r)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	snapshot, err := st.PublishRules(context.Background(), &version, []store.RuleChange{{Kind: "create", ID: r.ID, Rule: raw}}, rulesruntime.ValidateSnapshot)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	decoded, err := rulesruntime.DecodeSnapshot(snapshot)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	for _, x := range decoded {
		if x.ID == r.ID {
			return x
		}
	}
	t.Fatal("missing published rule")
	return r
}

// Include values, not just counts: accidental account/session/key updates must fail.
func simBusinessState(t *testing.T, st *store.Store) string {
	t.Helper()
	state := map[string]any{}
	for _, table := range []string{"rules", "rule_set_metadata", "middlewares", "captures", "accounts", "api_keys", "usage_records", "settings"} {
		rows, err := st.QueryContext(context.Background(), "SELECT * FROM "+table)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
		columns, err := rows.Columns()
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
		data := [][]any{}
		for rows.Next() {
			values := make([]any, len(columns))
			dest := make([]any, len(columns))
			for i := range values {
				dest[i] = &values[i]
			}
			{
				err = rows.Scan(dest...)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			for i, v := range values {
				if b, ok := v.([]byte); ok {
					values[i] = string(b)
				}
			}
			data = append(data, values)
		}
		{
			err = rows.Err()
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		rows.Close()
		state[table] = data
	}
	return simJSON(t, state)
}

func TestCaptureSimulationAuthenticationReadonlyAndNoUpstream(t *testing.T) {
	var upstreamCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		t.Error("simulation called upstream")
		http.Error(w, "forbidden", 503)
	}))
	defer target.Close()
	h := newProxyHarness(t, target.URL, "sse")
	{
		err := rulesruntime.New(h.db).Ensure(context.Background())
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	c := saveSimCapture(t, h.db, &store.Capture{APIKeyID: 1, ClientTransport: "http", Model: "historical-final", RequestBody: `{"model":"already-mutated"}`, ResponseFrames: []store.Frame{simFrame(2, "client_in", store.KindRequestBody, "", `{"model":"original","input":"hello"}`)}})
	draft := simRule("offline", rules.PhaseRequest, rules.Action{ID: "model", Type: "rewrite_model", Params: map[string]any{"model": "simulated"}})
	payload := simJSON(t, map[string]any{"capture_id": c.ID, "rule": draft, "phase": draft.Phase})
	h.request(t, "POST", "/api/rules/simulate-capture", payload, 401, false)
	before := simBusinessState(t, h.db)
	result := h.request(t, "POST", "/api/rules/simulate-capture", payload, 200, true)
	reportTestFailure(t, result["valid"] != true, func() {
		t.Fatalf("result=%v", result)
	})
	after := simBusinessState(t, h.db)
	reportTestFailure(t, before != after, func() {
		t.Fatal("simulation modified business state")
	})
	reportTestFailure(t, upstreamCalls.Load() != 0, func() {
		t.Fatalf("upstream calls=%d", upstreamCalls.Load())
	})
	items := result["results"].([]any)
	item := items[0].(map[string]any)
	reportTestFailure(t, item["source"] != "reconstructed" || item["before"].(map[string]any)["model"] != "original" || item["result"].(map[string]any)["model"] != "simulated", func() {
		t.Fatalf("result=%v", result)
	})
}

func TestCaptureSimulationDoesNotMigrateAndRuleOnlyWorks(t *testing.T) {
	st, mux := newRulesAdmin(t, false)
	c := saveSimCapture(t, st, &store.Capture{ResponseFrames: []store.Frame{simFrame(1, "client_in", store.KindRequestBody, "", `{"model":"m"}`)}})
	request := map[string]any{"capture_id": c.ID, "rule": simRule("draft", rules.PhaseRequest), "phase": "request"}
	before := simBusinessState(t, st)
	simPost(t, mux, request, 422)
	request["scope"] = "rule"
	out := simPost(t, mux, request, 200)
	reportTestFailure(t, out.RulesVersion != 0, func() {
		t.Fatal("unexpected migration")
	})
	reportTestFailure(t, before != simBusinessState(t, st), func() {
		t.Fatal("simulation migrated or wrote legacy configuration")
	})
}

func TestCaptureSimulationReplacementOrderingEnableStopAndRollback(t *testing.T) {
	st, mux := newRulesAdmin(t, true)
	c := saveSimCapture(t, st, &store.Capture{ResponseFrames: []store.Frame{simFrame(1, "client_in", store.KindRequestBody, "", `{"model":"m","input":[]}`)}})
	first := simStoredRule(t, st, simRule("a-first", rules.PhaseRequest, simAction("set", "json_set", "/marker", "saved")))
	draft := first
	draft.Actions[0].Params["value"] = "draft"
	request := map[string]any{"capture_id": c.ID, "rule": draft, "phase": "request"}
	out := simPost(t, mux, request, 200)
	reportTestFailure(t, len(out.Results[0].Result.Traces) != 1 || simBody(t, out.Results[0].Result.Body)["marker"] != "draft", func() {
		t.Fatalf("replacement=%+v", out)
	})
	// IDs would sort before the existing rule if caller-controlled order_index leaked in.
	newer := simRule("0-new", rules.PhaseRequest, simAction("set", "json_set", "/marker", "new-last"))
	newer.OrderIndex = -100
	request["rule"] = newer
	out = simPost(t, mux, request, 200)
	reportTestFailure(t, simBody(t, out.Results[0].Result.Body)["marker"] != "new-last", func() {
		t.Fatal("new draft did not follow publication append order")
	})
	newer.Enabled = false
	request["rule"] = newer
	out = simPost(t, mux, request, 200)
	reportTestFailure(t, simBody(t, out.Results[0].Result.Body)["marker"] != "saved", func() {
		t.Fatal("disabled draft executed")
	})
	request["enable_draft"] = true
	out = simPost(t, mux, request, 200)
	reportTestFailure(t, simBody(t, out.Results[0].Result.Body)["marker"] != "new-last", func() {
		t.Fatal("temporary enable not applied")
	})
	request["scope"] = "rule"
	request["enable_draft"] = false
	out = simPost(t, mux, request, 200)
	reportTestFailure(t, out.Results[0].Result.Changed, func() {
		t.Fatal("disabled single rule changed input")
	})
	failed := simRule("failing", rules.PhaseRequest, simAction("early", "json_set", "/early", true), rules.Action{ID: "fail", Type: "json_remove", Params: map[string]any{"paths": []any{"/missing"}, "on_missing": "error"}})
	failed.OnError = rules.OnErrorSkipRule
	request["rule"] = failed
	out = simPost(t, mux, request, 200)
	reportTestFailure(t, out.Results[0].Result.Changed || !out.Results[0].Result.Traces[0].RolledBack || out.Results[0].Result.Traces[0].Status != "rolled_back", func() {
		t.Fatalf("rollback=%+v", out.Results[0])
	})
	stop := simRule("stop", rules.PhaseRequest)
	stop.Priority = -1
	stop.StopAfterMatch = true
	simStoredRule(t, st, stop)
	request["scope"] = "ruleset"
	request["rule"] = newer
	request["enable_draft"] = true
	out = simPost(t, mux, request, 200)
	reportTestFailure(t, out.Results[0].Result.Changed || len(out.Results[0].Result.Traces) != 1, func() {
		t.Fatalf("stop=%+v", out.Results[0])
	})
}

func TestCaptureSimulationEventPagingTokensVersionsAndTruncation(t *testing.T) {
	st, mux := newRulesAdmin(t, true)
	c := &store.Capture{Model: "m", Truncated: true}
	for i := 0; i < 55; i++ {
		kind := store.KindSSEEvent
		if i%2 == 1 {
			kind = store.KindWSFrame
		}
		c.ResponseFrames = append(c.ResponseFrames, simFrame(i*2, "in", kind, "response.output_text.delta", fmt.Sprintf(`{"type":"response.output_text.delta","delta":%q}`, fmt.Sprintf("original-%d", i))), simFrame(i*2+1, "client_out", kind, "response.output_text.delta", `{"type":"response.output_text.delta","delta":"modified-client"}`))
	}
	// Preserve an unavailable frame in the batch and do not execute its valid prefix.
	c.ResponseFrames[4].Text = `{"type":"response.output_text.delta","delta":"prefix"}`
	c.ResponseFrames[4].Data = nil
	saveSimCapture(t, st, c)
	draft := simRule("event", rules.PhaseResponseEvent, simAction("replace", "json_set", "/delta", "simulated"))
	request := map[string]any{"capture_id": c.ID, "rule": draft, "phase": draft.Phase, "batch": true}
	before := simBusinessState(t, st)
	out := simPost(t, mux, request, 200)
	reportTestFailure(t, len(out.Results) != 50 || out.Total != 55 || !out.HasMore || out.NextAfterSeq == nil || *out.NextAfterSeq != 98 || out.BatchToken == "", func() {
		t.Fatalf("page=%+v", out)
	})
	reportTestFailure(t, out.Results[2].Error == "" || out.Results[2].Before != nil, func() {
		t.Fatal("batch executed truncated event")
	})
	reportTestFailure(t, simBody(t, out.Results[1].Before)["delta"] != "original-1" || simBody(t, out.Results[1].Result.Body)["delta"] != "simulated", func() {
		t.Fatal("used client_out event")
	})
	request["after_seq"] = *out.NextAfterSeq
	simPost(t, mux, request, 400)
	request["expected_version"] = out.RulesVersion
	simPost(t, mux, request, 400)
	request["batch_token"] = out.BatchToken
	next := simPost(t, mux, request, 200)
	reportTestFailure(t, len(next.Results) != 5 || next.Total != 55 || next.HasMore || *next.Results[0].FrameSeq != 100, func() {
		t.Fatalf("next=%+v", next)
	})
	changed := draft
	changed.Name = "changed draft"
	request["rule"] = changed
	simPost(t, mux, request, 409)
	request["rule"] = draft
	reportTestFailure(t, before != simBusinessState(t, st), func() {
		t.Fatal("paging changed state")
	})
	simStoredRule(t, st, simRule("published-during-batch", rules.PhaseRequest))
	simPost(t, mux, request, 409)
	single := map[string]any{"capture_id": c.ID, "rule": draft, "phase": draft.Phase, "frame_seq": 2}
	selected := simPost(t, mux, single, 200)
	reportTestFailure(t, len(selected.Results) != 1 || *selected.Results[0].FrameSeq != 2 || selected.Results[0].Source != "recorded_event", func() {
		t.Fatalf("single=%+v", selected)
	})
	single["frame_seq"] = 3
	simPost(t, mux, single, 422)
	single["frame_seq"] = 4
	simPost(t, mux, single, 422)
	single["limit"] = 201
	simPost(t, mux, single, 400)
}

func TestCaptureSimulationContextValidationAndPreviewBudgets(t *testing.T) {
	st, mux := newRulesAdmin(t, true)
	c := saveSimCapture(t, st, &store.Capture{ResponseFrames: []store.Frame{simFrame(1, "client_in", store.KindRequestBody, "", `{"model":"m"}`)}})
	draft := simRule("unknown", rules.PhaseRequest)
	draft.When = rules.Condition{Op: "not_exists", Source: "context", Path: "/request_path"}
	request := map[string]any{"capture_id": c.ID, "rule": draft, "phase": "request"}
	out := simPost(t, mux, request, 200)
	reportTestFailure(t, !strings.Contains(out.Results[0].Error, "/context/request_path") || out.Results[0].Result.Changed, func() {
		t.Fatalf("guessed missing context: %+v", out)
	})
	draft.When = rules.Condition{Op: "all", Conditions: []rules.Condition{{Op: "exists", Path: "/no"}, draft.When}}
	request["rule"] = draft
	out = simPost(t, mux, request, 200)
	reportTestFailure(t, out.Results[0].Error != "", func() {
		t.Fatal("missing context checked in short-circuited condition")
	})
	traces := out.Results[0].Result.ConditionTraces
	reportTestFailure(t, len(traces) != 3 || traces[1].Status != "skipped", func() {
		t.Fatalf("conditions=%+v", traces)
	})
	recorder := capture.New(nil, capture.Options{})
	recorder.OnRuleInput(rules.PhaseRequest, &rules.Input{Body: map[string]any{"model": "m", "huge": strings.Repeat("x", 100<<10)}, Model: "m"}, false)
	large := recorder.Finalize(store.OutcomeOK, "")
	saveSimCapture(t, st, large)
	request["capture_id"] = large.ID
	request["rule"] = simRule("preview", rules.PhaseRequest)
	out = simPost(t, mux, request, 200)
	reportTestFailure(t, !out.Results[0].PreviewTruncated || simBody(t, out.Results[0].Before)["truncated"] != true, func() {
		t.Fatal("large preview was not bounded")
	})
	reportTestFailure(t, len(simJSON(t, out)) > simulationPreviewBudget+simulationDiagnosticBudget, func() {
		t.Fatal("response exceeded aggregate diagnostics/preview budget")
	})
	request["capture_id"] = 999999
	simPost(t, mux, request, 404)
	request["capture_id"] = c.ID
	request["rule"] = map[string]any{"schema_version": 1, "enabled": true, "phase": "request", "when": map[string]any{"op": "always"}, "actions": []any{}, "unknown": true}
	simPost(t, mux, request, 400)
}

func TestCaptureSimulationAggregateBudgetsAndCancellation(t *testing.T) {
	st, mux := newRulesAdmin(t, true)
	c := &store.Capture{Model: "m"}
	payload := fmt.Sprintf(`{"type":"response.output_text.delta","delta":%q}`, strings.Repeat("x", 12000))
	for i := 0; i < 200; i++ {
		c.ResponseFrames = append(c.ResponseFrames, simFrame(i, "in", store.KindSSEEvent, "response.output_text.delta", payload))
	}
	saveSimCapture(t, st, c)
	draft := simRule("many", rules.PhaseResponseEvent)
	draft.When = rules.Condition{Op: "all"}
	for i := 0; i < 100; i++ {
		draft.When.Conditions = append(draft.When.Conditions, rules.Condition{Op: "always"})
	}
	request := map[string]any{"capture_id": c.ID, "rule": draft, "phase": draft.Phase, "batch": true, "limit": 200}
	out := simPost(t, mux, request, 200)
	reportTestFailure(t, len(out.Results) != 200 || out.HasMore || out.Results[0].FrameSeq == nil || *out.Results[0].FrameSeq != 0, func() {
		t.Fatalf("bounded page=%d more=%v", len(out.Results), out.HasMore)
	})
	traceBytes, previewBytes := 0, 0
	for _, item := range out.Results {
		traceBytes += len(simJSON(t, item.Result.Traces)) + len(simJSON(t, item.Result.ConditionTraces))
		previewBytes += len(simJSON(t, item.Before)) + len(simJSON(t, item.Result.Body))
		reportTestFailure(t, item.Result.ConditionTraceOmitted == 0 || !item.PreviewTruncated, func() {
			t.Fatal("diagnostic/preview omission not reported")
		})
	}
	// Array delimiters/null fields are outside collectors' byte accounting.
	reportTestFailure(t, traceBytes > simulationDiagnosticBudget+2000 || previewBytes > simulationPreviewBudget, func() {
		t.Fatalf("budgets traces=%d preview=%d", traceBytes, previewBytes)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	empty, _ := rules.Compile(nil)
	_, _, _, err := selectSimulationSamples(ctx, c, &store.Settings{}, empty, captureSimulationRequest{Phase: rules.PhaseResponseEvent, Batch: true, Limit: 200})
	reportTestFailure(t, err != context.Canceled, func() {
		t.Fatalf("cancellation=%v", err)
	})
	pending := saveSimCapture(t, st, &store.Capture{Outcome: store.OutcomePending})
	request["capture_id"] = pending.ID
	simPost(t, mux, request, 422)
}
