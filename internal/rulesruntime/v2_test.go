package rulesruntime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"pi-gateway/internal/rules"
	"pi-gateway/internal/store"
)

func TestV2UpgradesPersistedV1AndDoesNotReseed(t *testing.T) {
	st := runtimeStore(t, filepath.Join(runtimeTempDir(t), "v1.db"))
	raw := json.RawMessage(`{"schema_version":1,"name":"my old rule","enabled":false,"priority":37,"phase":"request","when":{"op":"always"},"actions":[{"id":"legacy","type":"drop_environment_context","params":{}}],"on_error":"abort"}`)
	before, err := st.InitializeRules(t.Context(), func(context.Context, []*store.MiddlewareRow) ([]*store.RuleRow, error) {
		return []*store.RuleRow{{ID: "old-rule", Rule: raw, Source: "user"}}, nil
	}, ValidateSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := New(st).Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := st.LoadRuleSet(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	old, err := st.GetRule(t.Context(), "old-rule")
	if err != nil {
		t.Fatal(err)
	}
	definition, err := rules.ParseRule(old.Rule)
	if err != nil {
		t.Fatal(err)
	}
	if definition.SchemaVersion != 2 || definition.Enabled || definition.Priority != 37 || definition.Name != "my old rule" || definition.Actions[0].Type != "sequence" {
		t.Fatalf("legacy preferences or expansion lost: %+v", definition)
	}
	if after.Version != before.Version+2 {
		t.Fatalf("unexpected publication version: %d", after.Version)
	}
	changes := []store.RuleChange{}
	for _, row := range after.Rules {
		changes = append(changes, store.RuleChange{Kind: "delete", ID: row.ID, ExpectedRevision: row.Revision})
	}
	deleted, err := st.PublishRules(t.Context(), &after.Version, changes, ValidateSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := New(st).Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := st.LoadRuleSet(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Rules) != 0 || snapshot.Version != deleted.Version {
		t.Fatal("restart reinstalled deleted defaults")
	}
}

func TestProtocolDefaultsUpgradeEarlierV2Installation(t *testing.T) {
	st := runtimeStore(t, filepath.Join(runtimeTempDir(t), "earlier-v2.db"))
	profile := rules.DefaultProfile()
	edited := profile[0]
	edited.Name, edited.Enabled, edited.Priority = "my protocol", false, 45
	_, err := st.InitializeRules(t.Context(), func(context.Context, []*store.MiddlewareRow) ([]*store.RuleRow, error) {
		definitions := []rules.Rule{edited, {SchemaVersion: 2, ID: "language-v2-installed", Name: "V2", Enabled: false, Phase: rules.PhaseRequest, When: rules.Condition{Op: "always"}, Actions: []rules.Action{}, OnError: rules.OnErrorAbort}}
		rows := []*store.RuleRow{}
		for _, definition := range definitions {
			raw, err := EditableJSON(definition)
			if err != nil {
				return nil, err
			}
			rows = append(rows, &store.RuleRow{ID: definition.ID, Rule: raw, Source: "user"})
		}
		return rows, nil
	}, ValidateSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.GetRule(t.Context(), edited.ID)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := New(st).Ensure(t.Context()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	after, err := st.GetRule(t.Context(), edited.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("edited default overwritten: %+v -> %+v err=%v", before, after, err)
	}
	changes := []store.RuleChange{}
	for _, definition := range profile {
		row, err := st.GetRule(t.Context(), definition.ID)
		if err != nil || row == nil {
			t.Fatalf("missing protocol default %s: %v", definition.ID, err)
		}
		changes = append(changes, store.RuleChange{Kind: "delete", ID: row.ID, ExpectedRevision: row.Revision})
	}
	deleted, err := st.PublishRules(t.Context(), nil, changes, ValidateSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := New(st).Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	version, err := st.RuleSetVersion(t.Context())
	if err != nil || version != deleted.Version {
		t.Fatalf("restart reseeded deleted defaults: version=%d err=%v", version, err)
	}
}

func TestV2RestoresEarlierInstallationMarkerBeforeRuleDeletion(t *testing.T) {
	st := runtimeStore(t, filepath.Join(runtimeTempDir(t), "earlier-marker.db"))
	if err := New(st).Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ExecContext(t.Context(), `DELETE FROM settings WHERE key='rules.language.v2'`); err != nil {
		t.Fatal(err)
	}
	before, err := st.LoadRuleSet(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := New(st).Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	installed, err := st.RulesV2Installed(t.Context())
	if err != nil || !installed {
		t.Fatalf("old installation marker not restored: installed=%t err=%v", installed, err)
	}
	changes := []store.RuleChange{}
	for _, row := range before.Rules {
		changes = append(changes, store.RuleChange{Kind: "delete", ID: row.ID, ExpectedRevision: row.Revision})
	}
	deleted, err := st.PublishRules(t.Context(), &before.Version, changes, ValidateSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := New(st).Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := st.LoadRuleSet(t.Context())
	if err != nil || len(after.Rules) != 0 || after.Version != deleted.Version {
		t.Fatalf("restart reinstalled deleted old defaults: %+v err=%v", after, err)
	}
}
