package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func policyAccount(t *testing.T, s *Store, name string) *Account {
	t.Helper()
	a := &Account{Name: name, Enabled: true, AccessToken: "fake-primary", RefreshToken: "fake-refresh", Status: AccountStatusReady}
	if err := s.CreateAccount(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

func policyGroup(t *testing.T, s *Store, name string, enabled bool, ids []int64, models ...string) *AccountGroup {
	t.Helper()
	g := &AccountGroup{Name: name, Enabled: enabled, AccountIDs: ids, DisabledModels: models}
	if err := s.CreateAccountGroup(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	return g
}

func policyKey(t *testing.T, s *Store, name string, ids ...int64) *APIKey {
	t.Helper()
	k := &APIKey{Name: name, Enabled: true, GroupIDs: ids}
	if err := s.CreateKey(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestPolicyPublicAccountScopeMatrix(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	s.db.SetMaxOpenConns(1)
	if groups, err := s.ListAccountGroups(ctx); err != nil || len(groups) != 0 {
		t.Fatalf("new DB groups=%v %v", groups, err)
	}
	public := policyAccount(t, s, "public")
	a := policyAccount(t, s, "alpha")
	b := policyAccount(t, s, "beta")
	offMember := policyAccount(t, s, "disabled-group-member")
	disabled := &Account{Name: "disabled-account", Enabled: false}
	if err := s.CreateAccount(ctx, disabled); err != nil {
		t.Fatal(err)
	}
	alpha := policyGroup(t, s, "alpha", true, []int64{a.ID})
	beta := policyGroup(t, s, "beta", true, []int64{b.ID})
	off := policyGroup(t, s, "off", false, []int64{offMember.ID})
	empty := policyGroup(t, s, "empty", true, nil)
	if len(empty.AccountIDs) != 0 {
		t.Fatal("new group must be empty")
	}
	all := policyKey(t, s, "all")
	alphaKey := policyKey(t, s, "alpha-key", alpha.ID)
	offKey := policyKey(t, s, "off-key", off.ID)
	multiKey := policyKey(t, s, "multi-key", alpha.ID, beta.ID)
	emptyKey := policyKey(t, s, "empty-key", empty.ID)
	check := func(key *APIKey, want ...int64) {
		t.Helper()
		got, err := s.EligibleAccountsForKey(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]int64, 0, len(got))
		for _, a := range got {
			ids = append(ids, a.ID)
		}
		if !reflect.DeepEqual(ids, want) {
			t.Fatalf("key %v: got %v want %v", key, ids, want)
		}
	}
	check(nil, public.ID, a.ID, b.ID, offMember.ID)
	check(all, public.ID, a.ID, b.ID, offMember.ID)
	check(alphaKey, public.ID, a.ID)
	check(offKey, public.ID)
	check(emptyKey, public.ID)
	check(multiKey, public.ID, a.ID, b.ID)
	no := false
	if err := s.PatchAccountGroup(ctx, alpha.ID, AccountGroupPatch{Enabled: &no}); err != nil {
		t.Fatal(err)
	}
	check(alphaKey, public.ID)
	check(multiKey, public.ID, b.ID)
	got, err := s.GetAccountGroup(ctx, alpha.ID)
	if err != nil || !reflect.DeepEqual(got.AccountIDs, []int64{a.ID}) {
		t.Fatalf("disabled membership changed: %+v %v", got, err)
	}
	if err := s.SetAccountGroupAccounts(ctx, alpha.ID, []int64{}); err != nil {
		t.Fatal(err)
	}
	check(offKey, public.ID, a.ID)
	if err := s.DeleteAccountGroup(ctx, alpha.ID); err != nil {
		t.Fatal(err)
	}
	check(alphaKey, public.ID, a.ID, b.ID, offMember.ID)
	ids, err := s.ListKeyGroupIDs(ctx, alphaKey.ID)
	if err != nil || len(ids) != 0 {
		t.Fatalf("deleted group key references: %v %v", ids, err)
	}
}

func TestPolicyUnionOmissionClearAndAtomicity(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := policyAccount(t, s, "member")
	public := policyAccount(t, s, "public")
	g1 := policyGroup(t, s, "First", true, []int64{a.ID}, " one ", "shared", "one")
	g2 := policyGroup(t, s, "Second", true, []int64{a.ID}, "two", "shared")
	policyGroup(t, s, "Disabled", false, []int64{a.ID}, "ignored")
	keyGroup := policyGroup(t, s, "Key policy", true, nil, "key-only", "shared")
	key := policyKey(t, s, "scoped", g1.ID, keyGroup.ID)
	if err := s.PatchAccountManagementFields(ctx, a.ID, AccountManagementPatch{DisabledModels: []string{" own ", "shared", "own", " "}}); err != nil {
		t.Fatal(err)
	}
	checkModels := func(id int64, key *APIKey, want []string) {
		t.Helper()
		got, err := s.DisabledModelsForAccount(ctx, id, key)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("models=%v want=%v err=%v", got, want, err)
		}
	}
	checkModels(a.ID, nil, []string{"one", "own", "shared", "two"})
	checkModels(a.ID, key, []string{"key-only", "one", "own", "shared", "two"})
	checkModels(public.ID, key, []string{"key-only", "one", "shared"})
	checkModels(public.ID, nil, []string{})
	got, err := s.GetAccount(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.GroupIDs) != 3 || !reflect.DeepEqual(got.DisabledModels, []string{"own", "shared"}) {
		t.Fatalf("account=%+v", got)
	}
	wantInherited := []InheritedModelRestriction{{Model: "one", GroupNames: []string{"First"}}, {Model: "shared", GroupNames: []string{"First", "Second"}}, {Model: "two", GroupNames: []string{"Second"}}}
	if !reflect.DeepEqual(got.InheritedModelRestrictions, wantInherited) {
		t.Fatalf("inherited=%+v", got.InheritedModelRestrictions)
	}
	name := "renamed"
	if err := s.PatchAccountManagementFields(ctx, a.ID, AccountManagementPatch{Name: &name}); err != nil {
		t.Fatal(err)
	}
	checkModels(a.ID, nil, []string{"one", "own", "shared", "two"})
	badName := "must rollback"
	if err := s.PatchAccountManagementFields(ctx, a.ID, AccountManagementPatch{Name: &badName, GroupIDs: []int64{g1.ID, 9999}, DisabledModels: []string{"bad"}}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("invalid group error: %v", err)
	}
	got, _ = s.GetAccount(ctx, a.ID)
	if got.Name != name || len(got.GroupIDs) != 3 {
		t.Fatalf("partial account patch persisted: %+v", got)
	}
	checkModels(a.ID, nil, []string{"one", "own", "shared", "two"})
	if err := s.PatchAccountGroup(ctx, g1.ID, AccountGroupPatch{Name: &badName, AccountIDs: []int64{99999}, DisabledModels: []string{"bad"}}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("invalid member error: %v", err)
	}
	group, err := s.GetAccountGroup(ctx, g1.ID)
	if err != nil || group.Name != "First" || !reflect.DeepEqual(group.AccountIDs, []int64{a.ID}) || !reflect.DeepEqual(group.DisabledModels, []string{"one", "shared"}) {
		t.Fatalf("partial group patch: %+v %v", group, err)
	}
	if err := s.PatchAccountGroup(ctx, g1.ID, AccountGroupPatch{Notes: &name}); err != nil {
		t.Fatal(err)
	}
	checkModels(a.ID, nil, []string{"one", "own", "shared", "two"})
	if err := s.PatchAccountGroup(ctx, g2.ID, AccountGroupPatch{DisabledModels: []string{}}); err != nil {
		t.Fatal(err)
	}
	checkModels(a.ID, nil, []string{"one", "own", "shared"})
	if err := s.PatchAccountManagementFields(ctx, a.ID, AccountManagementPatch{GroupIDs: []int64{}, DisabledModels: []string{}}); err != nil {
		t.Fatal(err)
	}
	checkModels(a.ID, nil, []string{})
	got, _ = s.GetAccount(ctx, a.ID)
	if got.GroupIDs == nil || len(got.GroupIDs) != 0 || got.DisabledModels == nil || got.InheritedModelRestrictions == nil {
		t.Fatalf("cleared lists must serialize as arrays: %+v", got)
	}
	if err := s.DeleteAccountGroup(ctx, keyGroup.ID); err != nil {
		t.Fatal(err)
	}
	checkModels(public.ID, key, []string{"one", "shared"})
}

func TestPolicyProxySnapshotCannotRestoreUnboundProxy(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	p := &Proxy{Name: "fake proxy", URL: "http://fake:secret@127.0.0.1:1"}
	if err := s.CreateProxy(ctx, p); err != nil {
		t.Fatal(err)
	}
	a := &Account{Name: "original", Enabled: true, ProxyID: &p.ID, ProxyURL: p.URL, AccessToken: "fake-old"}
	if err := s.CreateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	stale, err := s.GetAccount(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetProxyAccounts(ctx, p.ID, []int64{}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAccountCredentials(ctx, a.ID, "fake-new", "fake-rotated", 123, AccountStatusReady, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAccountManagementFields(ctx, a.ID, AccountManagementFields{Name: "legacy rename", Enabled: true, Weight: 1, Concurrency: 3, ProxyURL: stale.ProxyURL}); err != nil {
		t.Fatal(err)
	}
	name := "typed rename"
	if err := s.PatchAccountManagementFields(ctx, a.ID, AccountManagementPatch{Name: &name}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccount(ctx, a.ID)
	if err != nil || got.ProxyID != nil || got.ProxyURL != "" || got.AccessToken != "fake-new" || got.RefreshToken != "fake-rotated" {
		t.Fatalf("stale management overwrite: %+v %v", got, err)
	}
	if err := s.PatchAccountManagementFields(ctx, a.ID, AccountManagementPatch{Proxy: &AccountProxyPatch{ID: &p.ID, URL: p.URL}, GroupIDs: []int64{99999}}); err == nil {
		t.Fatal("invalid groups accepted")
	}
	got, _ = s.GetAccount(ctx, a.ID)
	if got.ProxyID != nil || got.ProxyURL != "" {
		t.Fatal("proxy committed despite transaction rollback")
	}
	if err := s.PatchAccountManagementFields(ctx, a.ID, AccountManagementPatch{Proxy: &AccountProxyPatch{ID: &p.ID, URL: p.URL}}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetAccount(ctx, a.ID)
	if got.ProxyID == nil || *got.ProxyID != p.ID || got.ProxyURL != p.URL {
		t.Fatal("explicit proxy update was not atomic")
	}
	if err := s.UpdateAccountManagementFields(ctx, a.ID, AccountManagementFields{Name: name, Enabled: true, Weight: 1, Concurrency: 3}, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetAccount(ctx, a.ID)
	if got.ProxyID != nil || got.ProxyURL != "" {
		t.Fatal("legacy explicit clear failed")
	}
}

func TestPolicyValidationAndFailuresAreClosed(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := policyAccount(t, s, "public")
	g := policyGroup(t, s, "group", true, nil, "blocked")
	k := policyKey(t, s, "key", g.ID)
	for _, models := range [][]string{make([]string, 513), {strings.Repeat("a", 257)}, {"a\nb"}} {
		if err := s.PatchAccountManagementFields(ctx, a.ID, AccountManagementPatch{DisabledModels: models}); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("invalid models accepted: %v", err)
		}
	}
	if err := s.CreateAccountGroup(ctx, &AccountGroup{Name: " group "}); !errors.Is(err, ErrGroupNameConflict) {
		t.Fatalf("duplicate=%v", err)
	}
	if err := s.CreateAccountGroup(ctx, &AccountGroup{Name: "GROUP"}); !errors.Is(err, ErrGroupNameConflict) {
		t.Fatalf("case duplicate=%v", err)
	}
	if err := s.CreateAccountGroup(ctx, &AccountGroup{Name: " "}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("empty name=%v", err)
	}
	if _, err := s.DisabledModelsForAccount(ctx, 9999, k); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("nonexistent account=%v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE account_groups SET disabled_models='not-json' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DisabledModelsForAccount(ctx, a.ID, k); err == nil {
		t.Fatal("malformed key restriction allowed")
	}
	no := false
	if err := s.PatchAccountGroup(ctx, g.ID, AccountGroupPatch{Enabled: &no}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DisabledModelsForAccount(ctx, a.ID, k); err != nil {
		t.Fatalf("disabled group applied policy: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE accounts SET disabled_models='null' WHERE id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DisabledModelsForAccount(ctx, a.ID, nil); err == nil {
		t.Fatal("null policy accepted")
	}
	if _, err := s.GetAccount(ctx, a.ID); err == nil {
		t.Fatal("malformed account model policy swallowed")
	}
}

func TestPolicyMigrationPersistenceAndAccountDeleteCleanup(t *testing.T) {
	path := storeTestPath(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	a := policyAccount(t, s, "existing")
	g := policyGroup(t, s, "legacy group", true, []int64{a.ID})
	k := policyKey(t, s, "legacy key", g.ID)
	// Emulate a previous schema while preserving user group membership.
	for _, query := range []string{`ALTER TABLE accounts DROP COLUMN disabled_models`, `ALTER TABLE account_groups DROP COLUMN disabled_models`, `DROP TABLE account_model_catalog`} {
		if _, err := s.db.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccount(ctx, a.ID)
	if err != nil || !reflect.DeepEqual(got.GroupIDs, []int64{g.ID}) || len(got.DisabledModels) != 0 || got.AccessToken != "fake-primary" {
		t.Fatalf("migration lost user data: %+v %v", got, err)
	}
	if err := s.PatchAccountManagementFields(ctx, a.ID, AccountManagementPatch{DisabledModels: []string{"persist"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO account_model_catalog(account_id) VALUES(?)`, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAccount(ctx, a.ID)
	if err != nil || !reflect.DeepEqual(got.DisabledModels, []string{"persist"}) {
		t.Fatalf("restart lost bans: %+v %v", got, err)
	}
	if key, err := s.GetKey(ctx, k.ID); err != nil || !reflect.DeepEqual(key.GroupIDs, []int64{g.ID}) {
		t.Fatalf("restart lost key group: %+v %v", key, err)
	}
	if err := s.DeleteAccount(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"account_group_accounts", "account_model_catalog"} {
		var count int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE account_id=?", a.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("dangling %s: %d %v", table, count, err)
		}
	}
}
