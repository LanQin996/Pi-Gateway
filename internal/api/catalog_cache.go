package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"pi-gateway/internal/session"
	"pi-gateway/internal/store"
)

const (
	catalogCacheSchema = 1
	catalogFillLease   = 5 * time.Second
	catalogFillTimeout = 2 * time.Second
)

// This DTO deliberately excludes account objects, catalog errors, authorization
// decisions and credentials. Raw model metadata is sanitized before caching and
// again when reading; database/admin model catalog reads remain lossless.
type publicCatalogSnapshot struct {
	Schema   int                            `json:"schema"`
	Revision string                         `json:"revision"`
	Accounts map[int64][]store.CatalogModel `json:"accounts"`
}

type modelCatalogCache struct {
	backend session.CatalogCache
	ttl     time.Duration
	timeout time.Duration
	logger  *slog.Logger

	hits, misses, failures, loads, fills, getNanos atomic.Uint64
}

func newModelCatalogCache(backend session.CatalogCache, ttl, timeout time.Duration, logger *slog.Logger) *modelCatalogCache {
	if backend == nil {
		return nil
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if timeout <= 0 {
		timeout = 100 * time.Millisecond
	}
	return &modelCatalogCache{backend: backend, ttl: ttl, timeout: timeout, logger: logger}
}

func publicCatalogs(catalogs map[int64]*store.ModelCatalog) map[int64][]store.CatalogModel {
	out := make(map[int64][]store.CatalogModel, len(catalogs))
	for id, catalog := range catalogs {
		if catalog == nil {
			continue
		}
		models := make([]store.CatalogModel, 0, len(catalog.Models))
		for _, model := range catalog.Models {
			model.Metadata = publicModelMetadata(model.Metadata)
			models = append(models, model)
		}
		out[id] = models
	}
	return out
}

func catalogCacheName(revision string, ids []int64) string {
	ordered := append([]int64(nil), ids...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	raw, _ := json.Marshal(struct {
		Revision string  `json:"revision"`
		Accounts []int64 `json:"accounts"`
	}{revision, ordered})
	return "model-catalog-v1:" + session.Fingerprint("public-catalog", string(raw))
}

func decodePublicCatalog(raw []byte, revision string, ids []int64) (map[int64][]store.CatalogModel, bool) {
	if len(raw) == 0 || len(raw) > session.MaxCatalogBytes {
		return nil, false
	}
	var snapshot publicCatalogSnapshot
	if json.Unmarshal(raw, &snapshot) != nil || snapshot.Schema != catalogCacheSchema || snapshot.Revision != revision || snapshot.Accounts == nil {
		return nil, false
	}
	wanted := make(map[int64]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	if len(snapshot.Accounts) != len(wanted) {
		return nil, false
	}
	for id, models := range snapshot.Accounts {
		if !wanted[id] {
			return nil, false
		}
		for i := range models {
			if strings.TrimSpace(models[i].ID) == "" {
				return nil, false
			}
			models[i].Metadata = publicModelMetadata(models[i].Metadata)
		}
	}
	return snapshot.Accounts, true
}

// loadModelCatalogs only caches public model descriptions. Key authentication,
// account eligibility, live restrictions and alias settings are ALWAYS loaded
// separately on every request. Redis unavailable/malformed data falls back to
// the database, never to a real upstream request or stale authorization result.
func (s *Server) loadModelCatalogs(ctx context.Context, ids []int64) (map[int64][]store.CatalogModel, error) {
	if len(ids) == 0 {
		return map[int64][]store.CatalogModel{}, nil
	}
	cache := s.catalogCache
	load := func(ctx context.Context) (map[int64][]store.CatalogModel, error) {
		if cache != nil {
			cache.loads.Add(1)
		}
		catalogs, err := s.store.GetAccountModelCatalogs(ctx, ids)
		if err != nil {
			return nil, err
		}
		return publicCatalogs(catalogs), nil
	}
	if cache == nil {
		return load(ctx)
	}
	revision, err := s.store.CatalogRevision(ctx)
	if err != nil {
		cache.failures.Add(1)
		return load(ctx)
	}
	name := catalogCacheName(revision, ids)
	getCtx, cancel := context.WithTimeout(ctx, cache.timeout)
	started := time.Now()
	raw, getErr := cache.backend.GetCatalog(getCtx, name)
	cache.getNanos.Add(uint64(time.Since(started)))
	cancel()
	if getErr == nil {
		if models, valid := decodePublicCatalog(raw, revision, ids); valid {
			cache.hits.Add(1)
			return models, nil
		}
		getErr = session.ErrInvalid
	}
	cache.misses.Add(1)
	if !errors.Is(getErr, session.ErrMiss) {
		cache.failures.Add(1)
		// Do not perform another Redis round-trip during an outage. Corrupt
		// entries expire naturally; a metadata problem cannot break /models.
		return load(ctx)
	}

	return cache.fillCatalog(ctx, name, revision, load)
}

func (cache *modelCatalogCache) fillCatalog(ctx context.Context, name string, revision string, load func(context.Context) (map[int64][]store.CatalogModel, error)) (map[int64][]store.CatalogModel, error) {
	leaseCtx, stop := context.WithTimeout(ctx, cache.timeout)
	token, owned, leaseErr := cache.backend.AcquireFillLease(leaseCtx, name, catalogFillLease)
	stop()
	if leaseErr != nil || !owned {
		if leaseErr != nil {
			cache.failures.Add(1)
		}
		// A non-owner may query the DB but must never write another owner's fill.
		return load(ctx)
	}
	committed := false
	defer func() {
		if committed {
			return // Commit atomically consumed the lease; avoid a redundant RPC.
		}
		releaseCtx, release := context.WithTimeout(context.WithoutCancel(ctx), cache.timeout)
		defer release()
		_ = cache.backend.ReleaseFillLease(releaseCtx, name, token)
	}()
	// No queue or background worker: both DB fill and shared write are bounded
	// by a timeout shorter than the lease. Late owners are fenced in the store.
	fillCtx, finish := context.WithTimeout(ctx, catalogFillTimeout)
	defer finish()
	models, err := load(fillCtx)
	if err != nil {
		if fillCtx.Err() != nil && ctx.Err() == nil {
			// The optional fill budget is not the caller's database deadline.
			// Retry without caching rather than fail a still-live slow request.
			cache.failures.Add(1)
			return load(ctx)
		}
		return nil, err
	}
	payload, err := json.Marshal(publicCatalogSnapshot{catalogCacheSchema, revision, models})
	if err != nil || len(payload) > session.MaxCatalogBytes {
		return models, nil
	}
	writeCtx, writeCancel := context.WithTimeout(fillCtx, cache.timeout)
	err = cache.backend.CommitCatalog(writeCtx, name, token, payload, cache.ttl)
	writeCancel()
	if err == nil {
		committed = true
		cache.fills.Add(1)
	} else {
		cache.failures.Add(1)
		cache.logFillFailure(err)
	}
	// A cache write cannot revoke an authoritative successful database read.
	return models, nil
}

func (cache *modelCatalogCache) logFillFailure(err error) {
	if cache.logger != nil {
		cache.logger.Debug("model catalog cache fill unavailable", "status", session.StatusOf(err))
	}
}
