package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"pi-gateway/internal/piwire"
)

func act(kind string, p map[string]any) Action { return Action{ID: kind, Type: kind, Params: p} }
func rule(id string, actions ...Action) Rule {
	return Rule{SchemaVersion: 1, ID: id, Name: id, Enabled: true, Priority: 100, Phase: PhaseRequest, When: Condition{Op: "always"}, Actions: append([]Action{}, actions...), OnError: OnErrorAbort}
}
func compileTest(t testing.TB, rs ...Rule) *Engine {
	t.Helper()
	e, err := Compile(rs)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return e
}
func applyTest(t testing.TB, r Rule, body any) Result {
	t.Helper()
	res, err := compileTest(t, r).Apply(context.Background(), r.Phase, &Input{Body: body, Trace: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return res
}
func pathValue(t testing.TB, body any, path string) any {
	t.Helper()
	v, ok, e := pointerGet(body, path)
	if e != nil || !ok {
		t.Fatalf("missing %s: %v", path, e)
	}
	return v
}
func assertJSON(t testing.TB, a, b any) {
	t.Helper()
	if !jsonEqual(a, b) {
		aa, _ := json.Marshal(a)
		bb, _ := json.Marshal(b)
		t.Fatalf("got %s, want %s", aa, bb)
	}
}
func ref(source, path string) map[string]any {
	return map[string]any{"$ref": map[string]any{"source": source, "path": path}}
}

func TestActionsAll(t *testing.T) {
	tests := []struct {
		name       string
		p          map[string]any
		body, want any
	}{
		{"rewrite_model", map[string]any{"model": "new"}, map[string]any{"model": "old"}, map[string]any{"model": "new"}},
		{"drop_environment_context", map[string]any{}, map[string]any{"input": "<environment_context>secret</environment_context> hello"}, map[string]any{"input": "hello"}},
		{"drop_input_items", map[string]any{"types": []any{"reasoning"}, "pattern": "bye", "target": "text"}, map[string]any{"input": []any{map[string]any{"type": "reasoning"}, map[string]any{"content": "bye"}, map[string]any{"content": []any{map[string]any{"text": "stay"}}}}}, map[string]any{"input": []any{map[string]any{"content": []any{map[string]any{"text": "stay"}}}}}},
		{"drop_tools", map[string]any{"names": []any{"gone"}}, map[string]any{"tools": []any{map[string]any{"name": "gone"}}}, map[string]any{}},
		{"set_reasoning", map[string]any{"effort": "custom", "summary": ""}, map[string]any{"reasoning": map[string]any{"summary": "auto"}}, map[string]any{"reasoning": map[string]any{"summary": "auto", "effort": "custom"}}},
		{"json_set", map[string]any{"path": "/nested/value", "value": nil, "create_parents": true}, map[string]any{}, map[string]any{"nested": map[string]any{"value": nil}}},
		{"json_remove", map[string]any{"paths": []any{"/a/1", "/gone"}}, map[string]any{"a": []any{1, 2, 3}}, map[string]any{"a": []any{1, 3}}},
		{"json_merge", map[string]any{"path": "", "value": map[string]any{"x": map[string]any{"y": 2}, "a": []any{2}}, "mode": "deep", "array_mode": "append"}, map[string]any{"x": map[string]any{"z": 1}, "a": []any{1}}, map[string]any{"x": map[string]any{"z": 1, "y": 2}, "a": []any{1, 2}}},
		{"json_transfer", map[string]any{"source_path": "/a", "target_path": "/b", "operation": "move"}, map[string]any{"a": map[string]any{"x": 1}}, map[string]any{"b": map[string]any{"x": 1}}},
		{"array_insert", map[string]any{"path": "/a", "values": []any{2, 3}, "position": "index", "index": 1}, map[string]any{"a": []any{1, 4}}, map[string]any{"a": []any{1, 2, 3, 4}}},
		{"array_filter", map[string]any{"path": "/a", "mode": "keep_matches", "predicate": Condition{Op: "gte", Source: "context", Path: "/item_index", Value: 1}}, map[string]any{"a": []any{1, 2, 3}}, map[string]any{"a": []any{2, 3}}},
		{"text_replace", map[string]any{"path": "/text", "match": "regex", "pattern": "(a)", "replacement": "${1}!", "replace_all": false}, map[string]any{"text": "a a"}, map[string]any{"text": "a! a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := rule(tt.name, act(tt.name, tt.p))
			original := mustClone(tt.body)
			res := applyTest(t, r, tt.body)
			assertJSON(t, res.Body, tt.want)
			assertJSON(t, tt.body, original)
			if !res.Changed || len(res.Traces) == 0 || res.Traces[len(res.Traces)-1].Status != "changed" {
				t.Fatalf("incorrect change trace %+v", res)
			}
			if tt.name == "rewrite_model" && res.Model != "new" {
				t.Fatal("model not synchronized")
			}
		})
	}
	t.Run("passthrough_fields", func(t *testing.T) {
		r := rule("pass", act("passthrough_fields", map[string]any{"fields": []any{"keep", "nil", "missing", "metadata"}}), Action{ID: "mutate", Type: "json_set", Params: map[string]any{"path": "/metadata/a", "value": 2}}, Action{ID: "copy", Type: "json_set", Params: map[string]any{"path": "/original", "value": ref("client", "/metadata/a")}})
		client := map[string]any{"keep": 2, "nil": nil, "metadata": map[string]any{"a": 1}}
		res, e := compileTest(t, r).Apply(context.Background(), PhaseRequest, &Input{Body: map[string]any{"keep": 1}, ClientBody: client})
		if e != nil {
			t.Fatal(e)
		}
		assertJSON(t, res.Body, map[string]any{"keep": 1, "metadata": map[string]any{"a": 2}, "original": 1})
		assertJSON(t, client["metadata"], map[string]any{"a": 1})
	})
	t.Run("reject_request", func(t *testing.T) {
		r := rule("reject", act("reject_request", map[string]any{}))
		res := applyTest(t, r, map[string]any{})
		if !res.Blocked || res.Status != 403 || res.Dropped || res.Changed || res.Traces[0].Status != "blocked" {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("drop_event", func(t *testing.T) {
		r := rule("drop", act("drop_event", map[string]any{}))
		r.Phase = PhaseResponseEvent
		res := applyTest(t, r, map[string]any{"type": "response.output_text.delta", "item_id": "x", "delta": "a"})
		if !res.Dropped || res.Blocked || res.Changed || res.Traces[0].Status != "dropped" {
			t.Fatalf("%+v", res)
		}
	})
}

func TestOrderingStoppingAndSnapshots(t *testing.T) {
	a := rule("b", act("json_set", map[string]any{"path": "/v", "value": "b"}))
	b := rule("a", act("json_set", map[string]any{"path": "/v", "value": "a"}))
	later := rule("later", act("json_set", map[string]any{"path": "/seen", "value": ref("current", "/v")}))
	later.Priority = 101
	e := compileTest(t, a, b, later)
	a.Actions[0].Params["value"] = "mutated"
	res, err := e.Apply(context.Background(), PhaseRequest, &Input{Body: map[string]any{}, Trace: true})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, res.Body, map[string]any{"v": "b", "seen": "b"})
	if res.Traces[0].RuleID != "a" || res.Traces[1].RuleID != "b" {
		t.Fatal("unstable tie order")
	}
	a.StopAfterMatch = true
	a.OrderIndex = -1
	a.Actions = []Action{}
	res, err = compileTest(t, a, b).Apply(context.Background(), PhaseRequest, &Input{Body: map[string]any{}, Trace: true})
	if err != nil || res.Changed || len(res.Traces) != 1 || res.Traces[0].Status != "no_change" {
		t.Fatalf("%+v %v", res, err)
	}
	a.Enabled = false
	res, err = compileTest(t, a, b).Apply(context.Background(), PhaseRequest, &Input{Body: map[string]any{}})
	if err != nil || pathValue(t, res.Body, "/v") != "a" {
		t.Fatal("disabled rule executed")
	}
}
func TestAtomicRollback(t *testing.T) {
	first := rule("first", act("json_set", map[string]any{"path": "/committed", "value": true}))
	first.Priority = 1
	bad := rule("bad", act("rewrite_model", map[string]any{"model": "changed"}), act("json_set", map[string]any{"path": "/partial", "value": true}), act("text_replace", map[string]any{"path": "/n", "pattern": "x"}))
	last := rule("last", act("json_set", map[string]any{"path": "/later", "value": true}))
	last.Priority = 200
	for _, policy := range []string{OnErrorAbort, OnErrorSkipRule} {
		t.Run(policy, func(t *testing.T) { testAtomicRollbackCase5(t, bad, first, last, policy) })
	}
}
func TestNoChangeAndRealDiff(t *testing.T) {
	r := rule("real", act("set_reasoning", map[string]any{"effort": "low"}), act("json_set", map[string]any{"path": "/explicit_null", "value": nil}))
	res := applyTest(t, r, map[string]any{"reasoning": map[string]any{"effort": "low"}})
	if res.Traces[0].Status != "no_change" || len(res.Traces[0].Changes) != 0 {
		t.Fatal("middleware note became fake diff")
	}
	change := res.Traces[len(res.Traces)-1].Changes[0]
	if change.Path != "/explicit_null" || change.BeforeExists || !change.AfterExists || change.After != nil {
		t.Fatalf("null/missing conflated %+v", change)
	}
}
func TestOrderedMapPreserved(t *testing.T) {
	body := piwire.NewOrderedMap().Set("z", 1).Set("model", "m").Set("a", 2)
	r := rule("order", act("rewrite_model", map[string]any{"model": "n"}), act("json_set", map[string]any{"path": "/x", "value": 1}))
	res, err := compileTest(t, r).Apply(context.Background(), PhaseRequest, &Input{Body: body, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := res.Body.(*piwire.OrderedMap)
	if !ok {
		t.Fatal("ordered body lost")
	}
	if strings.Join(got.Keys(), ",") != "z,model,a,x" {
		t.Fatal(got.Keys())
	}
	if body.Has("x") {
		t.Fatal("input modified")
	}
}
func TestManyRulesAndConcurrency(t *testing.T) {
	for _, count := range []int{1, 100, 1000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			rs := make([]Rule, count)
			for i := range rs {
				rs[i] = rule(fmt.Sprintf("r-%04d", i))
				rs[i].OrderIndex = int64(i)
			}
			rs[count-1].Actions = []Action{act("json_set", map[string]any{"path": "/last", "value": count})}
			e := compileTest(t, rs...)
			var wg sync.WaitGroup
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					res, err := e.Apply(context.Background(), PhaseRequest, &Input{Body: map[string]any{}})
					if err != nil {
						t.Error(err)
						return
					}
					if !jsonEqual(pathValue(t, res.Body, "/last"), count) {
						t.Error("last rule omitted")
					}
				}()
			}
			wg.Wait()
		})
	}
}
func TestCancellationAndEmptyFastPath(t *testing.T) {
	e := compileTest(t)
	body := map[string]any{"model": "m"}
	r, err := e.Apply(context.Background(), PhaseRequest, &Input{Body: body, Model: "m", ClientBody: make(chan int)})
	if err != nil || r.Changed || e.HasPhase(PhaseRequest) {
		t.Fatalf("empty engine should not inspect client %v", err)
	}
	r.Body.(map[string]any)["proof"] = true
	if !body["proof"].(bool) {
		t.Fatal("fast path copied body")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Apply(ctx, PhaseRequest, &Input{}); err != context.Canceled {
		t.Fatal(err)
	}
}

func testAtomicRollbackCase5(t *testing.T, bad Rule, first Rule, last Rule, policy string) {
	bad.OnError = policy
	res, e := compileTest(t, first, bad, last).Apply(context.Background(), PhaseRequest, &Input{Body: map[string]any{"model": "old", "n": 1}, Model: "old", Trace: true})
	requireTruef(t, (e != nil) == (policy == OnErrorAbort), "wrong error %v", e)
	requireCondition(t, res.Model == "old", "model not rolled back")
	{
		_, ok, _ := pointerGet(res.Body, "/partial")
		requireTrue(t, !(ok), "partial mutation escaped")
	}
	requireCondition(t, pathValue(t, res.Body, "/committed") == true, "prior commit rolled back")
	_, later, _ := pointerGet(res.Body, "/later")
	requireCondition(t, later == (policy == OnErrorSkipRule), "wrong stop/continue")
	for _, tr := range res.Traces {
		requireTruef(t, !(tr.RuleID == "bad" && (!tr.RolledBack || len(tr.Changes) != 0)), "rollback falsely attributed %+v", tr)
	}
}
