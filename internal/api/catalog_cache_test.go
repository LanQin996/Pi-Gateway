package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"pi-gateway/internal/session"
	"pi-gateway/internal/store"
)

// These backends are deliberately distinct: memory never opens a Redis server,
// and miniredis exercises session.Redis's actual protocol and Lua scripts. No
// external Redis or PostgreSQL service is implied by this local matrix.
type catalogCacheFixture struct {
	backend session.CatalogCache
	memory  *session.Memory
	redis   *session.Redis
	mini    *miniredis.Miniredis
}

func newCatalogCacheFixture(tb testing.TB, kind string, options session.Options) *catalogCacheFixture {
	tb.Helper()
	if options.CommitTimeout == 0 {
		options.CommitTimeout = time.Second
	}
	options.Namespace = "api-catalog-" + session.Fingerprint("test", tb.Name())
	f := &catalogCacheFixture{}
	switch kind {
	case "memory":
		f.memory = session.NewMemory(options)
		f.backend = f.memory
		tb.Cleanup(func() { _ = f.memory.Close() })
	case "miniredis":
		var err error
		f.mini, err = miniredis.Run()
		if err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(f.mini.Close)
		f.redis, err = session.NewRedis(session.RedisOptions{URL: "redis://" + f.mini.Addr(), Options: options})
		if err != nil {
			tb.Fatal(err)
		}
		f.backend = f.redis
		tb.Cleanup(func() { _ = f.redis.Close() })
	default:
		tb.Fatalf("unknown local catalog backend %q", kind)
	}
	return f
}

func newCatalogCacheHarness(t *testing.T, backend session.CatalogCache, ttl, timeout time.Duration) *testHarness {
	t.Helper()
	var upstreamCalls atomic.Uint64
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		http.Error(w, "catalog reads must never contact upstream", http.StatusBadGateway)
	}), "sse")
	// Install exactly once, before any request can read the Server field.
	h.dataPlane.catalogCache = newModelCatalogCache(backend, ttl, timeout, discardLogger())
	t.Cleanup(func() {
		{
			calls := upstreamCalls.Load()
			reportTestFailure(t, calls != 0, func() {
				t.Errorf("/models unexpectedly made %d upstream requests", calls)
			})
		}
	})
	return h
}

type catalogCacheCounts struct {
	hits, misses, failures, loads, fills, getNanos uint64
}

func catalogCacheMetrics(cache *modelCatalogCache) catalogCacheCounts {
	return catalogCacheCounts{
		hits: cache.hits.Load(), misses: cache.misses.Load(), failures: cache.failures.Load(),
		loads: cache.loads.Load(), fills: cache.fills.Load(), getNanos: cache.getNanos.Load(),
	}
}

func catalogCacheFetch(ctx context.Context, h *testHarness, key, suffix string) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.server.URL+"/v1/models"+suffix, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := h.server.Client().Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4*session.MaxCatalogBytes))
	return resp.StatusCode, resp.Header, raw, err
}

func catalogCacheRead(t *testing.T, h *testHarness, key, suffix string) []map[string]json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	status, headers, raw, err := catalogCacheFetch(ctx, h, key, suffix)
	reportTestFailure(t, err != nil || status != http.StatusOK, func() {
		t.Fatalf("models%s: status=%d err=%v body=%s", suffix, status, err, raw)
	})
	reportTestFailure(t, headers.Get("Cache-Control") != "private, no-store", func() {
		t.Fatalf("unsafe public cache header: %q", headers.Get("Cache-Control"))
	})
	var envelope map[string]json.RawMessage
	{
		err := json.Unmarshal(raw, &envelope)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	field := "data"
	if suffix != "" {
		field = "models"
		reportTestFailure(t, len(envelope) != 1, func() {
			t.Fatalf("wrong Codex envelope: %s", raw)
		})
	} else {
		reportTestFailure(t, len(envelope) != 2 || string(envelope["object"]) != `"list"`, func() {
			t.Fatalf("wrong OpenAI envelope: %s", raw)
		})
	}
	var entries []map[string]json.RawMessage
	{
		err := json.Unmarshal(envelope[field], &entries)
		reportTestFailure(t, err != nil || entries == nil, func() {
			t.Fatalf("invalid model entries: %s err=%v", raw, err)
		})
	}
	return entries
}

func catalogCacheWantIDs(t *testing.T, h *testHarness, key string, want ...string) {
	t.Helper()
	entries := catalogCacheRead(t, h, key, "")
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		var id string
		{
			err := json.Unmarshal(entry["id"], &id)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		got = append(got, id)
	}
	want = append([]string{}, want...)
	sort.Strings(want)
	reportTestFailure(t, !reflect.DeepEqual(got, want), func() {
		t.Fatalf("models=%v want=%v", got, want)
	})
}

func catalogCacheWantUnauthorized(t *testing.T, h *testHarness, key string) {
	t.Helper()
	before := catalogCacheMetrics(h.dataPlane.catalogCache)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	status, _, raw, err := catalogCacheFetch(ctx, h, key, "")
	reportTestFailure(t, err != nil || status != http.StatusUnauthorized, func() {
		t.Fatalf("rejected credential status=%d err=%v body=%s", status, err, raw)
	})
	{
		after := catalogCacheMetrics(h.dataPlane.catalogCache)
		reportTestFailure(t, after != before, func() {
			t.Fatalf("unauthorized request reached catalog cache: before=%+v after=%+v", before, after)
		})
	}
}

func catalogCacheRevision(t testing.TB, st *store.Store) string {
	t.Helper()
	revision, err := st.CatalogRevision(context.Background())
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	return revision
}

func catalogCacheSave(t testing.TB, st *store.Store, id, fetched int64, models ...store.CatalogModel) {
	t.Helper()
	{
		err := st.SaveAccountModelCatalog(context.Background(), id, &store.ModelCatalog{
			Models: models, FetchedAt: fetched, AttemptedAt: fetched,
		})
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
}

func catalogCacheCommit(t testing.TB, backend session.CatalogCache, name string, raw []byte, ttl time.Duration) {
	t.Helper()
	ctx := context.Background()
	token, owned, err := backend.AcquireFillLease(ctx, name, 5*time.Second)
	reportTestFailure(t, err != nil || !owned, func() {
		t.Fatalf("acquire test fill lease: owned=%v err=%v", owned, err)
	})
	{
		err := backend.CommitCatalog(ctx, name, token, raw, ttl)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
}

func TestCatalogCacheWarmLoadsAndRevisionInvalidation(t *testing.T) {
	for _, kind := range []string{"memory", "miniredis"} {
		t.Run(kind, func(t *testing.T) {
			f := newCatalogCacheFixture(t, kind, session.Options{})
			h := newCatalogCacheHarness(t, f.backend, time.Hour, time.Second)
			ctx := context.Background()
			anchor := &store.Account{Name: "remaining account", Enabled: true, Status: store.AccountStatusReady}
			{
				err := h.store.CreateAccount(ctx, anchor)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			catalogCacheSave(t, h.store, anchor.ID, 100, store.CatalogModel{ID: "anchor"})
			catalogCacheSave(t, h.store, h.accountID, 100, store.CatalogModel{ID: "old"})
			cache := h.dataPlane.catalogCache
			catalogCacheWantIDs(t, h, h.key, "anchor", "old")
			first := catalogCacheMetrics(cache)
			reportTestFailure(t, first.loads != 1 || first.misses != 1 || first.fills != 1 || first.hits != 0 || first.failures != 0, func() {
				t.Fatalf("cold metrics=%+v", first)
			})
			catalogCacheWantIDs(t, h, h.key, "anchor", "old")
			{
				got := catalogCacheMetrics(cache)
				reportTestFailure(t, got.loads != first.loads || got.hits != first.hits+1 || got.fills != first.fills || got.getNanos < first.getNanos, func() {
					t.Fatalf("second request did not avoid a catalog DB load: %+v", got)
				})
			}

			changes := []struct {
				name string
				edit func() error
				want []string
			}{
				{"same_fetched_at", func() error {
					return h.store.SaveAccountModelCatalog(ctx, h.accountID, &store.ModelCatalog{Models: []store.CatalogModel{{ID: "new"}}, FetchedAt: 100, AttemptedAt: 100})
				}, []string{"anchor", "new"}},
				{"supplemental_add", func() error {
					return h.store.PatchAccountManagementFields(ctx, h.accountID, store.AccountManagementPatch{SupplementalModels: []string{"manual-a"}})
				}, []string{"anchor", "manual-a", "new"}},
				{"supplemental_replace", func() error {
					return h.store.PatchAccountManagementFields(ctx, h.accountID, store.AccountManagementPatch{SupplementalModels: []string{"manual-b"}})
				}, []string{"anchor", "manual-b", "new"}},
				{"supplemental_delete", func() error {
					return h.store.PatchAccountManagementFields(ctx, h.accountID, store.AccountManagementPatch{SupplementalModels: []string{}})
				}, []string{"anchor", "new"}},
				{"account_delete", func() error { return h.store.DeleteAccount(ctx, h.accountID) }, []string{"anchor"}},
			}
			for _, change := range changes {
				t.Run(change.name, func(t *testing.T) {
					previous := catalogCacheRevision(t, h.store)
					oldName := catalogCacheName(previous, []int64{h.accountID, anchor.ID})
					before := catalogCacheMetrics(cache)
					{
						err := change.edit()
						reportTestFailure(t, err != nil, func() {
							t.Fatal(err)
						})
					}
					reportTestFailure(t, catalogCacheRevision(t, h.store) == previous, func() {
						t.Fatal("committed mutation did not change catalog revision")
					})
					{
						_, err := f.backend.GetCatalog(ctx, oldName)
						reportTestFailure(t, err != nil, func() {
							t.Fatalf("old entry was not still warm; invalidation was not exercised: %v", err)
						})
					}
					catalogCacheWantIDs(t, h, h.key, change.want...)
					{
						after := catalogCacheMetrics(cache)
						reportTestFailure(t, after.loads != before.loads+1 || after.fills != before.fills+1, func() {
							t.Fatalf("revision failed to force a fresh load: before=%+v after=%+v", before, after)
						})
					}
					catalogCacheWantIDs(t, h, h.key, change.want...)
					{
						after := catalogCacheMetrics(cache)
						reportTestFailure(t, after.loads != before.loads+1 || after.hits != before.hits+1, func() {
							t.Fatalf("new revision did not become warm: %+v", after)
						})
					}
				})
			}
		})
	}
}

func TestCatalogCacheWarmAuthenticationAndAccountHealthRemainLive(t *testing.T) {
	for _, kind := range []string{"memory", "miniredis"} {
		t.Run(kind, func(t *testing.T) {
			f := newCatalogCacheFixture(t, kind, session.Options{})
			h := newCatalogCacheHarness(t, f.backend, time.Hour, time.Second)
			ctx := context.Background()
			seedModelCatalog(t, h, h.accountID, "observed")
			catalogCacheWantIDs(t, h, h.key, "observed")
			catalogCacheWantIDs(t, h, h.key, "observed")
			cache := h.dataPlane.catalogCache
			revision := catalogCacheRevision(t, h.store)
			key, err := h.store.GetKeyByValue(ctx, h.key)
			reportTestFailure(t, err != nil || key == nil, func() {
				t.Fatalf("get fixture key: %v", err)
			})
			key.Enabled = false
			{
				err := h.store.UpdateKey(ctx, key)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			catalogCacheWantUnauthorized(t, h, h.key)
			key.Enabled = true
			{
				err := h.store.UpdateKey(ctx, key)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			catalogCacheWantIDs(t, h, h.key, "observed")
			// UpdateKey intentionally does not rotate credentials; exercise the DB
			// token change directly rather than mutating the authenticated Server.
			rotated := "sk-catalog-rotated"
			{
				_, err := h.store.ExecContext(ctx, `UPDATE api_keys SET key=? WHERE id=?`, rotated, key.ID)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			catalogCacheWantUnauthorized(t, h, h.key)
			catalogCacheWantIDs(t, h, rotated, "observed")
			{
				err := h.store.UpdateAccountCredentials(ctx, h.accountID, "new-account-access", "new-account-refresh", time.Now().Add(time.Hour).UnixMilli(), store.AccountStatusReady, "")
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			catalogCacheWantIDs(t, h, rotated, "observed")

			for _, status := range []string{store.AccountStatusExpired, store.AccountStatusInvalid, store.AccountStatusBanned, "quota_exhausted"} {
				{
					err := h.store.SetAccountStatus(ctx, h.accountID, status, "private-health-diagnostic")
					reportTestFailure(t, err != nil, func() {
						t.Fatal(err)
					})
				}
				catalogCacheWantIDs(t, h, rotated)
				{
					err := h.store.SetAccountStatus(ctx, h.accountID, store.AccountStatusReady, "")
					reportTestFailure(t, err != nil, func() {
						t.Fatal(err)
					})
				}
				catalogCacheWantIDs(t, h, rotated, "observed")
			}
			for _, enabled := range []bool{false, true} {
				{
					err := h.store.PatchAccountManagementFields(ctx, h.accountID, store.AccountManagementPatch{Enabled: &enabled})
					reportTestFailure(t, err != nil, func() {
						t.Fatal(err)
					})
				}
				if enabled {
					catalogCacheWantIDs(t, h, rotated, "observed")
				} else {
					catalogCacheWantIDs(t, h, rotated)
				}
			}
			{
				_, err := h.store.ExecContext(ctx, `UPDATE accounts SET cooldown_until=? WHERE id=?`, store.NowMS()+60000, h.accountID)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			catalogCacheWantIDs(t, h, rotated)
			{
				_, err := h.store.ExecContext(ctx, `UPDATE accounts SET cooldown_until=0 WHERE id=?`, h.accountID)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			catalogCacheWantIDs(t, h, rotated, "observed")
			{
				got := catalogCacheMetrics(cache)
				reportTestFailure(t, got.loads != 1 || got.hits < 10 || catalogCacheRevision(t, h.store) != revision, func() {
					t.Fatalf("live auth/health unexpectedly depended on catalog invalidation: %+v", got)
				})
			}
		})
	}
}

func TestCatalogCacheWarmPoliciesAliasesAndKeyIsolation(t *testing.T) {
	for _, kind := range []string{"memory", "miniredis"} {
		t.Run(kind, func(t *testing.T) {
			f := newCatalogCacheFixture(t, kind, session.Options{})
			h := newCatalogCacheHarness(t, f.backend, time.Hour, time.Second)
			ctx := context.Background()
			seedModelCatalog(t, h, h.accountID, "blue", "red")
			redGroup := &store.AccountGroup{Name: "red permission", Enabled: true, DisabledModels: []string{"blue"}}
			blueGroup := &store.AccountGroup{Name: "blue permission", Enabled: true, DisabledModels: []string{"red"}}
			for _, group := range []*store.AccountGroup{redGroup, blueGroup} {
				{
					err := h.store.CreateAccountGroup(ctx, group)
					reportTestFailure(t, err != nil, func() {
						t.Fatal(err)
					})
				}
			}
			redKey := &store.APIKey{Name: "red key", Key: "sk-red", Enabled: true, GroupIDs: []int64{redGroup.ID}}
			blueKey := &store.APIKey{Name: "blue key", Key: "sk-blue", Enabled: true, GroupIDs: []int64{blueGroup.ID}}
			for _, key := range []*store.APIKey{redKey, blueKey} {
				{
					err := h.store.CreateKey(ctx, key)
					reportTestFailure(t, err != nil, func() {
						t.Fatal(err)
					})
				}
			}
			revision := catalogCacheRevision(t, h.store)
			// Both keys have identical account IDs and therefore share the same
			// public snapshot, but their permission-filtered results must differ.
			for i := 0; i < 3; i++ {
				catalogCacheWantIDs(t, h, redKey.Key, "red")
				catalogCacheWantIDs(t, h, blueKey.Key, "blue")
				catalogCacheWantIDs(t, h, h.key, "blue", "red")
			}
			{
				err := h.store.PatchAccountGroup(ctx, redGroup.ID, store.AccountGroupPatch{DisabledModels: []string{"red"}})
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			catalogCacheWantIDs(t, h, redKey.Key, "blue")
			{
				err := h.store.PatchAccountManagementFields(ctx, h.accountID, store.AccountManagementPatch{DisabledModels: []string{"blue"}})
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			catalogCacheWantIDs(t, h, redKey.Key)
			catalogCacheWantIDs(t, h, h.key, "red")
			{
				err := h.store.PatchAccountManagementFields(ctx, h.accountID, store.AccountManagementPatch{DisabledModels: []string{}})
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			no := false
			{
				err := h.store.PatchAccountGroup(ctx, redGroup.ID, store.AccountGroupPatch{Enabled: &no})
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			catalogCacheWantIDs(t, h, redKey.Key, "blue", "red")

			runtime := h.dataPlane.settings.Get()
			runtime.ModelMappings = map[string]string{"alias": "blue"}
			{
				err := h.dataPlane.settings.Set(ctx, runtime)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			catalogCacheWantIDs(t, h, blueKey.Key, "alias", "blue")
			runtime = h.dataPlane.settings.Get()
			runtime.ModelMappings = map[string]string{"alias": "red", "new-alias": "blue"}
			{
				err := h.dataPlane.settings.Set(ctx, runtime)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			catalogCacheWantIDs(t, h, blueKey.Key, "blue", "new-alias")
			catalogCacheWantIDs(t, h, h.key, "alias", "blue", "new-alias", "red")
			runtime = h.dataPlane.settings.Get()
			runtime.ModelMappings = map[string]string{}
			{
				err := h.dataPlane.settings.Set(ctx, runtime)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			catalogCacheWantIDs(t, h, h.key, "blue", "red")
			{
				got := catalogCacheMetrics(h.dataPlane.catalogCache)
				reportTestFailure(t, got.loads != 1 || got.fills != 1 || got.hits < 15 || catalogCacheRevision(t, h.store) != revision, func() {
					t.Fatalf("policy/alias result was not recomputed from the warm public snapshot: %+v", got)
				})
			}
		})
	}
}

func TestCatalogCacheWarmGroupScopeChanges(t *testing.T) {
	for _, kind := range []string{"memory", "miniredis"} {
		t.Run(kind, func(t *testing.T) {
			f := newCatalogCacheFixture(t, kind, session.Options{})
			h := newCatalogCacheHarness(t, f.backend, time.Hour, time.Second)
			ctx := context.Background()
			seedModelCatalog(t, h, h.accountID, "public")
			private := &store.Account{Name: "private", Enabled: true, Status: store.AccountStatusReady}
			{
				err := h.store.CreateAccount(ctx, private)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			seedModelCatalog(t, h, private.ID, "private")
			group := &store.AccountGroup{Name: "private group", Enabled: true, AccountIDs: []int64{private.ID}}
			other := &store.AccountGroup{Name: "other group", Enabled: true}
			for _, g := range []*store.AccountGroup{group, other} {
				{
					err := h.store.CreateAccountGroup(ctx, g)
					reportTestFailure(t, err != nil, func() {
						t.Fatal(err)
					})
				}
			}
			key := &store.APIKey{Name: "scoped", Key: "sk-scoped", Enabled: true, GroupIDs: []int64{group.ID}}
			{
				err := h.store.CreateKey(ctx, key)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			revision := catalogCacheRevision(t, h.store)
			catalogCacheWantIDs(t, h, key.Key, "private", "public")
			catalogCacheWantIDs(t, h, key.Key, "private", "public")
			{
				err := h.store.SetKeyGroups(ctx, key.ID, []int64{other.ID})
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			catalogCacheWantIDs(t, h, key.Key, "public")
			catalogCacheWantIDs(t, h, h.key, "private", "public")
			{
				err := h.store.SetKeyGroups(ctx, key.ID, []int64{group.ID})
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			catalogCacheWantIDs(t, h, key.Key, "private", "public")
			group.Enabled = false
			{
				err := h.store.UpdateAccountGroup(ctx, group)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			catalogCacheWantIDs(t, h, key.Key, "public")
			group.Enabled = true
			{
				err := h.store.UpdateAccountGroup(ctx, group)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			catalogCacheWantIDs(t, h, key.Key, "private", "public")
			{
				err := h.store.PatchAccountManagementFields(ctx, private.ID, store.AccountManagementPatch{GroupIDs: []int64{other.ID}})
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			catalogCacheWantIDs(t, h, key.Key, "public")
			{
				got := catalogCacheMetrics(h.dataPlane.catalogCache)
				reportTestFailure(t, got.loads != 2 || got.hits != 6 || catalogCacheRevision(t, h.store) != revision, func() {
					t.Fatalf("warm account sets leaked a stale group authorization: %+v", got)
				})
			}
		})
	}
}

func TestCatalogCachePublicMetadataAndReturnedValueIsolation(t *testing.T) {
	for _, kind := range []string{"memory", "miniredis"} {
		t.Run(kind, func(t *testing.T) {
			f := newCatalogCacheFixture(t, kind, session.Options{})
			h := newCatalogCacheHarness(t, f.backend, time.Hour, time.Second)
			ctx := context.Background()
			model := store.CatalogModel{ID: "rich", Name: "Rich", Metadata: map[string]json.RawMessage{
				"future_capability": json.RawMessage(`{"enabled":false,"empty":[],"nullable":null,"limit":9007199254740993}`),
				"context_window":    json.RawMessage(`9007199254740993`),
				"access_token":      json.RawMessage(`"catalog-secret-access"`),
				"refreshToken":      json.RawMessage(`"catalog-secret-refresh"`),
				"credentials":       json.RawMessage(`{"password":"catalog-secret-password"}`),
				"bearer":            json.RawMessage(`"Bearer catalog-secret-bearer"`),
				"bearer_token":      json.RawMessage(`"catalog-secret-bearer-token"`),
				"X-Api-Key":         json.RawMessage(`"catalog-secret-api-key"`),
				"x_auth_token":      json.RawMessage(`"catalog-secret-auth-token"`),
				"header":            json.RawMessage(`{"X-Api-Key":"catalog-secret-header"}`),
				"headers":           json.RawMessage(`{"Authorization":"Bearer catalog-secret-authorization"}`),
				"proxy_url":         json.RawMessage(`"http://catalog-secret-proxy:password@127.0.0.1:9"`),
				"nested":            json.RawMessage(`{"items":[{"id_token":"catalog-secret-id","supported":false,"values":[],"nullable":null}]}`),
			}}
			catalogCacheSave(t, h.store, h.accountID, 100, model)
			{
				err := h.store.SaveAccountModelCatalog(ctx, h.accountID, &store.ModelCatalog{AttemptedAt: 101, Error: "catalog-secret-diagnostic"})
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			assertPublic := func(raw []byte) {
				t.Helper()
				for _, forbidden := range []string{"catalog-secret-", "refresh-token", h.key, "Bearer "} {
					reportTestFailure(t, strings.Contains(string(raw), forbidden), func() {
						t.Errorf("public snapshot/response leaked %q", forbidden)
					})
				}
			}
			for _, suffix := range []string{"", "?client_version=0.120.0", ""} {
				entries := catalogCacheRead(t, h, h.key, suffix)
				reportTestFailure(t, len(entries) != 1 || string(entries[0]["context_window"]) != "9007199254740993", func() {
					t.Fatalf("unknown capability precision changed: %s", entries)
				})
				var future map[string]json.RawMessage
				{
					err := json.Unmarshal(entries[0]["future_capability"], &future)
					reportTestFailure(t, err != nil || string(future["enabled"]) != "false" || string(future["empty"]) != "[]" || string(future["nullable"]) != "null" || string(future["limit"]) != "9007199254740993", func() {
						t.Fatalf("unknown capability was truncated: %s err=%v", entries[0]["future_capability"], err)
					})
				}
				raw, _ := json.Marshal(entries)
				assertPublic(raw)
			}
			name := catalogCacheName(catalogCacheRevision(t, h.store), []int64{h.accountID})
			raw, err := f.backend.GetCatalog(ctx, name)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			assertPublic(raw)
			var envelope map[string]json.RawMessage
			{
				err := json.Unmarshal(raw, &envelope)
				reportTestFailure(t, err != nil || len(envelope) != 3 || envelope["schema"] == nil || envelope["revision"] == nil || envelope["accounts"] == nil, func() {
					t.Fatalf("cache is not a public-only DTO: %s err=%v", raw, err)
				})
			}
			reportTestFailure(t, strings.Contains(string(raw), `"error"`) || strings.Contains(string(raw), `"fetched_at"`) || strings.Contains(string(raw), `"attempted_at"`), func() {
				t.Fatal("private catalog envelope entered cache")
			})
			loaded, err := h.dataPlane.loadModelCatalogs(ctx, []int64{h.accountID})
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			loaded[h.accountID][0].Name = "caller mutation"
			loaded[h.accountID][0].Metadata["context_window"][0] = '0'
			loaded[h.accountID][0].Metadata["invented"] = json.RawMessage(`true`)
			delete(loaded, h.accountID)
			again, err := h.dataPlane.loadModelCatalogs(ctx, []int64{h.accountID})
			reportTestFailure(t, err != nil || again[h.accountID][0].Name != "Rich" || string(again[h.accountID][0].Metadata["context_window"]) != "9007199254740993" || again[h.accountID][0].Metadata["invented"] != nil, func() {
				t.Fatalf("returned snapshot shared mutable storage: %+v err=%v", again, err)
			})
			stored, err := h.store.GetAccountModelCatalog(ctx, h.accountID)
			reportTestFailure(t, err != nil || stored.Error != "catalog-secret-diagnostic" || !reflect.DeepEqual(stored.Models[0].Metadata, model.Metadata), func() {
				t.Fatalf("public projection mutated authoritative metadata: %+v err=%v", stored, err)
			})
			{
				got := catalogCacheMetrics(h.dataPlane.catalogCache)
				reportTestFailure(t, got.loads != 1 || got.hits != 4, func() {
					t.Fatalf("metadata checks did not exercise warm hits: %+v", got)
				})
			}
		})
	}
}

func TestCatalogCacheInvalidSnapshotsFallBackToDatabase(t *testing.T) {
	for _, kind := range []string{"memory", "miniredis"} {
		t.Run(kind, func(t *testing.T) {
			f := newCatalogCacheFixture(t, kind, session.Options{})
			h := newCatalogCacheHarness(t, f.backend, time.Hour, time.Second)
			seedModelCatalog(t, h, h.accountID, "authoritative")
			catalogCacheWantIDs(t, h, h.key, "authoritative")
			revision := catalogCacheRevision(t, h.store)
			name := catalogCacheName(revision, []int64{h.accountID})
			for _, tc := range []struct {
				name     string
				snapshot publicCatalogSnapshot
			}{
				{"schema", publicCatalogSnapshot{Schema: catalogCacheSchema + 1, Revision: revision, Accounts: map[int64][]store.CatalogModel{h.accountID: {{ID: "poison"}}}}},
				{"revision", publicCatalogSnapshot{Schema: catalogCacheSchema, Revision: "obsolete", Accounts: map[int64][]store.CatalogModel{h.accountID: {{ID: "poison"}}}}},
				{"null_accounts", publicCatalogSnapshot{Schema: catalogCacheSchema, Revision: revision}},
				{"wrong_account", publicCatalogSnapshot{Schema: catalogCacheSchema, Revision: revision, Accounts: map[int64][]store.CatalogModel{h.accountID + 99: {{ID: "poison"}}}}},
				{"empty_model_id", publicCatalogSnapshot{Schema: catalogCacheSchema, Revision: revision, Accounts: map[int64][]store.CatalogModel{h.accountID: {{ID: " "}}}}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					raw, err := json.Marshal(tc.snapshot)
					reportTestFailure(t, err != nil, func() {
						t.Fatal(err)
					})
					catalogCacheCommit(t, f.backend, name, raw, time.Hour)
					before := catalogCacheMetrics(h.dataPlane.catalogCache)
					catalogCacheWantIDs(t, h, h.key, "authoritative")
					{
						after := catalogCacheMetrics(h.dataPlane.catalogCache)
						reportTestFailure(t, after.loads != before.loads+1 || after.failures != before.failures+1 || after.fills != before.fills || after.hits != before.hits, func() {
							t.Fatalf("invalid snapshot did not fall back: before=%+v after=%+v", before, after)
						})
					}
				})
			}
		})
	}
}

func TestCatalogCacheSanitizesDecodedMetadata(t *testing.T) {
	for _, kind := range []string{"memory", "miniredis"} {
		t.Run(kind, func(t *testing.T) {
			f := newCatalogCacheFixture(t, kind, session.Options{})
			h := newCatalogCacheHarness(t, f.backend, time.Hour, time.Second)
			seedModelCatalog(t, h, h.accountID, "authoritative")
			revision := catalogCacheRevision(t, h.store)
			raw, err := json.Marshal(publicCatalogSnapshot{Schema: catalogCacheSchema, Revision: revision, Accounts: map[int64][]store.CatalogModel{
				h.accountID: {{ID: "authoritative", Metadata: map[string]json.RawMessage{
					"access_token": json.RawMessage(`"poison-access"`),
					"nested":       json.RawMessage(`{"headers":{"Authorization":"Bearer poison"},"future":false}`),
				}}},
			}})
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			catalogCacheCommit(t, f.backend, catalogCacheName(revision, []int64{h.accountID}), raw, time.Hour)
			entries := catalogCacheRead(t, h, h.key, "")
			wire, _ := json.Marshal(entries)
			reportTestFailure(t, strings.Contains(string(wire), "poison") || strings.Contains(string(wire), "Bearer") || !strings.Contains(string(wire), `"future":false`), func() {
				t.Fatalf("cached metadata was not sanitized again: %s", wire)
			})
			{
				got := catalogCacheMetrics(h.dataPlane.catalogCache)
				reportTestFailure(t, got.loads != 0 || got.hits != 1, func() {
					t.Fatalf("injected snapshot did not exercise a real backend hit: %+v", got)
				})
			}
		})
	}
}

func TestCatalogCacheUnavailableFallsBackWithoutUpstream(t *testing.T) {
	for _, kind := range []string{"memory", "miniredis"} {
		t.Run(kind, func(t *testing.T) {
			f := newCatalogCacheFixture(t, kind, session.Options{CommitTimeout: 50 * time.Millisecond})
			h := newCatalogCacheHarness(t, f.backend, time.Hour, 50*time.Millisecond)
			seedModelCatalog(t, h, h.accountID, "authoritative")
			catalogCacheWantIDs(t, h, h.key, "authoritative")
			if kind == "memory" {
				_ = f.memory.Close()
			} else {
				f.mini.Close() // Close the actual Redis transport, not a fake cache interface.
			}
			before := catalogCacheMetrics(h.dataPlane.catalogCache)
			started := time.Now()
			catalogCacheWantIDs(t, h, h.key, "authoritative")
			{
				elapsed := time.Since(started)
				reportTestFailure(t, elapsed > time.Second, func() {
					t.Fatalf("optional cache outage blocked /models for %s", elapsed)
				})
			}
			{
				after := catalogCacheMetrics(h.dataPlane.catalogCache)
				reportTestFailure(t, after.failures != before.failures+1 || after.loads != before.loads+1 || after.fills != before.fills, func() {
					t.Fatalf("outage did not degrade to DB: before=%+v after=%+v", before, after)
				})
			}
		})
	}
}

func catalogCacheRedisKey(t testing.TB, f *catalogCacheFixture) string {
	t.Helper()
	var found []string
	for _, key := range f.mini.Keys() {
		if strings.Contains(key, ":catalog:") {
			found = append(found, key)
		}
	}
	reportTestFailure(t, len(found) != 1, func() {
		t.Fatalf("expected one real Redis catalog key, got %v", found)
	})
	return found[0]
}

func TestCatalogCacheMiniredisCorruptAndOversizedValues(t *testing.T) {
	for _, corruption := range []string{"malformed_json", "oversized", "missing_ttl", "wrong_type"} {
		t.Run(corruption, func(t *testing.T) {
			f := newCatalogCacheFixture(t, "miniredis", session.Options{})
			h := newCatalogCacheHarness(t, f.backend, time.Hour, time.Second)
			seedModelCatalog(t, h, h.accountID, "authoritative")
			catalogCacheWantIDs(t, h, h.key, "authoritative")
			key := catalogCacheRedisKey(t, f)
			switch corruption {
			case "malformed_json":
				{
					err := f.mini.Set(key, "not-json")
					reportTestFailure(t, err != nil, func() {
						t.Fatal(err)
					})
				}
				f.mini.SetTTL(key, time.Hour)
			case "oversized":
				{
					err := f.mini.Set(key, `"`+strings.Repeat("x", session.MaxCatalogBytes)+`"`)
					reportTestFailure(t, err != nil, func() {
						t.Fatal(err)
					})
				}
				f.mini.SetTTL(key, time.Hour)
			case "missing_ttl":
				f.mini.Del(key)
				{
					err := f.mini.Set(key, `{}`)
					reportTestFailure(t, err != nil, func() {
						t.Fatal(err)
					})
				}
			case "wrong_type":
				f.mini.Del(key)
				{
					_, err := f.mini.Lpush(key, "not-a-string")
					reportTestFailure(t, err != nil, func() {
						t.Fatal(err)
					})
				}
				f.mini.SetTTL(key, time.Hour)
			}
			name := catalogCacheName(catalogCacheRevision(t, h.store), []int64{h.accountID})
			{
				raw, err := f.backend.GetCatalog(context.Background(), name)
				reportTestFailure(t, !errors.Is(err, session.ErrInvalid) || len(raw) != 0, func() {
					t.Fatalf("Redis failed to reject corrupt bytes before serving them: bytes=%d err=%v", len(raw), err)
				})
			}
			before := catalogCacheMetrics(h.dataPlane.catalogCache)
			catalogCacheWantIDs(t, h, h.key, "authoritative")
			{
				after := catalogCacheMetrics(h.dataPlane.catalogCache)
				reportTestFailure(t, after.failures != before.failures+1 || after.loads != before.loads+1 || after.fills != before.fills, func() {
					t.Fatalf("corrupt Redis value prevented DB fallback: %+v", after)
				})
			}
		})
	}
}

func TestCatalogCacheOversizedFillAndCapacityAreOptional(t *testing.T) {
	for _, kind := range []string{"memory", "miniredis"} {
		for _, mode := range []string{"oversized_snapshot", "capacity_rejected"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				options := session.Options{}
				model := store.CatalogModel{ID: "authoritative"}
				if mode == "oversized_snapshot" {
					model.Metadata = map[string]json.RawMessage{"future_text": json.RawMessage(`"` + strings.Repeat("x", session.MaxCatalogBytes) + `"`)}
				} else {
					options.MaxBytes = 64 // A fill lease fits, but the completed catalog does not.
				}
				f := newCatalogCacheFixture(t, kind, options)
				h := newCatalogCacheHarness(t, f.backend, time.Hour, time.Second)
				catalogCacheSave(t, h.store, h.accountID, 100, model)
				catalogCacheWantIDs(t, h, h.key, "authoritative")
				catalogCacheWantIDs(t, h, h.key, "authoritative")
				got := catalogCacheMetrics(h.dataPlane.catalogCache)
				reportTestFailure(t, got.loads != 2 || got.hits != 0 || got.fills != 0 || got.misses != 2, func() {
					t.Fatalf("uncacheable snapshot was not served from DB: %+v", got)
				})
				reportTestFailure(t, mode == "capacity_rejected" && got.failures != 2, func() {
					t.Fatalf("cache capacity failure not observed: %+v", got)
				})
				name := catalogCacheName(catalogCacheRevision(t, h.store), []int64{h.accountID})
				{
					_, err := f.backend.GetCatalog(context.Background(), name)
					reportTestFailure(t, !errors.Is(err, session.ErrMiss), func() {
						t.Fatalf("oversized/rejected catalog unexpectedly persisted: %v", err)
					})
				}
			})
		}
	}
}

func TestCatalogCacheConcurrentLeaseLosersDoNotBlockOrFill(t *testing.T) {
	for _, kind := range []string{"memory", "miniredis"} {
		t.Run(kind, func(t *testing.T) { runCatalogCacheConcurrentLeaseLosersDoNotBlockOrFillCase(t, &kind) })
	}
}

// The gate controls only scheduling; storage, ownership checks and lease release
// still run through the actual memory/miniredis backend.
type catalogCacheCommitAttempt struct {
	name, token string
	budget      time.Duration
}

type catalogCacheCommitGate struct {
	session.CatalogCache
	entered  chan catalogCacheCommitAttempt
	proceed  <-chan struct{}
	leaseTTL atomic.Int64
}

func (g *catalogCacheCommitGate) AcquireFillLease(ctx context.Context, name string, ttl time.Duration) (string, bool, error) {
	g.leaseTTL.Store(int64(ttl))
	return g.CatalogCache.AcquireFillLease(ctx, name, ttl)
}

func (g *catalogCacheCommitGate) CommitCatalog(ctx context.Context, name, token string, raw []byte, ttl time.Duration) error {
	deadline, _ := ctx.Deadline()
	g.entered <- catalogCacheCommitAttempt{name: name, token: token, budget: time.Until(deadline)}
	select {
	case <-g.proceed:
		return g.CatalogCache.CommitCatalog(ctx, name, token, raw, ttl)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestCatalogCacheCommitTimeoutStillServesDatabase(t *testing.T) {
	for _, kind := range []string{"memory", "miniredis"} {
		t.Run(kind, func(t *testing.T) {
			f := newCatalogCacheFixture(t, kind, session.Options{})
			gate := &catalogCacheCommitGate{CatalogCache: f.backend, entered: make(chan catalogCacheCommitAttempt, 1), proceed: make(chan struct{})}
			h := newCatalogCacheHarness(t, gate, time.Hour, 50*time.Millisecond)
			seedModelCatalog(t, h, h.accountID, "authoritative")
			started := time.Now()
			catalogCacheWantIDs(t, h, h.key, "authoritative")
			{
				elapsed := time.Since(started)
				reportTestFailure(t, elapsed > time.Second, func() {
					t.Fatalf("cache write timeout blocked successful DB response for %s", elapsed)
				})
			}
			var attempt catalogCacheCommitAttempt
			select {
			case attempt = <-gate.entered:
			default:
				t.Fatal("request never reached the controlled commit timeout")
			}
			reportTestFailure(t, attempt.budget <= 0 || attempt.budget > 50*time.Millisecond || gate.leaseTTL.Load() != int64(5*time.Second), func() {
				t.Fatalf("wrong commit budget/lease: budget=%s lease=%s", attempt.budget, time.Duration(gate.leaseTTL.Load()))
			})
			{
				got := catalogCacheMetrics(h.dataPlane.catalogCache)
				reportTestFailure(t, got.loads != 1 || got.misses != 1 || got.failures != 1 || got.fills != 0, func() {
					t.Fatalf("timed-out write hid successful DB read: %+v", got)
				})
			}
			ctx := context.Background()
			{
				_, err := f.backend.GetCatalog(ctx, attempt.name)
				reportTestFailure(t, !errors.Is(err, session.ErrMiss), func() {
					t.Fatalf("timed-out fill persisted a value: %v", err)
				})
			}
			token, owned, err := f.backend.AcquireFillLease(ctx, attempt.name, 5*time.Second)
			reportTestFailure(t, err != nil || !owned, func() {
				t.Fatalf("timed-out fill leaked its lease: owned=%v err=%v", owned, err)
			})
			{
				err := f.backend.ReleaseFillLease(ctx, attempt.name, token)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
		})
	}
}

func TestCatalogCacheMiniredisExpiredFillerIsFenced(t *testing.T) {
	f := newCatalogCacheFixture(t, "miniredis", session.Options{})
	proceed := make(chan struct{})
	gate := &catalogCacheCommitGate{CatalogCache: f.backend, entered: make(chan catalogCacheCommitAttempt, 1), proceed: proceed}
	h := newCatalogCacheHarness(t, gate, time.Hour, 3*time.Second)
	seedModelCatalog(t, h, h.accountID, "authoritative")
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(proceed) }) }
	t.Cleanup(release)
	result := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		status, _, raw, err := catalogCacheFetch(ctx, h, h.key, "")
		if err == nil && (status != http.StatusOK || !strings.Contains(string(raw), `"id":"authoritative"`)) {
			err = fmt.Errorf("expired fill response: status=%d body=%s", status, raw)
		}
		result <- err
	}()
	var attempt catalogCacheCommitAttempt
	select {
	case attempt = <-gate.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("catalog filler never reached commit")
	}
	reportTestFailure(t, attempt.budget <= time.Second || attempt.budget > 2*time.Second || gate.leaseTTL.Load() != int64(5*time.Second), func() {
		t.Fatalf("fill is not bounded to 2s under a 5s lease: budget=%s lease=%s", attempt.budget, time.Duration(gate.leaseTTL.Load()))
	})
	// Advance both Redis TIME and TTLs, including the shared expiry index.
	f.mini.SetTime(time.Now().Add(6 * time.Second))
	f.mini.FastForward(6 * time.Second)
	ctx := context.Background()
	newToken, owned, err := f.backend.AcquireFillLease(ctx, attempt.name, 5*time.Second)
	reportTestFailure(t, err != nil || !owned || newToken == attempt.token, func() {
		t.Fatalf("replacement lease: owned=%v same_token=%v err=%v", owned, newToken == attempt.token, err)
	})
	release()
	{
		err := <-result
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	{
		got := catalogCacheMetrics(h.dataPlane.catalogCache)
		reportTestFailure(t, got.loads != 1 || got.fills != 0 || got.failures != 1, func() {
			t.Fatalf("stale filler incorrectly committed: %+v", got)
		})
	}
	{
		_, err := f.backend.GetCatalog(ctx, attempt.name)
		reportTestFailure(t, !errors.Is(err, session.ErrMiss), func() {
			t.Fatalf("stale filler published a catalog: %v", err)
		})
	}
	{
		_, acquired, err := f.backend.AcquireFillLease(ctx, attempt.name, 5*time.Second)
		reportTestFailure(t, err != nil || acquired, func() {
			t.Fatalf("stale deferred release removed successor lease: acquired=%v err=%v", acquired, err)
		})
	}
	raw, err := json.Marshal(publicCatalogSnapshot{Schema: catalogCacheSchema, Revision: catalogCacheRevision(t, h.store), Accounts: map[int64][]store.CatalogModel{
		h.accountID: {{ID: "authoritative", Name: "successor snapshot"}},
	}})
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	{
		err := f.backend.CommitCatalog(ctx, attempt.name, newToken, raw, time.Hour)
		reportTestFailure(t, err != nil, func() {
			t.Fatalf("successor could not commit after stale fill: %v", err)
		})
	}
	entries := catalogCacheRead(t, h, h.key, "")
	reportTestFailure(t, len(entries) != 1 || string(entries[0]["name"]) != `"successor snapshot"`, func() {
		t.Fatalf("successor snapshot lost: %s", entries)
	})
}

func TestCatalogCacheHitsDoNotExtendTTL(t *testing.T) {
	for _, kind := range []string{"memory", "miniredis"} {
		t.Run(kind, func(t *testing.T) {
			f := newCatalogCacheFixture(t, kind, session.Options{})
			const ttl = 2 * time.Second
			h := newCatalogCacheHarness(t, f.backend, ttl, time.Second)
			seedModelCatalog(t, h, h.accountID, "authoritative")
			catalogCacheWantIDs(t, h, h.key, "authoritative")
			filled := time.Now()
			var key string
			if kind == "memory" {
				time.Sleep(ttl / 2)
			} else {
				key = catalogCacheRedisKey(t, f)
				f.mini.FastForward(ttl / 2)
			}
			catalogCacheWantIDs(t, h, h.key, "authoritative")
			{
				got := catalogCacheMetrics(h.dataPlane.catalogCache)
				reportTestFailure(t, got.loads != 1 || got.hits != 1, func() {
					t.Fatalf("mid-TTL read was not a hit: %+v", got)
				})
			}
			if kind == "memory" {
				// Deadline is measured from the original fill, not from the hit.
				time.Sleep(time.Until(filled.Add(ttl + 100*time.Millisecond)))
			} else {
				{
					remaining := f.mini.TTL(key)
					reportTestFailure(t, remaining != ttl/2, func() {
						t.Fatalf("hit changed Redis TTL: remaining=%s", remaining)
					})
				}
				f.mini.FastForward(ttl/2 + time.Millisecond)
			}
			catalogCacheWantIDs(t, h, h.key, "authoritative")
			{
				got := catalogCacheMetrics(h.dataPlane.catalogCache)
				reportTestFailure(t, got.loads != 2 || got.hits != 1 || got.fills != 2, func() {
					t.Fatalf("hit extended original catalog expiry: %+v", got)
				})
			}
		})
	}
}

// Each benchmark invocation owns one fixed on-disk database and 20/100 fixed
// account catalogs. There is no testing.T fixture, OAuth client or upstream.
func benchmarkCatalogCacheStore(b *testing.B, n int) (*store.Store, []int64) {
	b.Helper()
	st, err := store.Open(filepath.Join(b.TempDir(), "catalog-bench.db"))
	reportTestFailure(b, err != nil, func() {
		b.Fatal(err)
	})
	b.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		account := &store.Account{Name: fmt.Sprintf("catalog-%03d", i), Enabled: true, Status: store.AccountStatusReady, SupplementalModels: []string{"manual"}}
		{
			err := st.CreateAccount(ctx, account)
			reportTestFailure(b, err != nil, func() {
				b.Fatal(err)
			})
		}
		models := make([]store.CatalogModel, 4)
		for j := range models {
			models[j] = store.CatalogModel{ID: fmt.Sprintf("model-%02d", j), Name: "Benchmark model", Metadata: map[string]json.RawMessage{
				"context_window": json.RawMessage(`9007199254740993`),
				"future":         json.RawMessage(`{"tools":true,"images":false,"nullable":null,"options":[]}`),
			}}
		}
		catalogCacheSave(b, st, account.ID, 100, models...)
		ids = append(ids, account.ID)
	}
	return st, ids
}

func BenchmarkCatalogCacheLoad(b *testing.B) {
	ctx := context.Background()
	for _, n := range []int{20, 100} {
		b.Run(fmt.Sprintf("n%d", n), func(b *testing.B) { verifyBenchmarkCatalogCacheLoad(b, &ctx, &n) })
	}
}

// runCatalogCacheConcurrentLeaseLosersDoNotBlockOrFillCase exercises one isolated test phase with the original shared state.
func runCatalogCacheConcurrentLeaseLosersDoNotBlockOrFillCase(t *testing.T, capturedKind *string) {
	t.Helper()

	f := newCatalogCacheFixture(t, (*capturedKind), session.Options{})
	h := newCatalogCacheHarness(t, f.backend, time.Hour, time.Second)
	seedModelCatalog(t, h, h.accountID, "authoritative")
	ctx := context.Background()
	name := catalogCacheName(catalogCacheRevision(t, h.store), []int64{h.accountID})
	token, owned, err := f.backend.AcquireFillLease(ctx, name, 5*time.Second)
	reportTestFailure(t, err != nil || !owned, func() {
		t.Fatalf("test owner: owned=%v err=%v", owned, err)
	})
	const clients = 8
	results := make(chan error, clients)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := 0; i < clients; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			status, _, raw, err := catalogCacheFetch(ctx, h, h.key, "")
			if err == nil && (status != http.StatusOK || !strings.Contains(string(raw), `"id":"authoritative"`)) {
				err = fmt.Errorf("lease loser response: status=%d body=%s", status, raw)
			}
			results <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		reportTestFailure(t, err != nil, func() {
			t.Error(err)
		})
	}
	{
		got := catalogCacheMetrics(h.dataPlane.catalogCache)
		reportTestFailure(t, got.loads != clients || got.misses != clients || got.fills != 0 || got.hits != 0 || got.failures != 0, func() {
			t.Fatalf("lease losers performed an unauthorized fill: %+v", got)
		})
	}
	{
		_, err := f.backend.GetCatalog(ctx, name)
		reportTestFailure(t, !errors.Is(err, session.ErrMiss), func() {
			t.Fatalf("non-owner wrote a catalog: %v", err)
		})
	}
	{
		err := f.backend.ReleaseFillLease(ctx, name, token)
		reportTestFailure(t, err != nil, func() {
			t.Fatalf("non-owner stole/released the original lease: %v", err)
		})
	}
	catalogCacheWantIDs(t, h, h.key, "authoritative")
	catalogCacheWantIDs(t, h, h.key, "authoritative")
	{
		got := catalogCacheMetrics(h.dataPlane.catalogCache)
		reportTestFailure(t, got.loads != clients+1 || got.fills != 1 || got.hits != 1, func() {
			t.Fatalf("catalog failed to recover after lease release: %+v", got)
		})
	}
}

// verifyBenchmarkCatalogCacheLoad exercises one isolated test phase with the original shared state.
func verifyBenchmarkCatalogCacheLoad(b *testing.B, capturedCtx *context.Context, capturedN *int) {
	st, ids := benchmarkCatalogCacheStore(b, (*capturedN))
	for _, kind := range []string{"off", "memory", "miniredis"} {
		b.Run(kind+"/warm", func(b *testing.B) {
			verifyBenchmarkCatalogCacheLoad2(b, &capturedCtx, &capturedN, &st, &ids, &kind)
		})
	}
}

// verifyBenchmarkCatalogCacheLoad2 exercises one isolated test phase with the original shared state.
func verifyBenchmarkCatalogCacheLoad2(b *testing.B, capturedCapturedCtx **context.Context, capturedCapturedN **int, capturedSt **store.Store, capturedIds *[]int64, capturedKind *string) {
	s := &Server{store: (*capturedSt)}
	if (*capturedKind) != "off" {
		f := newCatalogCacheFixture(b, (*capturedKind), session.Options{})
		s.catalogCache = newModelCatalogCache(f.backend, time.Hour, time.Second, discardLogger())
	}
	// Off has no catalogCache counters; count actual successful calls
	// through its unconditional one-batch-load path, not a b.N formula.
	var offLoads uint64
	load := func() {
		catalogs, err := s.loadModelCatalogs((*(*capturedCapturedCtx)), (*capturedIds))
		reportTestFailure(b, err != nil || len(catalogs) != (*(*capturedCapturedN)), func() {
			b.Fatalf("load catalogs: count=%d err=%v", len(catalogs), err)
		})
		if s.catalogCache == nil {
			offLoads++
		}
	}
	load() // Warm only the public directory, outside the measured loop.
	var before catalogCacheCounts
	if s.catalogCache != nil {
		before = catalogCacheMetrics(s.catalogCache)
		reportTestFailure(b, before.fills != 1, func() {
			b.Fatalf("warmup did not fill the requested backend: %+v", before)
		})
	}
	offBefore := offLoads
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		load()
	}
	b.StopTimer()
	loads, hits := offLoads-offBefore, uint64(0)
	if s.catalogCache != nil {
		after := catalogCacheMetrics(s.catalogCache)
		loads, hits = after.loads-before.loads, after.hits-before.hits
		reportTestFailure(b, after.failures != before.failures, func() {
			b.Fatalf("cache benchmark silently degraded to DB: before=%+v after=%+v", before, after)
		})
	}
	b.ReportMetric(float64(loads)/float64(b.N), "catalog_loads/op")
	b.ReportMetric(float64(hits)/float64(b.N), "cache_hits/op")
}
