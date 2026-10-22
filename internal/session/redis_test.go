package session

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newRedisFixture deliberately borrows its client: even the closed-store contract
// must leave the transport available until namespace cleanup has completed.
func newRedisFixture(t *testing.T, url string, opts Options) fixture {
	t.Helper()
	if opts.CommitTimeout == 0 {
		opts.CommitTimeout = 2 * time.Second
	}
	connection, err := NewRedis(RedisOptions{URL: url, Options: opts})
	if err != nil {
		t.Fatalf("create Redis contract connection: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return newRedisNamespaceFixture(t, connection.client, opts)
}

func newRedisNamespaceFixture(t *testing.T, client redis.UniversalClient, opts Options) fixture {
	t.Helper()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	opts.Namespace = fmt.Sprintf("%s:test:%x", opts.normalized().Namespace, nonce)
	if opts.CommitTimeout == 0 {
		opts.CommitTimeout = 2 * time.Second
	}
	r, err := NewRedisWithClient(client, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer r.Close()
		ctx, cancel := context.WithTimeout(context.Background(), opts.CommitTimeout)
		defer cancel()
		keys, err := redisNamespaceKeys(ctx, r)
		if err != nil {
			t.Errorf("scan Redis contract namespace for cleanup: %v", err)
			return
		}
		// DEL receives only enumerated keys checked against this fixture's unique
		// prefix. Never flush a database or delete a caller-supplied pattern.
		if len(keys) > 0 {
			if err := client.Del(ctx, keys...).Err(); err != nil {
				t.Errorf("clean Redis contract namespace: %v", err)
			}
		}
	})
	return fixture{store: r, redis: r, advance: func(d time.Duration) { advanceRedisTTL(t, r, d) }}
}

func redisNamespaceKeys(ctx context.Context, r *Redis) ([]string, error) {
	var keys []string
	var cursor uint64
	seen := make(map[string]bool)
	for {
		batch, next, err := r.client.Scan(ctx, cursor, r.prefix+"*", 100).Result()
		if err != nil {
			return nil, err
		}
		for _, key := range batch {
			if !strings.HasPrefix(key, r.prefix) {
				return nil, fmt.Errorf("Redis scan escaped contract namespace")
			}
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
		cursor = next
		if cursor == 0 {
			return keys, nil
		}
	}
}

// Redis has no injected clock. Translate only this fixture's server-side TTLs
// and expiry scores using PTTL/PEXPIRE, instead of sleeping for minutes/hours.
// Stored payloads are not rewritten: an idempotent write or read must preserve
// their original timestamps as well as the shortened server TTL.
func advanceRedisTTL(t *testing.T, r *Redis, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), r.options.CommitTimeout)
	defer cancel()
	keys, err := redisNamespaceKeys(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	members, err := r.client.ZRangeWithScores(ctx, r.indexKey(), 0, -1).Result()
	if err != nil {
		t.Fatal(err)
	}
	pipe := r.client.TxPipeline()
	for _, member := range members {
		key, ok := member.Member.(string)
		if !ok || !strings.HasPrefix(key, r.prefix) {
			t.Fatal("expiry index escaped contract namespace")
		}
		pipe.ZAdd(ctx, r.indexKey(), redis.Z{Score: member.Score - float64(d.Milliseconds()), Member: key})
	}
	for _, key := range keys {
		ttl, err := r.client.PTTL(ctx, key).Result()
		if err != nil {
			t.Fatal(err)
		}
		if ttl >= 0 {
			pipe.PExpire(ctx, key, ttl-d)
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) remainingTTL(t *testing.T, responseID string) time.Duration {
	t.Helper()
	if f.memory != nil {
		f.memory.mu.Lock()
		defer f.memory.mu.Unlock()
		return f.memory.entries[responseKey(responseID)].expires.Sub(f.memory.now())
	}
	ctx, cancel := context.WithTimeout(context.Background(), f.redis.options.CommitTimeout)
	defer cancel()
	ttl, err := f.redis.client.PTTL(ctx, f.redis.key(responseID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	return ttl
}

func TestRedisAuthenticationTLSAndTimeoutOptions(t *testing.T) {
	server := miniredis.RunT(t)
	server.RequireAuth("correct-secret")
	r, err := NewRedis(RedisOptions{URL: "redis://:wrong-secret@" + server.Addr() + "/2?max_retries=100&read_timeout=20s", Password: "correct-secret", Options: Options{CommitTimeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	requireStatus(t, r.RecordCompleted(context.Background(), sample("authenticated")), Hit)
	if !server.DB(2).Exists(r.key("authenticated")) {
		t.Fatal("Redis database from URL ignored")
	}
	opts := r.client.(*redis.Client).Options()
	if opts.MaxRetries != 0 || opts.ReadTimeout != time.Second || !opts.ContextTimeoutEnabled || opts.PoolTimeout != time.Second || opts.MaxActiveConns != 8 {
		t.Fatalf("short-timeout/retry options not enforced: %+v", opts)
	}
	for _, url := range []string{"rediss://localhost:6379", "redis://localhost:6379"} {
		secure, err := NewRedis(RedisOptions{URL: url, TLS: true})
		if err != nil {
			t.Fatal(err)
		}
		options := secure.client.(*redis.Client).Options()
		if options.TLSConfig == nil || options.TLSConfig.InsecureSkipVerify || options.TLSConfig.MinVersion < tls.VersionTLS12 {
			t.Fatal("unsafe TLS options")
		}
		_ = secure.Close()
	}
	bad, err := NewRedis(RedisOptions{URL: "redis://:credential-never-log@" + server.Addr(), Options: Options{CommitTimeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	_, err = bad.Resolve(context.Background(), 11, "secret-response")
	requireStatus(t, err, Unavailable)
	if strings.Contains(err.Error(), "credential-never-log") || strings.Contains(err.Error(), "secret-response") {
		t.Fatal("error leaked secrets")
	}
	for _, url := range []string{"https://user:secret@host", "redis://host/not-a-db", "bad-url"} {
		if _, err := NewRedis(RedisOptions{URL: url}); err != ErrInvalid {
			t.Fatalf("URL validation = %v", err)
		}
	}
}

func TestRedisUnavailableIsBounded(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	r, err := NewRedis(RedisOptions{URL: "redis://" + listener.Addr().String(), Options: Options{CommitTimeout: 40 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	start := time.Now()
	_, err = r.Resolve(context.Background(), 11, "timeout-response")
	requireStatus(t, err, Unavailable)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("cache timeout exceeded budget: %s", elapsed)
	}
	select {
	case conn := <-accepted:
		_ = conn.Close()
	case <-time.After(time.Second):
		t.Fatal("client never dialed test listener")
	}
}

func TestRedisRejectsCorruptFingerprintSchemaAndOwner(t *testing.T) {
	runRedisCorruptionContract(t, func(t *testing.T, opts Options) fixture { return newFixture(t, "redis", opts) })
}

func runRedisCorruptionContract(t *testing.T, makeFixture fixtureFactory) {
	f := makeFixture(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	record := sample("integrity")
	requireStatus(t, f.store.RecordCompleted(ctx, record), Hit)
	stored, err := f.store.Resolve(ctx, record.KeyID, record.ResponseID)
	requireStatus(t, err, Hit)
	original, err := f.redis.client.Get(ctx, f.redis.key(record.ResponseID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, payload string }{
		{"empty", ""},
		{"malformed_json", "not-json"},
		{"incomplete_schema", `{"schema_version":2}`},
		{"oversize", strings.Repeat("x", MaxRecordBytes+1)},
		{"schema", strings.Replace(original, `"schema_version":1`, `"schema_version":999`, 1)},
		{"response_fingerprint", strings.Replace(original, Fingerprint("response", record.ResponseID), Fingerprint("response", "other"), 1)},
		{"session_fingerprint", strings.Replace(original, Fingerprint("session", record.SessionID), Fingerprint("session", "other"), 1)},
		{"owner", strings.Replace(original, `"key_id":"11"`, `"key_id":"0"`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.payload == original {
				t.Fatal("corruption fixture did not change the payload")
			}
			if err := f.redis.client.Set(ctx, f.redis.key(record.ResponseID), tc.payload, time.Minute).Err(); err != nil {
				t.Fatal(err)
			}
			got, err := f.store.Resolve(ctx, record.KeyID, record.ResponseID)
			requireStatus(t, err, Invalid)
			if got != nil {
				t.Fatal("corrupt record was exposed")
			}
			requireStatus(t, f.store.RecordCompleted(ctx, record), Invalid)
			requireStatus(t, f.store.Invalidate(ctx, invalidation(*stored)), Invalid)
		})
	}
	t.Run("wrong_type", func(t *testing.T) {
		if err := f.redis.client.Del(ctx, f.redis.key(record.ResponseID)).Err(); err != nil {
			t.Fatal(err)
		}
		if err := f.redis.client.LPush(ctx, f.redis.key(record.ResponseID), "not-a-record").Err(); err != nil {
			t.Fatal(err)
		}
		_, err := f.store.Resolve(ctx, record.KeyID, record.ResponseID)
		requireStatus(t, err, Invalid)
		requireStatus(t, f.store.RecordCompleted(ctx, record), Invalid)
		requireStatus(t, f.store.Invalidate(ctx, invalidation(*stored)), Invalid)
	})
}

func TestRedisRejectedFirstWriteLeavesNoUnboundedIndex(t *testing.T) {
	runRedisRejectedWriteContract(t, func(t *testing.T, opts Options) fixture { return newFixture(t, "redis", opts) })
}

func runRedisRejectedWriteContract(t *testing.T, makeFixture fixtureFactory) {
	f := makeFixture(t, Options{MaxBytes: 1})
	requireStatus(t, f.store.RecordCompleted(context.Background(), sample("over-budget")), Unavailable)
	ctx, cancel := context.WithTimeout(context.Background(), f.redis.options.CommitTimeout)
	defer cancel()
	keys, err := redisNamespaceKeys(ctx, f.redis)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("rejected write leaked nonexpiring metadata: %v", keys)
	}
}

func runRedisNamespaceContract(t *testing.T, makeFixture fixtureFactory) {
	f := makeFixture(t, Options{Namespace: "test:tenant", MaxEntries: 2})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	record := sample("raw-response-secret")
	requireStatus(t, f.store.RecordCompleted(ctx, record), Hit)
	if ttl := f.remainingTTL(t, record.ResponseID); ttl <= 0 || ttl > DefaultTTL {
		t.Fatalf("TTL = %v", ttl)
	}
	keys, err := redisNamespaceKeys(ctx, f.redis)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if !strings.HasPrefix(key, "test:tenant:") || strings.Contains(key, record.ResponseID) || strings.Contains(key, record.SessionID) {
			t.Fatalf("unsafe key %q", key)
		}
	}
	raw, err := f.redis.client.Get(ctx, f.redis.key(record.ResponseID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{record.ResponseID, "request_body", "transcript", "access_token", "authorization", "password"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("forbidden field/identifier %q", forbidden)
		}
	}
	// Both namespaces share the same connection/database, not separate servers.
	other := newRedisNamespaceFixture(t, f.redis.client, Options{Namespace: "test:tenant", MaxEntries: 2})
	if f.redis.prefix == other.redis.prefix {
		t.Fatal("test namespaces collided")
	}
	_, err = other.store.Resolve(ctx, record.KeyID, record.ResponseID)
	requireStatus(t, err, Miss)
	replacement := sample(record.ResponseID)
	replacement.KeyID++
	requireStatus(t, other.store.RecordCompleted(ctx, replacement), Hit)
	original, err := f.store.Resolve(ctx, record.KeyID, record.ResponseID)
	requireStatus(t, err, Hit)
	token, acquired, err := f.redis.AcquireFillLease(ctx, "same-lease", time.Minute)
	requireStatus(t, err, Hit)
	if !acquired {
		t.Fatal("first namespace lease not acquired")
	}
	otherToken, acquired, err := other.redis.AcquireFillLease(ctx, "same-lease", time.Minute)
	requireStatus(t, err, Hit)
	if !acquired || token == otherToken {
		t.Fatal("namespaces shared a lease")
	}
	requireStatus(t, other.redis.ReleaseFillLease(ctx, "same-lease", token), WrongOwner)
	requireStatus(t, f.store.RecordCompleted(ctx, sample("over-capacity")), Unavailable)
	requireStatus(t, other.store.RecordCompleted(ctx, sample("over-capacity")), Unavailable)
	requireStatus(t, f.store.Invalidate(ctx, invalidation(*original)), Hit)
	requireStatus(t, f.store.RecordCompleted(ctx, sample("after-invalidation")), Hit)
	_, err = other.store.Resolve(ctx, replacement.KeyID, replacement.ResponseID)
	requireStatus(t, err, Hit)
	requireStatus(t, other.store.RecordCompleted(ctx, sample("still-over-capacity")), Unavailable)
	requireStatus(t, other.redis.ReleaseFillLease(ctx, "same-lease", otherToken), Hit)
	requireStatus(t, f.redis.ReleaseFillLease(ctx, "same-lease", token), Hit)
}

// requireRedisAccounting checks the bounded, expiring index and byte ledger
// against actual serialized values, including after a rejected write/update.
func requireRedisAccounting(t *testing.T, r *Redis, responseIDs ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), r.options.CommitTimeout)
	defer cancel()
	if len(responseIDs) == 0 {
		n, err := r.client.Exists(ctx, r.indexKey(), r.sizeKey()).Result()
		reportTestFailure(t, err != nil || n != 0, func() {
			t.Fatalf("empty namespace retained accounting: count=%d error=%v", n, err)
		})
		return
	}
	members, err := r.client.ZRange(ctx, r.indexKey(), 0, -1).Result()
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	sizes, err := r.client.HGetAll(ctx, r.sizeKey()).Result()
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	reportTestFailure(t, len(members) != len(responseIDs) || len(members) > r.options.MaxEntries || len(sizes) != len(members)+1, func() {
		t.Fatalf("index/size cardinality mismatch: index=%d sizes=%d want=%d", len(members), len(sizes), len(responseIDs))
	})
	wanted := make(map[string]bool, len(responseIDs))
	for _, id := range responseIDs {
		wanted[r.key(id)] = true
	}
	var total int64
	for _, key := range members {
		reportTestFailure(t, !wanted[key], func() {
			t.Fatalf("unexpected expiry index member %q", key)
		})
		size, err := r.client.StrLen(ctx, key).Result()
		reportTestFailure(t, err != nil || size <= 0, func() {
			t.Fatalf("indexed record missing: size=%d error=%v", size, err)
		})
		reportTestFailure(t, sizes[key] != strconv.FormatInt(size, 10), func() {
			t.Fatalf("incorrect byte ledger for %q: got=%s want=%d", key, sizes[key], size)
		})
		total += size
	}
	reportTestFailure(t, sizes["_total"] != strconv.FormatInt(total, 10) || total > r.options.MaxBytes, func() {
		t.Fatalf("incorrect/beyond-limit total: got=%s actual=%d limit=%d", sizes["_total"], total, r.options.MaxBytes)
	})
	for _, key := range []string{r.indexKey(), r.sizeKey()} {
		ttl, err := r.client.PTTL(ctx, key).Result()
		reportTestFailure(t, err != nil || ttl <= 0 || ttl > r.options.TTL, func() {
			t.Fatalf("unbounded accounting TTL: ttl=%v error=%v", ttl, err)
		})
	}
}

func TestRedisCapacityIndexContract(t *testing.T) {
	runRedisCapacityIndexContract(t, func(t *testing.T, opts Options) fixture { return newFixture(t, "redis", opts) })
}

func runRedisCapacityIndexContract(t *testing.T, makeFixture fixtureFactory) {
	f := makeFixture(t, Options{MaxEntries: 2, TTL: time.Hour})
	ctx := context.Background()
	requireStatus(t, f.store.RecordCompleted(ctx, sample("one")), Hit)
	requireRedisAccounting(t, f.redis, "one")
	f.advance(30 * time.Minute)
	requireStatus(t, f.store.RecordCompleted(ctx, sample("two")), Hit)
	requireRedisAccounting(t, f.redis, "one", "two")
	requireStatus(t, f.store.RecordCompleted(ctx, sample("three")), Unavailable)
	requireRedisAccounting(t, f.redis, "one", "two")
	// Only one record expires; the live record keeps index and size metadata
	// alive. The next write must prune the expired member and reclaim bytes.
	f.advance(31 * time.Minute)
	_, err := f.store.Resolve(ctx, 11, "one")
	requireStatus(t, err, Miss)
	requireStatus(t, f.store.RecordCompleted(ctx, sample("three")), Hit)
	requireRedisAccounting(t, f.redis, "two", "three")
	for _, id := range []string{"two", "three"} {
		stored, err := f.store.Resolve(ctx, 11, id)
		requireStatus(t, err, Hit)
		requireStatus(t, f.store.Invalidate(ctx, invalidation(*stored)), Hit)
	}
	requireRedisAccounting(t, f.redis)
}

// This harness also runs against miniredis locally, without setting an env var,
// so the real-Redis fixture/TTL commands/cleanup are tested even when opt-in is off.
func runRedisIntegrationContract(t *testing.T, url string) {
	makeFixture := func(t *testing.T, opts Options) fixture { return newRedisFixture(t, url, opts) }
	probe := makeFixture(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), probe.redis.options.CommitTimeout)
	err := probe.redis.client.Ping(ctx).Err()
	cancel()
	if err != nil {
		t.Fatalf("configured Redis is unavailable (integration contracts cannot be skipped): %v", err)
	}
	runStoreContract(t, makeFixture)
	t.Run("catalog", func(t *testing.T) { runCatalogContract(t, makeFixture) })
	t.Run("namespace_fingerprint_and_server_ttl", func(t *testing.T) { runRedisNamespaceContract(t, makeFixture) })
	t.Run("corruption_oversize_and_schema", func(t *testing.T) { runRedisCorruptionContract(t, makeFixture) })
	t.Run("capacity_index", func(t *testing.T) { runRedisCapacityIndexContract(t, makeFixture) })
	t.Run("rejected_first_write", func(t *testing.T) { runRedisRejectedWriteContract(t, makeFixture) })
}

func TestRedisIntegrationHarness(t *testing.T) {
	server := miniredis.RunT(t)
	const sentinel = "unrelated:integration-harness-sentinel"
	if err := server.Set(sentinel, "must-survive"); err != nil {
		t.Fatal(err)
	}
	t.Run("contracts", func(t *testing.T) { runRedisIntegrationContract(t, "redis://"+server.Addr()) })
	keys := server.Keys()
	if len(keys) != 1 || keys[0] != sentinel {
		t.Fatalf("cleanup leaked test keys or touched unrelated data: %v", keys)
	}
	if value, err := server.Get(sentinel); err != nil || value != "must-survive" {
		t.Fatalf("cleanup changed unrelated data: value=%q error=%v", value, err)
	}
}

func TestRecordHardSizeLimitAndDefaults(t *testing.T) {
	o := Options{MaxRecordBytes: MaxRecordBytes * 2}.normalized()
	if o.MaxRecordBytes != MaxRecordBytes || o.MaxBytes != 64<<20 || o.MaxEntries != 10000 || o.TTL != 4*time.Hour || o.CommitTimeout != 100*time.Millisecond {
		t.Fatalf("defaults=%+v", o)
	}
	r := sample("large")
	r.OpaqueState = make([]byte, MaxRecordBytes)
	_, _, err := prepare(r, nil, o, time.Now())
	requireStatus(t, err, Invalid)
}

func (f fixture) catalogRemainingTTL(t *testing.T, name string) time.Duration {
	t.Helper()
	key, err := catalogKey(name)
	if err != nil {
		t.Fatal(err)
	}
	if f.memory != nil {
		f.memory.mu.Lock()
		defer f.memory.mu.Unlock()
		return f.memory.entries[key].expires.Sub(f.memory.now())
	}
	ttl, err := f.redis.client.PTTL(context.Background(), f.redis.prefix+key).Result()
	if err != nil {
		t.Fatal(err)
	}
	return ttl
}

func (f fixture) requireCatalogAccounting(t *testing.T, name string, withLease bool) {
	t.Helper()
	var keys []string
	if name != "" {
		key, _ := catalogKey(name)
		keys = append(keys, key)
		if withLease {
			lease, _ := leaseKey(name)
			keys = append(keys, lease)
		}
	}
	if f.memory != nil {
		f.requireMemoryCatalogAccounting(t, keys)
		return
	}
	r := f.redis
	ctx, cancel := context.WithTimeout(context.Background(), r.options.CommitTimeout)
	defer cancel()
	if len(keys) == 0 {
		all, err := redisNamespaceKeys(ctx, r)
		reportTestFailure(t, err != nil || len(all) != 0, func() {
			t.Fatalf("empty catalog namespace retained keys: %v (%v)", all, err)
		})
		return
	}
	members, err := r.client.ZRange(ctx, r.indexKey(), 0, -1).Result()
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	sizes, err := r.client.HGetAll(ctx, r.sizeKey()).Result()
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	reportTestFailure(t, len(members) != len(keys) || len(members) > r.options.MaxEntries || len(sizes) != len(keys)+1, func() {
		t.Fatalf("catalog index sizes: entries=%d ledger=%d want=%d", len(members), len(sizes), len(keys))
	})
	var total int64
	for _, key := range keys {
		key = r.prefix + key
		{
			_, err := r.client.ZScore(ctx, r.indexKey(), key).Result()
			reportTestFailure(t, err != nil, func() {
				t.Fatalf("missing expiry entry: %v", err)
			})
		}
		size, err := r.client.StrLen(ctx, key).Result()
		reportTestFailure(t, err != nil || sizes[key] != strconv.FormatInt(size, 10), func() {
			t.Fatalf("catalog ledger=%s actual=%d err=%v", sizes[key], size, err)
		})
		total += size
	}
	reportTestFailure(t, sizes["_total"] != strconv.FormatInt(total, 10) || total > r.options.MaxBytes, func() {
		t.Fatalf("catalog total=%s actual=%d limit=%d", sizes["_total"], total, r.options.MaxBytes)
	})
	for _, key := range []string{r.indexKey(), r.sizeKey()} {
		ttl, err := r.client.PTTL(ctx, key).Result()
		reportTestFailure(t, err != nil || ttl <= 0 || ttl > DefaultCatalogTTL, func() {
			t.Fatalf("catalog index has unbounded TTL: %v (%v)", ttl, err)
		})
	}
	all, err := redisNamespaceKeys(ctx, r)
	reportTestFailure(t, err != nil || len(all) != len(keys)+2, func() {
		t.Fatalf("catalog leaked auxiliary keys: keys=%v err=%v", all, err)
	})
}

func runCatalogCorruptionContract(t *testing.T, makeFixture fixtureFactory) {
	f := makeFixture(t, Options{})
	cache := f.store.(CatalogCache)
	ctx := context.Background()
	const name = "catalog-corruption"
	key, _ := catalogKey(name)
	for _, tc := range []struct {
		name    string
		payload []byte
	}{{"empty", nil}, {"bad_json", []byte(`{"unfinished":`)}, {"oversize", catalogPayload(MaxCatalogBytes + 1)}} {
		t.Run(tc.name, func(t *testing.T) { checkCorruptCatalogPayload(t, f, cache, ctx, name, key, tc.payload) })
	}
	t.Run("malformed_lease", func(t *testing.T) {
		g := makeFixture(t, Options{})
		other := g.store.(CatalogCache)
		token := acquireCatalogLease(t, other, name)
		lease, _ := leaseKey(name)
		if g.memory != nil {
			g.memory.mu.Lock()
			g.memory.put(lease, []byte("broken"), g.memory.now().Add(5*time.Second))
			g.memory.mu.Unlock()
		} else {
			err := g.redis.client.Set(ctx, g.redis.prefix+lease, "broken", 5*time.Second).Err()
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		requireStatus(t, other.CommitCatalog(ctx, name, token, []byte(`[]`), 0), Invalid)
		_, err := other.GetCatalog(ctx, name)
		requireStatus(t, err, Miss)
	})
	if f.redis == nil {
		return
	}
	t.Run("wrong_type_and_no_expiry", func(t *testing.T) {
		rawKey := f.redis.prefix + key
		{
			err := f.redis.client.Del(ctx, rawKey).Err()
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		{
			err := f.redis.client.LPush(ctx, rawKey, "not-a-catalog").Err()
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		_, err := cache.GetCatalog(ctx, name)
		requireStatus(t, err, Invalid)
		{
			err := f.redis.client.Set(ctx, rawKey, `[]`, 0).Err()
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		_, err = cache.GetCatalog(ctx, name)
		requireStatus(t, err, Invalid)
	})
	for _, corruption := range []struct{ field, value string }{
		{"_total", "broken"}, {"_total", "-1"}, {"_total", "0"},
		{"catalog", "broken"}, {"catalog", "-1"},
	} {
		t.Run("ledger_"+corruption.field+"_"+corruption.value, func(t *testing.T) {
			g := makeFixture(t, Options{})
			token := acquireCatalogLease(t, g.redis, name)
			requireStatus(t, g.redis.CommitCatalog(ctx, name, token, []byte(`{"models":[]}`), 0), Hit)
			token = acquireCatalogLease(t, g.redis, name)
			field := corruption.field
			if field == "catalog" {
				field = g.redis.prefix + key
			}
			{
				err := g.redis.client.HSet(ctx, g.redis.sizeKey(), field, corruption.value).Err()
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			requireStatus(t, g.redis.CommitCatalog(ctx, name, token, []byte(`[]`), 0), Invalid)
			got, err := g.redis.GetCatalog(ctx, name)
			requireStatus(t, err, Hit)
			reportTestFailure(t, string(got) != `{"models":[]}`, func() {
				t.Fatal("corrupt ledger allowed catalog overwrite")
			})
			lease, _ := leaseKey(name)
			owner, err := g.redis.client.Get(ctx, g.redis.prefix+lease).Result()
			reportTestFailure(t, err != nil || owner != token, func() {
				t.Fatalf("invalid commit consumed lease: %v", err)
			})
		})
	}
	runCatalogMetadataCorruption(t, makeFixture, ctx, name)
}

func TestRedisCatalogUnavailable(t *testing.T) {
	server := miniredis.RunT(t)
	r, err := NewRedis(RedisOptions{URL: "redis://" + server.Addr(), Options: Options{CommitTimeout: 50 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	token := acquireCatalogLease(t, r, "catalog")
	server.Close()
	_, getErr := r.GetCatalog(ctx, "catalog")
	_, _, acquireErr := r.AcquireFillLease(ctx, "catalog", 5*time.Second)
	for _, err := range []error{getErr, acquireErr, r.CommitCatalog(ctx, "catalog", token, []byte(`[]`), 0), r.ReleaseFillLease(ctx, "catalog", token)} {
		requireStatus(t, err, Unavailable)
	}
}

func runCatalogNamespaceContract(t *testing.T, makeFixture fixtureFactory) {
	f := makeFixture(t, Options{Namespace: "catalog:namespace"})
	var other fixture
	if f.redis != nil {
		other = newRedisNamespaceFixture(t, f.redis.client, Options{Namespace: "catalog:namespace"})
	} else {
		other = makeFixture(t, Options{Namespace: "catalog:other-namespace"})
	}
	cache, second := f.store.(CatalogCache), other.store.(CatalogCache)
	ctx := context.Background()
	const name = "model-catalog-v1:private-revision-and-account-set"
	catalog, _ := catalogKey(name)
	lease, _ := leaseKey(name)
	reportTestFailure(t, catalog == lease || catalog == responseKey(name) || Fingerprint("catalog", name) == Fingerprint("lease", name), func() {
		t.Fatal("catalog, lease, and response key domains overlap")
	})
	token := acquireCatalogLease(t, cache, name)
	secondToken := acquireCatalogLease(t, second, name)
	reportTestFailure(t, token == secondToken, func() {
		t.Fatal("namespaces share lease tokens")
	})
	requireStatus(t, second.CommitCatalog(ctx, name, token, []byte(`[]`), 0), WrongOwner)
	requireStatus(t, cache.ReleaseFillLease(ctx, name, secondToken), WrongOwner)
	requireStatus(t, cache.CommitCatalog(ctx, name, token, []byte(`{"models":["first"]}`), 0), Hit)
	requireStatus(t, second.CommitCatalog(ctx, name, secondToken, []byte(`{"models":["second"]}`), 0), Hit)
	requireStatus(t, f.store.RecordCompleted(ctx, sample(name)), Hit)
	for _, tc := range []struct {
		cache CatalogCache
		want  string
	}{{cache, `{"models":["first"]}`}, {second, `{"models":["second"]}`}} {
		got, err := tc.cache.GetCatalog(ctx, name)
		requireStatus(t, err, Hit)
		reportTestFailure(t, string(got) != tc.want, func() {
			t.Fatal("catalog escaped namespace or shared response storage")
		})
		for _, changed := range []string{name + ":new-revision", name + ":new-account-set"} {
			_, err := tc.cache.GetCatalog(ctx, changed)
			requireStatus(t, err, Miss)
		}
	}
	if f.memory != nil {
		return
	}
	keys, err := redisNamespaceKeys(ctx, f.redis)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	for _, key := range keys {
		reportTestFailure(t, strings.Contains(key, name) || !strings.HasPrefix(key, f.redis.prefix), func() {
			t.Fatal("catalog key leaked raw identity or escaped namespace")
		})
	}
	raw, err := f.redis.client.Get(ctx, f.redis.prefix+catalog).Result()
	reportTestFailure(t, err != nil || raw != `{"models":["first"]}`, func() {
		t.Fatalf("catalog stored data other than caller's JSON: err=%v", err)
	})
}

func (f fixture) requireMemoryCatalogAccounting(t *testing.T, keys []string) {
	t.Helper()
	m := f.memory
	m.mu.Lock()
	defer m.mu.Unlock()
	reportTestFailure(t, len(m.entries) != len(keys) || len(m.entries) > m.options.MaxEntries, func() {
		t.Fatalf("memory entries=%d, expected %d", len(m.entries), len(keys))
	})
	var total int64
	for _, key := range keys {
		entry, ok := m.entries[key]
		reportTestFailure(t, !ok || !m.now().Before(entry.expires), func() {
			t.Fatalf("missing/expired entry %q", key)
		})
		total += int64(len(entry.payload))
	}
	reportTestFailure(t, m.bytes != total || total > m.options.MaxBytes, func() {
		t.Fatalf("memory bytes=%d actual=%d limit=%d", m.bytes, total, m.options.MaxBytes)
	})
	return
}

func checkCorruptCatalogPayload(t *testing.T, f fixture, cache CatalogCache, ctx context.Context, name, key string, payload []byte) {
	if f.memory != nil {
		f.memory.mu.Lock()
		f.memory.put(key, payload, f.memory.now().Add(DefaultCatalogTTL))
		f.memory.mu.Unlock()
	} else {
		err := f.redis.client.Set(ctx, f.redis.prefix+key, payload, DefaultCatalogTTL).Err()
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	got, err := cache.GetCatalog(ctx, name)
	requireStatus(t, err, Invalid)
	reportTestFailure(t, got != nil, func() {
		t.Fatal("corrupt catalog exposed to caller")
	})
	if f.redis != nil && len(payload) > MaxCatalogBytes {
		result, err := catalogReadScript.Run(ctx, f.redis.client, []string{f.redis.prefix + key}, MaxCatalogBytes).Slice()
		reportTestFailure(t, err != nil || len(result) != 2 || result[0] != int64(3) || result[1] != "", func() {
			t.Fatalf("oversize catalog returned bytes across Redis transport: %v", err)
		})
	}
}

func runCatalogMetadataCorruption(t *testing.T, makeFixture fixtureFactory, ctx context.Context, name string) {
	for _, target := range []string{"lease_type", "lease_no_expiry", "expiry_type", "sizes_type"} {
		t.Run(target, func(t *testing.T) {
			g := makeFixture(t, Options{})
			token := acquireCatalogLease(t, g.redis, name)
			lease, _ := leaseKey(name)
			key := g.redis.prefix + lease
			if target == "lease_no_expiry" {
				{
					err := g.redis.client.Persist(ctx, key).Err()
					reportTestFailure(t, err != nil, func() {
						t.Fatal(err)
					})
				}
			} else {
				if target == "expiry_type" {
					key = g.redis.indexKey()
				} else if target == "sizes_type" {
					key = g.redis.sizeKey()
				}
				{
					err := g.redis.client.Del(ctx, key).Err()
					reportTestFailure(t, err != nil, func() {
						t.Fatal(err)
					})
				}
				{
					err := g.redis.client.LPush(ctx, key, "broken").Err()
					reportTestFailure(t, err != nil, func() {
						t.Fatal(err)
					})
				}
			}
			requireStatus(t, g.redis.CommitCatalog(ctx, name, token, []byte(`[]`), 0), Invalid)
			_, err := g.redis.GetCatalog(ctx, name)
			requireStatus(t, err, Miss)
		})
	}
}
