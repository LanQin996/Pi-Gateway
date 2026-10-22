package accounts

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"pi-gateway/internal/store"
)

func TestAcquirePinnedRetainsAccountAndIdempotentRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := newAccountsTestStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		decoy := routingAccount(t, st, "first available", 1)
		target := routingAccount(t, st, "continuation account", 2)
		key := routingKey(t, st)
		m := New(st, nil, nil, Options{})
		// Put the decoy first for ordinary routing. Pinned routing must ignore
		// ordering and stickiness, not merely return the first eligible account.
		m.SetRotationStrategy(string(StrategySingle))
		requireDataNoError(t, m.sticky.SetAffinity(ctx, m.affinityKey(key, []*store.Account{decoy, target}, "model", "", string(StrategySticky)), decoy.ID, m.affinityTTL))
		holders := make([]func(), 0, 2)
		for i := 0; i < 2; i++ {
			got, release, err := m.AcquirePinned(ctx, key, target.ID, "model")
			requireDataTruef(t, !(err != nil || got == nil || got.ID != target.ID || release == nil), "pinned routing changed account: account=%v err=%v", got, err)
			defer release()
			holders = append(holders, release)
		}
		finished := routingQueuedAcquire(t, func() (*store.Account, func(), error) {
			return m.AcquirePinned(ctx, key, target.ID, "model")
		})
		requireDataCondition(t, !(m.Inflight(target.ID) != 2 || m.Inflight(decoy.ID) != 0), "full pinned account fell back or exceeded its ceiling")
		var releases sync.WaitGroup
		for i := 0; i < 16; i++ {
			releases.Go(holders[0])
		}
		releases.Wait()
		{
			got := m.Inflight(target.ID)
			requireDataTruef(t, got == 1, "duplicate release stole the other held slot: inflight=%d", got)
		}
		result := routingAwaitAcquire(t, finished)
		if result.release != nil {
			defer result.release()
		}
		reportTestFailure(t, result.err != nil || result.account == nil || result.account.ID != target.ID || result.release == nil, func() {
			t.Fatalf("woken continuation switched accounts: account=%v err=%v", result.account, result.err)
		})
		requireDataCondition(t, !(m.Inflight(target.ID) != 2 || m.Inflight(decoy.ID) != 0), "woken continuation reserved the wrong slot")
		result.release()
		result.release()
		{
			got := m.Inflight(target.ID)
			requireDataTruef(t, got == 1, "waiter release changed another request's slot: inflight=%d", got)
		}
		holders[1]()
		requireDataCondition(t, !(m.Inflight(target.ID) != 0 || m.Inflight(decoy.ID) != 0), "pinned acquisition leaked a reservation")
	})
}

func TestAcquirePinnedIneligibleTargetNeverFallsBack(t *testing.T) {
	for _, reason := range []string{
		"missing", "zero ID", "negative ID", "disabled", "expired", "invalid", "banned",
		"quota_exhausted", "cooldown", "request interval", "group disabled", "group revoked",
		"account model disabled", "membership model disabled",
	} {
		t.Run(reason, func(t *testing.T) {
			st := newAccountsTestStore(t)
			ctx := context.Background()
			target := routingAccount(t, st, "target", 1)
			decoy := routingAccount(t, st, "allowed public fallback", 1)
			group := routingGroup(t, st, "target group", target.ID)
			key := routingKey(t, st, group.ID)
			m := New(st, nil, nil, Options{})
			accountID := target.ID
			no := false
			var err error
			switch reason {
			case "missing":
				accountID += 1000
			case "zero ID":
				accountID = 0
			case "negative ID":
				accountID = -1
			case "disabled":
				err = st.PatchAccountManagementFields(ctx, target.ID, store.AccountManagementPatch{Enabled: &no})
			case "expired", "invalid", "banned", "quota_exhausted":
				err = st.SetAccountStatus(ctx, target.ID, reason, "")
			case "cooldown":
				_, err = st.ExecContext(ctx, `UPDATE accounts SET cooldown_until=? WHERE id=?`, time.Now().Add(time.Hour).UnixMilli(), target.ID)
			case "request interval":
				m.SetRequestInterval(time.Hour)
				_, err = st.ExecContext(ctx, `UPDATE accounts SET last_started_at=? WHERE id=?`, time.Now().UnixMilli(), target.ID)
			case "group disabled":
				err = st.PatchAccountGroup(ctx, group.ID, store.AccountGroupPatch{Enabled: &no})
			case "group revoked":
				other := routingGroup(t, st, "other")
				err = st.SetKeyGroups(ctx, key.ID, []int64{other.ID})
			case "account model disabled":
				err = st.PatchAccountManagementFields(ctx, target.ID, store.AccountManagementPatch{DisabledModels: []string{"model"}})
			case "membership model disabled":
				// Keep the group restriction account-local: a nil group list on
				// the key allows both accounts but must still reject this target.
				err = st.SetKeyGroups(ctx, key.ID, nil)
				if err == nil {
					err = st.PatchAccountGroup(ctx, group.ID, store.AccountGroupPatch{DisabledModels: []string{"model"}})
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			// A healthy eligible fallback really exists, so this tests pinning
			// rather than merely testing an exhausted pool.
			got, release, err := m.Acquire(ctx, key, "model")
			if err != nil || got == nil || release == nil {
				t.Fatalf("no fallback available for control request: account=%v err=%v", got, err)
			}
			release()
			got, release, err = m.AcquirePinned(ctx, key, accountID, "model")
			routingAssertDenied(t, routingAcquireResult{got, release, err}, ErrNoAccounts)
			if m.Inflight(target.ID) != 0 || m.Inflight(decoy.ID) != 0 {
				t.Fatal("ineligible target leaked or switched reservations")
			}
		})
	}
}

func TestAcquirePinnedWaitingTimeoutAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{
		{"timeout", ErrAccountConcurrency},
		{"cancel", context.Canceled},
		{"deadline", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st := newAccountsTestStore(t)
				ctx := context.Background()
				target := routingAccount(t, st, "held", 1)
				decoy := routingAccount(t, st, "free", 1)
				key := routingKey(t, st)
				m := New(st, nil, nil, Options{})
				wait := 75 * time.Millisecond
				m.SetConcurrencyWaitTimeout(wait)
				_, release, err := m.AcquirePinned(ctx, key, target.ID, "model")
				requireDataNoError(t, err)
				defer release()
				waitCtx, cancel := context.WithCancel(ctx)
				if tc.name == "deadline" {
					cancel()
					wait = 25 * time.Millisecond
					waitCtx, cancel = context.WithTimeout(ctx, wait)
				}
				defer cancel()
				started := time.Now()
				finished := routingQueuedAcquire(t, func() (*store.Account, func(), error) {
					return m.AcquirePinned(waitCtx, key, target.ID, "model")
				})
				if tc.name == "cancel" {
					cancel()
					wait = 0
				}
				routingAssertDenied(t, routingAwaitAcquire(t, finished), tc.want)
				{
					elapsed := time.Since(started)
					requireDataTruef(t, elapsed == wait, "wait ended at %v, want %v (virtual time)", elapsed, wait)
				}
				requireDataCondition(t, !(m.Inflight(target.ID) != 1 || m.Inflight(decoy.ID) != 0), "timed-out/canceled waiter stole or switched a reservation")
				release()
				requireDataCondition(t, !(m.Inflight(target.ID) != 0 || m.Inflight(decoy.ID) != 0), "timed-out/canceled waiter leaked a reservation")
				got, free, err := m.AcquirePinned(ctx, key, target.ID, "model")
				requireDataTruef(t, !(err != nil || got == nil || got.ID != target.ID || free == nil), "slot not reusable after waiter exit: account=%v err=%v", got, err)
				free()
			})
		})
	}
}

func TestAcquirePinnedPreCanceledContext(t *testing.T) {
	st := newAccountsTestStore(t)
	target := routingAccount(t, st, "free", 1)
	key := routingKey(t, st)
	m := New(st, nil, nil, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, release, err := m.AcquirePinned(ctx, key, target.ID, "model")
	routingAssertDenied(t, routingAcquireResult{got, release, err}, context.Canceled)
	if m.Inflight(target.ID) != 0 {
		t.Fatal("pre-canceled pinned request reserved a slot")
	}
}

func TestAcquirePinnedStoreErrorsFailClosed(t *testing.T) {
	for _, failure := range []string{"malformed model policy", "closed store"} {
		t.Run(failure, func(t *testing.T) {
			st := newAccountsTestStore(t)
			ctx := context.Background()
			target := routingAccount(t, st, "target", 1)
			decoy := routingAccount(t, st, "fallback", 1)
			key := routingKey(t, st)
			m := New(st, nil, nil, Options{})
			var err error
			if failure == "closed store" {
				err = st.Close()
			} else {
				_, err = st.ExecContext(ctx, `UPDATE accounts SET disabled_models='broken' WHERE id=?`, target.ID)
			}
			requireDataNoError(t, err)
			got, release, err := m.AcquirePinned(ctx, key, target.ID, "model")
			if release != nil {
				release()
			}
			requireDataTruef(t, !(err == nil || errors.Is(err, ErrNoAccounts) || errors.Is(err, ErrModelUnavailable) || got != nil || release != nil), "store error became availability or denial: account=%v err=%v", got, err)
			requireDataCondition(t, !(m.Inflight(target.ID) != 0 || m.Inflight(decoy.ID) != 0), "store error leaked a reservation")
		})
	}
}
