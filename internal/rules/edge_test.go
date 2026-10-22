package rules

import (
	"context"
	"strings"
	"testing"
)

func TestBoundaryRegressions(t *testing.T) {
	t.Run("null-rule-list", func(t *testing.T) { testBoundaryRegressionsCase1(t) })
	t.Run("pointer-depth", func(t *testing.T) { testBoundaryRegressionsCase2(t) })
	t.Run("result-depth", func(t *testing.T) { testBoundaryRegressionsCase3(t) })
	t.Run("keep-does-not-resolve-unused-value", func(t *testing.T) { testBoundaryRegressionsCase4(t) })
	t.Run("whitespace-text-pattern", func(t *testing.T) { testBoundaryRegressionsCase5(t) })
	t.Run("original-model-context", func(t *testing.T) { testBoundaryRegressionsCase6(t) })
	t.Run("event-root-remains-object", func(t *testing.T) { testBoundaryRegressionsCase7(t) })
	t.Run("source-path-error", func(t *testing.T) { testBoundaryRegressionsCase8(t) })
	t.Run("catalog-phases-detached", func(t *testing.T) { testBoundaryRegressionsCase9(t) })
}

func testBoundaryRegressionsCase1(t *testing.T) {
	{
		_, e := ParseRules([]byte("null"))
		requireTrue(t, e != nil, "null list accepted")
	}
}
func testBoundaryRegressionsCase2(t *testing.T) {
	r := rule("deep", act("json_set", map[string]any{"path": strings.Repeat("/x", MaxDepth+1), "value": 1, "create_parents": true}))
	{
		_, e := Compile([]Rule{r})
		requireTrue(t, e != nil, "unbounded pointer accepted")
	}
}
func testBoundaryRegressionsCase3(t *testing.T) {
	r := rule("deep", act("json_set", map[string]any{"path": strings.Repeat("/x", MaxDepth), "value": map[string]any{"too_deep": 1}, "create_parents": true}))
	res, e := compileTest(t, r).Apply(context.Background(), PhaseRequest, &Input{Body: map[string]any{}})
	requireCondition(t, !(e == nil || res.Changed), "nested result budget not enforced")
}
func testBoundaryRegressionsCase4(t *testing.T) {
	r := rule("keep", act("json_set", map[string]any{"path": "/x", "if_exists": "keep", "value": ref("client", "/absent")}))
	res := applyTest(t, r, map[string]any{"x": nil})
	requireCondition(t, !(res.Changed), "existing null not kept")
}
func testBoundaryRegressionsCase5(t *testing.T) {
	r := rule("space", act("text_replace", map[string]any{"path": "/x", "pattern": " ", "replacement": "_"}))
	res := applyTest(t, r, map[string]any{"x": "a b"})
	requireCondition(t, pathValue(t, res.Body, "/x") == "a_b", "whitespace pattern rejected")
}
func testBoundaryRegressionsCase6(t *testing.T) {
	r := rule("original", act("json_set", map[string]any{"path": "/original", "value": ref("context", "/original_model")}))
	r.Phase = PhaseResponseBody
	res, e := compileTest(t, r).Apply(context.Background(), PhaseResponseBody, &Input{Body: map[string]any{}, Model: "current", Context: map[string]any{"original_model": "original"}})
	requireTrue(t, !(e != nil || pathValue(t, res.Body, "/original") != "original"), "original model lost", e)
}
func testBoundaryRegressionsCase7(t *testing.T) {
	r := rule("root", act("json_set", map[string]any{"path": "", "value": "invalid-event"}))
	r.Phase = PhaseResponseEvent
	res, e := compileTest(t, r).Apply(context.Background(), PhaseResponseEvent, &Input{Body: map[string]any{}, EventType: "response.output_text.delta"})
	requireCondition(t, !(e == nil || res.Changed), "nonobject event accepted")
}
func testBoundaryRegressionsCase8(t *testing.T) {
	r := rule("source", act("json_transfer", map[string]any{"source": "context", "source_path": "/account_id", "target_path": "/x"}))
	_, e := Compile([]Rule{r})
	{
		ve, ok := e.(*ValidationError)
		requireTrue(t, !(!ok || ve.Path != "/0/actions/0/params/source_path"), e)
	}
}
func testBoundaryRegressionsCase9(t *testing.T) {
	c := Catalog()
	c.Actions[6].Phases[0] = "mutated"
	c.Conditions[0].Phases[0] = "mutated"
	c.ValueExpressions[0].Phases[0] = "mutated"
	compileTest(t, rule("valid", act("json_set", map[string]any{"path": "/v", "value": 1})))
	requireCondition(t, Catalog().Phases[0] == PhaseClientRequest, "catalog mutated validator")
}
