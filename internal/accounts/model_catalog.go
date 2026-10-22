package accounts

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"pi-gateway/internal/config"
	"pi-gateway/internal/egress"
	"pi-gateway/internal/piwire"
	"pi-gateway/internal/store"
)

const (
	maxCatalogBytes  = 2 << 20
	maxCatalogModels = 2048
)

// ModelHeaderOptions uses the same primary ChatGPT header configuration as model
// traffic. The optional Codex credentials are intentionally not accepted here.
func ModelHeaderOptions(cfg *config.Config, userAgent, token, accountID, session string) piwire.HeaderOptions {
	if strings.TrimSpace(userAgent) == "" {
		userAgent = cfg.Upstream.UserAgent
	}
	return piwire.HeaderOptions{
		AccessToken: token, AccountID: accountID, SessionID: session,
		Originator: cfg.Upstream.Originator, UserAgent: userAgent,
		TimeoutSeconds: cfg.Upstream.RequestTimeoutSeconds,
		Platform: piwire.Platform{
			OS: cfg.Upstream.StainlessOS, Arch: cfg.Upstream.StainlessArch,
			Runtime: cfg.Upstream.StainlessRuntime, RuntimeVersion: cfg.Upstream.StainlessRuntimeVersion,
		},
	}
}

// ModelOperationTimeout bounds diagnostics independently of the upstream idle
// timeout. A smaller explicitly configured request timeout is respected.
func ModelOperationTimeout(cfg *config.Config, ceiling time.Duration) time.Duration {
	if seconds := cfg.Upstream.RequestTimeoutSeconds; seconds > 0 && time.Duration(seconds)*time.Second < ceiling {
		return time.Duration(seconds) * time.Second
	}
	return ceiling
}

// StartModelCatalogRefresher keeps enabled accounts' ChatGPT and linked Codex
// model catalogs current. interval is in seconds; zero disables automatic sync.
// The provider is read for every sweep so runtime user-agent edits take effect.
func (m *Manager) StartModelCatalogRefresher(ctx context.Context, interval, concurrency int, cfg *config.Config, userAgent func() string) {
	if interval <= 0 || cfg == nil || m == nil || m.store == nil || ctx.Err() != nil {
		return
	}
	period := time.Duration(interval) * time.Second
	if period < 30*time.Second {
		period = 30 * time.Second
	}
	if concurrency <= 0 {
		concurrency = 2
	}
	m.mu.Lock()
	if m.modelCatalogRefresherRunning {
		m.mu.Unlock()
		return
	}
	m.modelCatalogRefresherRunning = true
	m.mu.Unlock()
	go func() {
		defer func() {
			m.mu.Lock()
			m.modelCatalogRefresherRunning = false
			m.mu.Unlock()
		}()
		// Let startup finish before contacting account backends.
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			m.refreshAllModelCatalogs(ctx, concurrency, cfg, userAgent, period)
			// Discover newly imported or newly linked accounts promptly without
			// refetching catalogs whose last attempt is still within the interval.
			timer.Reset(min(period, 30*time.Second))
		}
	}()
}

func (m *Manager) refreshAllModelCatalogs(ctx context.Context, concurrency int, cfg *config.Config, userAgent func() string, period time.Duration) {
	if ctx.Err() != nil || cfg == nil || !m.modelCatalogSweepMu.TryLock() {
		return
	}
	defer m.modelCatalogSweepMu.Unlock()
	if concurrency <= 0 {
		concurrency = 2
	}
	accounts, err := m.store.ListEnabledAccounts(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.logger.Error("listing accounts for a model catalog refresh failed", "error", err)
		}
		return
	}
	if len(accounts) == 0 {
		return
	}
	ids := make([]int64, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.ID)
	}
	catalogs, err := m.store.GetAccountModelCatalogs(ctx, ids)
	if err != nil {
		if ctx.Err() == nil {
			m.logger.Error("reading model catalogs for a scheduled refresh failed", "error", err)
		}
		return
	}
	now := m.nowTime().UnixMilli()
	ua := ""
	if userAgent != nil {
		ua = userAgent()
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	defer wg.Wait()
	for _, account := range accounts {
		if !modelCatalogRefreshDue(account, catalogs[account.ID], now, period) {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
		}
		if ctx.Err() != nil {
			<-sem
			return
		}
		wg.Add(1)
		go func(accountID int64) {
			defer wg.Done()
			defer func() { <-sem }()
			// Accounts can be disabled or deleted while waiting for a slot.
			account, err := m.store.GetAccount(ctx, accountID)
			if err != nil {
				if ctx.Err() == nil {
					m.logger.Debug("loading account for a model catalog refresh failed", "account_id", accountID, "error", err)
				}
				return
			}
			if account == nil || !account.Enabled || ctx.Err() != nil {
				return
			}
			if _, err := m.RefreshModelCatalog(ctx, account, cfg, ua); err != nil && ctx.Err() == nil {
				m.logger.Debug("scheduled model catalog refresh failed", "account", account.Name, "error", err)
			}
		}(account.ID)
	}
}

func modelCatalogRefreshDue(account *store.Account, catalog *store.ModelCatalog, now int64, period time.Duration) bool {
	if catalog == nil || catalog.AttemptedAt <= 0 || now-catalog.AttemptedAt >= period.Milliseconds() {
		return true
	}
	primary, exists := catalog.SourceCatalogs[store.ModelSourceChatGPT]
	if !exists || primary.AttemptedAt <= 0 {
		return true
	}
	if account.CodexLinked() {
		codex, exists := catalog.SourceCatalogs[store.ModelSourceCodex]
		return !exists || codex.Skipped || codex.AttemptedAt <= 0
	}
	return false
}

// RefreshModelCatalog explicitly refreshes independent ChatGPT/Codex catalogs.
// Only the matching credential reaches each endpoint. Successful sources replace
// their own snapshots; failures never erase the other source or manual models.
func (m *Manager) RefreshModelCatalog(ctx context.Context, account *store.Account, cfg *config.Config, userAgent string) (*store.ModelCatalog, error) {
	if account == nil || account.ID <= 0 {
		return nil, ErrModelCatalogAccountMissing
	}
	if m == nil || m.store == nil || m.factory == nil || cfg == nil {
		return nil, ErrModelCatalogStorage
	}
	fetchCtx, cancel := context.WithTimeout(ctx, ModelOperationTimeout(cfg, 30*time.Second))
	defer cancel()
	attempt, err := m.store.BeginAccountModelCatalogRefresh(fetchCtx, account.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrModelCatalogAccountMissing
	}
	if err != nil || attempt == nil {
		return nil, fmt.Errorf("%w: could not prepare catalog refresh", ErrModelCatalogStorage)
	}

	// Exactly two workers, both owned by this bounded request. Each worker loads
	// its own account so credential refreshes cannot race on the same Go object.
	type sourceFetch struct {
		source string
		result store.ModelCatalogSourceResult
	}
	sources := []string{store.ModelSourceChatGPT, store.ModelSourceCodex}
	completed := make(chan sourceFetch, len(sources))
	for _, source := range sources {
		go func(source string) {
			result := m.fetchCatalogSource(fetchCtx, account.ID, source, attempt.AttemptedAt, cfg, userAgent)
			completed <- sourceFetch{source, result}
		}(source)
	}
	results := make(map[string]store.ModelCatalogSourceResult, len(sources))
	for range sources {
		done := <-completed
		results[done.source] = done.result
	}

	// Only local persistence is detached from client cancellation, and it has
	// its own short deadline. The store fences both request and credential epochs.
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer saveCancel()
	applied, err := m.store.SaveAccountModelCatalogRefresh(saveCtx, account.ID, attempt, results)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrModelCatalogAccountMissing
	}
	if err != nil {
		return nil, fmt.Errorf("%w: could not persist catalog refresh", ErrModelCatalogStorage)
	}
	snapshot, err := m.store.GetAccountModelCatalog(saveCtx, account.ID)
	if err != nil {
		return nil, fmt.Errorf("%w: could not read catalog snapshot", ErrModelCatalogStorage)
	}
	if snapshot == nil {
		return nil, ErrModelCatalogAccountMissing
	}
	return summarizeCatalogRefresh(snapshot, sources, results, applied)
}

var (
	ErrModelCatalogAccountMissing = errors.New("account no longer exists")
	ErrModelCatalogStorage        = errors.New("model catalog storage is unavailable")
	ErrModelCatalogSuperseded     = errors.New("model catalog refresh was superseded or its credential changed")
)

func modelCatalogSourceName(source string) string {
	if source == store.ModelSourceCodex {
		return "Codex"
	}
	return "ChatGPT"
}

func (m *Manager) fetchCatalogSource(ctx context.Context, accountID int64, source string, attemptedAt int64, cfg *config.Config, userAgent string) store.ModelCatalogSourceResult {
	result := store.ModelCatalogSourceResult{Catalog: store.ModelCatalogSource{
		Models: []store.CatalogModel{}, AttemptedAt: attemptedAt,
	}}
	// Loading the account snapshot is a local store operation. Do not let a
	// client cancellation interrupt a concurrent SQLite connection acquisition;
	// the network fetch below still observes ctx and remains cancellable. The
	// short independent bound also prevents a broken store from hanging refresh.
	accountCtx, accountCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer accountCancel()
	account, err := m.store.GetAccount(accountCtx, accountID)
	if err != nil || account == nil {
		result.Catalog.Error = "could not load the current account for model synchronization"
		if ctx.Err() != nil {
			result.Catalog.Error = modelCatalogContextError(ctx.Err()).Error()
		}
		return result
	}
	if source == store.ModelSourceCodex && !account.CodexLinked() {
		result.Catalog.Skipped = true
		result.Catalog.SkipReason = "codex_not_linked"
		result.CredentialFingerprint = store.ModelCatalogCredentialFingerprint(account, source)
		return result
	}
	var models []store.CatalogModel
	if source == store.ModelSourceCodex {
		models, err = m.fetchCodexModelCatalog(ctx, account, cfg)
	} else {
		models, err = m.fetchModelCatalog(ctx, account, cfg, userAgent)
	}
	// Compute after token refresh: the write must bind to the credential actually
	// used, not to the stale token from before its OAuth exchange.
	result.CredentialFingerprint = store.ModelCatalogCredentialFingerprint(account, source)
	if ctx.Err() != nil {
		err = modelCatalogContextError(ctx.Err())
	}
	if err != nil {
		result.Catalog.Error = err.Error()
		return result
	}
	if models != nil {
		result.Catalog.Models = models
	}
	result.Catalog.FetchedAt = time.Now().UnixMilli()
	return result
}

func (m *Manager) fetchCodexModelCatalog(ctx context.Context, account *store.Account, cfg *config.Config) ([]store.CatalogModel, error) {
	token, accountID, linked, err := m.EnsureFreshCodexToken(ctx, account)
	if err != nil {
		return nil, errors.New("could not obtain the account's linked Codex credential")
	}
	if !linked || token == "" || strings.TrimSpace(accountID) == "" {
		return nil, errors.New("a linked Codex credential and its own account identity are required")
	}
	version, err := m.resolveCodexClientVersion(ctx, cfg.Models.CodexClientVersion, account.ProxyURL)
	if err != nil {
		return nil, modelCatalogContextError(err)
	}
	// Use a request-local copy; concurrent catalog refreshes and primary requests
	// must never observe a mutated shared configuration.
	resolved := *cfg
	resolved.Models.CodexClientVersion = version
	// Primary ChatGPT headers deliberately omit this Codex identity. Build this
	// small header allowlist separately; never copy incoming/browser/Pi headers.
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("ChatGPT-Account-Id", accountID)
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", "codex-tui/"+version)
	headers.Set("Originator", codexCatalogOriginator)
	headers.Set("Version", version)
	return m.fetchModelCatalogRequest(ctx, account, resolved.CodexModelsURL(), headers, store.ModelSourceCodex)
}

func modelCatalogContextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("model catalog request timed out")
	}
	return errors.New("model catalog request canceled")
}

func (m *Manager) fetchModelCatalog(ctx context.Context, a *store.Account, cfg *config.Config, userAgent string) ([]store.CatalogModel, error) {
	token, accountID, err := m.EnsureFreshToken(ctx, a)
	if err != nil {
		return nil, errors.New("could not obtain the account's primary ChatGPT credential")
	}
	headers := piwire.BuildSSEHeaders(ModelHeaderOptions(cfg, userAgent, token, accountID, ""))
	return m.fetchModelCatalogRequest(ctx, a, cfg.UpstreamModelsURL(), headers, store.ModelSourceChatGPT)
}

func (m *Manager) fetchModelCatalogRequest(ctx context.Context, a *store.Account, endpoint string, headers http.Header, source string) ([]store.CatalogModel, error) {
	client, err := m.factory.HTTPClient(a.ProxyURL)
	if err != nil {
		return nil, errors.New("account proxy configuration is invalid")
	}
	boundedClient := *client
	boundedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("configured %s model catalog URL is invalid", strings.ToLower(source))
	}
	if (req.URL.Scheme != "https" && req.URL.Scheme != "http") || req.URL.Hostname() == "" || req.URL.User != nil || req.URL.Fragment != "" {
		return nil, errors.New("configured model catalog URL is invalid")
	}
	if err := validateCatalogCredentialTarget(req, source); err != nil {
		return nil, err
	}
	req.Header = headers.Clone()
	req.Header.Set("Accept", "application/json")
	resp, err := boundedClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s model catalog upstream connection failed", strings.ToLower(source))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("%s model catalog upstream returned HTTP %d", strings.ToLower(source), resp.StatusCode)
	}
	body, err := egress.DecodeBody(resp)
	if err != nil {
		resp.Body.Close()
		return nil, fmt.Errorf("%s model catalog response could not be decompressed", strings.ToLower(source))
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, maxCatalogBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s model catalog response could not be read", strings.ToLower(source))
	}
	if len(raw) > maxCatalogBytes {
		return nil, fmt.Errorf("%s model catalog response exceeds size limit", strings.ToLower(source))
	}
	models, err := parseModelCatalog(raw)
	if err != nil {
		return nil, fmt.Errorf("%s model catalog: %w", strings.ToLower(source), err)
	}
	for i := range models {
		models[i].Origins = []string{source}
	}
	return models, nil
}

func parseModelCatalog(raw []byte) ([]store.CatalogModel, error) {
	if !json.Valid(raw) {
		return nil, errors.New("model catalog upstream returned invalid JSON")
	}
	entries, ok := catalogEntries(raw, 0)
	if !ok {
		return nil, errors.New("model catalog upstream did not return a model list")
	}
	if len(entries) > maxCatalogModels {
		return nil, errors.New("model catalog contains too many models")
	}
	out := make([]store.CatalogModel, 0, len(entries))
	seen := make(map[string]int, len(entries))
	for _, entry := range entries {
		model, err := parseCatalogModel(entry)
		if err != nil {
			return nil, err
		}
		if index, exists := seen[model.ID]; exists {
			mergeCatalogModel(&out[index], model)
			continue
		}
		seen[model.ID] = len(out)
		out = append(out, model)
	}
	return out, nil
}

func catalogEntries(value json.RawMessage, depth int) ([]json.RawMessage, bool) {
	value = bytes.TrimSpace(value)
	if depth > 3 || len(value) == 0 {
		return nil, false
	}
	switch value[0] {
	case '[':
		var entries []json.RawMessage
		if err := json.Unmarshal(value, &entries); err != nil {
			return nil, false
		}
		return entries, true
	case '{':
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(value, &fields); err != nil {
			return nil, false
		}
		if catalogHasError(fields) {
			return nil, false
		}
		return nestedCatalogEntries(fields, depth)
	}
	return nil, false
}

func catalogString(m map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		var s string
		if err := json.Unmarshal(m[key], &s); err == nil && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func catalogTruncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func parseCatalogModel(entry json.RawMessage) (store.CatalogModel, error) {
	var model store.CatalogModel
	entry = bytes.TrimSpace(entry)
	switch entry[0] {
	case '"':
		var id string
		if err := json.Unmarshal(entry, &id); err != nil {
			return store.CatalogModel{}, errors.New("model catalog contains an invalid model")
		}
		model.ID = strings.TrimSpace(id)
	case '{':
		var item map[string]json.RawMessage
		if err := json.Unmarshal(entry, &item); err != nil {
			return store.CatalogModel{}, errors.New("model catalog contains an invalid model")
		}
		model.ID = catalogString(item, "id", "slug", "model", "name")
		model.Name = catalogString(item, "display_name", "displayName", "title", "name")
		model.Description = catalogString(item, "description")
		model.Metadata = item
	default:
		return store.CatalogModel{}, errors.New("model catalog contains an invalid model")
	}
	if model.ID == "" || len(model.ID) > 256 || strings.ContainsAny(model.ID, "\r\n\x00") {
		return store.CatalogModel{}, errors.New("model catalog contains an invalid model identifier")
	}
	if model.Name == "" {
		model.Name = model.ID
	}
	model.Name = catalogTruncate(model.Name, 512)
	model.Description = catalogTruncate(model.Description, 4096)
	return model, nil
}

func mergeCatalogModel(previous *store.CatalogModel, model store.CatalogModel) {
	// Repeated IDs can contribute additional fields without overriding
	// the first entry's explicit null/false/empty or conflicting values.

	if catalogString(previous.Metadata, "display_name", "displayName", "title", "name") == "" {
		previous.Name = model.Name
	}
	if _, hasDescription := previous.Metadata["description"]; !hasDescription {
		previous.Description = model.Description
	}
	if previous.Metadata == nil && len(model.Metadata) > 0 {
		previous.Metadata = make(map[string]json.RawMessage, len(model.Metadata))
	}
	for field, value := range model.Metadata {
		if _, exists := previous.Metadata[field]; !exists {
			previous.Metadata[field] = value
		}
	}
}

func catalogHasError(fields map[string]json.RawMessage) bool {
	if e, exists := fields["error"]; exists {
		switch string(bytes.TrimSpace(e)) {
		case "null", "false", `""`:
		default:
			return true
		}
	}
	return false
}

func summarizeCatalogRefresh(snapshot *store.ModelCatalog, sources []string, results map[string]store.ModelCatalogSourceResult, applied map[string]bool) (*store.ModelCatalog, error) {
	succeeded, published := 0, 0
	var failures, discarded []string
	for _, source := range sources {
		result := results[source].Catalog
		if result.Error != "" {
			failures = append(failures, modelCatalogSourceName(source)+": "+result.Error)
		}
		if !applied[source] {
			discarded = append(discarded, modelCatalogSourceName(source)+": refresh superseded or credential changed")
			continue
		}
		published++
		if !result.Skipped && result.Error == "" && result.FetchedAt > 0 {
			succeeded++
		}
	}
	if succeeded > 0 {
		return catalogRefreshSucceeded(snapshot, discarded), nil
	}
	if published == 0 || len(failures) == 0 {
		return snapshot, ErrModelCatalogSuperseded
	}
	return snapshot, errors.New(strings.Join(failures, "; "))
}

func nestedCatalogEntries(fields map[string]json.RawMessage, depth int) ([]json.RawMessage, bool) {
	for _, key := range []string{"models", "data"} {
		if nested, exists := fields[key]; exists {
			if entries, ok := catalogEntries(nested, depth+1); ok {
				return entries, true
			}
		}
	}
	return nil, false
}

func validateCatalogCredentialTarget(req *http.Request, source string) error {
	host := strings.ToLower(strings.TrimRight(req.URL.Hostname(), "."))
	if source == store.ModelSourceCodex && host == "api.openai.com" {
		return errors.New("Codex model credentials cannot be sent to the primary model API")
	}
	if source == store.ModelSourceChatGPT && host == "chatgpt.com" && strings.HasPrefix(req.URL.Path, "/backend-api/") {
		return errors.New("primary ChatGPT credentials cannot be sent to the Codex backend")
	}
	return nil
}

func catalogRefreshSucceeded(snapshot *store.ModelCatalog, discarded []string) *store.ModelCatalog {

	if len(discarded) > 0 {
		if snapshot.Error != "" {
			discarded = append([]string{snapshot.Error}, discarded...)
		}
		snapshot.Error = strings.Join(discarded, "; ")
	}
	return snapshot
}
