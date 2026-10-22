package rulesruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pi-gateway/internal/rules"
	"pi-gateway/internal/store"
)

// Register directory cleanup before opening any stores so all handles close
// first, including tests that open the same database more than once.
func runtimeTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		var err error
		for attempt := 0; attempt < 50; attempt++ {
			if err = os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Errorf("remove runtime store directory: %v", err)
	})
	return dir
}

func runtimeStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func rewriteDefinition(t *testing.T, model string) json.RawMessage {
	t.Helper()
	raw, err := EditableJSON(rules.Rule{
		SchemaVersion: rules.SchemaVersion, Name: "hot rewrite", Enabled: true,
		Priority: 10000, Phase: rules.PhaseRequest, When: rules.Condition{Op: "always"},
		Actions: []rules.Action{{ID: "model", Type: "rewrite_model", Params: map[string]any{"model": model}}},
		OnError: rules.OnErrorAbort,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func modelFromEngine(t *testing.T, engine *rules.Engine) string {
	t.Helper()
	result, err := engine.Apply(t.Context(), rules.PhaseRequest, &rules.Input{
		Body: map[string]any{"model": "initial", "input": []any{}}, Model: "initial",
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Model
}

func TestRuntimePublicationInvalidationAndPinnedSnapshots(t *testing.T) {
	path := filepath.Join(runtimeTempDir(t), "runtime.db")
	first, second := runtimeStore(t, path), runtimeStore(t, path)
	services := []*Service{New(first), New(second)}
	ctx := t.Context()
	{
		err := services[0].Ensure(ctx)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	original, initialVersion, err := services[0].Load(ctx)
	reportTestFailure(t, err != nil || initialVersion != 1, func() {
		t.Fatalf("load initial: version=%d err=%v", initialVersion, err)
	})
	cached, version, err := services[0].Load(ctx)
	reportTestFailure(t, err != nil || version != initialVersion || cached != original, func() {
		t.Fatalf("unchanged version did not reuse compiled engine: %v", err)
	})
	{
		_, _, err := services[1].Load(ctx)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	snapshot, err := first.PublishRules(ctx, &initialVersion, []store.RuleChange{
		{Kind: "create", ID: "hot", Rule: rewriteDefinition(t, "first-published")},
	}, ValidateSnapshot)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	var pinned *rules.Engine
	for _, service := range services {
		engine, version, err := service.Load(ctx)
		reportTestFailure(t, err != nil || version != snapshot.Version || engine == original, func() {
			t.Fatalf("instance did not refresh: version=%d err=%v", version, err)
		})
		{
			got := modelFromEngine(t, engine)
			reportTestFailure(t, got != "first-published", func() {
				t.Fatalf("published model=%q", got)
			})
		}
		pinned = engine
	}
	{
		got := modelFromEngine(t, original)
		reportTestFailure(t, got != "initial", func() {
			t.Fatalf("initial in-flight snapshot changed: %q", got)
		})
	}
	row, err := first.GetRule(ctx, "hot")
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	snapshot, err = second.PublishRules(ctx, nil, []store.RuleChange{
		{Kind: "update", ID: row.ID, ExpectedRevision: row.Revision, Rule: rewriteDefinition(t, "second-published")},
	}, ValidateSnapshot)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	for _, service := range services {
		engine, version, err := service.Load(ctx)
		reportTestFailure(t, err != nil || version != snapshot.Version || modelFromEngine(t, engine) != "second-published", func() {
			t.Fatalf("second instance update: version=%d err=%v", version, err)
		})
		invalid := json.RawMessage(strings.Replace(string(rewriteDefinition(t, "invalid")), `"sequence"`, `"unknown_action"`, 1))
		{
			_, err := first.PublishRules(ctx, nil, []store.RuleChange{{Kind: "create", ID: "invalid", Rule: invalid}}, ValidateSnapshot)
			reportTestFailure(t, err == nil, func() {
				t.Fatal("invalid candidate was published")
			})
		}
		after, afterVersion, err := service.Load(ctx)
		reportTestFailure(t, err != nil || afterVersion != version || after != engine, func() {
			t.Fatalf("rejected update invalidated working snapshot: %v", err)
		})
	}
	{
		got := modelFromEngine(t, pinned)
		reportTestFailure(t, got != "first-published", func() {
			t.Fatalf("in-flight published snapshot changed: %q", got)
		})
	}
}

func TestRuntimeLoadFailsClosedAfterStoreFailure(t *testing.T) {
	st := runtimeStore(t, filepath.Join(runtimeTempDir(t), "closed.db"))
	service := New(st)
	if _, _, err := service.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if engine, version, err := service.Load(t.Context()); err == nil || engine != nil || version != 0 {
		t.Fatalf("returned revoked cache after store failure: engine=%p version=%d err=%v", engine, version, err)
	}
}

func TestRuntimeMigrationRepairConcurrentEnsureAndDeletedRules(t *testing.T) {
	path := filepath.Join(runtimeTempDir(t), "migration.db")
	first, second := runtimeStore(t, path), runtimeStore(t, path)
	ctx := t.Context()
	broken := &store.MiddlewareRow{Name: "block_prompt", Enabled: true, Config: `{"pattern":"["}`}
	if err := first.UpsertMiddleware(ctx, broken); err != nil {
		t.Fatal(err)
	}
	service := New(first)
	if err := service.Ensure(ctx); !errors.Is(err, ErrMigration) {
		t.Fatalf("invalid legacy config did not report repair requirement: %v", err)
	}
	if engine, version, err := service.Load(ctx); !errors.Is(err, ErrMigration) || engine != nil || version != 0 {
		t.Fatalf("failed migration switched engines: engine=%p version=%d err=%v", engine, version, err)
	}
	snapshot, err := first.LoadRuleSet(ctx)
	if err != nil || snapshot.Version != 0 || snapshot.LegacyMigrated || len(snapshot.Rules) != 0 {
		t.Fatalf("migration partially committed: %+v err=%v", snapshot, err)
	}
	broken.Config = `{"pattern":"blocked"}`
	if err := first.RepairLegacyMiddleware(ctx, broken); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, st := range []*store.Store{first, second, first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := New(st).Ensure(ctx); err != nil {
				t.Errorf("concurrent migration: %v", err)
			}
		}()
	}
	wg.Wait()
	snapshot, err = first.LoadRuleSet(ctx)
	if err != nil || snapshot.Version != 1 || !snapshot.LegacyMigrated || len(snapshot.Rules) != 13 {
		t.Fatalf("migration did not initialize exactly once: %+v err=%v", snapshot, err)
	}
	deletions := make([]store.RuleChange, len(snapshot.Rules))
	for i, row := range snapshot.Rules {
		deletions[i] = store.RuleChange{Kind: "delete", ID: row.ID, ExpectedRevision: row.Revision}
	}
	if _, err := first.PublishRules(ctx, &snapshot.Version, deletions, ValidateSnapshot); err != nil {
		t.Fatal(err)
	}
	if err := New(second).Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	engine, version, err := New(second).Load(ctx)
	if err != nil || version != 2 || engine.HasPhase(rules.PhaseRequest) {
		t.Fatalf("restart resurrected deleted rules: version=%d err=%v", version, err)
	}
}

func TestRuntimeMetadataAndStrictSnapshotDecoding(t *testing.T) {
	definition := rules.Rule{
		SchemaVersion: rules.SchemaVersion, Name: "metadata", Enabled: true, Phase: rules.PhaseRequest,
		When: rules.Condition{Op: "always"}, Actions: []rules.Action{}, OnError: rules.OnErrorAbort,
		ID: "untrusted", Revision: 999, OrderIndex: 999, CreatedAt: "2000-01-01T00:00:00Z",
		UpdatedAt: "2000-01-01T00:00:00Z", LegacyName: "untrusted", Source: "untrusted",
	}
	editable, err := EditableJSON(definition)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(editable, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "revision", "order_index", "created_at", "updated_at", "legacy_name", "source"} {
		if _, ok := fields[key]; ok {
			t.Errorf("persisted editable AST contains %q", key)
		}
	}
	raw, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2025, time.January, 2, 3, 4, 5, 6_000_000, time.UTC)
	row := &store.RuleRow{ID: "trusted", Revision: 7, OrderIndex: 23, CreatedAt: stamp.UnixMilli(), UpdatedAt: stamp.UnixMilli(), LegacyName: "rewrite_model", Source: "legacy", Rule: raw}
	snapshot := &store.RuleSetSnapshot{Version: 2, LegacyMigrated: true, Rules: []*store.RuleRow{row}}
	decoded, err := DecodeSnapshot(snapshot)
	if err != nil || len(decoded) != 1 {
		t.Fatalf("metadata decode: %v", err)
	}
	got := decoded[0]
	if got.ID != row.ID || got.Revision != row.Revision || got.OrderIndex != row.OrderIndex || got.LegacyName != row.LegacyName || got.Source != row.Source || got.CreatedAt != stamp.Format(time.RFC3339Nano) || got.UpdatedAt != got.CreatedAt {
		t.Fatalf("untrusted metadata escaped: %+v", got)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := ValidateSnapshot(cancelled, snapshot); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled validation: %v", err)
	}
	for name, rows := range map[string][]*store.RuleRow{
		"nil row":           {nil},
		"unknown AST field": {{ID: "bad", Rule: json.RawMessage(`{"schema_version":1,"unknown":true}`)}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeSnapshot(&store.RuleSetSnapshot{Rules: rows}); err == nil {
				t.Fatal("malformed row silently accepted")
			}
		})
	}
}
