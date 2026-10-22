package rulesruntime

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"pi-gateway/internal/rules"
	"pi-gateway/internal/store"
)

func TestDefaultImageExclusionIsAnEditableRule(t *testing.T) {
	ctx := context.Background()
	rows, err := ConvertLegacy(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := DecodeSnapshot(&store.RuleSetSnapshot{Rules: rows})
	if err != nil {
		t.Fatal(err)
	}
	var filter rules.Rule
	for _, definition := range definitions {
		if definition.ID == "default-drop-image-generation" {
			filter = definition
		}
	}
	if !filter.Enabled || len(filter.Actions) != 1 || filter.Actions[0].Type != "sequence" {
		t.Fatalf("missing default rule: %+v", filter)
	}
	image := map[string]any{"type": "image_generation"}
	input := &rules.Input{Body: map[string]any{"tools": []any{image, map[string]any{"type": "namespace", "name": "images", "tools": []any{image}}, map[string]any{"type": "function", "name": "image_generation"}}, "tool_choice": image}, Trace: true}
	engine, err := rules.Compile([]rules.Rule{filter})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.Apply(ctx, rules.PhaseRequest, input)
	if err != nil {
		t.Fatal(err)
	}
	body := result.Body.(map[string]any)
	if len(body["tools"].([]any)) != 1 || body["tool_choice"] != nil || len(result.Traces) == 0 {
		t.Fatalf("exclusion failed: %+v", result)
	}
	filter.Enabled = false
	engine, err = rules.Compile([]rules.Rule{filter})
	if err != nil {
		t.Fatal(err)
	}
	result, err = engine.Apply(ctx, rules.PhaseRequest, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Body.(map[string]any)["tools"].([]any)) != 3 {
		t.Fatal("disabled rule still filtered tools")
	}
}

// Simulate a database whose rules were migrated before the image default existed.
func oldRulesStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st := runtimeStore(t, path)
	_, err := st.InitializeRules(t.Context(), func(ctx context.Context, rows []*store.MiddlewareRow) ([]*store.RuleRow, error) {
		converted, err := ConvertLegacy(ctx, rows)
		if err != nil {
			return nil, err
		}
		out := []*store.RuleRow{}
		for _, row := range converted {
			if row.ID != "default-drop-image-generation" {
				out = append(out, row)
			}
		}
		return out, nil
	}, ValidateSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestOldInstallationAddsImageDefaultOnce(t *testing.T) {
	for _, entry := range []string{"ensure", "load"} {
		t.Run(entry, func(t *testing.T) { oldInstallationAddsImageDefaultOnceCase1(t, entry) })
	}
}

func TestDefaultUpgradePreservesEditedDisabledRule(t *testing.T) {
	st := oldRulesStore(t, filepath.Join(runtimeTempDir(t), "edited.db"))
	definition := defaultImageExclusion()
	definition.Enabled, definition.Name, definition.Priority = false, "my custom filter", 42
	definition.Actions[0].Params = map[string]any{"types": []any{"file_search"}}
	raw, err := EditableJSON(definition)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := st.PublishRules(t.Context(), nil, []store.RuleChange{{Kind: "create", ID: definition.ID, Rule: raw, Source: "user"}}, ValidateSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := st.GetRule(t.Context(), definition.ID)
	if err := New(st).Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := st.GetRule(t.Context(), definition.ID)
	version, versionErr := st.RuleSetVersion(t.Context())
	if err != nil || versionErr != nil || !reflect.DeepEqual(before, after) || version != snapshot.Version {
		t.Fatalf("existing default overwritten: %+v -> %+v version=%d err=%v/%v", before, after, version, err, versionErr)
	}
}

func TestDefaultUpgradeValidationRollbackAllowsRetry(t *testing.T) {
	st := oldRulesStore(t, filepath.Join(runtimeTempDir(t), "rollback.db"))
	before, _ := st.RuleSetVersion(t.Context())
	marker := "internal_default_image_exclusion_v1"
	raw, err := EditableJSON(defaultImageExclusion())
	if err != nil {
		t.Fatal(err)
	}
	changes := []store.RuleChange{{Kind: "create", ID: "default-drop-image-generation", Rule: raw}}
	wantErr := errors.New("validation failed")
	if err := st.UpgradeDefaultRules(t.Context(), marker, changes, func(context.Context, *store.RuleSetSnapshot) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("failed upgrade: %v", err)
	}
	version, _ := st.RuleSetVersion(t.Context())
	filter, err := st.GetRule(t.Context(), changes[0].ID)
	if err != nil || filter != nil || version != before {
		t.Fatalf("failed upgrade partially committed: %+v version=%d err=%v", filter, version, err)
	}
	if err := New(st).Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	filter, err = st.GetRule(t.Context(), changes[0].ID)
	if err != nil || filter == nil || !filter.Enabled {
		t.Fatalf("failed upgrade left a completion marker: %+v err=%v", filter, err)
	}
}

func oldInstallationAddsImageDefaultOnceCase1(t *testing.T, entry string) {
	path := filepath.Join(runtimeTempDir(t), "old.db")
	first := oldRulesStore(t, path)
	second := runtimeStore(t, path)
	before, err := first.LoadRuleSet(t.Context())
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	var wg sync.WaitGroup
	for _, st := range []*store.Store{first, second, first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			service := New(st)
			var err error
			if entry == "ensure" {
				err = service.Ensure(t.Context())
			} else {
				_, _, err = service.Load(t.Context())
			}
			reportTestFailure(t, err != nil, func() {
				t.Errorf("concurrent upgrade: %v", err)
			})
		}()
	}
	wg.Wait()
	after, err := first.LoadRuleSet(t.Context())
	reportTestFailure(t, err != nil || after.Version != before.Version+1 || len(after.Rules) != len(before.Rules)+1, func() {
		t.Fatalf("upgrade did not publish exactly once: %+v err=%v", after, err)
	})
	for _, row := range before.Rules {
		got, err := first.GetRule(t.Context(), row.ID)
		reportTestFailure(t, err != nil || !reflect.DeepEqual(got, row), func() {
			t.Fatalf("upgrade changed existing rule %s: %+v err=%v", row.ID, got, err)
		})
	}
	filter, err := first.GetRule(t.Context(), "default-drop-image-generation")
	reportTestFailure(t, err != nil || filter == nil || !filter.Enabled || filter.Source != "default", func() {
		t.Fatalf("missing enabled default: %+v err=%v", filter, err)
	})
	engine, _, err := New(second).Load(t.Context())
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	result, err := engine.Apply(t.Context(), rules.PhaseRequest, &rules.Input{Body: map[string]any{"tools": []any{map[string]any{"type": "image_generation"}}}})
	reportTestFailure(t, err != nil || result.Body.(map[string]any)["tools"] != nil, func() {
		t.Fatalf("upgraded default did not filter images: %+v err=%v", result, err)
	})
	deleted, err := first.PublishRules(t.Context(), nil, []store.RuleChange{{Kind: "delete", ID: filter.ID, ExpectedRevision: filter.Revision}}, ValidateSnapshot)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	{
		err := New(second).Ensure(t.Context())
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	_, version, err := New(second).Load(t.Context())
	filter, lookupErr := second.GetRule(t.Context(), filter.ID)
	reportTestFailure(t, err != nil || lookupErr != nil || filter != nil || version != deleted.Version, func() {
		t.Fatalf("restart resurrected deleted default: filter=%+v version=%d err=%v/%v", filter, version, err, lookupErr)
	})
}
