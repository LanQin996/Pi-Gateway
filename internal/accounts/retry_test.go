package accounts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pi-gateway/internal/store"
)

// Cancellation can cause the SQLite driver to replace a connection. A file
// database preserves its schema across replacement, unlike a :memory: store.
func newRetryTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Run after Close (cleanups are LIFO). SQLite WAL handles can take a
	// moment to disappear on Windows after cancellation replaces a connection.
	t.Cleanup(func() {
		var err error
		for attempt := 0; attempt < 50; attempt++ {
			if err = os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Errorf("remove retry store directory: %v", err)
	})
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close retry store: %v", err)
		}
	})
	return st
}

func retryQueueLength(m *Manager, accountID int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.retryQueues[accountID])
}

func retryWaitQueue(t *testing.T, m *Manager, accountID int64, count int) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for retryQueueLength(m, accountID) != count {
		select {
		case <-deadline.C:
			t.Fatalf("queue length = %d, want %d", retryQueueLength(m, accountID), count)
		case <-ticker.C:
		}
	}
}

func retryStart(m *Manager, ctx context.Context, key *store.APIKey, accountID int64) <-chan routingAcquireResult {
	finished := make(chan routingAcquireResult, 1)
	go func() {
		a, release, err := m.AcquireRetry(ctx, key, accountID, "model")
		finished <- routingAcquireResult{a, release, err}
	}()
	return finished
}

func retryAssertWaiting(t *testing.T, finished <-chan routingAcquireResult) {
	t.Helper()
	select {
	case result := <-finished:
		if result.release != nil {
			result.release()
		}
		t.Fatalf("unexpected early result: account=%v err=%v", result.account, result.err)
	default:
	}
}

func retryAssertAcquired(t *testing.T, m *Manager, finished <-chan routingAcquireResult, accountID int64) routingAcquireResult {
	t.Helper()
	result := routingAwaitAcquire(t, finished)
	if result.err != nil || result.account == nil || result.account.ID != accountID || result.release == nil {
		if result.release != nil {
			result.release()
		}
		t.Fatalf("same-account acquisition: account=%v err=%v", result.account, result.err)
	}
	t.Cleanup(result.release)
	return result
}

func TestAcquireRetryWaitsCooldownAndIntervalWithoutHoldingSlot(t *testing.T) {
	for _, scheduling := range []string{"cooldown", "interval"} {
		t.Run(scheduling, func(t *testing.T) {
			st := newRetryTestStore(t)
			a := routingAccount(t, st, "original", 1)
			decoy := routingAccount(t, st, "other", 1)
			key := routingKey(t, st)
			m := New(st, nil, nil, Options{})
			// Retry must not reset to, or inherit, the manager's short timeout.
			m.SetConcurrencyWaitTimeout(time.Millisecond)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			start := time.Now()
			readyAt := start.Add(120 * time.Millisecond).UnixMilli()
			var err error
			if scheduling == "cooldown" {
				// Override the longer manager cooldown: the persisted deadline
				// must still respect the upstream Retry-After value.
				_, err = st.ExecContext(ctx, `UPDATE accounts SET cooldown_429_seconds=0 WHERE id=?`, a.ID)
				if err == nil {
					err = m.RecordResult(ctx, a.ID, Result{StatusCode: 429, RetryAfter: 120 * time.Millisecond}, start)
				}
			} else {
				m.SetRequestInterval(120 * time.Millisecond)
				_, err = st.ExecContext(ctx, `UPDATE accounts SET last_started_at=? WHERE id=?`, start.UnixMilli(), a.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			finished := retryStart(m, ctx, key, a.ID)
			retryWaitQueue(t, m, a.ID, 1)
			if m.Inflight(a.ID) != 0 || m.Inflight(decoy.ID) != 0 {
				t.Fatal("cooling replay held a slot or switched accounts")
			}
			result := retryAssertAcquired(t, m, finished, a.ID)
			if time.Now().UnixMilli() < readyAt {
				t.Fatal("replay bypassed scheduling delay")
			}
			result.release()
			result.release()
			if m.Inflight(a.ID) != 0 || retryQueueLength(m, a.ID) != 0 {
				t.Fatal("successful replay leaked a ticket or reservation")
			}
		})
	}
}

func TestAcquireRetryFIFOConcurrencyAndOrdinaryPriority(t *testing.T) {
	st := newRetryTestStore(t)
	a := routingAccount(t, st, "original", 2)
	key := routingKey(t, st)
	m := New(st, nil, nil, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !m.tryReserve(a) || !m.tryReserve(a) {
		t.Fatal("could not hold both slots")
	}
	firstHeld, secondHeld := m.pinnedRelease(a.ID), m.pinnedRelease(a.ID)
	defer firstHeld()
	defer secondHeld()
	finished := make([]<-chan routingAcquireResult, 3)
	for i := range finished {
		finished[i] = retryStart(m, ctx, key, a.ID)
		retryWaitQueue(t, m, a.ID, i+1)
	}
	if m.Inflight(a.ID) != 2 {
		t.Fatal("queued replay exceeded concurrency")
	}
	firstHeld()
	first := retryAssertAcquired(t, m, finished[0], a.ID)
	retryWaitQueue(t, m, a.ID, 2)
	retryAssertWaiting(t, finished[1])
	retryAssertWaiting(t, finished[2])
	secondHeld()
	second := retryAssertAcquired(t, m, finished[1], a.ID)
	retryWaitQueue(t, m, a.ID, 1)
	if m.Inflight(a.ID) != 2 {
		t.Fatal("concurrent replays lost or exceeded slots")
	}
	retryAssertWaiting(t, finished[2])
	first.release()
	third := retryAssertAcquired(t, m, finished[2], a.ID)
	second.release()
	third.release()
	if m.Inflight(a.ID) != 0 || retryQueueLength(m, a.ID) != 0 {
		t.Fatal("FIFO acquisition leaked tickets or slots")
	}

	// Directly exercise ordinary admission with free capacity and queued
	// tickets, without relying on which goroutine the scheduler wakes first.
	ticket := m.enqueueRetry(a.ID)
	if m.tryReserve(a) {
		m.pinnedRelease(a.ID)()
		t.Fatal("ordinary acquisition overtook a retry ticket")
	}
	m.removeRetry(a.ID, ticket)
	if !m.tryReserve(a) {
		t.Fatal("queue removal did not restore ordinary admission")
	}
	m.pinnedRelease(a.ID)()
}

func TestAcquireRetryCancellationTimeoutAndLiveDenials(t *testing.T) {
	for _, mutation := range []string{"cancel", "timeout", "disable", "invalid", "key disable", "key rotate", "key delete", "group disable", "model deny", "group reassigned"} {
		t.Run(mutation, func(t *testing.T) {
			st := newRetryTestStore(t)
			a := routingAccount(t, st, "original", 1)
			group := routingGroup(t, st, "allowed", a.ID)
			key := routingKey(t, st, group.ID)
			m := New(st, nil, nil, Options{})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if mutation == "timeout" {
				ctx, cancel = context.WithTimeout(context.Background(), 120*time.Millisecond)
				defer cancel()
			}
			if !m.tryReserve(a) {
				t.Fatal("could not hold slot")
			}
			held := m.pinnedRelease(a.ID)
			defer held()
			finished := retryStart(m, ctx, key, a.ID)
			retryWaitQueue(t, m, a.ID, 1)
			want := ErrNoAccounts
			var err error
			no := false
			background := context.Background()
			switch mutation {
			case "cancel":
				cancel()
				want = context.Canceled
			case "timeout":
				want = context.DeadlineExceeded
			case "disable":
				err = st.PatchAccountManagementFields(background, a.ID, store.AccountManagementPatch{Enabled: &no})
			case "invalid":
				err = st.SetAccountStatus(background, a.ID, store.AccountStatusInvalid, "")
			case "key disable":
				current := *key
				current.Enabled = false
				err = st.UpdateKey(background, &current)
			case "key rotate":
				_, err = st.ExecContext(background, `UPDATE api_keys SET key=? WHERE id=?`, key.Key+"-rotated", key.ID)
			case "key delete":
				err = st.DeleteKey(background, key.ID)
			case "group disable":
				err = st.PatchAccountGroup(background, group.ID, store.AccountGroupPatch{Enabled: &no})
			case "model deny":
				err = st.PatchAccountManagementFields(background, a.ID, store.AccountManagementPatch{DisabledModels: []string{"model"}})
				want = ErrModelUnavailable
			case "group reassigned":
				other := routingGroup(t, st, "other")
				err = st.SetKeyGroups(background, key.ID, []int64{other.ID})
			}
			if err != nil {
				t.Fatal(err)
			}
			routingAssertDenied(t, routingAwaitAcquire(t, finished), want)
			if retryQueueLength(m, a.ID) != 0 || m.Inflight(a.ID) != 1 {
				t.Fatal("denied waiter leaked ticket or changed existing reservation")
			}
			held()
			if m.Inflight(a.ID) != 0 || !m.tryReserve(a) {
				t.Fatal("removed retry continued blocking ordinary admission")
			}
			m.pinnedRelease(a.ID)()
		})
	}
}

func TestAcquireRetryCanceledHeadAdvancesQueue(t *testing.T) {
	st := newRetryTestStore(t)
	a := routingAccount(t, st, "original", 1)
	key := routingKey(t, st)
	m := New(st, nil, nil, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !m.tryReserve(a) {
		t.Fatal("could not hold slot")
	}
	held := m.pinnedRelease(a.ID)
	defer held()
	firstCtx, firstCancel := context.WithCancel(ctx)
	defer firstCancel()
	first := retryStart(m, firstCtx, key, a.ID)
	retryWaitQueue(t, m, a.ID, 1)
	second := retryStart(m, ctx, key, a.ID)
	retryWaitQueue(t, m, a.ID, 2)
	firstCancel()
	routingAssertDenied(t, routingAwaitAcquire(t, first), context.Canceled)
	retryWaitQueue(t, m, a.ID, 1)
	held()
	result := retryAssertAcquired(t, m, second, a.ID)
	result.release()
	if retryQueueLength(m, a.ID) != 0 || m.Inflight(a.ID) != 0 {
		t.Fatal("canceled head leaked or blocked the next ticket")
	}
}

func TestAcquireRetryExtendedCooldownWhileQueued(t *testing.T) {
	st := newRetryTestStore(t)
	a := routingAccount(t, st, "original", 1)
	key := routingKey(t, st)
	m := New(st, nil, nil, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	initialReady := time.Now().Add(100 * time.Millisecond).UnixMilli()
	if _, err := st.ExecContext(ctx, `UPDATE accounts SET cooldown_until=? WHERE id=?`, initialReady, a.ID); err != nil {
		t.Fatal(err)
	}
	finished := retryStart(m, ctx, key, a.ID)
	retryWaitQueue(t, m, a.ID, 1)
	extendedReady := time.Now().Add(250 * time.Millisecond).UnixMilli()
	if _, err := st.ExecContext(ctx, `UPDATE accounts SET cooldown_until=? WHERE id=?`, extendedReady, a.ID); err != nil {
		t.Fatal(err)
	}
	result := retryAssertAcquired(t, m, finished, a.ID)
	if time.Now().UnixMilli() < extendedReady || result.account.CooldownUntil != extendedReady {
		t.Fatal("queued replay ignored the persisted cooldown extension")
	}
	result.release()
}

// Like the existing authorization race test, mutate at the eligibility clock
// seam after reading the old account snapshot but before reserving its slot.
func TestAcquireRetryRevalidatesAfterReservation(t *testing.T) {
	for _, mutation := range []string{"cooldown", "interval", "disable key", "disable account", "cancel after reserve"} {
		t.Run(mutation, func(t *testing.T) {
			st := newRetryTestStore(t)
			a := routingAccount(t, st, "original", 1)
			key := routingKey(t, st)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			calls := 0
			readyAt := int64(0)
			releasedBeforeWaiting := false
			var m *Manager
			m = New(st, nil, nil, Options{Now: func() time.Time {
				calls++
				if calls == 3 && mutation == "cancel after reserve" {
					cancel()
				}
				if calls == 1 {
					var err error
					readyAt = time.Now().Add(120 * time.Millisecond).UnixMilli()
					switch mutation {
					case "cooldown":
						_, err = st.ExecContext(ctx, `UPDATE accounts SET cooldown_until=? WHERE id=?`, readyAt, a.ID)
					case "interval":
						// Change persisted LastStartedAt rather than the runtime
						// interval so the initial snapshot still permits reserve.
						_, err = st.ExecContext(ctx, `UPDATE accounts SET last_started_at=? WHERE id=?`, readyAt-120, a.ID)
					case "disable key":
						current := *key
						current.Enabled = false
						err = st.UpdateKey(ctx, &current)
					case "disable account":
						no := false
						err = st.PatchAccountManagementFields(ctx, a.ID, store.AccountManagementPatch{Enabled: &no})
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				if calls == 5 && (mutation == "cooldown" || mutation == "interval") {
					// The fifth clock call is the next eligibility read, outside
					// m.mu, after the rejected reservation has been released.
					releasedBeforeWaiting = m.Inflight(a.ID) == 0
				}
				return time.Now()
			}})
			if mutation == "interval" {
				m.SetRequestInterval(120 * time.Millisecond)
			}
			account, release, err := m.AcquireRetry(ctx, key, a.ID, "model")
			if mutation == "cancel after reserve" {
				routingAssertDenied(t, routingAcquireResult{account, release, err}, context.Canceled)
			} else if mutation == "disable key" || mutation == "disable account" {
				routingAssertDenied(t, routingAcquireResult{account, release, err}, ErrNoAccounts)
			} else {
				if release != nil {
					defer release()
				}
				if err != nil || account == nil || account.ID != a.ID || release == nil || !releasedBeforeWaiting || time.Now().UnixMilli() < readyAt {
					t.Fatalf("temporary post-reserve change: account=%v err=%v released=%v", account, err, releasedBeforeWaiting)
				}
				release()
			}
			if retryQueueLength(m, a.ID) != 0 || m.Inflight(a.ID) != 0 {
				t.Fatal("post-reservation validation leaked a ticket or slot")
			}
		})
	}
}

func TestAcquireRetryAlreadyCanceledAndInvalidArguments(t *testing.T) {
	st := newRetryTestStore(t)
	a := routingAccount(t, st, "original", 1)
	key := routingKey(t, st)
	m := New(st, nil, nil, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, release, err := m.AcquireRetry(ctx, key, a.ID, "model")
	if !errors.Is(err, context.Canceled) || release != nil || retryQueueLength(m, a.ID) != 0 || m.Inflight(a.ID) != 0 {
		t.Fatal("already canceled replay reserved capacity or enqueued")
	}
	for _, badKey := range []*store.APIKey{nil, {}, {ID: key.ID, Key: "wrong-token"}} {
		got, release, err := m.AcquireRetry(context.Background(), badKey, a.ID, "model")
		routingAssertDenied(t, routingAcquireResult{got, release, err}, ErrNoAccounts)
	}
	if retryQueueLength(m, a.ID) != 0 || m.Inflight(a.ID) != 0 {
		t.Fatal("invalid replay leaked capacity or tickets")
	}
}
