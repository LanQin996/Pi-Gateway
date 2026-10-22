package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// MaxCatalogBytes is independent of the much smaller continuation record limit.
	MaxCatalogBytes   = 1 << 20
	DefaultCatalogTTL = 30 * time.Second
)

// Affinity is merely a routing hint; callers still enforce account/key access
// and provider credential validity. IdentityHash binds cache entries to a revision.
type AffinityKey struct {
	KeyID        int64
	SessionID    string
	Provider     string
	IdentityHash string
}

type AffinityStore interface {
	GetAffinity(context.Context, AffinityKey) (int64, error)
	SetAffinity(context.Context, AffinityKey, int64, time.Duration) error
}

type FillLeaser interface {
	AcquireFillLease(context.Context, string, time.Duration) (string, bool, error)
	ReleaseFillLease(context.Context, string, string) error
}

// CatalogCache stores only caller-supplied, credential-free catalog JSON. Use
// the same name for reads, fill leases, and commits. Commit requires the current
// unexpired lease token and consumes it on success; a later release may miss.
// A zero catalog TTL defaults to 30 seconds; negative/sub-millisecond TTLs are
// invalid. Catalogs, leases, affinity hints, and records share one capacity budget.
// Callers use a five-second fill lease for model catalog refreshes.
type CatalogCache interface {
	FillLeaser
	GetCatalog(context.Context, string) ([]byte, error)
	CommitCatalog(context.Context, string, string, []byte, time.Duration) error
}

func catalogKey(name string) (string, error) {
	if name == "" || len(name) > 4096 {
		return "", ErrInvalid
	}
	return "catalog:" + Fingerprint("catalog", name), nil
}

func catalogTTL(ttl time.Duration) (time.Duration, error) {
	if ttl == 0 {
		return DefaultCatalogTTL, nil
	}
	if ttl < time.Millisecond {
		return 0, ErrInvalid
	}
	return ttl.Truncate(time.Millisecond), nil
}

func validCatalog(payload []byte) bool {
	return len(payload) > 0 && len(payload) <= MaxCatalogBytes && json.Valid(payload)
}

func (m *Memory) GetCatalog(ctx context.Context, name string) ([]byte, error) {
	key, err := catalogKey(name)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ready(ctx); err != nil {
		return nil, err
	}
	if m.entries == nil || m.now == nil {
		return nil, ErrUnavailable
	}
	entry, ok := m.get(key, m.now())
	if !ok {
		return nil, ErrMiss
	}
	if !validCatalog(entry.payload) {
		return nil, ErrInvalid
	}
	return append([]byte(nil), entry.payload...), nil
}

func (m *Memory) CommitCatalog(ctx context.Context, name, token string, payload []byte, ttl time.Duration) error {
	key, err := catalogKey(name)
	if err != nil || len(token) != 64 {
		return ErrInvalid
	}
	ttl, err = catalogTTL(ttl)
	if err != nil {
		return err
	}
	if m == nil {
		return ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ready(ctx); err != nil {
		return err
	}
	if m.entries == nil || m.now == nil {
		return ErrUnavailable
	}
	if !validCatalog(payload) {
		return ErrInvalid
	}
	lease, _ := leaseKey(name)
	now := m.now()
	owner, ok := m.get(lease, now)
	if !ok {
		return ErrMiss
	}
	if len(owner.payload) != 64 {
		return ErrInvalid
	}
	if string(owner.payload) != token {
		return ErrWrongOwner
	}
	// The lease's entry and bytes can be replaced by the catalog atomically.
	// Restore the original lease (including its deadline) if capacity rejects it.
	m.remove(lease)
	if !m.room(key, len(payload), now) {
		m.put(lease, owner.payload, owner.expires)
		return ErrUnavailable
	}
	m.put(key, append([]byte(nil), payload...), now.Add(ttl))
	return nil
}

// Both the type/length checks and GET happen on the server, so a corrupt value
// larger than the catalog limit is never transferred to the process.
var catalogReadScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return {0, ''} end
if redis.call('TYPE', KEYS[1]).ok ~= 'string' then return {3, ''} end
if redis.call('STRLEN', KEYS[1]) > tonumber(ARGV[1]) then return {3, ''} end
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then return {3, ''} end
if ttl == 0 then return {0, ''} end
return {1, redis.call('GET', KEYS[1])}
`)

// KEYS are catalog, expiry, sizes, lease; all share the namespace hash slot.
// Never read the old catalog into Lua/client memory, and never mutate a value
// or its accounting until the lease has been checked in this same script.
var catalogCommitScript = redis.NewScript(`
local kind = redis.call('TYPE', KEYS[4]).ok
if kind == 'none' then return 0 end
if kind ~= 'string' or redis.call('STRLEN', KEYS[4]) ~= 64 then return 3 end
local leaseTTL = redis.call('PTTL', KEYS[4])
if leaseTTL < 0 then return 3 end
if leaseTTL == 0 then return 0 end
if redis.call('GET', KEYS[4]) ~= ARGV[1] then return 2 end
for i = 2, 3 do
  local expected = 'zset'
  if i == 3 then expected = 'hash' end
  if redis.call('TYPE', KEYS[i]).ok ~= expected then return 3 end
end
local tm = redis.call('TIME')
local now = tonumber(tm[1]) * 1000 + math.floor(tonumber(tm[2]) / 1000)
local function validSize(size)
  return size and size >= 0 and size % 1 == 0
end
local total = tonumber(redis.call('HGET', KEYS[3], '_total') or '')
local leaseSize = tonumber(redis.call('HGET', KEYS[3], KEYS[4]) or '')
local leaseExpiry = tonumber(redis.call('ZSCORE', KEYS[2], KEYS[4]) or '')
if not validSize(total) or leaseSize ~= 64 or not leaseExpiry then return 3 end
if leaseExpiry <= now then return 0 end
local expired = redis.call('ZRANGEBYSCORE', KEYS[2], '-inf', now)
for _, key in ipairs(expired) do
  local size = tonumber(redis.call('HGET', KEYS[3], key) or '')
  if not validSize(size) then return 3 end
  total = total - size
end
local oldsize = 0
local oldExpiry = tonumber(redis.call('ZSCORE', KEYS[2], KEYS[1]) or '')
local count = redis.call('ZCARD', KEYS[2]) - #expired - 1
if oldExpiry and oldExpiry > now then
  oldsize = tonumber(redis.call('HGET', KEYS[3], KEYS[1]) or '')
  if not validSize(oldsize) then return 3 end
else
  count = count + 1
end
if total < leaseSize + oldsize then return 3 end
local nextsize = total - leaseSize - oldsize + string.len(ARGV[2])
if count > tonumber(ARGV[4]) or nextsize > tonumber(ARGV[5]) then return 4 end
for _, key in ipairs(expired) do redis.call('HDEL', KEYS[3], key) end
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', now)
redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
redis.call('DEL', KEYS[4])
redis.call('ZREM', KEYS[2], KEYS[4])
redis.call('HDEL', KEYS[3], KEYS[4])
redis.call('ZADD', KEYS[2], now + tonumber(ARGV[3]), KEYS[1])
redis.call('HSET', KEYS[3], KEYS[1], string.len(ARGV[2]), '_total', nextsize)
for i = 2, 3 do
  if redis.call('PTTL', KEYS[i]) < tonumber(ARGV[3]) then redis.call('PEXPIRE', KEYS[i], ARGV[3]) end
end
return 1
`)

func (r *Redis) GetCatalog(ctx context.Context, name string) ([]byte, error) {
	key, err := catalogKey(name)
	if err != nil {
		return nil, err
	}
	if r == nil || r.client == nil {
		return nil, ErrUnavailable
	}
	ctx, cancel, err := r.context(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	values, err := catalogReadScript.Run(ctx, r.client, []string{r.prefix + key}, MaxCatalogBytes).Slice()
	if err != nil {
		return nil, unavailable(err)
	}
	if len(values) != 2 {
		return nil, ErrInvalid
	}
	status, ok := values[0].(int64)
	if !ok {
		return nil, ErrInvalid
	}
	if err := scriptStatus(status, nil); err != nil {
		return nil, err
	}
	value, ok := values[1].(string)
	if !ok {
		return nil, ErrInvalid
	}
	payload := []byte(value)
	if !validCatalog(payload) {
		return nil, ErrInvalid
	}
	return payload, nil
}

func (r *Redis) CommitCatalog(ctx context.Context, name, token string, payload []byte, ttl time.Duration) error {
	key, err := catalogKey(name)
	if err != nil || len(token) != 64 {
		return ErrInvalid
	}
	ttl, err = catalogTTL(ttl)
	if err != nil {
		return err
	}
	if r == nil || r.client == nil {
		return ErrUnavailable
	}
	ctx, cancel, err := r.context(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	if !validCatalog(payload) {
		return ErrInvalid
	}
	lease, _ := leaseKey(name)
	keys := append(r.keys(r.prefix+key), r.prefix+lease)
	n, err := catalogCommitScript.Run(ctx, r.client, keys, token, payload, ttl.Milliseconds(), r.options.MaxEntries, r.options.MaxBytes).Int64()
	return scriptStatus(n, err)
}

func affinityKey(k AffinityKey) (string, error) {
	if k.KeyID <= 0 || k.SessionID == "" || k.Provider == "" || k.IdentityHash == "" || len(k.SessionID) > 4096 || len(k.Provider) > 4096 || len(k.IdentityHash) > 4096 {
		return "", ErrInvalid
	}
	raw, _ := json.Marshal(k)
	return "affinity:" + Fingerprint("affinity", string(raw)), nil
}

func leaseKey(key string) (string, error) {
	if key == "" || len(key) > 4096 {
		return "", ErrInvalid
	}
	return "lease:" + Fingerprint("lease", key), nil
}

func cacheTTL(ttl, maximum time.Duration) time.Duration {
	if ttl < time.Millisecond || ttl > maximum {
		return maximum
	}
	return ttl
}

func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", ErrUnavailable
	}
	return hex.EncodeToString(b[:]), nil
}

func (m *Memory) GetAffinity(ctx context.Context, k AffinityKey) (int64, error) {
	key, err := affinityKey(k)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ready(ctx); err != nil {
		return 0, err
	}
	entry, ok := m.get(key, m.now())
	if !ok {
		return 0, ErrMiss
	}
	id, err := strconv.ParseInt(string(entry.payload), 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrInvalid
	}
	return id, nil
}

func (m *Memory) SetAffinity(ctx context.Context, k AffinityKey, accountID int64, ttl time.Duration) error {
	key, err := affinityKey(k)
	if err != nil {
		return err
	}
	if accountID <= 0 {
		return ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ready(ctx); err != nil {
		return err
	}
	payload := []byte(strconv.FormatInt(accountID, 10))
	now := m.now()
	if !m.room(key, len(payload), now) {
		return ErrUnavailable
	}
	m.put(key, payload, now.Add(cacheTTL(ttl, m.options.TTL)))
	return nil
}

func (m *Memory) AcquireFillLease(ctx context.Context, name string, ttl time.Duration) (string, bool, error) {
	key, err := leaseKey(name)
	if err != nil {
		return "", false, err
	}
	if m == nil {
		return "", false, ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ready(ctx); err != nil {
		return "", false, err
	}
	if m.entries == nil || m.now == nil {
		return "", false, ErrUnavailable
	}
	now := m.now()
	if _, ok := m.get(key, now); ok {
		return "", false, nil
	}
	token, err := newToken()
	if err != nil {
		return "", false, err
	}
	if !m.room(key, len(token), now) {
		return "", false, ErrUnavailable
	}
	m.put(key, []byte(token), now.Add(cacheTTL(ttl, time.Minute)))
	return token, true, nil
}

func (m *Memory) ReleaseFillLease(ctx context.Context, name, token string) error {
	key, err := leaseKey(name)
	if err != nil || len(token) != 64 {
		return ErrInvalid
	}
	if m == nil {
		return ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ready(ctx); err != nil {
		return err
	}
	if m.entries == nil || m.now == nil {
		return ErrUnavailable
	}
	entry, ok := m.get(key, m.now())
	if !ok {
		return ErrMiss
	}
	if string(entry.payload) != token {
		return ErrWrongOwner
	}
	m.remove(key)
	return nil
}

func (r *Redis) GetAffinity(ctx context.Context, k AffinityKey) (int64, error) {
	key, err := affinityKey(k)
	if err != nil {
		return 0, err
	}
	ctx, cancel, err := r.context(ctx)
	if err != nil {
		return 0, err
	}
	defer cancel()
	payload, err := r.read(ctx, r.prefix+key)
	if err != nil {
		return 0, err
	}
	id, err := strconv.ParseInt(string(payload), 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrInvalid
	}
	return id, nil
}

func (r *Redis) SetAffinity(ctx context.Context, k AffinityKey, accountID int64, ttl time.Duration) error {
	key, err := affinityKey(k)
	if err != nil {
		return err
	}
	if accountID <= 0 {
		return ErrInvalid
	}
	ctx, cancel, err := r.context(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	key = r.prefix + key
	old, err := r.read(ctx, key)
	if err != nil && err != ErrMiss {
		return err
	}
	n, err := writeScript.Run(ctx, r.client, r.keys(key), string(old), strconv.FormatInt(accountID, 10), cacheTTL(ttl, r.options.TTL).Milliseconds(), r.options.MaxEntries, r.options.MaxBytes).Int64()
	return scriptStatus(n, err)
}

func (r *Redis) AcquireFillLease(ctx context.Context, name string, ttl time.Duration) (string, bool, error) {
	key, err := leaseKey(name)
	if err != nil {
		return "", false, err
	}
	if r == nil || r.client == nil {
		return "", false, ErrUnavailable
	}
	ctx, cancel, err := r.context(ctx)
	if err != nil {
		return "", false, err
	}
	defer cancel()
	token, err := newToken()
	if err != nil {
		return "", false, err
	}
	key = r.prefix + key
	n, err := writeScript.Run(ctx, r.client, r.keys(key), "", token, cacheTTL(ttl, time.Minute).Milliseconds(), r.options.MaxEntries, r.options.MaxBytes).Int64()
	if err == nil && n == 3 {
		return "", false, nil
	}
	if err := scriptStatus(n, err); err != nil {
		return "", false, err
	}
	return token, true, nil
}

func (r *Redis) ReleaseFillLease(ctx context.Context, name, token string) error {
	key, err := leaseKey(name)
	if err != nil || len(token) != 64 {
		return ErrInvalid
	}
	if r == nil || r.client == nil {
		return ErrUnavailable
	}
	ctx, cancel, err := r.context(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	n, err := deleteScript.Run(ctx, r.client, r.keys(r.prefix+key), token).Int64()
	if err == nil && n == 3 {
		return ErrWrongOwner
	}
	return scriptStatus(n, err)
}

var _ AffinityStore = (*Memory)(nil)
var _ AffinityStore = (*Redis)(nil)
var _ FillLeaser = (*Memory)(nil)
var _ FillLeaser = (*Redis)(nil)
var _ CatalogCache = (*Memory)(nil)
var _ CatalogCache = (*Redis)(nil)
