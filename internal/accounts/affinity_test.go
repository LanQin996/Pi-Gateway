package accounts

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"pi-gateway/internal/session"
	"pi-gateway/internal/store"
)

type affinityProbe struct {
	mu     sync.Mutex
	values map[session.AffinityKey]int64
	gets   []session.AffinityKey
	sets   []session.AffinityKey
	ttls   []time.Duration
	get    func(context.Context, session.AffinityKey) (int64, error)
	set    func(context.Context, session.AffinityKey, int64) error
}

func (p *affinityProbe) GetAffinity(ctx context.Context, key session.AffinityKey) (int64, error) {
	p.mu.Lock()
	p.gets = append(p.gets, key)
	account, ok := p.values[key]
	p.mu.Unlock()
	if p.get != nil {
		return p.get(ctx, key)
	}
	if !ok {
		return 0, session.ErrMiss
	}
	return account, nil
}

func (p *affinityProbe) SetAffinity(ctx context.Context, key session.AffinityKey, account int64, ttl time.Duration) error {
	p.mu.Lock()
	p.sets = append(p.sets, key)
	p.ttls = append(p.ttls, ttl)
	p.mu.Unlock()
	if p.set != nil {
		if err := p.set(ctx, key, account); err != nil {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.values == nil {
		p.values = make(map[session.AffinityKey]int64)
	}
	p.values[key] = account
	return nil
}

func (p *affinityProbe) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.gets), len(p.sets)
}

func affinityCursor(m *Manager, cursor uint64) {
	m.mu.Lock()
	m.rr = cursor
	m.mu.Unlock()
}

func affinityAcquire(t *testing.T, m *Manager, key *store.APIKey, scope string, want int64) {
	t.Helper()
	account, release, err := m.AcquireWithAffinity(context.Background(), key, "model", scope)
	if release != nil {
		defer release()
	}
	if err != nil || account == nil || account.ID != want || release == nil {
		t.Fatalf("scope %q: account=%v release=%v err=%v, want %d", scope, account, release != nil, err, want)
	}
}

func TestAcquireAffinityScopeAndKeyIsolation(t *testing.T) {
	st := newAccountsTestStore(t)
	first := routingAccount(t, st, "first", 1)
	second := routingAccount(t, st, "second", 1)
	key := routingKey(t, st)
	otherKey := routingKey(t, st)
	backend := session.NewMemory(session.Options{})
	t.Cleanup(func() { _ = backend.Close() })
	m := New(st, nil, nil, Options{Affinity: backend, AffinityIdentity: "provider-config"})
	m.SetRotationStrategy(string(StrategyRoundRobin))
	affinityCursor(m, 1)
	affinityAcquire(t, m, key, "connection-a", second.ID)
	affinityCursor(m, 0)
	affinityAcquire(t, m, key, "connection-a", second.ID)
	affinityCursor(m, 0)
	affinityAcquire(t, m, key, "connection-b", first.ID)
	affinityCursor(m, 0)
	affinityAcquire(t, m, otherKey, "connection-a", first.ID)
	// Another process sharing a backend and provider identity reuses the hint.
	other := New(st, nil, nil, Options{Affinity: backend, AffinityIdentity: "provider-config"})
	other.SetRotationStrategy(string(StrategyRoundRobin))
	affinityAcquire(t, other, key, "connection-a", second.ID)
	// Empty scope keeps the ordinary non-sticky strategy and never uses the
	// externally stored connection hint.
	affinityCursor(m, 0)
	affinityAcquire(t, m, key, "", first.ID)
}

func TestAcquireAffinityBusyFallback(t *testing.T) {
	st := newAccountsTestStore(t)
	first := routingAccount(t, st, "fallback", 1)
	second := routingAccount(t, st, "preferred", 1)
	key := routingKey(t, st)
	probe := &affinityProbe{}
	m := New(st, nil, nil, Options{Affinity: probe})
	m.SetRotationStrategy(string(StrategyRoundRobin))
	affinityCursor(m, 1)
	affinityAcquire(t, m, key, "connection", second.ID)
	_, held, err := m.AcquireSpecific(context.Background(), second.ID, "model")
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	affinityCursor(m, 0)
	affinityAcquire(t, m, key, "connection", first.ID)
	if m.Inflight(first.ID) != 0 || m.Inflight(second.ID) != 1 {
		t.Fatal("busy hint bypassed concurrency or stole the held reservation")
	}
	held()
	// Successful fallback replaces the preference; it is not a hard pin.
	affinityCursor(m, 1)
	affinityAcquire(t, m, key, "connection", first.ID)
}

func TestAcquireAffinityUnavailableIsBestEffort(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   int64
		err  error
	}{
		{"miss", 0, session.ErrMiss},
		{"unavailable", 999, session.ErrUnavailable},
		{"corrupt", 999, session.ErrInvalid},
		{"unknown account", 999, nil},
		{"negative account", -1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newAccountsTestStore(t)
			a := routingAccount(t, st, "available", 1)
			key := routingKey(t, st)
			probe := &affinityProbe{
				get: func(context.Context, session.AffinityKey) (int64, error) { return tc.id, tc.err },
				set: func(context.Context, session.AffinityKey, int64) error { return session.ErrUnavailable },
			}
			m := New(st, nil, nil, Options{Affinity: probe})
			affinityAcquire(t, m, key, "connection", a.ID)
			if gets, sets := probe.counts(); gets != 1 || sets != 1 {
				t.Fatalf("cache operations = %d/%d, want one read/write", gets, sets)
			}
		})
	}
}

func TestAcquireAffinityTimeoutAndCancellation(t *testing.T) {
	t.Run("bounded backend timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			st := newAccountsTestStore(t)
			a := routingAccount(t, st, "available", 1)
			key := routingKey(t, st)
			const timeout = 25 * time.Millisecond
			wait := func(ctx context.Context) error {
				deadline, ok := ctx.Deadline()
				if !ok || deadline.Sub(time.Now()) != timeout {
					t.Errorf("missing bounded cache deadline: %v %v", deadline, ok)
				}
				<-ctx.Done()
				return ctx.Err()
			}
			probe := &affinityProbe{
				get: func(ctx context.Context, _ session.AffinityKey) (int64, error) { return 0, wait(ctx) },
				set: func(ctx context.Context, _ session.AffinityKey, _ int64) error { return wait(ctx) },
			}
			m := New(st, nil, nil, Options{Affinity: probe, AffinityTimeout: timeout})
			start := time.Now()
			affinityAcquire(t, m, key, "connection", a.ID)
			if elapsed := time.Since(start); elapsed != 2*timeout {
				t.Fatalf("cache IO took %v, want %v", elapsed, 2*timeout)
			}
			if m.Inflight(a.ID) != 0 {
				t.Fatal("cache timeout leaked a slot")
			}
		})
	})
	t.Run("request cancellation during cache write", func(t *testing.T) {
		st := newAccountsTestStore(t)
		a := routingAccount(t, st, "available", 1)
		key := routingKey(t, st)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		probe := &affinityProbe{set: func(context.Context, session.AffinityKey, int64) error {
			cancel()
			return context.Canceled
		}}
		m := New(st, nil, nil, Options{Affinity: probe})
		account, release, err := m.AcquireWithAffinity(ctx, key, "model", "connection")
		routingAssertDenied(t, routingAcquireResult{account, release, err}, context.Canceled)
		if m.Inflight(a.ID) != 0 {
			t.Fatal("canceled cache write leaked a reservation")
		}
	})
}

func TestAcquireAffinityExplicitAccountsDoNotReadOrWriteHints(t *testing.T) {
	st := newAccountsTestStore(t)
	first := routingAccount(t, st, "hint", 1)
	second := routingAccount(t, st, "explicit", 1)
	key := routingKey(t, st)
	probe := &affinityProbe{get: func(context.Context, session.AffinityKey) (int64, error) { return first.ID, nil }}
	m := New(st, nil, nil, Options{Affinity: probe})
	m.SetRotationStrategy(string(StrategySticky))
	for _, pinned := range []bool{false, true} {
		var account *store.Account
		var release func()
		var err error
		if pinned {
			account, release, err = m.AcquirePinned(context.Background(), key, second.ID, "model")
		} else {
			account, release, err = m.AcquireSpecific(context.Background(), second.ID, "model")
		}
		if release != nil {
			release()
		}
		if err != nil || account == nil || account.ID != second.ID {
			t.Fatalf("explicit selection used soft hint: %v %v", account, err)
		}
	}
	if gets, sets := probe.counts(); gets != 0 || sets != 0 {
		t.Fatalf("explicit account touched hint backend: %d/%d", gets, sets)
	}
}

func TestAcquireAffinityIdentityChangesDiscardOldHint(t *testing.T) {
	for _, mutation := range []string{"client token", "account token", "refresh token", "proxy", "protocol", "key scope", "strategy", "provider config", "other candidate token"} {
		t.Run(mutation, func(t *testing.T) { testAcquireAffinityIdentityChangesDiscardOldHintPhase1(t, &mutation) })
	}
}

func TestAcquireAffinityRevalidatesAfterCacheIO(t *testing.T) {
	for _, stage := range []string{"get", "set"} {
		for _, mutation := range []string{"disable key", "rotate key", "delete key", "scope revoked", "disable account", "model denied", "group disabled"} {
			t.Run(stage+"/"+mutation, func(t *testing.T) { testAcquireAffinityRevalidatesAfterCacheIOPhase1(t, &stage, &mutation) })
		}
	}
}

func TestAcquireAffinityReturnsFreshAccount(t *testing.T) {
	for _, stage := range []string{"get", "set"} {
		t.Run(stage, func(t *testing.T) { testAcquireAffinityReturnsFreshAccount(t, stage) })
	}
}

func testAcquireAffinityReturnsFreshAccount(t *testing.T, stage string) {
	t.Helper()
	st := newAccountsTestStore(t)
	ctx := context.Background()
	a := routingAccount(t, st, "target", 1)
	key := routingKey(t, st)
	mutate := func() {
		_, err := st.ExecContext(ctx, `UPDATE accounts SET access_token=?, proxy_url=? WHERE id=?`, "fresh-token", "http://fresh-proxy.invalid", a.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	probe := &affinityProbe{}
	if stage == "get" {
		probe.get = func(context.Context, session.AffinityKey) (int64, error) { mutate(); return a.ID, nil }
	} else {
		probe.set = func(context.Context, session.AffinityKey, int64) error { mutate(); return nil }
	}
	m := New(st, nil, nil, Options{Affinity: probe})
	account, release, err := m.AcquireWithAffinity(ctx, key, "model", "connection")
	if release != nil {
		defer release()
	}
	if err != nil || account == nil || account.AccessToken != "fresh-token" || account.ProxyURL != "http://fresh-proxy.invalid" {
		t.Fatalf("cache IO caused stale account forwarding: %v %v", account, err)
	}
}

func TestAcquireAffinityQueuedReadsAreBoundedAndRevalidated(t *testing.T) {
	for _, outcome := range []string{"unchanged", "identity changes", "key revoked", "account revoked", "model revoked"} {
		t.Run(outcome, func(t *testing.T) { testAcquireAffinityQueuedReadsAreBoundedAndRevalidatedPhase1(t, &outcome) })
	}
}

func TestAcquireLegacyStickyIsBoundedAndExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		m := New(nil, nil, nil, Options{AffinityTTL: time.Second})
		if m.affinityTimeout != 100*time.Millisecond {
			t.Fatalf("default timeout = %v", m.affinityTimeout)
		}
		for id := int64(1); id <= session.DefaultMaxEntries; id++ {
			key := session.AffinityKey{KeyID: id, SessionID: "legacy", Provider: "test", IdentityHash: "hash"}
			if err := m.sticky.SetAffinity(ctx, key, id, m.affinityTTL); err != nil {
				t.Fatal(err)
			}
		}
		extra := session.AffinityKey{KeyID: session.DefaultMaxEntries + 1, SessionID: "legacy", Provider: "test", IdentityHash: "hash"}
		if err := m.sticky.SetAffinity(ctx, extra, 1, m.affinityTTL); !errors.Is(err, session.ErrUnavailable) {
			t.Fatalf("unbounded legacy cache accepted another key: %v", err)
		}
		time.Sleep(time.Second)
		first := extra
		first.KeyID = 1
		if _, err := m.sticky.GetAffinity(ctx, first); !errors.Is(err, session.ErrMiss) {
			t.Fatalf("expired legacy key retained preference: %v", err)
		}
		if err := m.sticky.SetAffinity(ctx, extra, 1, m.affinityTTL); err != nil {
			t.Fatalf("expired legacy identities did not release capacity: %v", err)
		}
	})
}

func TestAcquireLegacyStickyDoesNotCrossIntoScopedRequests(t *testing.T) {
	st := newAccountsTestStore(t)
	first := routingAccount(t, st, "first", 1)
	second := routingAccount(t, st, "second", 1)
	key := routingKey(t, st)
	m := New(st, nil, nil, Options{Affinity: session.NewMemory(session.Options{})})
	m.SetRotationStrategy(string(StrategySticky))
	_, held, err := m.AcquireSpecific(context.Background(), first.ID, "model")
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	affinityAcquire(t, m, key, "", second.ID)
	held()
	affinityAcquire(t, m, key, "", second.ID)
	affinityAcquire(t, m, key, "new-connection", first.ID)
}

func TestAcquireAffinityFullMemoryDoesNotBlockRequests(t *testing.T) {
	st := newAccountsTestStore(t)
	a := routingAccount(t, st, "available", 1)
	first := routingKey(t, st)
	second := routingKey(t, st)
	backend := session.NewMemory(session.Options{MaxEntries: 1})
	m := New(st, nil, nil, Options{Affinity: backend})
	affinityAcquire(t, m, first, "connection", a.ID)
	affinityAcquire(t, m, second, "connection", a.ID)
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	affinityAcquire(t, m, first, "connection", a.ID)
}

func TestAcquireAffinityCannotRestoreIneligibleAccount(t *testing.T) {
	for _, reason := range []string{"key scope", "disabled", "banned", "quota", "cooldown", "interval", "model", "weight", "single"} {
		t.Run(reason, func(t *testing.T) {
			st := newAccountsTestStore(t)
			ctx := context.Background()
			allowed := routingAccount(t, st, "allowed", 1)
			denied := routingAccount(t, st, "hinted but denied", 1)
			key := routingKey(t, st)
			probe := &affinityProbe{get: func(context.Context, session.AffinityKey) (int64, error) { return denied.ID, nil }}
			m := New(st, nil, nil, Options{Affinity: probe})
			m.SetRotationStrategy(string(StrategyRoundRobin))
			no := false
			var err error
			switch reason {
			case "key scope":
				routingGroup(t, st, "not permitted", denied.ID)
				permitted := routingGroup(t, st, "permitted", allowed.ID)
				err = st.SetKeyGroups(ctx, key.ID, []int64{permitted.ID})
			case "disabled":
				err = st.PatchAccountManagementFields(ctx, denied.ID, store.AccountManagementPatch{Enabled: &no})
			case "banned":
				err = st.SetAccountStatus(ctx, denied.ID, store.AccountStatusBanned, "")
			case "quota":
				err = st.SetAccountStatus(ctx, denied.ID, "quota_exhausted", "")
			case "cooldown":
				_, err = st.ExecContext(ctx, `UPDATE accounts SET cooldown_until=? WHERE id=?`, time.Now().Add(time.Hour).UnixMilli(), denied.ID)
			case "interval":
				m.SetRequestInterval(time.Hour)
				_, err = st.ExecContext(ctx, `UPDATE accounts SET last_started_at=? WHERE id=?`, time.Now().UnixMilli(), denied.ID)
			case "model":
				err = st.PatchAccountManagementFields(ctx, denied.ID, store.AccountManagementPatch{DisabledModels: []string{"model"}})
			case "weight":
				_, err = st.ExecContext(ctx, `UPDATE accounts SET weight=? WHERE id=?`, 10, allowed.ID)
			case "single":
				m.SetRotationStrategy(string(StrategySingle))
			}
			if err != nil {
				t.Fatal(err)
			}
			affinityAcquire(t, m, key, "connection", allowed.ID)
			if m.Inflight(denied.ID) != 0 {
				t.Fatal("hint reserved an ineligible account")
			}
		})
	}
}

func TestAffinityIdentityCanonicalAndExcludesVolatileState(t *testing.T) {
	m := New(nil, nil, nil, Options{})
	if m.affinityTTL != 4*time.Hour || m.affinityTimeout != 100*time.Millisecond {
		t.Fatalf("unexpected affinity defaults: %v/%v", m.affinityTTL, m.affinityTimeout)
	}
	key := &store.APIKey{ID: 1, Key: "secret", GroupIDs: []int64{2, 1}}
	first := &store.Account{ID: 1, AccessToken: "one", GroupIDs: []int64{3, 1}}
	second := &store.Account{ID: 2, AccessToken: "two"}
	original := m.affinityKey(key, []*store.Account{first, second}, "model", "scope", "smart")
	key.GroupIDs = []int64{1, 2}
	first.GroupIDs = []int64{1, 3}
	key.LastUsedAt, key.RequestCount, key.UpdatedAt = 12, 4, 30
	first.LastUsedAt, first.RequestCount, first.ErrorCount = 13, 5, 1
	first.LastStartedAt, first.UpdatedAt, first.EWMAFailureRateBP = 15, 31, 20
	requireDataCondition(t, m.affinityKey(key, []*store.Account{second, first}, "model", "scope", "smart") == original, "membership order or request telemetry changed the credential identity")
	for _, mutate := range []func(*store.APIKey){
		func(k *store.APIKey) { k.MaxConcurrency++ },
		func(k *store.APIKey) { k.RequestsPerMinute++ },
		func(k *store.APIKey) { k.DailyLimitUSD++ },
		func(k *store.APIKey) { k.WeeklyLimitUSD++ },
		func(k *store.APIKey) { k.Transport = "websocket" },
	} {
		changed := *key
		mutate(&changed)
		requireDataCondition(t, m.affinityKey(&changed, []*store.Account{first, second}, "model", "scope", "smart") != original, "key budget/concurrency/transport policy reused an old identity")
	}
}

func TestAffinityRequestCachesMissesAndFailures(t *testing.T) {
	for _, cacheErr := range []error{session.ErrMiss, session.ErrInvalid, session.ErrUnavailable} {
		t.Run(cacheErr.Error(), func(t *testing.T) {
			probe := &affinityProbe{get: func(context.Context, session.AffinityKey) (int64, error) { return 1, cacheErr }}
			m := New(nil, nil, nil, Options{Affinity: probe})
			key := &store.APIKey{ID: 1, Key: "secret"}
			candidates := []*store.Account{{ID: 1, AccessToken: "one"}}
			var request requestAffinity
			for i := 0; i < 100; i++ {
				requireDataCondition(t, request.load(context.Background(), m, key, candidates, "model", "scope", "smart") == 0, "faulty cache value became a preference")
			}
			{
				gets, _ := probe.counts()
				requireDataTruef(t, gets == 1, "same-identity miss/failure was fetched %d times", gets)
			}
			candidates[0].AccessToken = "rotated"
			request.load(context.Background(), m, key, candidates, "model", "scope", "smart")
			{
				gets, _ := probe.counts()
				requireDataTruef(t, gets == 2, "changed identity was not re-read: %d", gets)
			}
		})
	}
}

func BenchmarkAffinityIdentity(b *testing.B) {
	for _, size := range []int{1, 32, 128} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			m := New(nil, nil, nil, Options{AffinityIdentity: "provider-config"})
			key := &store.APIKey{ID: 1, Key: "fake-client-token", GroupIDs: []int64{2, 1}}
			candidates := make([]*store.Account, size)
			for i := range candidates {
				candidates[i] = &store.Account{ID: int64(i + 1), AccessToken: strings.Repeat("x", 1500), ProxyURL: "http://local.invalid", UpstreamProtocol: "ws"}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				m.affinityKey(key, candidates, "model", "connection", string(StrategyRoundRobin))
			}
		})
	}
}

func BenchmarkAcquireAffinity(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		b.Run(fmt.Sprintf("hint=%v", enabled), func(b *testing.B) {
			st, err := store.Open(filepath.Join(b.TempDir(), "benchmark.db"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = st.Close() })
			ctx := context.Background()
			for i := 0; i < 32; i++ {
				a := &store.Account{Name: fmt.Sprintf("account-%d", i), Enabled: true, Status: store.AccountStatusReady, Concurrency: 1, AccessToken: strings.Repeat("x", 1500)}
				if err := st.CreateAccount(ctx, a); err != nil {
					b.Fatal(err)
				}
			}
			key := &store.APIKey{Name: "client", Enabled: true}
			if err := st.CreateKey(ctx, key); err != nil {
				b.Fatal(err)
			}
			opts := Options{}
			if enabled {
				opts.Affinity = session.NewMemory(session.Options{})
			}
			m := New(st, nil, nil, opts)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, release, err := m.AcquireWithAffinity(ctx, key, "model", "connection")
				if err != nil {
					b.Fatal(err)
				}
				release()
			}
		})
	}
}

// testAcquireAffinityIdentityChangesDiscardOldHintPhase1 exercises one isolated test phase with the original shared state.
func testAcquireAffinityIdentityChangesDiscardOldHintPhase1(t *testing.T, capturedMutation *string) {
	ctx := context.Background()
	st := newAccountsTestStore(t)
	first := routingAccount(t, st, "first", 1)
	second := routingAccount(t, st, "previous preference", 1)
	key := routingKey(t, st)
	probe := &affinityProbe{}
	m := New(st, nil, nil, Options{Affinity: probe, AffinityIdentity: "config-v1"})
	m.SetRotationStrategy(string(StrategyRoundRobin))
	affinityCursor(m, 1)
	affinityAcquire(t, m, key, "connection", second.ID)
	var err error
	switch *capturedMutation {
	case "client token":
		_, err = st.ExecContext(ctx, `UPDATE api_keys SET key=? WHERE id=?`, key.Key+"-rotated", key.ID)
		if err == nil {
			key, err = st.GetKey(ctx, key.ID)
		}
	case "account token":
		_, err = st.ExecContext(ctx, `UPDATE accounts SET access_token=? WHERE id=?`, "rotated-access", second.ID)
	case "refresh token":
		_, err = st.ExecContext(ctx, `UPDATE accounts SET refresh_token=? WHERE id=?`, "rotated-refresh", second.ID)
	case "proxy":
		_, err = st.ExecContext(ctx, `UPDATE accounts SET proxy_url=? WHERE id=?`, "http://fake:secret@127.0.0.1:8080", second.ID)
	case "protocol":
		_, err = st.ExecContext(ctx, `UPDATE accounts SET upstream_protocol=? WHERE id=?`, "ws", second.ID)
	case "key scope":
		group := routingGroup(t, st, "new-scope")
		err = st.SetKeyGroups(ctx, key.ID, []int64{group.ID})
	case "strategy":
		updated := *key
		updated.Strategy = string(StrategyLeastInflight)
		err = st.UpdateKey(ctx, &updated)
	case "provider config":
		m = New(st, nil, nil, Options{Affinity: probe, AffinityIdentity: "config-v2"})
		m.SetRotationStrategy(string(StrategyRoundRobin))
	case "other candidate token":
		_, err = st.ExecContext(ctx, `UPDATE accounts SET access_token=? WHERE id=?`, "other-rotated", first.ID)
	}
	requireDataNoError(t, err)
	affinityCursor(m, 0)
	affinityAcquire(t, m, key, "connection", first.ID)
	requireDataCondition(t, !(len(probe.gets) != 2 || probe.gets[0] == probe.gets[1]), "identity change reused a previously cached preference")
	for _, cacheKey := range probe.gets {
		raw := fmt.Sprintf("%+v", cacheKey)
		for _, secret := range []string{key.Key, "fake-token", "fake:secret", "config-v1", "config-v2", "connection"} {
			requireDataTruef(t, !(strings.Contains(raw, secret)), "hint key exposed identity material: %q", secret)
		}
		requireDataCondition(t, !(len(cacheKey.IdentityHash) != 64 || len(cacheKey.SessionID) != 64), "cache received something other than bounded hashes")
	}
}

// testAcquireAffinityRevalidatesAfterCacheIOPhase1 exercises one isolated test phase with the original shared state.
func testAcquireAffinityRevalidatesAfterCacheIOPhase1(t *testing.T, capturedStage *string, capturedMutation *string) {
	st := newAccountsTestStore(t)
	ctx := context.Background()
	a := routingAccount(t, st, "target", 1)
	group := routingGroup(t, st, "scope", a.ID)
	key := routingKey(t, st, group.ID)
	want := ErrNoAccounts
	if (*capturedMutation) == "model denied" {
		want = ErrModelUnavailable
	}
	mutate := func() {
		var err error
		no := false
		switch *capturedMutation {
		case "disable key":
			current := *key
			current.Enabled = false
			err = st.UpdateKey(ctx, &current)
		case "rotate key":
			_, err = st.ExecContext(ctx, `UPDATE api_keys SET key=? WHERE id=?`, key.Key+"-rotated", key.ID)
		case "delete key":
			err = st.DeleteKey(ctx, key.ID)
		case "scope revoked":
			other := routingGroup(t, st, "other")
			err = st.SetKeyGroups(ctx, key.ID, []int64{other.ID})
		case "disable account":
			err = st.PatchAccountManagementFields(ctx, a.ID, store.AccountManagementPatch{Enabled: &no})
		case "model denied":
			err = st.PatchAccountManagementFields(ctx, a.ID, store.AccountManagementPatch{DisabledModels: []string{"model"}})
		case "group disabled":
			err = st.PatchAccountGroup(ctx, group.ID, store.AccountGroupPatch{Enabled: &no})
		}
		requireDataNoError(t, err)
	}
	probe := &affinityProbe{}
	if (*capturedStage) == "get" {
		probe.get = func(context.Context, session.AffinityKey) (int64, error) { mutate(); return a.ID, nil }
	} else {
		probe.set = func(context.Context, session.AffinityKey, int64) error { mutate(); return nil }
	}
	m := New(st, nil, nil, Options{Affinity: probe})
	account, release, err := m.AcquireWithAffinity(ctx, key, "model", "connection")
	routingAssertDenied(t, routingAcquireResult{account, release, err}, want)
	requireDataCondition(t, m.Inflight(a.ID) == 0, "denied hinted reservation leaked a slot")
}

// testAcquireAffinityQueuedReadsAreBoundedAndRevalidatedPhase1 exercises one isolated test phase with the original shared state.
func testAcquireAffinityQueuedReadsAreBoundedAndRevalidatedPhase1(t *testing.T, capturedOutcome *string) {
	synctest.Test(t, func(t *testing.T) {
		st := newAccountsTestStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		a := routingAccount(t, st, "held", 1)
		key := routingKey(t, st)
		probe := &affinityProbe{get: func(context.Context, session.AffinityKey) (int64, error) { return a.ID, nil }}
		m := New(st, nil, nil, Options{Affinity: probe})
		_, held, err := m.AcquireSpecific(ctx, a.ID, "model")
		requireDataNoError(t, err)
		defer held()
		finished := routingQueuedAcquire(t, func() (*store.Account, func(), error) {
			return m.AcquireWithAffinity(ctx, key, "model", "connection")
		})
		time.Sleep(300 * time.Millisecond)
		synctest.Wait()
		{
			gets, sets := probe.counts()
			requireDataTruef(t, !(gets != 1 || sets != 0), "unchanged waiting identity repeatedly hit cache: %d/%d", gets, sets)
		}
		wantGets := 1
		var want error
		switch *capturedOutcome {
		case "identity changes":
			for i := 0; i < maxAffinityReads+3; i++ {
				_, err = st.ExecContext(ctx, `UPDATE accounts SET access_token=? WHERE id=?`, fmt.Sprintf("rotated-%d", i), a.ID)
				requireDataNoError(t, err)
				time.Sleep(50 * time.Millisecond)
				synctest.Wait()
			}
			wantGets = maxAffinityReads
		case "key revoked":
			current := *key
			current.Enabled = false
			err = st.UpdateKey(ctx, &current)
			want = ErrNoAccounts
		case "account revoked":
			no := false
			err = st.PatchAccountManagementFields(ctx, a.ID, store.AccountManagementPatch{Enabled: &no})
			want = ErrNoAccounts
		case "model revoked":
			err = st.PatchAccountManagementFields(ctx, a.ID, store.AccountManagementPatch{DisabledModels: []string{"model"}})
			want = ErrModelUnavailable
		}
		requireDataNoError(t, err)
		held()
		result := routingAwaitAcquire(t, finished)
		wantSets := 0
		if want != nil {
			routingAssertDenied(t, result, want)
		} else {
			reportTestFailure(t, result.err != nil || result.account == nil || result.account.ID != a.ID || result.release == nil, func() {
				t.Fatalf("queued acquisition failed: %v %v", result.account, result.err)
			})
			result.release()
			wantSets = 1
		}
		{
			gets, sets := probe.counts()
			requireDataTruef(t, !(gets != wantGets || sets != wantSets), "cache calls=%d/%d, want %d/%d", gets, sets, wantGets, wantSets)
		}
		requireDataCondition(t, m.Inflight(a.ID) == 0, "queued hint handling leaked a slot")
	})
}
