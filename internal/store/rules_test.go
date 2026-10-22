package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func testRuleJSON(name string, priority int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"schema_version":1,"name":%q,"description":"searchable","enabled":true,"priority":%d,"phase":"request","when":{"op":"always"},"actions":[],"stop_after_match":false,"on_error":"abort"}`, name, priority))
}

func acceptRuleSet(context.Context, *RuleSetSnapshot) error { return nil }

func exerciseRulePublication(t *testing.T, first, second *Store) {
	t.Helper()
	ctx := context.Background()
	initial, err := first.LoadRuleSet(ctx)
	requireDataTruef(t, !(err != nil || initial.Version != 0 || len(initial.Rules) != 0), "initial=%+v err=%v", initial, err)
	version := int64(0)
	snapshot, err := first.PublishRules(ctx, &version, []RuleChange{
		{Kind: "create", ID: "b", Rule: testRuleJSON("same name", 20)},
		{Kind: "create", ID: "a", Rule: testRuleJSON("same name", 10)},
	}, acceptRuleSet)
	requireDataTruef(t, !(err != nil || snapshot.Version != 1 || len(snapshot.Rules) != 2 || snapshot.Rules[0].ID != "a"), "create=%+v err=%v", snapshot, err)
	{
		_, err := first.PublishRules(ctx, &version, []RuleChange{{Kind: "create", ID: "c", Rule: testRuleJSON("stale", 0)}}, acceptRuleSet)
		requireDataTruef(t, !(!errors.Is(err, ErrRuleConflict)), "stale version accepted: %v", err)
	}
	{
		_, err := first.PublishRules(ctx, nil, []RuleChange{{Kind: "update", ID: "a", ExpectedRevision: 2, Rule: testRuleJSON("stale", 0)}}, acceptRuleSet)
		requireDataTruef(t, !(!errors.Is(err, ErrRuleConflict)), "stale revision accepted: %v", err)
	}
	rejected := errors.New("candidate compilation rejected")
	calls := 0
	_, err = first.PublishRules(ctx, nil, []RuleChange{{Kind: "update", ID: "a", ExpectedRevision: 1, Rule: testRuleJSON("reject", 0)}}, func(_ context.Context, candidate *RuleSetSnapshot) error {
		calls++
		requireDataTruef(t, !(candidate.Version != 2 || len(candidate.Rules) != 2), "validator got partial candidate: %+v", candidate)
		return rejected
	})
	requireDataTruef(t, !(!errors.Is(err, rejected) || calls != 1), "validator=%d error=%v", calls, err)
	seen, err := second.LoadRuleSet(ctx)
	requireDataTruef(t, !(err != nil || seen.Version != 1 || seen.Rules[0].Name != "same name" || seen.Rules[0].Revision != 1), "failed mutation escaped transaction: %+v %v", seen, err)
	page, err := second.ListRules(ctx, RuleListFilter{Search: "SAME", Page: 2, PageSize: 1})
	requireDataTruef(t, !(err != nil || page.Total != 2 || page.Version != 1 || len(page.Rules) != 1 || page.Rules[0].ID != "b"), "page=%+v err=%v", page, err)
	page, err = second.ListRules(ctx, RuleListFilter{Search: "%"})
	requireDataTruef(t, !(err != nil || page.Total != 0), "search wildcard was not literal: %+v %v", page, err)
	priority, order, disabled := 10, int64(0), false
	snapshot, err = first.PublishRules(ctx, nil, []RuleChange{
		{Kind: "patch", ID: "a", ExpectedRevision: 1, OrderIndex: &order},
		{Kind: "patch", ID: "b", ExpectedRevision: 1, Priority: &priority, OrderIndex: &order, Enabled: &disabled},
	}, acceptRuleSet)
	requireDataTruef(t, !(err != nil || snapshot.Version != 2 || snapshot.Rules[0].ID != "a" || snapshot.Rules[1].Enabled), "reorder=%+v err=%v", snapshot, err)
	var doc map[string]any
	{
		err := json.Unmarshal(snapshot.Rules[1].Rule, &doc)
		requireDataTruef(t, !(err != nil || doc["enabled"] != false || doc["priority"] != float64(10)), "index and AST disagree: %v %v", doc, err)
	}
	_, err = first.PublishRules(ctx, nil, []RuleChange{
		{Kind: "delete", ID: "a", ExpectedRevision: 2},
		{Kind: "delete", ID: "b", ExpectedRevision: 1},
	}, acceptRuleSet)
	requireDataTruef(t, !(!errors.Is(err, ErrRuleConflict)), "non-atomic batch: %v", err)
	{
		row, _ := first.GetRule(ctx, "a")
		requireDataTrue(t, row != nil, "first batch deletion committed before conflict")
	}
	snapshot, err = first.PublishRules(ctx, nil, []RuleChange{{Kind: "duplicate", ID: "a", NewID: "copy", ExpectedRevision: 2}}, acceptRuleSet)
	requireDataTruef(t, !(err != nil || snapshot.Version != 3 || len(snapshot.Rules) != 3), "duplicate=%+v %v", snapshot, err)
	copyRow, _ := first.GetRule(ctx, "copy")
	requireDataTruef(t, !(copyRow.Revision != 1 || copyRow.LegacyName != "" || copyRow.Source != "duplicate:a" || copyRow.Name != "same name (copy)"), "copy metadata=%+v", copyRow)
	encoded, err := json.Marshal(copyRow)
	requireDataTruef(t, !(err != nil || !strings.Contains(string(encoded), `"schema_version":1`) || !strings.Contains(string(encoded), `"created_at":"`) || strings.Contains(string(encoded), `"rule":`)), "flat API JSON=%s err=%v", encoded, err)
	var wins, conflicts atomic.Int32
	var wg sync.WaitGroup
	for _, st := range []*Store{first, second} {
		wg.Add(1)
		go func(st *Store) { verifyExerciseRulePublication(st, &t, &ctx, &wins, &conflicts, &wg) }(st)
	}
	wg.Wait()
	reportStoreTestFailure(t, wins.Load() != 1 || conflicts.Load() != 1, func() {
		t.Fatalf("wins=%d conflicts=%d", wins.Load(), conflicts.Load())
	})
	{
		version, err := first.RuleSetVersion(ctx)
		requireDataTruef(t, !(err != nil || version != 4), "published version=%d %v", version, err)
	}
	exerciseRuleSnapshotMarker(t, first, second)
}

// Shared by SQLite and the real PostgreSQL publication test. In particular,
// PostgreSQL rejects even an ON CONFLICT DO NOTHING insert in a read-only tx.
func exerciseRuleSnapshotMarker(t *testing.T, first, second *Store) {
	t.Helper()
	ctx := context.Background()
	published, err := first.PublishRules(ctx, nil, []RuleChange{
		{Kind: "create", ID: "language-v2-installed", Rule: testRuleJSON("V2 installed", 0)},
	}, acceptRuleSet)
	requireDataNoError(t, err)
	installed, err := second.RulesV2Installed(ctx)
	requireDataTruef(t, !(err != nil || !installed), "publication did not persist installation marker: installed=%t err=%v", installed, err)
	{
		_, err := first.ExecContext(ctx, `DELETE FROM settings WHERE key='rules.language.v2'`)
		requireDataTrue(t, err == nil, err)
	}
	// Mimic an earlier V2 installation that has the rule but no durable setting.
	loaded, err := second.LoadRuleSet(ctx)
	requireDataTruef(t, !(err != nil || loaded.Version != published.Version || !hasV2Marker(loaded)), "read-only V2 snapshot: %+v err=%v", loaded, err)
	installed, err = second.RulesV2Installed(ctx)
	requireDataTruef(t, !(err != nil || installed), "snapshot load must not write a setting: installed=%t err=%v", installed, err)
	for range 2 {
		installed, err = second.RestoreRulesV2Marker(ctx)
		requireDataTruef(t, !(err != nil || !installed), "restore old installation marker: installed=%t err=%v", installed, err)
	}
	{
		version, err := first.RuleSetVersion(ctx)
		requireDataTruef(t, !(err != nil || version != published.Version), "marker repair changed publication version: %d %v", version, err)
	}
	deleted, err := first.PublishRules(ctx, nil, []RuleChange{
		{Kind: "delete", ID: "language-v2-installed", ExpectedRevision: 1},
	}, acceptRuleSet)
	requireDataNoError(t, err)
	installed, err = second.RestoreRulesV2Marker(ctx)
	requireDataTruef(t, !(err != nil || !installed), "rule deletion lost durable installation: installed=%t err=%v", installed, err)
	loaded, err = second.LoadRuleSet(ctx)
	requireDataTruef(t, !(err != nil || loaded.Version != deleted.Version || hasV2Marker(loaded)), "marker repair resurrected deleted rule: %+v err=%v", loaded, err)
}

func TestRulesSQLitePublication(t *testing.T) {
	path := storeTestPath(t)
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	exerciseRulePublication(t, first, second)
}

func TestRulesFullSnapshotBeyondManagementPage(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	changes := make([]RuleChange, 1000)
	for i := range changes {
		changes[i] = RuleChange{Kind: "create", ID: fmt.Sprintf("r-%04d", i), Rule: testRuleJSON("repeated", i)}
	}
	snapshot, err := s.PublishRules(ctx, nil, changes, func(_ context.Context, candidate *RuleSetSnapshot) error {
		if len(candidate.Rules) != 1000 || candidate.Rules[999].ID != "r-0999" {
			t.Fatal("validator was paginated")
		}
		return nil
	})
	if err != nil || len(snapshot.Rules) != 1000 {
		t.Fatalf("create 1000: %v", err)
	}
	page, err := s.ListRules(ctx, RuleListFilter{PageSize: 1000})
	if err != nil || page.Total != 1000 || len(page.Rules) != 200 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	loaded, err := s.LoadRuleSet(ctx)
	if err != nil || len(loaded.Rules) != 1000 || loaded.Rules[999].ID != "r-0999" {
		t.Fatalf("data plane paginated: %v", err)
	}
}

func TestRulesInitializationAtomicOnceAndDeletion(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	legacy := &MiddlewareRow{Name: "rewrite_model", Enabled: true, Config: `{"model":"old"}`}
	if err := s.UpsertMiddleware(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	calls := 0
	convert := func(_ context.Context, old []*MiddlewareRow) ([]*RuleRow, error) {
		calls++
		assertLegacyRuleSource(t, old, legacy)
		return []*RuleRow{{ID: "legacy:rewrite_model", Rule: testRuleJSON("legacy", 0), LegacyName: legacy.Name, Source: "legacy"}}, nil
	}
	if _, err := s.InitializeRules(ctx, convert, func(context.Context, *RuleSetSnapshot) error { return errors.New("invalid legacy") }); err == nil {
		t.Fatal("invalid legacy migration committed")
	}
	unchanged, _ := s.LoadRuleSet(ctx)
	if unchanged.Version != 0 || unchanged.LegacyMigrated || len(unchanged.Rules) != 0 {
		t.Fatalf("partial migration=%+v", unchanged)
	}
	snapshot, err := s.InitializeRules(ctx, convert, acceptRuleSet)
	if err != nil || snapshot.Version != 1 || !snapshot.LegacyMigrated {
		t.Fatalf("initialize=%+v err=%v", snapshot, err)
	}
	assertLegacyRuleSourceRecorded(t, s, ctx)
	if _, err := s.PublishRules(ctx, nil, []RuleChange{{Kind: "delete", ID: "legacy:rewrite_model", ExpectedRevision: 1}}, acceptRuleSet); err != nil {
		t.Fatal(err)
	}
	assertDeletedLegacyRulesStayDeleted(t, s, ctx, convert, &calls)
	old, err := s.GetMiddleware(ctx, legacy.Name)
	if err != nil || old.Config != legacy.Config {
		t.Fatalf("legacy table mutated: %+v %v", old, err)
	}
}

func assertLegacyRuleSourceRecorded(t *testing.T, s *Store, ctx context.Context) {
	t.Helper()
	var raw string
	if err := s.QueryRowContext(ctx, `SELECT legacy_source FROM rule_set_metadata WHERE singleton=1`).Scan(&raw); err != nil || !strings.Contains(raw, "rewrite_model") {
		t.Fatalf("original migration source missing: %s %v", raw, err)
	}
}

func assertLegacyRuleSource(t *testing.T, old []*MiddlewareRow, legacy *MiddlewareRow) {
	t.Helper()
	if len(old) != 1 || old[0].Config != legacy.Config {
		t.Fatal("legacy source changed")
	}
}

func assertDeletedLegacyRulesStayDeleted(t *testing.T, s *Store, ctx context.Context, convert LegacyRuleConverter, calls *int) {
	t.Helper()
	for i := 0; i < 2; i++ {
		snapshot, err := s.InitializeRules(ctx, convert, acceptRuleSet)
		if err != nil || len(snapshot.Rules) != 0 || snapshot.Version != 2 || *calls != 2 {
			t.Fatalf("deleted rules resurrected: %+v calls=%d err=%v", snapshot, *calls, err)
		}
	}
}

func TestRulesWhitespaceAndMissingPreconditions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, raw := range []string{"", "  \n ", "null", "[]", "{}"} {
		if _, err := s.PublishRules(ctx, nil, []RuleChange{{Kind: "create", Rule: json.RawMessage(raw)}}, acceptRuleSet); !errors.Is(err, ErrRuleInvalid) {
			t.Fatalf("invalid %q: %v", raw, err)
		}
	}
	if _, err := s.PublishRules(ctx, nil, []RuleChange{{Kind: "create", Rule: testRuleJSON("no validator", 0)}}, nil); !errors.Is(err, ErrRuleInvalid) {
		t.Fatal("nil validator accepted")
	}
}

func TestRulesAdditiveMigrationKeepsLegacyData(t *testing.T) {
	path := storeTestPath(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE middlewares(name TEXT PRIMARY KEY,enabled INTEGER NOT NULL,order_index INTEGER NOT NULL,config TEXT NOT NULL,updated_at INTEGER NOT NULL);
	 INSERT INTO middlewares VALUES ('rewrite_model',0,17,'{"model":"legacy"}',123)`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		row, err := s.GetMiddleware(context.Background(), "rewrite_model")
		if err != nil || row.Enabled || row.OrderIndex != 17 || row.UpdatedAt != 123 {
			t.Errorf("legacy changed: %+v %v", row, err)
		}
		snapshot, err := s.LoadRuleSet(context.Background())
		if err != nil || snapshot.Version != 0 || snapshot.LegacyMigrated {
			t.Errorf("additive migration=%+v %v", snapshot, err)
		}
		s.Close()
	}
}

func TestRulesAdjacentMoveAcrossTiedPriorities(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	zero := int64(0)
	_, err := s.PublishRules(ctx, nil, []RuleChange{
		{Kind: "create", ID: "z", Rule: testRuleJSON("last tied", 0), OrderIndex: &zero},
		{Kind: "create", ID: "m", Rule: testRuleJSON("first tied", 0), OrderIndex: &zero},
		{Kind: "create", ID: "a", Rule: testRuleJSON("moves exactly once", 1), OrderIndex: &zero},
	}, acceptRuleSet)
	if err != nil {
		t.Fatal(err)
	}
	version := int64(1)
	snapshot, err := s.PublishRules(ctx, &version, []RuleChange{{Kind: "move_up", ID: "a", ExpectedRevision: 1}}, acceptRuleSet)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"m", "a", "z"} {
		if snapshot.Rules[i].ID != id {
			t.Fatalf("move skipped a tied neighbor: %+v", snapshot.Rules)
		}
	}
	if len(snapshot.ChangedIDs) != 2 {
		t.Fatalf("changed IDs=%v", snapshot.ChangedIDs)
	}
	before := snapshot.Version
	if _, err := s.PublishRules(ctx, &before, []RuleChange{{Kind: "move_up", ID: "m", ExpectedRevision: 1}}, acceptRuleSet); !errors.Is(err, ErrRuleInvalid) {
		t.Fatalf("phase-boundary move=%v", err)
	}
	if after, _ := s.RuleSetVersion(ctx); after != before {
		t.Fatal("failed boundary move changed version")
	}
}

func TestRulesPostgresPublicationAndUpgrade(t *testing.T) {
	dsn := os.Getenv("PI_GATEWAY_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PI_GATEWAY_TEST_POSTGRES_DSN is not set")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid PostgreSQL test configuration")
	}
	admin := stdlib.OpenDB(*cfg)
	defer admin.Close()
	schema := "rules_test_" + strings.TrimPrefix(GenerateKey(), "sk-pi-")
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
	cfg.RuntimeParams["search_path"] = schema
	registered := stdlib.RegisterConnConfig(cfg)
	defer stdlib.UnregisterConnConfig(registered)
	first, err := OpenOptions(Options{Driver: "postgres", DSN: registered})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	// Recreate the pre-rules version marker and remove only these test schema tables.
	if _, err := first.ExecContext(context.Background(), `DROP TABLE rules`); err != nil {
		t.Fatal(err)
	}
	if _, err := first.ExecContext(context.Background(), `DROP TABLE rule_set_metadata`); err != nil {
		t.Fatal(err)
	}
	if _, err := first.ExecContext(context.Background(), `DELETE FROM schema_migrations WHERE version=?`, postgresSchemaVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := first.ExecContext(context.Background(), `INSERT INTO schema_migrations(version,applied_at) VALUES(2,0)`); err != nil {
		t.Fatal(err)
	}
	second, err := OpenOptions(Options{Driver: "postgres", DSN: registered})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	exerciseRulePublication(t, first, second)
}

// verifyExerciseRulePublication exercises one isolated test phase with the original shared state.
func verifyExerciseRulePublication(st *Store, capturedT **testing.T, capturedCtx *context.Context, capturedWins *atomic.Int32, capturedConflicts *atomic.Int32, capturedWg *sync.WaitGroup) {
	(*capturedT).Helper()

	defer (*capturedWg).Done()
	_, err := st.PublishRules((*capturedCtx), nil, []RuleChange{{Kind: "update", ID: "a", ExpectedRevision: 2, Rule: testRuleJSON("concurrent winner", 10)}}, acceptRuleSet)
	if err == nil {
		(*capturedWins).Add(1)
	} else if errors.Is(err, ErrRuleConflict) {
		(*capturedConflicts).Add(1)
	} else {
		(*capturedT).Errorf("concurrent writer: %v", err)
	}
}
