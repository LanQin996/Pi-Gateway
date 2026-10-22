package rules

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestConditionDiagnosticsShortCircuitAndEquivalence(t *testing.T) {
	for _, op := range []string{"all", "any"} {
		t.Run(op, func(t *testing.T) { testConditionDiagnosticsShortCircuitAndEquivalenceCase1(t, op) })
	}
}

func TestConditionDiagnosticsPredicateIndicesRollbackAndLimit(t *testing.T) {
	r := rule("predicate", act("array_filter", map[string]any{"path": "/items", "predicate": Condition{Op: "gt", Source: "context", Path: "/item_index", Value: 0}, "mode": "keep_matches"}), act("json_remove", map[string]any{"paths": []any{"/absent"}, "on_missing": "error"}))
	r.OnError = OnErrorSkipRule
	input := &Input{Body: map[string]any{"items": []any{"a", "b", "c"}}, Trace: true, ConditionTrace: true}
	engine := compileTest(t, r)
	result, err := engine.Apply(context.Background(), PhaseRequest, input)
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, result.Body, input.Body)
	if result.Changed {
		t.Fatal("failed rule committed")
	}
	for i := 0; i < 3; i++ {
		tr := result.ConditionTraces[i+1]
		if tr.Path != "/actions/0/params/predicate" || tr.ItemIndex == nil || *tr.ItemIndex != i {
			t.Fatalf("predicate=%+v", tr)
		}
	}
	if len(result.Traces) != 2 || !result.Traces[0].RolledBack || result.Traces[0].Status != "rolled_back" {
		t.Fatalf("rollback=%+v", result.Traces)
	}
	input.MaxTraces = 1
	bounded, err := engine.Apply(context.Background(), PhaseRequest, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded.ConditionTraces) != 1 || bounded.ConditionTraceOmitted != 3 {
		t.Fatalf("bounded=%+v", bounded)
	}
	assertJSON(t, bounded.Body, result.Body)
	input.MaxTraces = 0
	input.TraceMaxBytes = 1
	bounded, err = engine.Apply(context.Background(), PhaseRequest, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded.ConditionTraces) != 0 || bounded.ConditionTraceOmitted != 4 {
		t.Fatalf("byte budget=%+v", bounded)
	}
	encoded, _ := json.Marshal(bounded.ConditionTraces)
	if len(encoded) > 4 {
		t.Fatal("trace exceeded byte budget")
	}
}

func TestMissingHistoricalContextIsNotFalseOrSkippable(t *testing.T) {
	for _, action := range []bool{false, true} {
		r := rule("missing")
		r.OnError = OnErrorSkipRule
		if action {
			r.Actions = []Action{act("json_set", map[string]any{"path": "/value", "value": ref("context", "/request_method")})}
		} else {
			r.When = Condition{Op: "not_exists", Source: "context", Path: "/request_method"}
		}
		result, err := compileTest(t, r).Apply(context.Background(), PhaseRequest, &Input{Body: map[string]any{}, ConditionTrace: true, Unavailable: []string{"/context/request_method"}})
		var missing *MissingInputError
		if !errors.As(err, &missing) || missing.Path != "/context/request_method" {
			t.Fatalf("error=%v result=%+v", err, result)
		}
	}
	for _, action := range []Action{
		act("json_set", map[string]any{"path": "/prompt_cache_key", "value": "fallback", "if_exists": "keep"}),
		act("json_remove", map[string]any{"paths": []any{"/prompt_cache_key"}}),
	} {
		_, err := compileTest(t, rule("unknown-current", action)).Apply(context.Background(), PhaseRequest, &Input{Body: map[string]any{}, Unavailable: []string{"/current/prompt_cache_key"}})
		var missing *MissingInputError
		if !errors.As(err, &missing) {
			t.Fatalf("action guessed unavailable normalized header field: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := compileTest(t, rule("cancel")).Apply(ctx, PhaseRequest, &Input{Body: map[string]any{}, ConditionTrace: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
}

func testConditionDiagnosticsShortCircuitAndEquivalenceCase1(t *testing.T, op string) {
	first := Condition{Op: "exists", Path: "/absent"}
	if op == "any" {
		first = Condition{Op: "always"}
	}
	r := rule("diagnostic", act("json_set", map[string]any{"path": "/changed", "value": true}))
	r.When = Condition{Op: op, Conditions: []Condition{first, {Op: "eq", Source: "context", Path: "/request_path", Value: "/v1/responses"}}}
	engine := compileTest(t, r)
	input := &Input{Body: map[string]any{}, Unavailable: []string{"/context/request_path"}}
	plain, err := engine.Apply(context.Background(), PhaseRequest, input)
	requireNoError(t, err)
	requireCondition(t, len(plain.ConditionTraces) == 0, "normal execution collected conditions")
	input.ConditionTrace = true
	traced, err := engine.Apply(context.Background(), PhaseRequest, input)
	requireTruef(t, err == nil, "short-circuited child was evaluated: %v", err)
	assertJSON(t, plain.Body, traced.Body)
	requireCondition(t, !(plain.Changed != traced.Changed || plain.Blocked != traced.Blocked || plain.Model != traced.Model), "diagnostics changed result")
	if len(traced.ConditionTraces) != 3 {
		t.Fatalf("traces=%+v", traced.ConditionTraces)
	}
	skip := traced.ConditionTraces[1]
	requireTruef(t, !(skip.Path != "/when/conditions/1" || skip.Status != "skipped" || skip.Matched != nil || skip.RuleID != r.ID), "skip=%+v", skip)
}
