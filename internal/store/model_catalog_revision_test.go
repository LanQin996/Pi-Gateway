package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func catalogRevisionForTest(t *testing.T, s *Store) string {
	t.Helper()
	revision, err := s.CatalogRevision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return revision
}

// Both real backends run this contract. The second Store has a separate SQL pool
// and observes changes immediately, without process-local invalidation calls.
func exerciseCatalogRevisions(t *testing.T, first, second *Store) {
	t.Helper()
	ctx := context.Background()
	previous := catalogRevisionForTest(t, first)
	seen := map[string]bool{previous: true}
	assertChanged := func() {
		t.Helper()
		next := catalogRevisionForTest(t, first)
		parsed, err := uuid.Parse(next)
		requireDataTruef(t, !(err != nil || parsed.Version() != 4 || seen[next]), "reused/non-random revision %q: %v", next, err)
		{
			got := catalogRevisionForTest(t, second)
			requireDataTruef(t, !(got != next), "cross-Store stale epoch: %q != %q", got, next)
		}
		seen[next] = true
		previous = next
	}
	assertUnchanged := func() {
		t.Helper()
		for _, s := range []*Store{first, second} {
			{
				next := catalogRevisionForTest(t, s)
				requireDataTruef(t, !(next != previous), "unexpected revision change %q => %q", previous, next)
			}
		}
	}
	a := &Account{Name: "revision-account", Enabled: true, SupplementalModels: []string{"manual"}}
	requireDataNoError(t, first.CreateAccount(ctx, a))
	assertChanged()
	// Identical writes with identical millisecond timestamps must still get
	// unique epochs. Epoch generation does not derive from fetched/attempted time.
	catalog := &ModelCatalog{Models: []CatalogModel{{ID: "upstream"}}, FetchedAt: 100, AttemptedAt: 100}
	for i := 0; i < 20; i++ {
		requireDataNoError(t, first.SaveAccountModelCatalog(ctx, a.ID, catalog))
		assertChanged()
	}
	requireDataNoError(t, second.SaveAccountModelCatalog(ctx, a.ID, &ModelCatalog{Error: "refresh failed", AttemptedAt: 110}))
	assertChanged()
	{
		got, err := first.GetAccountModelCatalog(ctx, a.ID)
		requireDataTruef(t, !(err != nil || got.FetchedAt != 100 || got.Error != "refresh failed" || len(got.Models) != 2), "failed refresh lost metadata: %+v err=%v", got, err)
	}
	for _, supplemental := range [][]string{{"new-manual"}, {"new-manual"}, {}} {
		requireDataNoError(t, first.PatchAccountManagementFields(ctx, a.ID, AccountManagementPatch{SupplementalModels: supplemental}))
		assertChanged()
		got, err := second.GetAccountModelCatalog(ctx, a.ID)
		requireDataTruef(t, !(err != nil || len(got.Models) != 1+len(supplemental)), "cross-Store stale manual list: %+v %v", got, err)
	}
	// Metadata, eligibility and credentials are read live by callers and do not
	// belong to the cache epoch. UpdateAccount historically excludes model lists.
	a.SupplementalModels = []string{"must-not-overwrite"}
	a.AccessToken = "rotated-test-token"
	a.Enabled = false
	requireDataNoError(t, first.UpdateAccount(ctx, a))
	assertUnchanged()
	{
		got, err := second.GetAccountModelCatalog(ctx, a.ID)
		requireDataTrue(t, !(err != nil || len(got.Models) != 1), "UpdateAccount changed supplemental-model semantics")
	}
	name := "revision-renamed"
	requireDataNoError(t, first.PatchAccountManagementFields(ctx, a.ID, AccountManagementPatch{Name: &name, DisabledModels: []string{"blocked"}}))
	assertUnchanged()
	requireDataNoError(t, first.UpdateAccountManagementFields(ctx, a.ID, AccountManagementFields{Name: name, Enabled: true, Weight: 1, Concurrency: 3}))
	requireDataNoError(t, first.UpdateAccountCredentials(ctx, a.ID, "next-access", "next-refresh", 500, AccountStatusReady, ""))
	requireDataNoError(t, first.MarkAccountUsed(ctx, a.ID, true))
	requireDataNoError(t, first.SetAccountStatus(ctx, a.ID, AccountStatusReady, ""))
	assertUnchanged()
	// A failed group validation rolls back the preceding supplemental write.
	{
		err := first.PatchAccountManagementFields(ctx, a.ID, AccountManagementPatch{SupplementalModels: []string{"rollback"}, GroupIDs: []int64{-99999}})
		requireDataTrue(t, !(err == nil), "invalid group accepted")
	}
	assertUnchanged()
	{
		got, err := second.GetAccountModelCatalog(ctx, a.ID)
		requireDataTrue(t, !(err != nil || len(got.Models) != 1), "failed transaction leaked manual-model mutation")
	}
	{
		err := first.CreateAccount(ctx, &Account{Name: "failed-create", GroupIDs: []int64{-99999}})
		requireDataTrue(t, !(err == nil), "invalid create accepted")
	}
	assertUnchanged()
	for _, invalid := range []*ModelCatalog{nil, {}, {FetchedAt: 1, Models: []CatalogModel{{ID: "invalid", Metadata: map[string]json.RawMessage{"bad": json.RawMessage(`not-json`)}}}}} {
		// Nil, absent timestamp and invalid raw metadata cannot change revision.
		{
			err := first.SaveAccountModelCatalog(ctx, a.ID, invalid)
			requireDataTrue(t, !(err == nil), "invalid catalog accepted")
		}
		assertUnchanged()
	}
	// A revision written within an uncommitted transaction is not yet visible,
	// and rolling that transaction back leaves the authoritative epoch unchanged.
	tx, err := first.BeginTx(ctx, nil)
	requireDataNoError(t, err)
	defer tx.Rollback()
	requireDataNoError(t, changeCatalogRevision(ctx, tx))
	requireDataCondition(t, !(catalogRevisionForTest(t, second) != previous), "uncommitted revision escaped transaction")
	requireDataNoError(t, tx.Rollback())
	assertUnchanged()
	requireDataNoError(t, second.DeleteAccount(ctx, a.ID))
	assertChanged()
	{
		got, err := first.GetAccountModelCatalog(ctx, a.ID)
		requireDataTrue(t, !(err != nil || got != nil), "deleted catalog still readable")
	}
	requireDataNoError(t, first.DeleteAccount(ctx, a.ID))
	requireDataNoError(t, first.SaveAccountModelCatalog(ctx, a.ID, catalog))
	assertUnchanged()
}

func TestCatalogRevisionDialectContract(t *testing.T) {
	ctx := context.Background()
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) { testCatalogRevisionDialectContractCase1(t, ctx, dialect) })
	}
}

func TestCatalogRevisionCrossStoreAndRestart(t *testing.T) {
	path := storeTestPath(t)
	first, err := Open(path)
	requireDataNoError(t, err)
	t.Cleanup(func() { _ = first.Close() })
	second, err := Open(path)
	requireDataNoError(t, err)
	t.Cleanup(func() { _ = second.Close() })
	for _, s := range []*Store{first, second} {
		{
			got := catalogRevisionForTest(t, s)
			requireDataTruef(t, !(got != "0"), "initial epoch=%q", got)
		}
	}
	exerciseCatalogRevisions(t, first, second)
	before := catalogRevisionForTest(t, second)
	requireDataNoError(t, first.Close())
	requireDataNoError(t, second.Close())
	reopened, err := Open(path)
	requireDataNoError(t, err)
	defer reopened.Close()
	requireDataCondition(t, !(catalogRevisionForTest(t, reopened) != before), "revision lost across restart")
	requireDataNoError(t, reopened.CreateAccount(context.Background(), &Account{Name: "after-restart"}))
	{
		after := catalogRevisionForTest(t, reopened)
		requireDataTrue(t, !(after == before || after == "0"), "restart reused old cache revision")
	}
}

func TestCatalogRevisionWriteFailureRollsBackEveryMutation(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	a := &Account{Name: "before", SupplementalModels: []string{"manual"}}
	requireDataNoError(t, s.CreateAccount(ctx, a))
	requireDataNoError(t, s.SaveAccountModelCatalog(ctx, a.ID, &ModelCatalog{Models: []CatalogModel{{ID: "old"}}, FetchedAt: 1}))
	beforeRevision := catalogRevisionForTest(t, s)
	beforeCatalog, err := s.GetAccountModelCatalog(ctx, a.ID)
	requireDataNoError(t, err)
	// Fail the epoch write after the catalog/account SQL has already succeeded.
	{
		_, err := s.ExecContext(ctx, `CREATE TRIGGER reject_catalog_revision BEFORE UPDATE ON settings
		WHEN NEW.key='`+catalogRevisionSetting+`' BEGIN SELECT RAISE(ABORT,'injected epoch write failure'); END`)
		requireDataTrue(t, !(err != nil), err)
	}
	for _, mutation := range []struct {
		name string
		run  func() error
	}{
		{"save", func() error {
			return s.SaveAccountModelCatalog(ctx, a.ID, &ModelCatalog{Models: []CatalogModel{{ID: "new"}}, FetchedAt: 2})
		}},
		{"manual", func() error {
			return s.PatchAccountManagementFields(ctx, a.ID, AccountManagementPatch{SupplementalModels: []string{"new-manual"}})
		}},
		{"create", func() error { return s.CreateAccount(ctx, &Account{Name: "must-rollback"}) }},
		{"delete", func() error { return s.DeleteAccount(ctx, a.ID) }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			testCatalogRevisionWriteFailureRollsBackEveryMutationCase2(t, a, beforeCatalog, beforeRevision, ctx, mutation, s)
		})
	}
	{
		_, err := s.ExecContext(ctx, `DROP TRIGGER reject_catalog_revision`)
		requireDataTrue(t, !(err != nil), err)
	}
	// Failure in a later delete statement also rolls back the earlier account
	// deletion, rather than merely clearing an in-process cache.
	{
		_, err := s.ExecContext(ctx, `CREATE TRIGGER reject_catalog_delete BEFORE DELETE ON account_model_catalog
		BEGIN SELECT RAISE(ABORT,'injected catalog delete failure'); END`)
		requireDataTrue(t, !(err != nil), err)
	}
	{
		err := s.DeleteAccount(ctx, a.ID)
		requireDataTrue(t, !(err == nil), "partial delete failure ignored")
	}
	requireDataCondition(t, !(catalogRevisionForTest(t, s) != beforeRevision), "partial delete changed epoch")
	{
		got, err := s.GetAccountModelCatalog(ctx, a.ID)
		requireDataTrue(t, !(err != nil || !reflect.DeepEqual(got, beforeCatalog)), "partial delete escaped rollback")
	}
}

func TestCatalogRevisionInitialWriteFailureAndCancellation(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	{
		_, err := s.ExecContext(ctx, `CREATE TRIGGER reject_initial_revision BEFORE INSERT ON settings
		WHEN NEW.key='`+catalogRevisionSetting+`' BEGIN SELECT RAISE(ABORT,'injected initial epoch failure'); END`)
		requireDataTrue(t, !(err != nil), err)
	}
	{
		err := s.CreateAccount(ctx, &Account{Name: "rollback"})
		requireDataTrue(t, !(err == nil), "initial epoch error ignored")
	}
	requireDataCondition(t, !(catalogRevisionForTest(t, s) != "0"), "failed first mutation persisted epoch")
	var count int
	{
		err := s.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts`).Scan(&count)
		requireDataTruef(t, !(err != nil || count != 0), "first mutation escaped rollback: %d %v", count, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	{
		revision, err := s.CatalogRevision(canceled)
		requireDataTruef(t, !(!errors.Is(err, context.Canceled) || revision != ""), "canceled revision read=%q %v", revision, err)
	}
	{
		out, err := s.GetAccountModelCatalogs(canceled, []int64{1})
		requireDataTruef(t, !(!errors.Is(err, context.Canceled) || out != nil), "canceled catalog read: %v %v", out, err)
	}
	{
		out, err := s.DisabledModelsForAccounts(canceled, []int64{1}, nil)
		requireDataTruef(t, !(!errors.Is(err, context.Canceled) || out != nil), "canceled restriction read: %v %v", out, err)
	}
}

func testCatalogRevisionDialectContractCase1(t *testing.T, ctx context.Context, dialect string) {
	s, observer := observedCatalogStore(t)
	s.driver = dialect
	{
		got := catalogRevisionForTest(t, s)
		requireDataTruef(t, !(got != "0"), "initial revision=%q", got)
	}
	// The revision upsert is portable and executes with native ?/$n
	// binding; no fake SQL results are used by this offline contract.
	tx, err := s.BeginTx(ctx, nil)
	requireDataNoError(t, err)
	defer tx.Rollback()
	requireDataNoError(t, changeCatalogRevision(ctx, tx))
	requireDataNoError(t, tx.Commit())
	observer.reset()
	revision := catalogRevisionForTest(t, s)
	{
		_, err := uuid.Parse(revision)
		requireDataTrue(t, !(err != nil), err)
	}
	reads := observer.snapshot()
	requireDataTruef(t, !(len(reads) != 1 || len(reads[0].args) != 1 || reads[0].args[0].Value != catalogRevisionSetting), "revision is not one authoritative read: %+v", reads)
	want := "SELECT value FROM settings WHERE key=?"
	if dialect == "postgres" {
		want = "SELECT value FROM settings WHERE key=$1"
	}
	reportStoreTestFailure(t, reads[0].sql != want, func() {
		t.Fatalf("revision dialect SQL=%q want %q", reads[0].sql, want)
	})
}
func testCatalogRevisionWriteFailureRollsBackEveryMutationCase2(t *testing.T, a *Account, beforeCatalog *ModelCatalog, beforeRevision string, ctx context.Context, mutation struct {
	name string
	run  func() error
}, s *Store) {
	{
		err := mutation.run()
		requireDataTrue(t, !(err == nil), "mutation ignored failed epoch persistence")
	}
	requireDataCondition(t, !(catalogRevisionForTest(t, s) != beforeRevision), "failed mutation changed revision")
	got, err := s.GetAccountModelCatalog(ctx, a.ID)
	requireDataTruef(t, !(err != nil || !reflect.DeepEqual(got, beforeCatalog)), "failed mutation leaked data: %+v %v", got, err)
	var count int
	{
		err := s.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts`).Scan(&count)
		requireDataTruef(t, !(err != nil || count != 1), "failed create/delete was not atomic: %d %v", count, err)
	}
}
