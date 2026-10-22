package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"modernc.org/sqlite"
)

// catalogSQLObserver wraps only a test driver's reads; production has no counters
// or observer API. The same real SQLite engine accepts both ? and $n bindings,
// allowing offline execution of the actual PostgreSQL-rebound read statements.
// This is a placeholder contract test, not a replacement for real PostgreSQL CI.
type catalogSQLObserver struct {
	mu      sync.Mutex
	queries []catalogObservedQuery
}

type catalogObservedQuery struct {
	sql  string
	args []driver.NamedValue
}

func (o *catalogSQLObserver) reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.queries = nil
}

func (o *catalogSQLObserver) snapshot() []catalogObservedQuery {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]catalogObservedQuery(nil), o.queries...)
}

type catalogObserverConnector struct {
	observer *catalogSQLObserver
	driver   sqlite.Driver
}

func (c *catalogObserverConnector) Driver() driver.Driver { return &c.driver }
func (c *catalogObserverConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(":memory:")
	if err != nil {
		return nil, err
	}
	return &catalogObservedConn{Conn: conn, observer: c.observer}, nil
}

type catalogObservedConn struct {
	driver.Conn
	observer *catalogSQLObserver
}

func (c *catalogObservedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func (c *catalogObservedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}
func (c *catalogObservedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.observer.mu.Lock()
	c.observer.queries = append(c.observer.queries, catalogObservedQuery{query, append([]driver.NamedValue(nil), args...)})
	c.observer.mu.Unlock()
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func observedCatalogStore(t *testing.T) (*Store, *catalogSQLObserver) {
	t.Helper()
	observer := &catalogSQLObserver{}
	db := sql.OpenDB(&catalogObserverConnector{observer: observer})
	db.SetMaxOpenConns(1)
	s := &Store{db: db, driver: "sqlite"}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.migrate(); err != nil {
		t.Fatal(err)
	}
	observer.reset()
	return s, observer
}

func TestAccountModelBatchReadCountAndDialectContract(t *testing.T) {
	ctx := context.Background()
	s, observer := observedCatalogStore(t)
	const count = accountModelBatchSize*2 + 1
	// Fixture insertion is intentionally batched in a transaction and excluded
	// from observed read counts, so large boundary tests stay inexpensive.
	tx, err := s.BeginTx(ctx, nil)
	requireDataNoError(t, err)
	defer tx.Rollback()
	for id := 1; id <= count; id++ {
		{
			_, err := tx.ExecContext(ctx, `INSERT INTO accounts(id,name,created_at,updated_at) VALUES(?,?,0,0)`, id, "fixture")
			requireDataTrue(t, !(err != nil), err)
		}
	}
	requireDataNoError(t, tx.Commit())
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			s.driver = dialect
			for _, n := range []int{0, 1, 20, 100, accountModelBatchSize, accountModelBatchSize + 1, count} {
				t.Run(fmt.Sprint(n), func(t *testing.T) {
					testAccountModelBatchReadCountAndDialectContractCase1(t, ctx, dialect, n, observer, s)
				})
			}
		})
	}
	s.driver = "sqlite"
	func() { verifyAccountModelBatchReadCountAndDialectContract(&t, &ctx, &s, &observer, &err) }()
	allIDs := make([]int64, count)
	for i := range allIDs {
		allIDs[i] = int64(i + 1)
	}
	// A corrupt policy/catalog in batch three must discard both good batches.
	{
		_, err := s.ExecContext(ctx, `UPDATE accounts SET disabled_models='null', supplemental_models='null' WHERE id=?`, count)
		requireDataTrue(t, !(err != nil), err)
	}
	{
		out, err := s.GetAccountModelCatalogs(ctx, allIDs)
		requireDataTrue(t, !(err == nil || out != nil), "late catalog failure returned partial results")
	}
	{
		out, err := s.DisabledModelsForAccounts(ctx, allIDs, nil)
		requireDataTrue(t, !(err == nil || out != nil), "late policy failure returned partial results")
	}
	{
		_, err := s.ExecContext(ctx, `DELETE FROM accounts WHERE id=?`, count)
		requireDataTrue(t, !(err != nil), err)
	}
	{
		out, err := s.DisabledModelsForAccounts(ctx, allIDs, nil)
		requireDataTrue(t, !(!errors.Is(err, sql.ErrNoRows) || out != nil), "late unknown ID returned partial restrictions")
	}
}

// Shared contract is also run by the optional real PostgreSQL integration test.
func exerciseAccountModelBatches(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	a := &Account{Name: "batch-rich", Enabled: true, SupplementalModels: []string{"shared", "manual"}, DisabledModels: []string{"account-blocked", "shared"}}
	b := &Account{Name: "batch-empty", Enabled: true}
	for _, account := range []*Account{a, b} {
		requireDataNoError(t, s.CreateAccount(ctx, account))
	}
	metadata := map[string]json.RawMessage{
		"large":  json.RawMessage(`9007199254740993123456789`),
		"nested": json.RawMessage(`{"x":[null,false,[],{"huge":18446744073709551617}]}`),
		"empty":  json.RawMessage(`[]`), "null": json.RawMessage(`null`),
		"false": json.RawMessage(`false`), "sensitive_unknown": json.RawMessage(`"preserved-not-sanitized-by-store"`),
	}
	requireDataNoError(t, s.SaveAccountModelCatalog(ctx, a.ID, &ModelCatalog{Models: []CatalogModel{
		{ID: " shared ", Name: "Rich", Description: "Complete", Metadata: metadata},
		{ID: "shared", Name: "duplicate"}, {ID: "", Name: "empty"},
	}, FetchedAt: 10, AttemptedAt: 11}))
	missing := max(a.ID, b.ID) + 999999
	ids := []int64{b.ID, missing, a.ID, a.ID, 0, -1}
	got, err := s.GetAccountModelCatalogs(ctx, ids)
	requireDataTruef(t, !(err != nil || len(got) != 2 || got[missing] != nil), "catalog unknown/duplicate semantics: %+v err=%v", got, err)
	for _, account := range []*Account{a, b} {
		one, err := s.GetAccountModelCatalog(ctx, account.ID)
		{
			reportStoreTestFailure(t, err != nil || !reflect.DeepEqual(one, got[account.ID]), func() {
				t.Fatalf("single/batch mismatch: %+v %+v err=%v", one, got[account.ID], err)
			})
		}
	}
	{
		reportStoreTestFailure(t, !reflect.DeepEqual(got[a.ID].Models[0].Metadata, metadata) || len(got[a.ID].Models) != 2 || !reflect.DeepEqual(got[a.ID].Models[1], NewManualCatalogModel("manual")), func() {
			t.Fatalf("metadata/manual defaults changed: %+v", got[a.ID])
		})
	}
	requireDataCondition(t, !(got[b.ID].Models == nil || got[b.ID].FetchedAt != 0 || len(got[b.ID].Models) != 0), "empty account fabricated fetched models")
	member := &AccountGroup{Name: "batch-member", Enabled: true, AccountIDs: []int64{a.ID}, DisabledModels: []string{"membership-blocked", "shared"}}
	keyGroup := &AccountGroup{Name: "batch-key", Enabled: true, DisabledModels: []string{"key-blocked", "shared"}}
	disabled := &AccountGroup{Name: "batch-disabled", Enabled: false, AccountIDs: []int64{a.ID, b.ID}, DisabledModels: []string{"ignored"}}
	for _, group := range []*AccountGroup{member, keyGroup, disabled} {
		requireDataNoError(t, s.CreateAccountGroup(ctx, group))
	}
	key := &APIKey{Name: "batch-key", GroupIDs: []int64{member.ID}}
	requireDataNoError(t, s.CreateKey(ctx, key))
	// Leave key.GroupIDs stale while changing its authoritative database bindings.
	requireDataNoError(t, s.SetKeyGroups(ctx, key.ID, []int64{keyGroup.ID, disabled.ID}))
	{
		_, err := s.ExecContext(ctx, `UPDATE account_groups SET disabled_models='not-json' WHERE id=?`, disabled.ID)
		requireDataTrue(t, !(err != nil), err)
	}
	assertRestrictions := func(k *APIKey, wantA, wantB []string) {
		verifyExerciseAccountModelBatches(&ctx, k, wantA, wantB, verifyExerciseAccountModelBatchesState{capturedT: &t, capturedS: &s, capturedA: &a, capturedB: &b})
	}
	assertRestrictions(key, []string{"account-blocked", "key-blocked", "membership-blocked", "shared"}, []string{"key-blocked", "shared"})
	assertRestrictions(nil, []string{"account-blocked", "membership-blocked", "shared"}, []string{})
	requireDataNoError(t, s.SetKeyGroups(ctx, key.ID, []int64{}))
	assertRestrictions(key, []string{"account-blocked", "membership-blocked", "shared"}, []string{})
	requireDataNoError(t, s.SetAccountGroupAccounts(ctx, member.ID, []int64{b.ID}))
	assertRestrictions(key, []string{"account-blocked", "shared"}, []string{"membership-blocked", "shared"})
	for _, id := range []int64{missing, 0, -1} {
		{
			out, err := s.DisabledModelsForAccounts(ctx, []int64{a.ID, id}, key)
			requireDataTruef(t, !(!errors.Is(err, sql.ErrNoRows) || out != nil), "unknown ID failed open: %v %v", out, err)
		}
		{
			out, err := s.DisabledModelsForAccount(ctx, id, key)
			requireDataTruef(t, !(!errors.Is(err, sql.ErrNoRows) || out != nil), "legacy missing semantics changed: %v %v", out, err)
		}
	}
	// All applicable policy sources must fail closed. Disabled groups above are
	// ignored even when malformed; newly enabled groups must fail immediately.
	requireDataNoError(t, s.SetKeyGroups(ctx, key.ID, []int64{keyGroup.ID}))
	func() {
		verifyExerciseAccountModelBatches2(&ctx, verifyExerciseAccountModelBatches2State{capturedT: &t, capturedS: &s, capturedA: &a, capturedB: &b, capturedMember: &member, capturedKeyGroup: &keyGroup, capturedKey: &key})
	}()
	for _, field := range []struct{ table, column, idColumn string }{
		{"account_model_catalog", "models_json", "account_id"}, {"accounts", "supplemental_models", "id"},
	} {
		query := "UPDATE " + field.table + " SET " + field.column + "=? WHERE " + field.idColumn + "=?"
		{
			_, err := s.ExecContext(ctx, query, "not-json", a.ID)
			requireDataTrue(t, !(err != nil), err)
		}
		{
			out, err := s.GetAccountModelCatalogs(ctx, []int64{a.ID, b.ID})
			requireDataTruef(t, !(err == nil || out != nil), "malformed catalog returned partial/success: %v %v", out, err)
		}
		{
			_, err := s.ExecContext(ctx, query, "[]", a.ID)
			requireDataTrue(t, !(err != nil), err)
		}
	}
}

func TestAccountModelBatchSemantics(t *testing.T) {
	exerciseAccountModelBatches(t, openTestStore(t))
}

func TestAccountModelBatchEmptyInputAndDatabaseFailures(t *testing.T) {
	s := openTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, ids := range [][]int64{nil, {}} {
		if out, err := s.GetAccountModelCatalogs(ctx, ids); err != nil || out == nil || len(out) != 0 {
			t.Fatalf("empty catalog unexpectedly read DB: %v %v", out, err)
		}
		if out, err := s.DisabledModelsForAccounts(ctx, ids, nil); err != nil || out == nil || len(out) != 0 {
			t.Fatalf("empty policy unexpectedly read DB: %v %v", out, err)
		}
	}
	if out, err := s.GetAccountModelCatalogs(ctx, []int64{1}); err == nil || out != nil {
		t.Fatal("catalog DB error swallowed")
	}
	if out, err := s.DisabledModelsForAccounts(ctx, []int64{1}, nil); err == nil || out != nil {
		t.Fatal("policy DB error swallowed")
	}
	if revision, err := s.CatalogRevision(ctx); err == nil || revision != "" {
		t.Fatal("revision DB error silently defaulted")
	}
}

func testAccountModelBatchReadCountAndDialectContractCase1(t *testing.T, ctx context.Context, dialect string, n int, observer *catalogSQLObserver, s *Store) {
	ids := make([]int64, 0, n*2)
	for id := n; id > 0; id-- {
		ids = append(ids, int64(id), int64(id))
	}
	original := append([]int64{}, ids...)
	for _, restrictions := range []bool{false, true} {
		observer.reset()
		if restrictions {
			got, err := s.DisabledModelsForAccounts(ctx, ids, &APIKey{ID: 42})
			{
				reportStoreTestFailure(t, err != nil || got == nil || len(got) != n, func() {
					t.Fatalf("restrictions size=%d err=%v", len(got), err)
				})
			}
		} else {
			got, err := s.GetAccountModelCatalogs(ctx, ids)
			{
				reportStoreTestFailure(t, err != nil || got == nil || len(got) != n, func() {
					t.Fatalf("catalog size=%d err=%v", len(got), err)
				})
			}
		}
		queries := observer.snapshot()
		{
			reportStoreTestFailure(t, len(queries) != (n+accountModelBatchSize-1)/accountModelBatchSize, func() {
				t.Fatalf("N+1 regression: n=%d reads=%d", n, len(queries))
			})
		}
		func() {
			verifyAccountModelBatchReadCountAndDialectContractCase12(&t, &dialect, &n, &restrictions, &queries)
		}()
	}
	requireDataCondition(t, !(!reflect.DeepEqual(ids, original)), "caller IDs mutated")
}

type verifyExerciseAccountModelBatchesState struct {
	capturedT **testing.T
	capturedS **Store
	capturedA **Account
	capturedB **Account
}

// verifyExerciseAccountModelBatches exercises one isolated test phase with the original shared state.
func verifyExerciseAccountModelBatches(capturedCtx *context.Context, k *APIKey, wantA, wantB []string, state verifyExerciseAccountModelBatchesState) {
	(*state.capturedT).Helper()
	out, err := (*state.capturedS).DisabledModelsForAccounts((*capturedCtx), []int64{(*state.capturedB).ID, (*state.capturedA).ID, (*state.capturedA).ID}, k)
	requireDataTruef((*state.capturedT), !(err != nil || len(out) != 2 || !reflect.DeepEqual(out[(*state.capturedA).ID], wantA) || !reflect.DeepEqual(out[(*state.capturedB).ID], wantB)), "restrictions=%+v err=%v want=%v/%v", out, err, wantA, wantB)
	for _, account := range []*Account{(*state.capturedA), (*state.capturedB)} {
		one, err := (*state.capturedS).DisabledModelsForAccount((*capturedCtx), account.ID, k)
		{
			reportStoreTestFailure((*state.capturedT), err != nil || !reflect.DeepEqual(one, out[account.ID]), func() {
				(*state.capturedT).Fatalf("single/batch restrictions mismatch: %v %v err=%v", one, out[account.ID], err)
			})
		}
	}
}

// verifyAccountModelBatchReadCountAndDialectContract exercises one isolated test phase with the original shared state.
func verifyAccountModelBatchReadCountAndDialectContract(capturedT **testing.T, capturedCtx *context.Context, capturedS **Store, capturedObserver **catalogSQLObserver, capturedErr *error) {
	(*capturedT).Helper()

	for _, restrictions := range []bool{false, true} {
		(*capturedObserver).reset()
		for id := int64(1); id <= 100; id++ {
			if restrictions {
				_, (*capturedErr) = (*capturedS).DisabledModelsForAccount((*capturedCtx), id, nil)
			} else {
				_, (*capturedErr) = (*capturedS).GetAccountModelCatalog((*capturedCtx), id)
			}
			requireDataNoError((*capturedT), (*capturedErr))
		}
		{
			got := len((*capturedObserver).snapshot())
			requireDataTruef((*capturedT), !(got != 100), "individual SQL baseline=%d want 100", got)
		}
	}
}

type verifyExerciseAccountModelBatches2State struct {
	capturedT        **testing.T
	capturedS        **Store
	capturedA        **Account
	capturedB        **Account
	capturedMember   **AccountGroup
	capturedKeyGroup **AccountGroup
	capturedKey      **APIKey
}

// verifyExerciseAccountModelBatches2 exercises one isolated test phase with the original shared state.
func verifyExerciseAccountModelBatches2(capturedCtx *context.Context, state verifyExerciseAccountModelBatches2State) {
	for _, source := range []struct {
		table string
		id    int64
	}{{"accounts", (*state.capturedA).ID}, {"account_groups", (*state.capturedMember).ID}, {"account_groups", (*state.capturedKeyGroup).ID}} {
		for _, malformed := range []string{"not-json", "null", `{}`, `[123]`, `[null]`} {
			query := "UPDATE " + source.table + " SET disabled_models=? WHERE id=?"
			{
				_, err := (*state.capturedS).ExecContext((*capturedCtx), query, malformed, source.id)
				requireDataTrue((*state.capturedT), !(err != nil), err)
			}
			{
				out, err := (*state.capturedS).DisabledModelsForAccounts((*capturedCtx), []int64{(*state.capturedA).ID, (*state.capturedB).ID}, (*state.capturedKey))
				reportStoreTestFailure((*state.capturedT), err == nil || out != nil, func() {
					(*state.capturedT).Fatalf("malformed %s %q failed open: %v %v", source.table, malformed, out, err)
				})
			}
			{
				_, err := (*state.capturedS).ExecContext((*capturedCtx), query, `[]`, source.id)
				requireDataTrue((*state.capturedT), !(err != nil), err)
			}
		}
	}
}

// verifyAccountModelBatchReadCountAndDialectContractCase1 exercises one isolated test phase with the original shared state.
func verifyAccountModelBatchReadCountAndDialectContractCase1(capturedT **testing.T, capturedRestrictions *bool, capturedBatch *int, capturedQuery *catalogObservedQuery) {
	(*capturedT).Helper()

	for i, arg := range (*capturedQuery).args {
		want := int64((*capturedBatch)*accountModelBatchSize + i + 1)
		if (*capturedRestrictions) && i == len((*capturedQuery).args)-1 {
			want = 42
		}
		requireDataTruef((*capturedT), !(arg.Ordinal != i+1 || arg.Value != want), "unstable/wrong arguments: %+v want %d", arg, want)
	}
}

// verifyAccountModelBatchReadCountAndDialectContractCase12 exercises one isolated test phase with the original shared state.
func verifyAccountModelBatchReadCountAndDialectContractCase12(capturedT **testing.T, capturedDialect *string, capturedN *int, capturedRestrictions *bool, capturedQueries *[]catalogObservedQuery) {
	(*capturedT).Helper()

	for batch, query := range *capturedQueries {
		args := min(accountModelBatchSize, (*capturedN)-batch*accountModelBatchSize)
		if *capturedRestrictions {
			args++
		}
		{
			reportStoreTestFailure((*capturedT), len(query.args) != args || args > 501, func() {
				(*capturedT).Fatalf("unbounded/wrong arguments: %d want %d", len(query.args), args)
			})
		}
		requireDataCondition((*capturedT), !(strings.Contains(query.sql, "access_token") || strings.Contains(query.sql, "refresh_token")), "catalog/policy read fetched account credentials")
		if (*capturedDialect) == "postgres" {
			{
				reportStoreTestFailure((*capturedT), strings.Contains(query.sql, "?") || !strings.Contains(query.sql, fmt.Sprintf("$%d", args)), func() {
					(*capturedT).Fatalf("bad PostgreSQL bindings: %s", query.sql)
				})
			}
		} else {
			reportStoreTestFailure((*capturedT), strings.Count(query.sql, "?") != args, func() {
				(*capturedT).Fatalf("bad SQLite bindings: %s", query.sql)
			})
		}
		func() {
			verifyAccountModelBatchReadCountAndDialectContractCase1(&(*capturedT), &(*capturedRestrictions), &batch, &query)
		}()
	}
}
