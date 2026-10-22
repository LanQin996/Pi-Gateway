package rules

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// Frontend coverage tooling consumes the exact validator registry, not a mirror:
// RULES_EXPORT_CATALOG=1 go test ./internal/rules -run '^TestExportCatalog$' -v
func TestExportCatalog(t *testing.T) {
	if os.Getenv("RULES_EXPORT_CATALOG") != "1" {
		t.Skip("catalog export is opt-in")
	}
	raw, e := json.Marshal(Catalog())
	if e != nil {
		t.Fatal(e)
	}
	fmt.Printf("RULES_CATALOG_JSON=%s\n", raw)
	defaults, err := MigrateLegacy(nil)
	if err != nil {
		t.Fatal(err)
	}
	defaults = append(defaults, DefaultProfile()...)
	for i, definition := range defaults {
		defaults[i], err = ExpandRule(definition)
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, err = json.Marshal(defaults)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("RULES_DEFAULTS_JSON=%s\n", raw)
}

func TestCatalogCompleteAndExamplesExecutable(t *testing.T) {
	c := Catalog()
	if len(c.Actions) != 22 || len(c.Conditions) != 22 {
		t.Fatalf("capability count %d/%d", len(c.Actions), len(c.Conditions))
	}
	seen := map[string]bool{}
	for _, group := range [][]FieldSpec{c.RuleFields, c.ContextFields, selectorFields()} {
		assertFieldGroupHelp(t, group)
	}
	for _, capability := range c.ValueExpressions {
		assertFieldGroupHelp(t, capability.Fields)
	}
	for _, a := range c.Actions {
		assertCatalogAction(t, a, seen)
	}
	for _, cond := range c.Conditions {
		requireCondition(t, !(cond.Label == "" || cond.Description == ""), "condition missing help")
		for _, f := range cond.Fields {
			assertFieldHelp(t, f)
			requireCondition(t, !(f.Label == "" || f.Description == "" || f.Control == ""), "condition field incomplete")
		}
	}
	requireCondition(t, !(len(c.ValueExpressions) != 4 || len(c.ContextFields) == 0), "missing expression or context schema")
	for _, r := range c.Examples {
		if r.Source == "protocol" {
			{
				_, err := Compile([]Rule{r})
				requireTrue(t, err == nil, err)
			}
			continue
		}
		t.Run(r.ID, func(t *testing.T) { testCatalogCompleteAndExamplesExecutableCase1(t, r) })
	}
	// Returned Catalog values are detached; a UI caller cannot corrupt validators.
	c.Actions[0].ID = "mutated"
	c.Actions[0].Fields[0].Name = "mutated"
	c.Phases[0] = "mutated"
	requireCondition(t, Catalog().Actions[0].ID != "mutated", "catalog aliases registry")
	compileTest(t, rule("untouched", act("rewrite_model", map[string]any{"model": "new"})))
}

func assertFieldHelp(t *testing.T, f FieldSpec) {
	t.Helper()
	if f.Description == "" || len(f.Examples) == 0 {
		t.Fatalf("field %s lacks description or examples", f.Name)
	}
	for _, option := range f.Enum {
		if f.EnumHelp[option] == "" {
			t.Fatalf("field %s lacks option help for %s", f.Name, option)
		}
	}
}

func testCatalogCompleteAndExamplesExecutableCase1(t *testing.T, r Rule) {
	raw, e := json.Marshal(r)
	requireNoError(t, e)
	parsed, e := ParseRule(raw)
	requireNoError(t, e)
	engine := compileTest(t, parsed)
	body := map[string]any{"model": "old-model", "input": []any{map[string]any{"type": "reasoning"}}, "delta": "hello", "type": "response.output_text.delta"}
	if r.Phase == PhaseRequest {
		delete(body, "type")
	}
	_, e = engine.Apply(t.Context(), r.Phase, &Input{Body: body, ClientBody: map[string]any{"metadata": map[string]any{"a": true}}, Model: "old-model", EventType: "response.output_text.delta", Trace: true})
	requireNoError(t, e)
}

func assertCatalogAction(t *testing.T, a Capability, seen map[string]bool) {
	t.Helper()
	if seen[a.ID] {
		t.Fatal("duplicate action", a.ID)
	}
	seen[a.ID] = true
	if a.Label == "" || a.Description == "" || len(a.Phases) == 0 {
		t.Fatal("missing help", a.ID)
	}
	fields := map[string]bool{}
	for _, f := range a.Fields {
		assertCatalogActionField(t, a.ID, f, fields)
	}
}

func assertCatalogActionField(t *testing.T, actionID string, f FieldSpec, fields map[string]bool) {
	t.Helper()
	assertFieldHelp(t, f)
	if fields[f.Name] || f.Label == "" || f.Description == "" || f.Control == "" || f.Type == "" {
		t.Fatalf("incomplete field %s/%s", actionID, f.Name)
	}
	fields[f.Name] = true
	if !contains([]string{"string", "pointer", "boolean", "integer", "string_array", "pointer_array", "value", "value_array", "condition", "condition_array", "action_array"}, f.Type) {
		t.Fatalf("unrenderable field %s", f.Type)
	}
}

func assertFieldGroupHelp(t *testing.T, fields []FieldSpec) {
	t.Helper()
	for _, f := range fields {
		assertFieldHelp(t, f)
	}
}
