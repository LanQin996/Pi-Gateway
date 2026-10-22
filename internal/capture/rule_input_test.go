package capture

import (
	"encoding/json"
	"strings"
	"testing"

	"pi-gateway/internal/rules"
	"pi-gateway/internal/store"
)

func TestRuleInputCheckpointBudgetsAndIndependence(t *testing.T) {
	for _, budget := range []int64{32, 4096} {
		recorder := New(nil, Options{MaxBytesPerRecord: budget})
		body := map[string]any{"model": "original", "input": []any{}}
		recorder.OnRuleInput(rules.PhaseRequest, &rules.Input{Body: body, ClientBody: map[string]any{"model": "client"}, Model: "original", Context: map[string]any{"request_path": "/responses"}, Trace: true, ConditionTrace: true}, false)
		body["model"] = "mutated"
		c := recorder.Finalize(store.OutcomeOK, "")
		reportTestFailure(t, len(c.ResponseFrames) != 1, func() {
			t.Fatalf("frames=%+v", c.ResponseFrames)
		})
		f := c.ResponseFrames[0]
		reportTestFailure(t, f.Kind != KindRuleInput || f.Dir != "internal" || f.Type != rules.PhaseRequest, func() {
			t.Fatalf("checkpoint=%+v", f)
		})
		reportTestFailure(t, recorder.seenBytes > budget, func() {
			t.Fatal("checkpoint escaped record budget")
		})
		if budget == 32 {
			reportTestFailure(t, f.Data != nil || f.Text != "" || !c.Truncated, func() {
				t.Fatal("incomplete checkpoint retained payload")
			})
			continue
		}
		raw, _ := json.Marshal(f.Data)
		var input rules.Input
		{
			err := json.Unmarshal(raw, &input)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		reportTestFailure(t, input.Body.(map[string]any)["model"] != "original" || input.Trace || input.ConditionTrace, func() {
			t.Fatalf("snapshot=%s", raw)
		})
	}
}

func TestRuleInputCheckpointSizeAndFrameCaps(t *testing.T) {
	recorder := New(nil, Options{MaxBytesPerRecord: 2 << 20})
	recorder.OnRuleInput(rules.PhaseRequest, &rules.Input{Body: map[string]any{"oversize": strings.Repeat("x", MaxRuleInputBytes+1)}}, false)
	if len(recorder.cap.ResponseFrames) != 1 || recorder.cap.ResponseFrames[0].Data != nil || !recorder.truncated {
		t.Fatal("oversize snapshot was retained")
	}
	recorder.maxFrames = 1
	recorder.OnRuleInput(rules.PhaseResponseBody, &rules.Input{Body: map[string]any{}}, false)
	if len(recorder.cap.ResponseFrames) != 1 {
		t.Fatal("snapshot escaped frame cap")
	}
}

func TestResponseRuleContextDoesNotDuplicateEvent(t *testing.T) {
	recorder := New(nil, Options{MaxBytesPerRecord: 4096})
	recorder.OnRuleInput(rules.PhaseResponseEvent, &rules.Input{Body: map[string]any{"huge_delta": "not retained"}, ClientBody: map[string]any{}, Model: "m", Context: map[string]any{"original_model": "o", "account_id": 1}}, true)
	raw, _ := json.Marshal(recorder.cap.ResponseFrames[0].Data)
	if strings.Contains(string(raw), "huge_delta") || recorder.cap.ResponseFrames[0].Kind != KindRuleContext {
		t.Fatalf("context duplicates event: %s", raw)
	}
}
