package accounts

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"pi-gateway/internal/store"
)

func routingAccount(t *testing.T, st *store.Store, name string, concurrency int) *store.Account {
	t.Helper()
	a := &store.Account{Name: name, Enabled: true, Status: store.AccountStatusReady, Concurrency: concurrency, AccessToken: "fake-token", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	if err := st.CreateAccount(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRoutingFinalModelPoliciesAndLiveUpdates(t *testing.T) {
	st := newAccountsTestStore(t)
	ctx := context.Background()
	a := routingAccount(t, st, "member", 2)
	public := routingAccount(t, st, "public", 2)
	g := &store.AccountGroup{Name: "group", Enabled: true, AccountIDs: []int64{a.ID}, DisabledModels: []string{"actual-upstream-model"}}
	if err := st.CreateAccountGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	key := &store.APIKey{Name: "scoped", Enabled: true, GroupIDs: []int64{g.ID}}
	if err := st.CreateKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	m := New(st, nil, nil, Options{})
	if got, release, err := m.Acquire(ctx, key, "actual-upstream-model"); !errors.Is(err, ErrModelUnavailable) || got != nil || release != nil {
		t.Fatalf("public account bypassed key ban: account=%v err=%v", got, err)
	}
	got, release, err := m.Acquire(ctx, nil, "actual-upstream-model")
	if err != nil || got.ID != public.ID {
		t.Fatalf("ungrouped key ignored account-group ban: %v %v", got, err)
	}
	release()
	if err := st.PatchAccountManagementFields(ctx, public.ID, store.AccountManagementPatch{DisabledModels: []string{"actual-upstream-model"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Acquire(ctx, nil, "actual-upstream-model"); !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("next request missed live ban: %v", err)
	}
	no := false
	if err := st.PatchAccountGroup(ctx, g.ID, store.AccountGroupPatch{Enabled: &no}); err != nil {
		t.Fatal(err)
	}
	got, release, err = m.Acquire(ctx, nil, "actual-upstream-model")
	if err != nil || got.ID != a.ID {
		t.Fatalf("disabled group still restricted account: %v %v", got, err)
	}
	release()
	if _, _, err := m.Acquire(ctx, key, "actual-upstream-model"); !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("disabled group member became public: %v", err)
	}
	if err := st.PatchAccountManagementFields(ctx, public.ID, store.AccountManagementPatch{DisabledModels: []string{}}); err != nil {
		t.Fatal(err)
	}
	got, release, err = m.Acquire(ctx, key, "actual-upstream-model")
	if err != nil || got.ID != public.ID {
		t.Fatalf("cleared policy did not take effect: %v %v", got, err)
	}
	release()
}

func TestRoutingSpecificHealthAndModelNeverFallsBack(t *testing.T) {
	for _, status := range []string{store.AccountStatusExpired, store.AccountStatusInvalid, store.AccountStatusBanned, "quota_exhausted", "disabled", "cooling"} {
		t.Run(status, func(t *testing.T) {
			st := newAccountsTestStore(t)
			ctx := context.Background()
			a := routingAccount(t, st, "target", 1)
			decoy := routingAccount(t, st, "decoy", 1)
			m := New(st, nil, nil, Options{})
			switch status {
			case "disabled":
				no := false
				requireDataNoError(t, st.PatchAccountManagementFields(ctx, a.ID, store.AccountManagementPatch{Enabled: &no}))
			case "cooling":
				{
					_, err := st.DB().ExecContext(ctx, `UPDATE accounts SET cooldown_until=? WHERE id=?`, time.Now().Add(time.Hour).UnixMilli(), a.ID)
					requireDataTrue(t, !(err != nil), err)
				}
			default:
				requireDataNoError(t, st.SetAccountStatus(ctx, a.ID, status, ""))
			}
			{
				got, release, err := m.AcquireSpecific(ctx, a.ID, "model")
				requireDataTruef(t, !(!errors.Is(err, ErrNoAccounts) || got != nil || release != nil), "unhealthy fixed account admitted: %+v %v", got, err)
			}
			requireDataCondition(t, !(m.Inflight(a.ID) != 0 || m.Inflight(decoy.ID) != 0), "failed fixed acquisition reserved a slot")
		})
	}
	st := newAccountsTestStore(t)
	ctx := context.Background()
	a := routingAccount(t, st, "target", 1)
	routingAccount(t, st, "decoy", 1)
	g := &store.AccountGroup{Name: "restricted", Enabled: true, AccountIDs: []int64{a.ID}, DisabledModels: []string{"model"}}
	requireDataNoError(t, st.CreateAccountGroup(ctx, g))
	m := New(st, nil, nil, Options{})
	{
		_, _, err := m.AcquireSpecific(ctx, a.ID, "model")
		requireDataTruef(t, !(!errors.Is(err, ErrModelUnavailable)), "inherited ban bypassed: %v", err)
	}
	{
		_, _, err := m.AcquireSpecific(ctx, 999999, "model")
		requireDataTruef(t, !(!errors.Is(err, ErrNoAccounts)), "missing fixed account: %v", err)
	}
	requireDataNoError(t, st.PatchAccountGroup(ctx, g.ID, store.AccountGroupPatch{DisabledModels: []string{}}))
	got, release, err := m.AcquireSpecific(ctx, a.ID, "model")
	requireDataTruef(t, !(err != nil || got.ID != a.ID), "wrong fixed account: %+v %v", got, err)
	release()
}

func TestRoutingConcurrentReservationsAndIdempotentRelease(t *testing.T) {
	st := newAccountsTestStore(t)
	a := routingAccount(t, st, "limited", 2)
	m := New(st, nil, nil, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	type result struct {
		release func()
		err     error
	}
	results := make(chan result, 16)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(specific bool) {
			defer wg.Done()
			<-start
			var release func()
			var err error
			if specific {
				_, release, err = m.AcquireSpecific(ctx, a.ID, "model")
			} else {
				_, release, err = m.Acquire(ctx, nil, "model")
			}
			results <- result{release, err}
		}(i%2 == 0)
	}
	close(start)
	first := <-results
	second := <-results
	reportTestFailure(t, first.err != nil || second.err != nil, func() {
		t.Fatalf("first slots failed: %v %v", first.err, second.err)
	})
	{
		got := m.Inflight(a.ID)
		requireDataTruef(t, !(got != 2), "inflight=%d want2", got)
	}
	cancel()
	wg.Wait()
	close(results)
	for result := range results {
		if result.err == nil {
			result.release()
			t.Fatal("concurrency ceiling exceeded")
		}
		reportTestFailure(t, !errors.Is(result.err, context.Canceled), func() {
			t.Fatalf("unexpected waiter error %v", result.err)
		})
	}
	var releases sync.WaitGroup
	for i := 0; i < 16; i++ {
		releases.Add(1)
		go func() { defer releases.Done(); first.release() }()
	}
	releases.Wait()
	{
		got := m.Inflight(a.ID)
		requireDataTruef(t, !(got != 1), "double release stole another request slot: %d", got)
	}
	second.release()
	second.release()
	{
		got := m.Inflight(a.ID)
		requireDataTruef(t, !(got != 0), "leaked slots: %d", got)
	}
}

func TestRoutingSpecificWaitCancellationAndLivePolicy(t *testing.T) {
	st := newAccountsTestStore(t)
	a := routingAccount(t, st, "target", 1)
	decoy := routingAccount(t, st, "decoy", 1)
	m := New(st, nil, nil, Options{})
	ctx := context.Background()
	_, release, err := m.AcquireSpecific(ctx, a.ID, "model")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	short, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
	defer cancel()
	if got, free, err := m.AcquireSpecific(short, a.ID, "model"); !errors.Is(err, context.DeadlineExceeded) || got != nil || free != nil {
		t.Fatalf("full fixed account switched or ignored cancel: %+v %v", got, err)
	}
	if m.Inflight(a.ID) != 1 || m.Inflight(decoy.ID) != 0 {
		t.Fatal("canceled reservation leaked or switched")
	}
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		close(started)
		_, free, err := m.AcquireSpecific(ctx, a.ID, "model")
		if free != nil {
			free()
		}
		finished <- err
	}()
	<-started
	if err := st.PatchAccountManagementFields(ctx, a.ID, store.AccountManagementPatch{DisabledModels: []string{"model"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if !errors.Is(err, ErrModelUnavailable) {
			t.Fatalf("queued request used stale restrictions: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued fixed request did not recheck policy")
	}
}

func TestRoutingDatabaseAndMalformedPolicyFailClosed(t *testing.T) {
	st := newAccountsTestStore(t)
	ctx := context.Background()
	a := routingAccount(t, st, "public", 1)
	g := &store.AccountGroup{Name: "key policy", Enabled: true}
	if err := st.CreateAccountGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	key := &store.APIKey{Name: "key", Enabled: true, GroupIDs: []int64{g.ID}}
	if err := st.CreateKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	m := New(st, nil, nil, Options{})
	if _, err := st.DB().ExecContext(ctx, `UPDATE account_groups SET disabled_models='broken' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}
	if got, free, err := m.Acquire(ctx, key, "model"); err == nil || errors.Is(err, ErrModelUnavailable) || got != nil || free != nil {
		t.Fatalf("store error became availability or denial: %+v %v", got, err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE accounts SET disabled_models='null' WHERE id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.AcquireSpecific(ctx, a.ID, "model"); err == nil || errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("specific malformed policy swallowed: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Acquire(ctx, key, "model"); err == nil || errors.Is(err, ErrNoAccounts) || errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("closed DB not propagated: %v", err)
	}
	if m.Inflight(a.ID) != 0 {
		t.Fatal("error path leaked reservation")
	}
}

func TestRoutingGlobalStrategyMatchesDisplayedSetting(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings store.Settings
		want     string
	}{
		{"configured default wins", store.Settings{DefaultStrategy: "round_robin", RotationStrategy: "smart"}, "round_robin"},
		{"saved default changes immediately", store.Settings{DefaultStrategy: "least_inflight", RotationStrategy: "smart"}, "least_inflight"},
		{"legacy fallback", store.Settings{RotationStrategy: "sticky"}, "sticky"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(nil, nil, nil, Options{})
			m.SetSettings(&tc.settings)
			for _, key := range []*store.APIKey{nil, {}} {
				if got := m.resolveStrategy(key); got != tc.want {
					t.Fatalf("inherited strategy = %q, want displayed %q", got, tc.want)
				}
			}
			if got := m.resolveStrategy(&store.APIKey{Strategy: "single"}); got != "single" {
				t.Fatalf("key override lost priority: %q", got)
			}
		})
	}
}
