package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pi-gateway/internal/codexbridge"

	"github.com/alicebob/miniredis/v2"
)

type fixture struct {
	store   Store
	advance func(time.Duration)
	memory  *Memory
	redis   *Redis
	server  *miniredis.Miniredis
}

func newFixture(t *testing.T, backend string, opts Options) fixture {
	t.Helper()
	if opts.CommitTimeout == 0 {
		opts.CommitTimeout = 2 * time.Second
	}
	if backend == "memory" {
		m := NewMemory(opts)
		now := time.Now()
		m.now = func() time.Time { return now }
		t.Cleanup(func() { _ = m.Close() })
		return fixture{store: m, memory: m, advance: func(d time.Duration) { now = now.Add(d) }}
	}
	srv := miniredis.RunT(t)
	now := time.Now()
	srv.SetTime(now)
	f := newRedisFixture(t, "redis://"+srv.Addr(), opts)
	f.server = srv
	f.advance = func(d time.Duration) { now = now.Add(d); srv.SetTime(now); srv.FastForward(d) }
	return f
}

func sample(id string) Record {
	return Record{ResponseID: id, KeyID: 11, AccountID: 22, SessionID: "internal-pool-anchor", InstanceID: "instance-A", ConnectionID: "connection-A", IdentityHash: "credential-revision-digest", Provider: "openai", Scope: ScopeConnectionLocal, OpaqueState: []byte("provider-issued-opaque-handle")}
}

func invalidation(r Record) Invalidation {
	return Invalidation{KeyID: r.KeyID, ResponseID: r.ResponseID, InstanceID: r.InstanceID, ConnectionID: r.ConnectionID, Version: r.Version}
}

func requireStatus(t *testing.T, err error, want Status) {
	t.Helper()
	if got := StatusOf(err); got != want {
		t.Fatalf("status = %s (%v), want %s", got, err, want)
	}
}

type fixtureFactory func(*testing.T, Options) fixture

func TestStoreContract(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			runStoreContract(t, func(t *testing.T, opts Options) fixture { return newFixture(t, backend, opts) })
		})
	}
}

// runStoreContract is shared by memory, miniredis, and the opt-in real Redis test.
func runStoreContract(t *testing.T, makeFixture fixtureFactory) {
	t.Run("codex_bridge_profile", func(t *testing.T) { runStoreContractCase1(t, makeFixture) })
	t.Run("ownership_version_and_invalidation", func(t *testing.T) { runStoreContractCase2(t, makeFixture) })
	t.Run("ttl_idempotency_and_copy_isolation", func(t *testing.T) { runStoreContractCase3(t, makeFixture) })
	t.Run("entry_and_byte_bounds", func(t *testing.T) { runStoreContractCase4(t, makeFixture) })
	t.Run("record_limit_invalid_and_cancellation", func(t *testing.T) { runStoreContractCase5(t, makeFixture) })
	t.Run("concurrent_CAS_and_capacity", func(t *testing.T) { runStoreContractCase6(t, makeFixture) })
	t.Run("affinity_and_token_leases", func(t *testing.T) { runStoreContractCase7(t, makeFixture) })
	t.Run("closed", func(t *testing.T) { runStoreContractCase8(t, makeFixture) })
}

func TestRedisNamespaceFingerprintCorruptionAndTTL(t *testing.T) {
	runRedisNamespaceContract(t, func(t *testing.T, opts Options) fixture { return newFixture(t, "redis", opts) })
}

// TestRedisIntegration is optional locally but mandatory whenever CI supplies
// PI_GATEWAY_TEST_REDIS_URL. An unreachable/misconfigured service is a failure.
func TestRedisIntegration(t *testing.T) {
	url := os.Getenv("PI_GATEWAY_TEST_REDIS_URL")
	if url == "" {
		t.Skip("PI_GATEWAY_TEST_REDIS_URL not configured; miniredis contracts still run")
	}
	runRedisIntegrationContract(t, url)
}

func TestCatalogCacheContract(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			runCatalogContract(t, func(t *testing.T, opts Options) fixture { return newFixture(t, backend, opts) })
		})
	}
}

func acquireCatalogLease(t *testing.T, cache CatalogCache, name string) string {
	t.Helper()
	token, acquired, err := cache.AcquireFillLease(context.Background(), name, 5*time.Second)
	requireStatus(t, err, Hit)
	if !acquired || len(token) != 64 {
		t.Fatalf("catalog lease not acquired: acquired=%v token length=%d", acquired, len(token))
	}
	return token
}

func catalogPayload(size int) []byte {
	return []byte(`"` + strings.Repeat("m", size-2) + `"`)
}

func runCatalogContract(t *testing.T, makeFixture fixtureFactory) {
	ctx := context.Background()
	const name = "model-catalog-v1:revision-and-account-ids-hash"
	t.Run("expired_owner_cannot_commit_or_release_replacement", func(t *testing.T) { runCatalogContractCase1(t, makeFixture, ctx, name) })
	t.Run("ttl_defaults_read_does_not_renew_and_copy_isolation", func(t *testing.T) { runCatalogContractCase2(t, makeFixture, ctx, name) })
	t.Run("independent_one_mib_limit_and_invalid_input", func(t *testing.T) { runCatalogContractCase3(t, makeFixture, ctx, name) })
	t.Run("entry_budget_shared_and_lease_slot_replaced", func(t *testing.T) { runCatalogContractCase4(t, makeFixture, ctx, name) })
	t.Run("byte_bound_failure_preserves_value_and_lease", func(t *testing.T) { runCatalogContractCase5(t, makeFixture, ctx, name) })
	t.Run("shared_record_affinity_and_catalog_byte_budget", func(t *testing.T) { runCatalogContractCase6(t, makeFixture, ctx, name) })
	t.Run("expired_catalog_reclaims_bytes_during_commit", func(t *testing.T) { runCatalogContractCase7(t, makeFixture, ctx, name) })
	t.Run("concurrent_unique_lease_and_single_commit", func(t *testing.T) { runCatalogContractCase8(t, makeFixture, ctx, name) })
	t.Run("cancel_and_closed", func(t *testing.T) { runCatalogContractCase9(t, makeFixture, ctx, name) })
	t.Run("corruption", func(t *testing.T) { runCatalogContractCase10(t, makeFixture, ctx, name) })
	t.Run("namespace_and_key_domains", func(t *testing.T) { runCatalogContractCase11(t, makeFixture, ctx, name) })
}

func TestCatalogUnavailableStoreDoesNotPanic(t *testing.T) {
	for _, cache := range []CatalogCache{(*Memory)(nil), &Memory{}, (*Redis)(nil), &Redis{}} {
		ctx := context.Background()
		_, getErr := cache.GetCatalog(ctx, "catalog")
		_, _, acquireErr := cache.AcquireFillLease(ctx, "catalog", 5*time.Second)
		token := strings.Repeat("a", 64)
		for _, err := range []error{getErr, acquireErr, cache.CommitCatalog(ctx, "catalog", token, []byte(`[]`), 0), cache.ReleaseFillLease(ctx, "catalog", token)} {
			requireStatus(t, err, Unavailable)
		}
	}
}

func BenchmarkMemoryCatalog(b *testing.B) {
	for _, size := range []int{1024, MaxCatalogBytes} {
		b.Run(fmt.Sprintf("read_%d", size), func(b *testing.B) {
			cache := NewMemory(Options{MaxEntries: 2})
			defer cache.Close()
			ctx := context.Background()
			token, _, err := cache.AcquireFillLease(ctx, "catalog", 5*time.Second)
			if err != nil {
				b.Fatal(err)
			}
			if err := cache.CommitCatalog(ctx, "catalog", token, catalogPayload(size), time.Hour); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := cache.GetCatalog(ctx, "catalog"); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("commit_%d", size), func(b *testing.B) {
			cache := NewMemory(Options{MaxEntries: 2})
			defer cache.Close()
			ctx := context.Background()
			payload := catalogPayload(size)
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				token, acquired, err := cache.AcquireFillLease(ctx, "catalog", 5*time.Second)
				if err != nil || !acquired {
					b.Fatalf("lease: acquired=%v err=%v", acquired, err)
				}
				if err := cache.CommitCatalog(ctx, "catalog", token, payload, 0); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func runStoreContractCase1(t *testing.T, makeFixture fixtureFactory) {
	f := makeFixture(t, Options{})
	ctx := context.Background()
	profile := codexbridge.Profile{Version: 1, Tools: codexbridge.ToolSpawnAgent}
	r := sample("bridge-profile")
	r.CodexBridge = profile
	requireStatus(t, f.store.RecordCompleted(ctx, r), Hit)
	got, err := f.store.Resolve(ctx, r.KeyID, r.ResponseID)
	requireStatus(t, err, Hit)
	reportTestFailure(t, got.CodexBridge != profile || string(got.OpaqueState) != string(r.OpaqueState), func() {
		t.Fatal("bounded profile was lost or placed in opaque state")
	})
	requireStatus(t, f.store.RecordCompleted(ctx, r), Hit)
	changed := r
	changed.CodexBridge = codexbridge.Profile{}
	requireStatus(t, f.store.RecordCompleted(ctx, changed), Invalid)
	changed = *got
	changed.CodexBridge = codexbridge.Profile{}
	requireStatus(t, f.store.RecordCompleted(ctx, changed), Invalid)
	for i, invalid := range []codexbridge.Profile{{Version: 99}, {Tools: 1}, {Version: 1, Tools: 1 << 31}} {
		bad := sample(fmt.Sprintf("invalid-profile-%d", i))
		bad.CodexBridge = invalid
		requireStatus(t, f.store.RecordCompleted(ctx, bad), Invalid)
	}
	legacy := sample("legacy-no-profile")
	requireStatus(t, f.store.RecordCompleted(ctx, legacy), Hit)
	got, err = f.store.Resolve(ctx, legacy.KeyID, legacy.ResponseID)
	requireStatus(t, err, Hit)
	reportTestFailure(t, got.CodexBridge != (codexbridge.Profile{}), func() {
		t.Fatal("zero-value legacy profile changed")
	})
	_, payload, err := prepare(r, nil, Options{}.normalized(), time.Now())
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	var wire map[string]any
	{
		err := json.Unmarshal(payload, &wire)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	wire["codex_bridge"] = map[string]any{"version": 99, "tools": 1}
	payload, err = json.Marshal(wire)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	_, err = decode(payload, r.ResponseID, Options{}.normalized(), time.Now())
	requireStatus(t, err, Invalid)
	delete(wire, "codex_bridge")
	payload, err = json.Marshal(wire)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	decoded, err := decode(payload, r.ResponseID, Options{}.normalized(), time.Now())
	requireStatus(t, err, Hit)
	reportTestFailure(t, decoded.CodexBridge != (codexbridge.Profile{}), func() {
		t.Fatal("old serialized records must remain valid")
	})
}

func runStoreContractCase2(t *testing.T, makeFixture fixtureFactory) {
	f := makeFixture(t, Options{})
	ctx := context.Background()
	r := sample("response-secret")
	got, err := f.store.Resolve(ctx, r.KeyID, r.ResponseID)
	requireStatus(t, err, Miss)
	reportTestFailure(t, got != nil, func() {
		t.Fatal("miss exposed record")
	})
	requireStatus(t, f.store.RecordCompleted(ctx, r), Hit)
	got, err = f.store.Resolve(ctx, r.KeyID, r.ResponseID)
	requireStatus(t, err, Hit)
	reportTestFailure(t, got.Version != 1 || got.ResponseID != r.ResponseID || got.SessionID != r.SessionID || got.ExpiresAt.Sub(got.CreatedAt) != DefaultTTL, func() {
		t.Fatalf("unexpected resolved record: %#v", got)
	})
	wrong, err := f.store.Resolve(ctx, r.KeyID+1, r.ResponseID)
	requireStatus(t, err, WrongOwner)
	reportTestFailure(t, wrong != nil, func() {
		t.Fatal("wrong owner exposed record")
	})
	for _, mutate := range []func(*Record){
		func(r *Record) { r.KeyID++ }, func(r *Record) { r.AccountID++ },
		func(r *Record) { r.InstanceID = "other" }, func(r *Record) { r.ConnectionID = "other" },
		func(r *Record) { r.SessionID = "other" }, func(r *Record) { r.Provider = "other" },
		func(r *Record) { r.IdentityHash = "rotated" }, func(r *Record) { r.Scope = ScopePersisted },
	} {
		attempt := got.clone()
		mutate(&attempt)
		requireStatus(t, f.store.RecordCompleted(ctx, attempt), WrongOwner)
	}
	stale := got.clone()
	stale.Version = 99
	requireStatus(t, f.store.RecordCompleted(ctx, stale), Invalid)
	stale = *got
	got.OpaqueState = []byte("next-opaque-handle")
	requireStatus(t, f.store.RecordCompleted(ctx, *got), Hit)
	requireStatus(t, f.store.RecordCompleted(ctx, stale), Invalid)
	latest, err := f.store.Resolve(ctx, r.KeyID, r.ResponseID)
	requireStatus(t, err, Hit)
	reportTestFailure(t, latest.Version != 2 || string(latest.OpaqueState) != "next-opaque-handle", func() {
		t.Fatal("CAS update lost")
	})
	requireStatus(t, f.store.Invalidate(ctx, invalidation(stale)), Invalid)
	bad := invalidation(*latest)
	bad.ConnectionID = "other"
	requireStatus(t, f.store.Invalidate(ctx, bad), WrongOwner)
	requireStatus(t, f.store.Invalidate(ctx, invalidation(*latest)), Hit)
	requireStatus(t, f.store.Invalidate(ctx, invalidation(*latest)), Miss)
}

func runStoreContractCase3(t *testing.T, makeFixture fixtureFactory) {
	f := makeFixture(t, Options{TTL: time.Hour})
	ctx := context.Background()
	r := sample("ttl")
	requireStatus(t, f.store.RecordCompleted(ctx, r), Hit)
	r.OpaqueState[0] = 'X'
	got, err := f.store.Resolve(ctx, r.KeyID, r.ResponseID)
	requireStatus(t, err, Hit)
	reportTestFailure(t, got.OpaqueState[0] == 'X', func() {
		t.Fatal("input slice alias")
	})
	expires := got.ExpiresAt
	got.OpaqueState[0] = 'Y'
	f.advance(30 * time.Minute)
	remaining := f.remainingTTL(t, r.ResponseID)
	reportTestFailure(t, remaining <= 0 || remaining > 30*time.Minute, func() {
		t.Fatalf("remaining TTL before retry = %v", remaining)
	})
	_, err = f.store.Resolve(ctx, r.KeyID, r.ResponseID)
	requireStatus(t, err, Hit)
	requireStatus(t, f.store.RecordCompleted(ctx, sample("ttl")), Hit)
	got, err = f.store.Resolve(ctx, r.KeyID, r.ResponseID)
	requireStatus(t, err, Hit)
	reportTestFailure(t, got.OpaqueState[0] == 'Y' || !got.ExpiresAt.Equal(expires) || got.Version != 1, func() {
		t.Fatal("idempotency extended TTL or aliased output")
	})
	{
		ttl := f.remainingTTL(t, r.ResponseID)
		reportTestFailure(t, ttl <= 0 || ttl > remaining, func() {
			t.Fatalf("read/retry renewed TTL: before=%v after=%v", remaining, ttl)
		})
	}
	f.advance(31 * time.Minute)
	_, err = f.store.Resolve(ctx, r.KeyID, r.ResponseID)
	requireStatus(t, err, Miss)
}

func runStoreContractCase4(t *testing.T, makeFixture fixtureFactory) {
	f := makeFixture(t, Options{MaxEntries: 1, TTL: time.Minute})
	ctx := context.Background()
	requireStatus(t, f.store.RecordCompleted(ctx, sample("one")), Hit)
	requireStatus(t, f.store.RecordCompleted(ctx, sample("two")), Unavailable)
	f.advance(2 * time.Minute)
	requireStatus(t, f.store.RecordCompleted(ctx, sample("two")), Hit)
	_, payload, err := prepare(sample("byte-one"), nil, Options{}.normalized(), time.Now())
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	b := makeFixture(t, Options{MaxBytes: int64(len(payload) + 64)})
	requireStatus(t, b.store.RecordCompleted(ctx, sample("byte-one")), Hit)
	requireStatus(t, b.store.RecordCompleted(ctx, sample("byte-two")), Unavailable)
	if b.redis != nil {
		requireRedisAccounting(t, b.redis, "byte-one")
	}
	old, err := b.store.Resolve(ctx, 11, "byte-one")
	requireStatus(t, err, Hit)
	old.OpaqueState = make([]byte, 1024)
	requireStatus(t, b.store.RecordCompleted(ctx, *old), Unavailable)
	unchanged, err := b.store.Resolve(ctx, 11, "byte-one")
	requireStatus(t, err, Hit)
	reportTestFailure(t, unchanged.Version != 1 || string(unchanged.OpaqueState) != string(sample("byte-one").OpaqueState), func() {
		t.Fatal("rejected update mutated existing value")
	})
	if b.redis != nil {
		requireRedisAccounting(t, b.redis, "byte-one")
	}
	requireStatus(t, b.store.Invalidate(ctx, invalidation(*unchanged)), Hit)
	if b.redis != nil {
		requireRedisAccounting(t, b.redis)
	}
	requireStatus(t, b.store.RecordCompleted(ctx, sample("byte-two")), Hit)
	if b.redis != nil {
		requireRedisAccounting(t, b.redis, "byte-two")
	}
}

func runStoreContractCase5(t *testing.T, makeFixture fixtureFactory) {
	f := makeFixture(t, Options{MaxRecordBytes: 2048})
	ctx := context.Background()
	r := sample("large")
	r.OpaqueState = make([]byte, 2048)
	requireStatus(t, f.store.RecordCompleted(ctx, r), Invalid)
	r = sample("missing-owner")
	r.IdentityHash = ""
	requireStatus(t, f.store.RecordCompleted(ctx, r), Invalid)
	_, err := f.store.Resolve(ctx, 0, "id")
	requireStatus(t, err, Invalid)
	r = sample("first-one")
	r.Version = 1
	requireStatus(t, f.store.RecordCompleted(ctx, r), Hit)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = f.store.Resolve(cancelled, 11, "first-one")
	requireStatus(t, err, Unavailable)
	reportTestFailure(t, !errors.Is(err, context.Canceled), func() {
		t.Fatal("lost context cancellation")
	})
}

func runStoreContractCase6(t *testing.T, makeFixture fixtureFactory) {
	f := makeFixture(t, Options{MaxEntries: 1})
	ctx := context.Background()
	requireStatus(t, f.store.RecordCompleted(ctx, sample("shared")), Hit)
	old, err := f.store.Resolve(ctx, 11, "shared")
	requireStatus(t, err, Hit)
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := old.clone()
			r.OpaqueState = []byte(fmt.Sprint(i))
			err := f.store.RecordCompleted(ctx, r)
			if err == nil {
				winners.Add(1)
			} else {
				reportTestFailure(t, !errors.Is(err, ErrInvalid), func() {
					t.Errorf("CAS: %v", err)
				})
			}
		}(i)
	}
	wg.Wait()
	reportTestFailure(t, winners.Load() != 1, func() {
		t.Fatalf("CAS winners=%d", winners.Load())
	})
	g := makeFixture(t, Options{MaxEntries: 3})
	winners.Store(0)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := g.store.RecordCompleted(ctx, sample(fmt.Sprint(i)))
			if err == nil {
				winners.Add(1)
			} else {
				reportTestFailure(t, !errors.Is(err, ErrUnavailable), func() {
					t.Errorf("capacity: %v", err)
				})
			}
		}(i)
	}
	wg.Wait()
	reportTestFailure(t, winners.Load() != 3, func() {
		t.Fatalf("capacity winners=%d", winners.Load())
	})
}

func runStoreContractCase7(t *testing.T, makeFixture fixtureFactory) {
	f := makeFixture(t, Options{MaxEntries: 3})
	ctx := context.Background()
	aff := f.store.(AffinityStore)
	leases := f.store.(FillLeaser)
	key := AffinityKey{KeyID: 11, SessionID: "client-session-secret", Provider: "provider", IdentityHash: "revision"}
	requireStatus(t, aff.SetAffinity(ctx, key, 22, time.Minute), Hit)
	account, err := aff.GetAffinity(ctx, key)
	requireStatus(t, err, Hit)
	reportTestFailure(t, account != 22, func() {
		t.Fatal("wrong account")
	})
	other := key
	other.KeyID++
	_, err = aff.GetAffinity(ctx, other)
	requireStatus(t, err, Miss)
	token, acquired, err := leases.AcquireFillLease(ctx, "fill", 10*time.Second)
	requireStatus(t, err, Hit)
	reportTestFailure(t, !acquired || token == "", func() {
		t.Fatal("lease not acquired")
	})
	_, acquired, err = leases.AcquireFillLease(ctx, "fill", 10*time.Second)
	requireStatus(t, err, Hit)
	reportTestFailure(t, acquired, func() {
		t.Fatal("lease acquired twice")
	})
	requireStatus(t, leases.ReleaseFillLease(ctx, "fill", strings.Repeat("x", 64)), WrongOwner)
	f.advance(11 * time.Second)
	newToken, acquired, err := leases.AcquireFillLease(ctx, "fill", 10*time.Second)
	requireStatus(t, err, Hit)
	reportTestFailure(t, !acquired || newToken == token, func() {
		t.Fatal("lease did not rotate")
	})
	requireStatus(t, leases.ReleaseFillLease(ctx, "fill", token), WrongOwner)
	_, acquired, err = leases.AcquireFillLease(ctx, "fill", 10*time.Second)
	requireStatus(t, err, Hit)
	reportTestFailure(t, acquired, func() {
		t.Fatal("old lease owner released the replacement lease")
	})
	requireStatus(t, leases.ReleaseFillLease(ctx, "fill", newToken), Hit)
	requireStatus(t, leases.ReleaseFillLease(ctx, "fill", newToken), Miss)
	f.advance(time.Minute)
	_, err = aff.GetAffinity(ctx, key)
	requireStatus(t, err, Miss)
}

func runStoreContractCase8(t *testing.T, makeFixture fixtureFactory) {
	f := makeFixture(t, Options{})
	if f.memory != nil {
		_ = f.memory.Close()
	} else {
		_ = f.redis.Close()
	}
	_, err := f.store.Resolve(context.Background(), 11, "anything")
	requireStatus(t, err, Unavailable)
}

func runCatalogContractCase1(t *testing.T, makeFixture fixtureFactory, ctx context.Context, name string) {
	f := makeFixture(t, Options{})
	cache := f.store.(CatalogCache)
	initial := acquireCatalogLease(t, cache, name)
	requireStatus(t, cache.CommitCatalog(ctx, name, initial, []byte(`{"models":["initial"]}`), 0), Hit)
	old := acquireCatalogLease(t, cache, name)
	f.advance(5 * time.Second)
	requireStatus(t, cache.CommitCatalog(ctx, name, old, []byte(`{"models":["expired"]}`), 0), Miss)
	current := acquireCatalogLease(t, cache, name)
	reportTestFailure(t, current == old, func() {
		t.Fatal("replacement reused old lease token")
	})
	requireStatus(t, cache.CommitCatalog(ctx, name, old, []byte(`{"models":["stale"]}`), 0), WrongOwner)
	requireStatus(t, cache.ReleaseFillLease(ctx, name, old), WrongOwner)
	_, acquired, err := cache.AcquireFillLease(ctx, name, 5*time.Second)
	requireStatus(t, err, Hit)
	reportTestFailure(t, acquired, func() {
		t.Fatal("stale owner removed the replacement lease")
	})
	got, err := cache.GetCatalog(ctx, name)
	requireStatus(t, err, Hit)
	reportTestFailure(t, string(got) != `{"models":["initial"]}`, func() {
		t.Fatal("stale owner overwrote existing catalog")
	})
	requireStatus(t, cache.CommitCatalog(ctx, name+"other", current, []byte(`[]`), 0), Miss)
	newValue := []byte(`{"models":["new-owner"]}`)
	requireStatus(t, cache.CommitCatalog(ctx, name, current, newValue, 0), Hit)
	for _, token := range []string{old, current} {
		requireStatus(t, cache.CommitCatalog(ctx, name, token, []byte(`[]`), 0), Miss)
		requireStatus(t, cache.ReleaseFillLease(ctx, name, token), Miss)
	}
	third := acquireCatalogLease(t, cache, name)
	requireStatus(t, cache.CommitCatalog(ctx, name, old, []byte(`[]`), 0), WrongOwner)
	requireStatus(t, cache.ReleaseFillLease(ctx, name, old), WrongOwner)
	got, err = cache.GetCatalog(ctx, name)
	requireStatus(t, err, Hit)
	reportTestFailure(t, string(got) != string(newValue), func() {
		t.Fatal("old owner changed replacement catalog")
	})
	requireStatus(t, cache.ReleaseFillLease(ctx, name, third), Hit)
	f.requireCatalogAccounting(t, name, false)
}

func runCatalogContractCase2(t *testing.T, makeFixture fixtureFactory, ctx context.Context, name string) {
	f := makeFixture(t, Options{TTL: time.Millisecond})
	cache := f.store.(CatalogCache)
	_, err := cache.GetCatalog(ctx, name)
	requireStatus(t, err, Miss)
	payload := []byte(`{"models":["a"]}`)
	original := string(payload)
	token := acquireCatalogLease(t, cache, name)
	requireStatus(t, cache.CommitCatalog(ctx, name, token, payload, 0), Hit)
	payload[0] = 'x'
	got, err := cache.GetCatalog(ctx, name)
	requireStatus(t, err, Hit)
	reportTestFailure(t, string(got) != original, func() {
		t.Fatal("catalog input slice aliased stored value")
	})
	got[0] = 'y'
	ttl := f.catalogRemainingTTL(t, name)
	reportTestFailure(t, ttl < 29*time.Second || ttl > DefaultCatalogTTL, func() {
		t.Fatalf("catalog default TTL inherited record TTL: %v", ttl)
	})
	f.advance(29 * time.Second)
	before := f.catalogRemainingTTL(t, name)
	got, err = cache.GetCatalog(ctx, name)
	requireStatus(t, err, Hit)
	reportTestFailure(t, string(got) != original, func() {
		t.Fatal("catalog output slice aliased stored value")
	})
	{
		after := f.catalogRemainingTTL(t, name)
		reportTestFailure(t, after <= 0 || after > before, func() {
			t.Fatalf("read renewed TTL: before=%v after=%v", before, after)
		})
	}
	f.advance(time.Second + time.Millisecond)
	got, err = cache.GetCatalog(ctx, name)
	requireStatus(t, err, Miss)
	reportTestFailure(t, got != nil, func() {
		t.Fatal("expired catalog was returned")
	})
	token = acquireCatalogLease(t, cache, name)
	requireStatus(t, cache.CommitCatalog(ctx, name, token, []byte(`[]`), 1250*time.Millisecond), Hit)
	{
		ttl := f.catalogRemainingTTL(t, name)
		reportTestFailure(t, ttl <= 0 || ttl > 1250*time.Millisecond, func() {
			t.Fatalf("explicit TTL ignored: %v", ttl)
		})
	}
	f.advance(1251 * time.Millisecond)
	_, err = cache.GetCatalog(ctx, name)
	requireStatus(t, err, Miss)
}

func runCatalogContractCase3(t *testing.T, makeFixture fixtureFactory, ctx context.Context, name string) {
	f := makeFixture(t, Options{MaxRecordBytes: 16})
	cache := f.store.(CatalogCache)
	token := acquireCatalogLease(t, cache, name)
	payload := catalogPayload(MaxCatalogBytes)
	requireStatus(t, cache.CommitCatalog(ctx, name, token, payload, 0), Hit)
	got, err := cache.GetCatalog(ctx, name)
	requireStatus(t, err, Hit)
	reportTestFailure(t, string(got) != string(payload) || len(got) != 1<<20 || MaxRecordBytes != 64<<10, func() {
		t.Fatal("catalog/continuation size limits are not independent")
	})
	token = acquireCatalogLease(t, cache, name)
	for _, invalid := range [][]byte{nil, {}, []byte("not-json"), catalogPayload(MaxCatalogBytes + 1)} {
		requireStatus(t, cache.CommitCatalog(ctx, name, token, invalid, 0), Invalid)
	}
	for _, ttl := range []time.Duration{-time.Second, time.Nanosecond, time.Millisecond - 1} {
		requireStatus(t, cache.CommitCatalog(ctx, name, token, []byte(`[]`), ttl), Invalid)
	}
	for _, invalidName := range []string{"", strings.Repeat("x", 4097)} {
		_, err := cache.GetCatalog(ctx, invalidName)
		requireStatus(t, err, Invalid)
		_, _, err = cache.AcquireFillLease(ctx, invalidName, 5*time.Second)
		requireStatus(t, err, Invalid)
		requireStatus(t, cache.CommitCatalog(ctx, invalidName, token, []byte(`[]`), 0), Invalid)
	}
	requireStatus(t, cache.CommitCatalog(ctx, name, "short", []byte(`[]`), 0), Invalid)
	got, err = cache.GetCatalog(ctx, name)
	requireStatus(t, err, Hit)
	reportTestFailure(t, string(got) != string(payload), func() {
		t.Fatal("rejected input changed existing catalog")
	})
	f.requireCatalogAccounting(t, name, true)
	requireStatus(t, cache.CommitCatalog(ctx, name, token, []byte(`[]`), 0), Hit)
	f.requireCatalogAccounting(t, name, false)
}

func runCatalogContractCase4(t *testing.T, makeFixture fixtureFactory, ctx context.Context, name string) {
	f := makeFixture(t, Options{MaxEntries: 1})
	cache := f.store.(CatalogCache)
	token := acquireCatalogLease(t, cache, name)
	requireStatus(t, cache.CommitCatalog(ctx, name, token, []byte(`[]`), 0), Hit)
	f.requireCatalogAccounting(t, name, false)
	_, _, err := cache.AcquireFillLease(ctx, name+"other", 5*time.Second)
	requireStatus(t, err, Unavailable)
	requireStatus(t, f.store.RecordCompleted(ctx, sample("full")), Unavailable)
	affinity := AffinityKey{KeyID: 1, SessionID: "session", Provider: "provider", IdentityHash: "revision"}
	requireStatus(t, f.store.(AffinityStore).SetAffinity(ctx, affinity, 1, time.Second), Unavailable)
	f.advance(DefaultCatalogTTL + time.Millisecond)
	token = acquireCatalogLease(t, cache, name)
	requireStatus(t, cache.CommitCatalog(ctx, name, token, []byte(`{}`), 0), Hit)
	f.requireCatalogAccounting(t, name, false)
}

func runCatalogContractCase5(t *testing.T, makeFixture fixtureFactory, ctx context.Context, name string) {
	f := makeFixture(t, Options{MaxBytes: 128, MaxEntries: 2})
	cache := f.store.(CatalogCache)
	token := acquireCatalogLease(t, cache, name)
	requireStatus(t, cache.CommitCatalog(ctx, name, token, catalogPayload(64), 0), Hit)
	token = acquireCatalogLease(t, cache, name)
	f.requireCatalogAccounting(t, name, true)
	requireStatus(t, cache.CommitCatalog(ctx, name, token, catalogPayload(129), 0), Unavailable)
	got, err := cache.GetCatalog(ctx, name)
	requireStatus(t, err, Hit)
	reportTestFailure(t, len(got) != 64, func() {
		t.Fatal("over-budget write replaced existing catalog")
	})
	f.requireCatalogAccounting(t, name, true)
	requireStatus(t, cache.CommitCatalog(ctx, name, token, catalogPayload(128), 0), Hit)
	f.requireCatalogAccounting(t, name, false)
	_, _, err = cache.AcquireFillLease(ctx, name, 5*time.Second)
	requireStatus(t, err, Unavailable)
	g := makeFixture(t, Options{MaxBytes: 64, MaxEntries: 1})
	other := g.store.(CatalogCache)
	token = acquireCatalogLease(t, other, name)
	requireStatus(t, other.CommitCatalog(ctx, name, token, catalogPayload(65), 0), Unavailable)
	_, err = other.GetCatalog(ctx, name)
	requireStatus(t, err, Miss)
	requireStatus(t, other.ReleaseFillLease(ctx, name, token), Hit)
	g.requireCatalogAccounting(t, "", false)
	h := makeFixture(t, Options{MaxBytes: 63})
	_, _, err = h.store.(CatalogCache).AcquireFillLease(ctx, name, 5*time.Second)
	requireStatus(t, err, Unavailable)
	h.requireCatalogAccounting(t, "", false)
}

func runCatalogContractCase6(t *testing.T, makeFixture fixtureFactory, ctx context.Context, name string) {
	f := makeFixture(t, Options{MaxEntries: 3, MaxBytes: 2048})
	cache := f.store.(CatalogCache)
	r := sample("shared-budget")
	requireStatus(t, f.store.RecordCompleted(ctx, r), Hit)
	var recordBytes int
	if f.memory != nil {
		recordBytes = len(f.memory.entries[responseKey(r.ResponseID)].payload)
	} else {
		size, err := f.redis.client.StrLen(ctx, f.redis.key(r.ResponseID)).Result()
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
		recordBytes = int(size)
	}
	affinity := AffinityKey{KeyID: 1, SessionID: "session", Provider: "provider", IdentityHash: "revision"}
	aff := f.store.(AffinityStore)
	requireStatus(t, aff.SetAffinity(ctx, affinity, 22, time.Minute), Hit)
	token := acquireCatalogLease(t, cache, name)
	remaining := 2048 - recordBytes - len("22")
	requireStatus(t, cache.CommitCatalog(ctx, name, token, catalogPayload(remaining+1), 0), Unavailable)
	requireStatus(t, cache.CommitCatalog(ctx, name, token, catalogPayload(remaining), 0), Hit)
	requireStatus(t, aff.SetAffinity(ctx, affinity, 222, time.Minute), Unavailable)
	requireStatus(t, f.store.RecordCompleted(ctx, sample("another-record")), Unavailable)
	account, err := aff.GetAffinity(ctx, affinity)
	requireStatus(t, err, Hit)
	reportTestFailure(t, account != 22, func() {
		t.Fatal("over-budget affinity update changed shared store")
	})
	_, err = f.store.Resolve(ctx, r.KeyID, r.ResponseID)
	requireStatus(t, err, Hit)
	if f.memory != nil {
		reportTestFailure(t, len(f.memory.entries) != 3 || f.memory.bytes != 2048, func() {
			t.Fatal("memory did not share exact catalog/record/affinity budget")
		})
	} else {
		count, err := f.redis.client.ZCard(ctx, f.redis.indexKey()).Result()
		reportTestFailure(t, err != nil || count != 3, func() {
			t.Fatalf("shared expiry entries=%d err=%v", count, err)
		})
		total, err := f.redis.client.HGet(ctx, f.redis.sizeKey(), "_total").Int64()
		reportTestFailure(t, err != nil || total != 2048, func() {
			t.Fatalf("shared ledger bytes=%d err=%v", total, err)
		})
	}
}

func runCatalogContractCase7(t *testing.T, makeFixture fixtureFactory, ctx context.Context, name string) {
	f := makeFixture(t, Options{MaxEntries: 2, MaxBytes: 128})
	cache := f.store.(CatalogCache)
	token := acquireCatalogLease(t, cache, name)
	requireStatus(t, cache.CommitCatalog(ctx, name, token, catalogPayload(64), 0), Hit)
	f.advance(29 * time.Second)
	token = acquireCatalogLease(t, cache, name)
	f.advance(2 * time.Second)
	_, err := cache.GetCatalog(ctx, name)
	requireStatus(t, err, Miss)
	requireStatus(t, cache.CommitCatalog(ctx, name, token, catalogPayload(128), 0), Hit)
	f.requireCatalogAccounting(t, name, false)
}

func runCatalogContractCase8(t *testing.T, makeFixture fixtureFactory, ctx context.Context, name string) {
	f := makeFixture(t, Options{MaxEntries: 1})
	cache := f.store.(CatalogCache)
	var owners atomic.Int32
	var wg sync.WaitGroup
	tokens := make(chan string, 16)
	for i := 0; i < cap(tokens); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, acquired, err := cache.AcquireFillLease(ctx, name, 5*time.Second)
			if err != nil {
				t.Errorf("acquire lease: %v", err)
			} else if acquired {
				owners.Add(1)
				tokens <- token
			} else {
				reportTestFailure(t, token != "", func() {
					t.Error("loser received owner's token")
				})
			}
		}()
	}
	wg.Wait()
	reportTestFailure(t, owners.Load() != 1, func() {
		t.Fatalf("lease owners=%d", owners.Load())
	})
	token := <-tokens
	var commits atomic.Int32
	for i := 0; i < cap(tokens); i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := cache.CommitCatalog(ctx, name, token, []byte(fmt.Sprintf(`{"winner":%d}`, i)), 0)
			if err == nil {
				commits.Add(1)
			} else {
				reportTestFailure(t, !errors.Is(err, ErrMiss), func() {
					t.Errorf("commit consumed lease: %v", err)
				})
			}
		}(i)
	}
	wg.Wait()
	reportTestFailure(t, commits.Load() != 1, func() {
		t.Fatalf("catalog commits=%d", commits.Load())
	})
	f.requireCatalogAccounting(t, name, false)
}

func runCatalogContractCase9(t *testing.T, makeFixture fixtureFactory, ctx context.Context, name string) {
	f := makeFixture(t, Options{})
	cache := f.store.(CatalogCache)
	token := acquireCatalogLease(t, cache, name)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	deadline, stop := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer stop()
	for _, testContext := range []context.Context{cancelled, deadline} {
		_, getErr := cache.GetCatalog(testContext, name)
		_, _, acquireErr := cache.AcquireFillLease(testContext, name, 5*time.Second)
		commitErr := cache.CommitCatalog(testContext, name, token, []byte(`[]`), 0)
		releaseErr := cache.ReleaseFillLease(testContext, name, token)
		for _, err := range []error{getErr, acquireErr, commitErr, releaseErr} {
			requireStatus(t, err, Unavailable)
			reportTestFailure(t, !errors.Is(err, testContext.Err()), func() {
				t.Fatalf("lost context error: %v", err)
			})
		}
	}
	requireStatus(t, cache.CommitCatalog(ctx, name, token, []byte(`[]`), 0), Hit)
	if f.memory != nil {
		_ = f.memory.Close()
	} else {
		_ = f.redis.Close()
	}
	_, getErr := cache.GetCatalog(ctx, name)
	_, _, acquireErr := cache.AcquireFillLease(ctx, name, 5*time.Second)
	for _, err := range []error{getErr, acquireErr, cache.CommitCatalog(ctx, name, token, []byte(`[]`), 0), cache.ReleaseFillLease(ctx, name, token)} {
		requireStatus(t, err, Unavailable)
	}
}

func runCatalogContractCase10(t *testing.T, makeFixture fixtureFactory, ctx context.Context, name string) {
	runCatalogCorruptionContract(t, makeFixture)
}

func runCatalogContractCase11(t *testing.T, makeFixture fixtureFactory, ctx context.Context, name string) {
	runCatalogNamespaceContract(t, makeFixture)
}
