package accounts

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"pi-gateway/internal/store"
)

type routingAcquireResult struct {
	account *store.Account
	release func()
	err     error
}

func routingKey(t *testing.T, st *store.Store, groupIDs ...int64) *store.APIKey {
	t.Helper()
	key := &store.APIKey{Name: "client", Enabled: true, GroupIDs: groupIDs}
	if err := st.CreateKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	return key
}

func routingGroup(t *testing.T, st *store.Store, name string, accountIDs ...int64) *store.AccountGroup {
	t.Helper()
	group := &store.AccountGroup{Name: name, Enabled: true, AccountIDs: accountIDs}
	if err := st.CreateAccountGroup(context.Background(), group); err != nil {
		t.Fatal(err)
	}
	return group
}

// Called inside synctest.Test: Wait proves the first admission attempt has
// finished and is blocked on the full slot, without scheduling sleeps or hooks
// in production code. All stores are isolated files created by t.TempDir.
func routingQueuedAcquire(t *testing.T, acquire func() (*store.Account, func(), error)) <-chan routingAcquireResult {
	t.Helper()
	finished := make(chan routingAcquireResult, 1)
	go func() {
		account, release, err := acquire()
		finished <- routingAcquireResult{account, release, err}
	}()
	synctest.Wait()
	select {
	case result := <-finished:
		if result.release != nil {
			result.release()
		}
		t.Fatalf("acquisition did not wait for the held slot: account=%v err=%v", result.account, result.err)
	default:
	}
	return finished
}

func routingAwaitAcquire(t *testing.T, finished <-chan routingAcquireResult) routingAcquireResult {
	t.Helper()
	select {
	case result := <-finished:
		return result
	case <-time.After(time.Second): // Virtual time inside the synctest bubble.
		t.Fatal("queued acquisition did not finish")
		return routingAcquireResult{}
	}
}

func routingAssertDenied(t *testing.T, result routingAcquireResult, want error) {
	t.Helper()
	if result.release != nil {
		result.release()
	}
	if !errors.Is(result.err, want) || result.account != nil || result.release != nil {
		t.Fatalf("denial: account=%v release=%v err=%v, want nil/nil/%v", result.account, result.release != nil, result.err, want)
	}
}

func TestAcquireWaitingKeyRevocation(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		name := "ordinary"
		if pinned {
			name = "pinned"
		}
		t.Run(name, func(t *testing.T) { testAcquireWaitingKeyRevocationPhase1(t, &pinned) })
	}
}

func TestAcquireWaitingCurrentPolicy(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		name := "ordinary"
		if pinned {
			name = "pinned"
		}
		t.Run(name, func(t *testing.T) { testAcquireWaitingCurrentPolicyPhase1(t, &pinned) })
	}
}

func TestAcquireUsesCurrentKeyStrategyWhileWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := newAccountsTestStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		first := routingAccount(t, st, "held", 1)
		second := routingAccount(t, st, "free", 1)
		key := routingKey(t, st)
		key.Strategy = string(StrategySingle)
		if err := st.UpdateKey(ctx, key); err != nil {
			t.Fatal(err)
		}
		m := New(st, nil, nil, Options{})
		_, release, err := m.Acquire(ctx, key, "model")
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		finished := routingQueuedAcquire(t, func() (*store.Account, func(), error) {
			return m.Acquire(ctx, key, "model")
		})
		current := *key
		current.Strategy = string(StrategyLeastInflight)
		if err := st.UpdateKey(ctx, &current); err != nil {
			t.Fatal(err)
		}
		result := routingAwaitAcquire(t, finished)
		if result.release != nil {
			defer result.release()
		}
		if result.err != nil || result.account == nil || result.account.ID != second.ID || result.release == nil {
			t.Fatalf("queued request ignored current strategy: account=%v err=%v", result.account, result.err)
		}
		if m.Inflight(first.ID) != 1 || m.Inflight(second.ID) != 1 {
			t.Fatal("strategy edit lost a reservation")
		}
		result.release()
		release()
		if m.Inflight(first.ID) != 0 || m.Inflight(second.ID) != 0 {
			t.Fatal("strategy edit leaked reservations")
		}
	})
}

func TestAcquireKeyIdentityAndNilSemantics(t *testing.T) {
	st := newAccountsTestStore(t)
	ctx := context.Background()
	a := routingAccount(t, st, "available", 1)
	key := routingKey(t, st)
	m := New(st, nil, nil, Options{})
	for _, tc := range []struct {
		name string
		key  *store.APIKey
	}{
		{"zero ID", &store.APIKey{Enabled: true, Key: key.Key}},
		{"negative ID", &store.APIKey{ID: -1, Enabled: true, Key: key.Key}},
		{"missing ID", &store.APIKey{ID: key.ID + 1000, Enabled: true, Key: key.Key}},
		{"wrong token", &store.APIKey{ID: key.ID, Enabled: true, Key: key.Key + "-old"}},
		{"empty token", &store.APIKey{ID: key.ID, Enabled: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, release, err := m.Acquire(ctx, tc.key, "model")
			routingAssertDenied(t, routingAcquireResult{got, release, err}, ErrNoAccounts)
			got, release, err = m.AcquirePinned(ctx, tc.key, a.ID, "model")
			routingAssertDenied(t, routingAcquireResult{got, release, err}, ErrNoAccounts)
			if m.Inflight(a.ID) != 0 {
				t.Fatal("invalid identity leaked a reservation")
			}
		})
	}
	// Enabled, group and strategy snapshots are not credentials. A valid token
	// must use the saved state even when these caller fields are stale.
	stale := *key
	stale.Enabled = false
	stale.GroupIDs = []int64{999999}
	for _, candidate := range []*store.APIKey{nil, &stale} {
		got, release, err := m.Acquire(ctx, candidate, "model")
		if err != nil || got == nil || got.ID != a.ID || release == nil {
			t.Fatalf("internal nil/current credential rejected: account=%v err=%v", got, err)
		}
		release()
	}
	got, release, err := m.AcquirePinned(ctx, nil, a.ID, "model")
	routingAssertDenied(t, routingAcquireResult{got, release, err}, ErrNoAccounts)
	if m.Inflight(a.ID) != 0 {
		t.Fatal("identity checks leaked a reservation")
	}
}

// The eligibility clock is called after the initial account/key read and before
// slot reservation. Mutating authoritative data at that seam deterministically
// exercises the same stale-read window found by the gateway race regression.
func TestAcquireRevalidatesAfterReservation(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		name := "ordinary"
		if pinned {
			name = "pinned"
		}
		t.Run(name, func(t *testing.T) { testAcquireRevalidatesAfterReservationPhase1(t, &pinned) })
	}
}

// testAcquireWaitingKeyRevocationPhase1 exercises one isolated test phase with the original shared state.
func testAcquireWaitingKeyRevocationPhase1(t *testing.T, capturedPinned *bool) {
	for _, mutation := range []string{"disable", "rotate", "delete"} {
		t.Run(mutation, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st := newAccountsTestStore(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				a := routingAccount(t, st, "held", 1)
				key := routingKey(t, st)
				m := New(st, nil, nil, Options{})
				_, release, err := m.Acquire(ctx, key, "model")
				requireDataNoError(t, err)
				defer release()
				finished := routingQueuedAcquire(t, func() (*store.Account, func(), error) {
					if *capturedPinned {
						return m.AcquirePinned(ctx, key, a.ID, "model")
					}
					return m.Acquire(ctx, key, "model")
				})
				// Edit only authoritative state; the authenticated key snapshot
				// must remain enabled and retain its original token.
				switch mutation {
				case "disable":
					current := *key
					current.Enabled = false
					err = st.UpdateKey(ctx, &current)
				case "rotate":
					_, err = st.ExecContext(ctx, `UPDATE api_keys SET key=? WHERE id=?`, key.Key+"-rotated", key.ID)
				case "delete":
					err = st.DeleteKey(ctx, key.ID)
				}
				requireDataNoError(t, err)
				requireDataCondition(t, !(m.Inflight(a.ID) != 1), "waiter modified the held reservation")
				release()
				routingAssertDenied(t, routingAwaitAcquire(t, finished), ErrNoAccounts)
				{
					got := m.Inflight(a.ID)
					requireDataTruef(t, !(got != 0), "revoked acquisition leaked %d slots", got)
				}
			})
		})
	}
}

// testAcquireWaitingCurrentPolicyPhase1 exercises one isolated test phase with the original shared state.
func testAcquireWaitingCurrentPolicyPhase1(t *testing.T, capturedPinned *bool) {
	for _, tc := range []struct {
		name string
		want error
	}{
		{"key group reassigned", ErrNoAccounts},
		{"group disabled", ErrNoAccounts},
		{"account group reassigned", ErrNoAccounts},
		{"account disabled", ErrNoAccounts},
		{"account model disabled", ErrModelUnavailable},
		{"membership group model disabled", ErrModelUnavailable},
		{"key group model disabled", ErrModelUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st := newAccountsTestStore(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				a := routingAccount(t, st, "held", 1)
				group := routingGroup(t, st, "membership", a.ID)
				other := routingGroup(t, st, "other")
				key := routingKey(t, st, group.ID)
				m := New(st, nil, nil, Options{})
				_, release, err := m.Acquire(ctx, key, "model")
				requireDataNoError(t, err)
				defer release()
				finished := routingQueuedAcquire(t, func() (*store.Account, func(), error) {
					if *capturedPinned {
						return m.AcquirePinned(ctx, key, a.ID, "model")
					}
					return m.Acquire(ctx, key, "model")
				})
				no := false
				switch tc.name {
				case "key group reassigned":
					err = st.SetKeyGroups(ctx, key.ID, []int64{other.ID})
				case "group disabled":
					err = st.PatchAccountGroup(ctx, group.ID, store.AccountGroupPatch{Enabled: &no})
				case "account group reassigned":
					err = st.PatchAccountManagementFields(ctx, a.ID, store.AccountManagementPatch{GroupIDs: []int64{other.ID}})
				case "account disabled":
					err = st.PatchAccountManagementFields(ctx, a.ID, store.AccountManagementPatch{Enabled: &no})
				case "account model disabled":
					err = st.PatchAccountManagementFields(ctx, a.ID, store.AccountManagementPatch{DisabledModels: []string{"model"}})
				case "membership group model disabled":
					err = st.PatchAccountGroup(ctx, group.ID, store.AccountGroupPatch{DisabledModels: []string{"model"}})
				case "key group model disabled":
					err = st.PatchAccountGroup(ctx, other.ID, store.AccountGroupPatch{DisabledModels: []string{"model"}})
					if err == nil {
						err = st.SetKeyGroups(ctx, key.ID, []int64{group.ID, other.ID})
					}
				}
				requireDataNoError(t, err)
				release()
				routingAssertDenied(t, routingAwaitAcquire(t, finished), tc.want)
				{
					got := m.Inflight(a.ID)
					requireDataTruef(t, !(got != 0), "policy denial leaked %d slots", got)
				}
			})
		})
	}
}

// testAcquireRevalidatesAfterReservationPhase1 exercises one isolated test phase with the original shared state.
func testAcquireRevalidatesAfterReservationPhase1(t *testing.T, capturedPinned *bool) {
	for _, mutation := range []string{"disable-key", "rotate-key", "delete-key", "disable-account"} {
		t.Run(mutation, func(t *testing.T) {
			st := newAccountsTestStore(t)
			ctx := context.Background()
			a := routingAccount(t, st, "target", 1)
			key := routingKey(t, st)
			mutated := false
			m := New(st, nil, nil, Options{Now: func() time.Time {
				if !mutated {
					mutated = true
					var err error
					switch mutation {
					case "disable-key":
						changed := *key
						changed.Enabled = false
						err = st.UpdateKey(ctx, &changed)
					case "rotate-key":
						_, err = st.ExecContext(ctx, `UPDATE api_keys SET key=? WHERE id=?`, key.Key+"-new", key.ID)
					case "delete-key":
						err = st.DeleteKey(ctx, key.ID)
					case "disable-account":
						no := false
						err = st.PatchAccountManagementFields(ctx, a.ID, store.AccountManagementPatch{Enabled: &no})
					}
					requireDataNoError(t, err)
				}
				return time.Now()
			}})
			var result routingAcquireResult
			if *capturedPinned {
				result.account, result.release, result.err = m.AcquirePinned(ctx, key, a.ID, "model")
			} else {
				result.account, result.release, result.err = m.Acquire(ctx, key, "model")
			}
			routingAssertDenied(t, result, ErrNoAccounts)
			requireDataCondition(t, !(!mutated || m.Inflight(a.ID) != 0), "post-reservation revalidation did not run or leaked a slot")
		})
	}
}
