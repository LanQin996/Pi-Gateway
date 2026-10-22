package store

import (
	"context"
	"reflect"
	"testing"
)

func TestAccountGroupsAndKeyCRUD(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	// Detect accidental nested queries that exhaust a single-connection pool.
	s.db.SetMaxOpenConns(1)
	a := &Account{Name: "a", Enabled: true}
	b := &Account{Name: "b", Enabled: true}
	disabled := &Account{Name: "disabled", Enabled: false}
	for _, account := range []*Account{a, b, disabled} {
		requireDataNoError(t, s.CreateAccount(ctx, account))
	}
	g := &AccountGroup{Name: "first", Enabled: true, Notes: "notes", AccountIDs: []int64{a.ID, disabled.ID, a.ID}}
	requireDataNoError(t, s.CreateAccountGroup(ctx, g))
	other := &AccountGroup{Name: "second", Enabled: false, AccountIDs: []int64{b.ID}}
	requireDataNoError(t, s.CreateAccountGroup(ctx, other))
	got, err := s.GetAccountGroup(ctx, g.ID)
	requireDataNoError(t, err)
	requireDataTruef(t, !(got.Name != "first" || got.Notes != "notes" || !reflect.DeepEqual(got.AccountIDs, []int64{a.ID, disabled.ID}) || got.CreatedAt == 0 || got.UpdatedAt == 0), "group=%+v", got)
	k := &APIKey{Name: "key", Enabled: true, Label: "label", MaxConcurrency: 3, RequestsPerMinute: 7, DailyLimitUSD: 1.25, WeeklyLimitUSD: 8.5, Strategy: "smart", Transport: "ws", GroupIDs: []int64{g.ID, other.ID, g.ID}}
	requireDataNoError(t, s.CreateKey(ctx, k))
	checkKey := func(key *APIKey) {
		t.Helper()
		requireDataTruef(t, !(key == nil || key.Label != k.Label || key.MaxConcurrency != k.MaxConcurrency || key.RequestsPerMinute != k.RequestsPerMinute || key.DailyLimitUSD != k.DailyLimitUSD || key.WeeklyLimitUSD != k.WeeklyLimitUSD || key.Strategy != k.Strategy || key.Transport != k.Transport || !reflect.DeepEqual(key.GroupIDs, uniqueIDs(k.GroupIDs))), "key=%+v want=%+v", key, k)
	}
	key, err := s.GetKey(ctx, k.ID)
	requireDataNoError(t, err)
	checkKey(key)
	key, err = s.GetKeyByValue(ctx, k.Key)
	requireDataNoError(t, err)
	checkKey(key)
	keys, err := s.ListKeys(ctx)
	requireDataTruef(t, !(err != nil || len(keys) != 1), "keys=%+v %v", keys, err)
	checkKey(keys[0])
	eligible, err := s.EligibleAccountsForKey(ctx, k)
	requireDataTruef(t, !(err != nil || len(eligible) != 1 || eligible[0].ID != a.ID), "eligible=%+v %v", eligible, err)
	g.Enabled = false
	g.AccountIDs = nil
	requireDataNoError(t, s.UpdateAccountGroup(ctx, g))
	eligible, err = s.EligibleAccountsForKey(ctx, k)
	requireDataTruef(t, !(err != nil || len(eligible) != 0), "disabled groups must not fall back to all: %+v %v", eligible, err)
	got, err = s.GetAccountGroup(ctx, g.ID)
	requireDataTruef(t, !(err != nil || len(got.AccountIDs) != 2), "disable deleted members: %+v %v", got, err)
	g.Enabled = true
	requireDataNoError(t, s.UpdateAccountGroup(ctx, g))
	requireDataNoError(t, s.SetAccountGroupAccounts(ctx, g.ID, []int64{b.ID, b.ID}))
	got, err = s.GetAccountGroup(ctx, g.ID)
	requireDataTruef(t, !(err != nil || !reflect.DeepEqual(got.AccountIDs, []int64{b.ID})), "replaced members=%+v %v", got, err)
	{
		err := s.SetAccountGroupAccounts(ctx, g.ID, []int64{a.ID, 9999})
		requireDataTrue(t, err != nil, "nonexistent account accepted")
	}
	got, err = s.GetAccountGroup(ctx, g.ID)
	requireDataTruef(t, !(err != nil || !reflect.DeepEqual(got.AccountIDs, []int64{b.ID})), "failed replacement not rolled back: %+v %v", got, err)
	requireDataNoError(t, s.SetKeyGroups(ctx, k.ID, []int64{other.ID}))
	{
		err := s.SetKeyGroups(ctx, k.ID, []int64{g.ID, 9999})
		requireDataTrue(t, err != nil, "nonexistent group accepted")
	}
	ids, err := s.ListKeyGroupIDs(ctx, k.ID)
	requireDataTruef(t, !(err != nil || !reflect.DeepEqual(ids, []int64{other.ID})), "key bindings not rolled back: %v %v", ids, err)
	k.GroupIDs = []int64{g.ID}
	k.Label = "edited"
	k.MaxConcurrency = 9
	k.RequestsPerMinute = 13
	k.DailyLimitUSD = 2.5
	k.WeeklyLimitUSD = 16
	requireDataNoError(t, s.UpdateKey(ctx, k))
	key, err = s.GetKey(ctx, k.ID)
	requireDataNoError(t, err)
	checkKey(key)
	requireDataNoError(t, s.MarkKeyUsed(ctx, k.ID))
	key, err = s.GetKey(ctx, k.ID)
	requireDataTruef(t, !(err != nil || key.RequestCount != 1 || key.LastUsedAt == 0), "key usage=%+v %v", key, err)
	func() {
		verifyAccountGroupsAndKeyCRUD(&ctx, verifyAccountGroupsAndKeyCRUDState{capturedT: &t, capturedS: &s, capturedG: &g, capturedOther: &other, capturedGot: &got, capturedErr: &err, capturedK: &k, capturedKey: &key, capturedEligible: &eligible, capturedIds: &ids})
	}()
}

func TestAccountSchedulingStateReadback(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := &Account{Name: "healthy", Enabled: true}
	if err := s.CreateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE accounts SET consecutive_failures=2,cooldown_until=123,cooldown_kind='rate_limit',last_started_at=100,ewma_first_output_ms=12.5,ewma_failure_rate_bp=2500 WHERE id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	// An administrative update from an older snapshot must not reset scheduling.
	if err := s.UpdateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccount(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConsecutiveFailures != 2 || got.CooldownUntil != 123 || got.CooldownKind != "rate_limit" || got.LastStartedAt != 100 || got.EWMAFirstOutputMS != 12.5 || got.EWMAFailureRateBP != 2500 {
		t.Fatalf("scheduling state=%+v", got)
	}
}

func TestKeyAndGroupWritesAreAtomic(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bad := &APIKey{Name: "bad", GroupIDs: []int64{999}}
	if err := s.CreateKey(ctx, bad); err == nil {
		t.Fatal("key with missing group created")
	}
	keys, err := s.ListKeys(ctx)
	if err != nil || len(keys) != 0 {
		t.Fatalf("partial key persisted: %+v %v", keys, err)
	}
	g := &AccountGroup{Name: "bad", AccountIDs: []int64{999}}
	if err := s.CreateAccountGroup(ctx, g); err == nil {
		t.Fatal("group with missing account created")
	}
	groups, err := s.ListAccountGroups(ctx)
	if err != nil || len(groups) != 0 {
		t.Fatalf("partial group persisted: %+v %v", groups, err)
	}
	k := &APIKey{Name: "original", Enabled: true}
	if err := s.CreateKey(ctx, k); err != nil {
		t.Fatal(err)
	}
	k.Name = "changed"
	k.GroupIDs = []int64{999}
	if err := s.UpdateKey(ctx, k); err == nil {
		t.Fatal("invalid binding update accepted")
	}
	got, err := s.GetKey(ctx, k.ID)
	if err != nil || got.Name != "original" {
		t.Fatalf("partial key update persisted: %+v %v", got, err)
	}
}

type verifyAccountGroupsAndKeyCRUDState struct {
	capturedT        **testing.T
	capturedS        **Store
	capturedG        **AccountGroup
	capturedOther    **AccountGroup
	capturedGot      **AccountGroup
	capturedErr      *error
	capturedK        **APIKey
	capturedKey      **APIKey
	capturedEligible *[]*Account
	capturedIds      *[]int64
}

// verifyAccountGroupsAndKeyCRUD exercises one isolated test phase with the original shared state.
func verifyAccountGroupsAndKeyCRUD(capturedCtx *context.Context, state verifyAccountGroupsAndKeyCRUDState) {
	requireDataNoError((*state.capturedT), (*state.capturedS).SetKeyGroups((*capturedCtx), (*state.capturedK).ID, nil))
	(*state.capturedEligible), (*state.capturedErr) = (*state.capturedS).EligibleAccountsForKey((*capturedCtx), (*state.capturedK))
	requireDataTruef((*state.capturedT), !((*state.capturedErr) != nil || len((*state.capturedEligible)) != 2), "unbound key=%+v %v", (*state.capturedEligible), (*state.capturedErr))
	groups, err := (*state.capturedS).ListAccountGroups((*capturedCtx))
	requireDataTruef((*state.capturedT), !(err != nil || len(groups) != 2), "groups=%+v %v", groups, err)
	requireDataNoError((*state.capturedT), (*state.capturedS).SetKeyGroups((*capturedCtx), (*state.capturedK).ID, []int64{(*state.capturedG).ID}))
	requireDataNoError((*state.capturedT), (*state.capturedS).DeleteAccountGroup((*capturedCtx), (*state.capturedG).ID))
	(*state.capturedIds), err = (*state.capturedS).ListKeyGroupIDs((*capturedCtx), (*state.capturedK).ID)
	requireDataTruef((*state.capturedT), !(err != nil || len((*state.capturedIds)) != 0), "dangling group binding=%v %v", (*state.capturedIds), err)
	(*state.capturedGot), err = (*state.capturedS).GetAccountGroup((*capturedCtx), (*state.capturedG).ID)
	requireDataTruef((*state.capturedT), !(err != nil || (*state.capturedGot) != nil), "deleted group=%+v %v", (*state.capturedGot), err)
	requireDataNoError((*state.capturedT), (*state.capturedS).SetKeyGroups((*capturedCtx), (*state.capturedK).ID, []int64{(*state.capturedOther).ID}))
	requireDataNoError((*state.capturedT), (*state.capturedS).DeleteKey((*capturedCtx), (*state.capturedK).ID))
	(*state.capturedKey), err = (*state.capturedS).GetKey((*capturedCtx), (*state.capturedK).ID)
	requireDataTruef((*state.capturedT), !(err != nil || (*state.capturedKey) != nil), "deleted key=%+v %v", (*state.capturedKey), err)
	(*state.capturedKey), err = (*state.capturedS).GetKeyByValue((*capturedCtx), (*state.capturedK).Key)
	requireDataTruef((*state.capturedT), !(err != nil || (*state.capturedKey) != nil), "deleted key value=%+v %v", (*state.capturedKey), err)
	(*state.capturedIds), err = (*state.capturedS).ListKeyGroupIDs((*capturedCtx), (*state.capturedK).ID)
	requireDataTruef((*state.capturedT), !(err != nil || len((*state.capturedIds)) != 0), "dangling key binding=%v %v", (*state.capturedIds), err)
}
