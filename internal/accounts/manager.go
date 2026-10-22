// Package accounts owns the ChatGPT subscription accounts: credential refresh,
// selection for a given client key, per-account concurrency and egress proxy.
package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"pi-gateway/internal/egress"
	"pi-gateway/internal/oauth"
	"pi-gateway/internal/quota"
	"pi-gateway/internal/session"
	"pi-gateway/internal/store"
)

// ErrNoAccounts is returned when no account can serve a request.
var ErrNoAccounts = errors.New("no enabled account is available")

// Manager coordinates account state.
type Manager struct {
	store   *store.Store
	oauth   *oauth.Client
	logger  *slog.Logger
	factory *egress.Factory

	// Codex catalog negotiation is independent of Pi generation identity.
	codexVersions codexVersionCache

	mu sync.Mutex
	// inflight tracks concurrent requests per account id.
	inflight map[int64]int
	// retryQueues gives same-account replays priority over ordinary selection.
	retryQueues map[int64][]*retryTicket
	// refreshMu protects the per-account refresh mutex map. Refreshes are kept
	// outside m.mu so account selection and credential rotation cannot deadlock.
	refreshMu    sync.Mutex
	refreshLocks map[int64]*credentialRefreshLock
	// rr is the round-robin cursor for deterministic rotation.
	rr uint64
	// sticky preserves unscoped legacy stickiness without retaining deleted keys
	// forever. Both caches hold only opaque identity hashes and account IDs.
	sticky           *session.Memory
	affinity         session.AffinityStore
	affinityIdentity string
	affinityTTL      time.Duration
	affinityTimeout  time.Duration
	// quotaClient fetches rate-limit state; optional.
	quotaClient     *quota.Client
	officialUsageMu *credentialRefreshLock

	refreshMargin     time.Duration
	maxPerAccount     int
	rotationStrategy  string
	requestInterval   time.Duration
	accountCooldown   time.Duration
	waitTimeout       time.Duration
	maxAttempts       int
	smartWeights      SmartWeights
	now               func() time.Time
	warnedSmartConfig bool
}

// Options configures the manager.
type Options struct {
	RefreshMargin time.Duration
	MaxPerAccount int
	Logger        *slog.Logger
	Now           func() time.Time
	// Affinity is an optional, context-aware soft-hint backend shared with the
	// deployment's session store. Its lifecycle remains owned by the caller.
	Affinity         session.AffinityStore
	AffinityIdentity string
	AffinityTTL      time.Duration
	AffinityTimeout  time.Duration
}

// New creates an account manager.
func New(st *store.Store, oauthClient *oauth.Client, factory *egress.Factory, opts Options) *Manager {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.RefreshMargin <= 0 {
		opts.RefreshMargin = time.Hour
	}
	if opts.MaxPerAccount < 0 {
		opts.MaxPerAccount = 0
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.AffinityTTL <= 0 {
		opts.AffinityTTL = session.DefaultTTL
	}
	if opts.AffinityTimeout <= 0 {
		opts.AffinityTimeout = session.DefaultCommitTimeout
	}
	m := &Manager{
		store:            st,
		oauth:            oauthClient,
		factory:          factory,
		logger:           opts.Logger,
		inflight:         map[int64]int{},
		retryQueues:      map[int64][]*retryTicket{},
		sticky:           session.NewMemory(session.Options{TTL: opts.AffinityTTL, MaxEntries: session.DefaultMaxEntries, MaxBytes: 1 << 20}),
		affinity:         opts.Affinity,
		affinityIdentity: opts.AffinityIdentity,
		affinityTTL:      opts.AffinityTTL,
		affinityTimeout:  opts.AffinityTimeout,
		refreshLocks:     map[int64]*credentialRefreshLock{},
		refreshMargin:    opts.RefreshMargin,
		maxPerAccount:    opts.MaxPerAccount,
		rotationStrategy: "smart",
		accountCooldown:  60 * time.Second,
		waitTimeout:      30 * time.Second,
		maxAttempts:      2,
		smartWeights:     DefaultSmartWeights(),
		now:              opts.Now,
	}
	if st != nil {
		if settings, err := st.LoadSettings(context.Background(), nil); err == nil {
			m.SetSettings(settings)
		}
	}
	return m
}

// SetRefreshMargin updates the refresh margin (runtime settings).
func (m *Manager) SetRefreshMargin(d time.Duration) {
	if d > 0 {
		m.refreshMargin = d
	}
}

// SetMaxPerAccount updates the per-account concurrency ceiling.
func (m *Manager) SetMaxPerAccount(n int) {
	if n > 0 {
		m.mu.Lock()
		m.maxPerAccount = n
		m.mu.Unlock()
	}
}

// Acquire reserves an account slot for a request. The returned release function
// must always be called.
func (m *Manager) Acquire(ctx context.Context, key *store.APIKey, requestedModel string) (*store.Account, func(), error) {
	return m.AcquireWithAffinity(ctx, key, requestedModel, "")
}

// AcquireWithAffinity reserves an account using scope only as a soft preference.
// scope is an internal connection/session fingerprint, never an upstream field.
// Cached IDs can only reorder freshly authorized candidates, not grant access.
func (m *Manager) AcquireWithAffinity(ctx context.Context, key *store.APIKey, requestedModel, scope string) (*store.Account, func(), error) {
	return m.AcquireExcluding(ctx, key, requestedModel, scope, nil)
}

// AcquireExcluding skips accounts already attempted by this request.
func (m *Manager) AcquireExcluding(ctx context.Context, key *store.APIKey, requestedModel, scope string, excluded map[int64]bool) (*store.Account, func(), error) {
	var hint requestAffinity
	m.mu.Lock()
	waitTimeout := m.waitTimeout
	m.mu.Unlock()
	if waitTimeout <= 0 {
		waitTimeout = 30 * time.Second
	}
	timer := time.NewTimer(waitTimeout)
	defer timer.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		// Re-read the credential as well as scope, health and policy on every
		// attempt. Waiting must not keep a revoked or rotated key authorized.
		current, err := m.currentKey(ctx, key)
		if err != nil {
			return nil, nil, err
		}
		candidates, err := m.eligible(ctx, current, requestedModel)
		if err != nil {
			return nil, nil, err
		}
		candidates = excludeAccounts(candidates, excluded)
		if len(candidates) == 0 {
			return nil, nil, ErrNoAccounts
		}
		strategy := m.resolveStrategy(current)
		preferred := hint.load(ctx, m, current, candidates, requestedModel, scope, strategy)
		ordered := m.order(candidates, strategy)
		preferAccount(ordered, preferred)
		fresh, release, err := m.tryAcquireOrdered(ctx, key, requestedModel, ordered, &hint)
		if fresh != nil || err != nil {
			return fresh, release, err
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-timer.C:
			return nil, nil, ErrAccountConcurrency
		case <-ticker.C:
		}
	}
}

// currentKey validates the original credential against authoritative state.
// A nil key retains Acquire's internal, unscoped semantics; a missing non-nil
// key must never become an unscoped request. Policy and strategy edits take
// effect without invalidating a credential whose token is still unchanged.
func (m *Manager) currentKey(ctx context.Context, key *store.APIKey) (*store.APIKey, error) {
	if key == nil {
		return nil, nil
	}
	if key.ID <= 0 {
		return nil, ErrNoAccounts
	}
	current, err := m.store.GetKey(ctx, key.ID)
	if err != nil {
		return nil, err
	}
	if current == nil || !current.Enabled || current.Key != key.Key {
		return nil, ErrNoAccounts
	}
	return current, nil
}

// revalidateReservation runs only after owning the selected account's slot.
// Keep this check in both acquisition paths: pre-reservation reads may straddle
// credential, scope or account edits while a queued request is waking up.
func (m *Manager) revalidateReservation(ctx context.Context, key *store.APIKey, accountID int64, model string) (*store.Account, error) {
	current, err := m.currentKey(ctx, key)
	if err != nil {
		return nil, err
	}
	candidates, err := m.eligible(ctx, current, model)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, candidate := range candidates {
		if candidate.ID == accountID {
			return candidate, nil
		}
	}
	return nil, ErrNoAccounts
}

// ErrModelUnavailable identifies policy denial separately from pool exhaustion
// and database failure; HTTP entry points may map only this error to 403.
var ErrModelUnavailable = errors.New("requested model is disabled for all eligible accounts")
var ErrAccountConcurrency = errors.New("account concurrency limit reached")

// AcquireSpecific reserves only accountID, including its enabled-group model
// restrictions. It never falls back to another account and release is idempotent.
func (m *Manager) AcquireSpecific(ctx context.Context, accountID int64, requestedModel string) (*store.Account, func(), error) {
	m.mu.Lock()
	waitTimeout := m.waitTimeout
	m.mu.Unlock()
	if waitTimeout <= 0 {
		waitTimeout = 30 * time.Second
	}
	timer := time.NewTimer(waitTimeout)
	defer timer.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		a, err := m.specificEligibleAccount(ctx, accountID, requestedModel)
		if err != nil {
			return nil, nil, err
		}
		if m.tryReserve(a) {
			release := m.pinnedRelease(a.ID)
			if err := ctx.Err(); err != nil {
				release()
				return nil, nil, err
			}
			return a, release, nil
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-timer.C:
			return nil, nil, ErrAccountConcurrency
		case <-ticker.C:
		}
	}
}

// eligible lists the enabled accounts a key may use.
func (m *Manager) eligible(ctx context.Context, key *store.APIKey, requestedModels ...string) ([]*store.Account, error) {
	model := ""
	if len(requestedModels) > 0 {
		model = strings.TrimSpace(requestedModels[0])
	}
	return m.eligibleFor(ctx, key, model, 0, false)
}

// eligibleFor shares authorization, health and model policy across admission
// paths. Replays retain temporarily cooling accounts, but never permanent denials.
// accountID restricts policy evaluation to the original account, not a fallback.
func (m *Manager) eligibleFor(ctx context.Context, key *store.APIKey, model string, accountID int64, includeWaiting bool) ([]*store.Account, error) {
	all, err := m.store.EligibleAccountsForKey(ctx, key)
	if err != nil {
		return nil, err
	}
	now := m.nowTime().UnixMilli()
	m.mu.Lock()
	interval := m.requestInterval.Milliseconds()
	m.mu.Unlock()
	model = strings.TrimSpace(model)
	healthy, ids := healthyAccounts(all, accountID, includeWaiting, interval, now)
	if len(healthy) == 0 {
		return nil, nil
	}
	disabled, err := m.store.DisabledModelsForAccounts(ctx, ids, key)
	if err != nil {
		return nil, err
	}
	modelDenied := false
	out := make([]*store.Account, 0, len(healthy))
	for _, a := range healthy {
		blocked := modelIsDisabled(model, disabled[a.ID])
		if blocked {
			modelDenied = true
			continue
		}
		out = append(out, a)
	}
	if len(out) == 0 && modelDenied {
		return nil, fmt.Errorf("%w: %s", ErrModelUnavailable, model)
	}
	return out, nil
}

func (m *Manager) resolveStrategy(key *store.APIKey) string {
	if key != nil && key.Strategy != "" {
		return key.Strategy
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rotationStrategy != "" {
		return m.rotationStrategy
	}
	return string(StrategyRoundRobin)
}

// order sorts accounts according to the strategy.
func (m *Manager) order(accounts []*store.Account, strategy string) []*store.Account {
	if len(accounts) == 0 {
		return nil
	}
	m.mu.Lock()
	cursor := m.rr
	weights := m.smartWeights
	maxPerAccount := m.maxPerAccount
	now := m.now
	m.mu.Unlock()
	if now == nil {
		now = time.Now
	}
	candidates := make([]Candidate, 0, len(accounts))
	for _, a := range accounts {
		c := Candidate{Account: a, InFlight: m.Inflight(a.ID), LastStartedAt: a.LastStartedAt, QuotaResetAt: quotaResetAt(a), QuotaRemainingRank: quotaRemainingRank(a), FailureRate: float64(a.EWMAFailureRateBP) / 10000, FirstOutputMS: a.EWMAFirstOutputMS, CooldownUntil: a.CooldownUntil}
		candidates = append(candidates, c)
	}
	ordered := selectAt(candidates, Strategy(strategy), weights, nil, &cursor, now(), maxPerAccount)
	m.mu.Lock()
	if strategy == string(StrategyRoundRobin) || strategy == string(StrategySmart) {
		m.rr = cursor
	}
	m.mu.Unlock()
	out := make([]*store.Account, 0, len(ordered))
	for _, c := range ordered {
		out = append(out, c.Account)
	}
	return out
}

func quotaResetAt(a *store.Account) int64 {
	var s quota.Snapshot
	if a == nil || a.QuotaJSON == "" || json.Unmarshal([]byte(a.QuotaJSON), &s) != nil || s.Report == nil {
		return 0
	}
	var reset int64
	for _, w := range s.Report.Windows {
		if w.ResetAt > 0 && (reset == 0 || w.ResetAt < reset) {
			reset = w.ResetAt
		}
	}
	return reset
}
func quotaRemainingRank(a *store.Account) float64 {
	var s quota.Snapshot
	if a == nil || a.QuotaJSON == "" || json.Unmarshal([]byte(a.QuotaJSON), &s) != nil || s.Report == nil {
		return .5
	}
	best := 1.0
	found := false
	for _, w := range s.Report.Windows {
		found = true
		v := 1 - w.UsedPercent/100
		if v < best {
			best = v
		}
	}
	if !found {
		return .5
	}
	return best
}

// tryReserve increments the in-flight counter when under the ceiling.
func (m *Manager) tryReserve(a *store.Account) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.retryQueues[a.ID]) > 0 {
		return false
	}
	return m.reserveLocked(a)
}

// reserveLocked reads both the runtime limit and counters under m.mu.
func (m *Manager) reserveLocked(a *store.Account) bool {
	limit := a.Concurrency
	if limit <= 0 {
		limit = m.maxPerAccount
	}
	if m.inflight[a.ID] >= limit {
		return false
	}
	m.inflight[a.ID]++
	return true
}

// Inflight reports the current in-flight count for an account.
func (m *Manager) Inflight(id int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inflight[id]
}

// EnsureFreshToken returns a usable access token for an account, refreshing it
// when it is expired or about to expire. The per-account lock also covers the
// background refresher and imports that call Refresh directly.
func (m *Manager) EnsureFreshToken(ctx context.Context, a *store.Account) (string, string, error) {
	if a == nil {
		return "", "", errors.New("account is nil")
	}
	lock := m.refreshLock(a.ID)
	if err := lock.LockContext(ctx); err != nil {
		return "", "", err
	}
	defer lock.Unlock()
	if a.AccessToken != "" && a.ExpiresAt > 0 && time.Now().Add(2*time.Minute).Before(time.UnixMilli(a.ExpiresAt)) {
		return a.AccessToken, a.AccountID, nil
	}
	if a.RefreshToken == "" {
		if a.AccessToken == "" {
			return "", "", fmt.Errorf("account %q has no usable credentials", a.Name)
		}
		return a.AccessToken, a.AccountID, nil
	}
	if err := m.refreshPrimaryLocked(ctx, a); err != nil {
		return "", "", err
	}
	return a.AccessToken, a.AccountID, nil
}

// Refresh forces a token refresh for one account's primary (Sign in with ChatGPT)
// credential. It is serialized with EnsureFreshToken for this account.
func (m *Manager) Refresh(ctx context.Context, a *store.Account) error {
	if a == nil {
		return errors.New("account is nil")
	}
	lock := m.refreshLock(a.ID)
	if err := lock.LockContext(ctx); err != nil {
		return err
	}
	defer lock.Unlock()
	return m.refreshPrimaryLocked(ctx, a)
}

func (m *Manager) refreshPrimaryLocked(ctx context.Context, a *store.Account) error {
	client, err := m.oauthClientFor(a.ProxyURL)
	var tok *oauth.Token
	if err == nil {
		tok, err = client.RefreshChatGPT(ctx, a.RefreshToken, a.OAuthClientID)
	}
	if err != nil {
		status := store.AccountStatusExpired
		if strings.Contains(err.Error(), "invalid_grant") {
			status = store.AccountStatusInvalid
		}
		a.Status, a.LastError = status, err.Error()
		if statusErr := m.store.SetAccountStatus(ctx, a.ID, status, a.LastError); statusErr != nil {
			err = errors.Join(err, fmt.Errorf("recording failed refresh status: %w", statusErr))
		}
		m.logger.Warn("account token refresh failed", "account", a.Name, "error", err)
		return err
	}

	a.AccessToken = tok.Access
	if tok.Refresh != "" {
		a.RefreshToken = tok.Refresh
	}
	a.ExpiresAt = tok.ExpiresAt.UnixMilli()
	if tok.AccountID != "" {
		a.AccountID = tok.AccountID
	}
	if tok.PlanType != "" {
		a.PlanType = tok.PlanType
	}
	if tok.ClientID != "" {
		a.OAuthClientID = tok.ClientID
	}
	a.Status = store.AccountStatusReady
	a.LastError = ""

	if err := m.store.UpdateAccountCredentials(ctx, a.ID, a.AccessToken, tok.Refresh, a.ExpiresAt, a.Status, ""); err != nil {
		return err
	}
	m.logger.Info("account token refreshed", "account", a.Name, "expires_at", tok.ExpiresAt.Format(time.RFC3339))
	return nil
}

func (m *Manager) refreshLock(id int64) *credentialRefreshLock {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	if lock := m.refreshLocks[id]; lock != nil {
		return lock
	}
	lock := newCredentialRefreshLock()
	m.refreshLocks[id] = lock
	return lock
}

// EnsureFreshCodexToken returns a usable access token for the optional legacy
// Codex credential, refreshing it when needed. It reports ok=false when the account
// has no Codex credential attached. Codex account identity is mandatory; it is never
// substituted with the model-access account id.
func (m *Manager) EnsureFreshCodexToken(ctx context.Context, a *store.Account) (token, accountID string, ok bool, err error) {
	token, accountID, _, ok, err = m.ensureFreshCodexToken(ctx, a)
	return token, accountID, ok, err
}

func (m *Manager) ensureFreshCodexToken(ctx context.Context, a *store.Account) (token, accountID, refreshToken string, ok bool, err error) {
	if a == nil {
		return "", "", "", false, errors.New("account is nil")
	}
	lock := m.refreshLock(a.ID)
	if err := lock.LockContext(ctx); err != nil {
		return "", "", "", false, err
	}
	defer lock.Unlock()
	if !a.CodexLinked() {
		return "", "", "", false, nil
	}
	if strings.TrimSpace(a.CodexAccountID) == "" {
		return "", "", "", true, errors.New("account has a linked Codex credential but no Codex account id")
	}
	if a.CodexAccessToken != "" && a.CodexExpiresAt > 0 && time.Now().Add(2*time.Minute).Before(time.UnixMilli(a.CodexExpiresAt)) {
		return a.CodexAccessToken, a.CodexAccountID, a.CodexRefreshToken, true, nil
	}

	oldRefresh := a.CodexRefreshToken
	client, clientErr := m.oauthClientFor(a.ProxyURL)
	if clientErr != nil {
		return "", "", "", true, clientErr
	}
	tok, refreshErr := client.Refresh(ctx, oldRefresh)
	if refreshErr != nil {
		return "", "", "", true, refreshErr
	}
	newAccountID := strings.TrimSpace(tok.AccountID)
	if newAccountID == "" {
		newAccountID = strings.TrimSpace(a.CodexAccountID)
	}
	if newAccountID == "" {
		return "", "", "", true, errors.New("refreshed Codex credential has no account id")
	}
	newRefresh := oldRefresh
	if tok.Refresh != "" {
		newRefresh = tok.Refresh
	}
	newExpiresAt := tok.ExpiresAt.UnixMilli()
	if err := m.store.UpdateCodexCredentialsIfCurrent(ctx, a.ID, oldRefresh, tok.Access, tok.Refresh, newExpiresAt, newAccountID); err != nil {
		return "", "", "", true, err
	}
	a.CodexAccessToken = tok.Access
	a.CodexRefreshToken = newRefresh
	a.CodexExpiresAt = newExpiresAt
	a.CodexAccountID = newAccountID
	return a.CodexAccessToken, a.CodexAccountID, newRefresh, true, nil
}

// oauthClientFor uses exactly the requested account egress. Invalid proxy
// configuration must never silently downgrade an authentication request to direct.
func (m *Manager) oauthClientFor(proxyURL string) (*oauth.Client, error) {
	httpClient, err := m.factory.HTTPClient(proxyURL)
	if err != nil {
		return nil, err
	}
	return m.oauth.WithHTTPClient(httpClient), nil
}

// OAuth returns the shared direct OAuth client (used by the login flow).
func (m *Manager) OAuth() *oauth.Client { return m.oauth }

// CreateFromToken persists a newly authorized Sign in with ChatGPT account.
//
// The ChatGPT credential carries no chatgpt_account_id, so identity is derived from
// the id-token email when available (otherwise a generated id) and the issued
// OAuth client id is stored for later refreshes.
func (m *Manager) CreateFromToken(ctx context.Context, name string, tok *oauth.Token, proxyURL string) (*store.Account, error) {
	if tok == nil || tok.Access == "" {
		return nil, errors.New("accounts: missing token")
	}
	if err := oauth.ValidateChatGPTAccessToken(tok.Access); err != nil {
		return nil, err
	}
	if err := oauth.ValidateChatGPTClientID(tok.ClientID); err != nil {
		return nil, err
	}
	// A ChatGPT model-access credential must not carry the Codex account identity.
	// Reject metadata supplied by a caller as well as the JWT shape checked above.
	if strings.TrimSpace(tok.AccountID) != "" {
		return nil, errors.New("accounts: Codex credentials cannot be used as the ChatGPT account credential")
	}

	// Sign in with ChatGPT tokens do not carry a Codex account id, so identify the
	// account by email when available, then use a fresh stable row identifier.
	accountID := ""
	if tok.Email != "" {
		accountID = "chatgpt:" + tok.Email
	}
	if accountID == "" {
		accountID = "chatgpt:" + uuid.NewString()
	}

	var existing *store.Account
	if found, err := m.findByAccountID(ctx, accountID); err != nil {
		return nil, err
	} else {
		existing = found
	}

	if name == "" {
		if tok.Email != "" {
			name = tok.Email
		} else {
			name = "account-" + shortID(accountID)
		}
	}

	if existing != nil {
		return m.updateFromToken(ctx, existing, tok, proxyURL)
	}

	m.mu.Lock()
	maxPerAccount := m.maxPerAccount
	m.mu.Unlock()
	a := &store.Account{
		Name:          name,
		Email:         tok.Email,
		AccountID:     accountID,
		PlanType:      tok.PlanType,
		AccessToken:   tok.Access,
		RefreshToken:  tok.Refresh,
		IDToken:       tok.IDToken,
		ExpiresAt:     tok.ExpiresAt.UnixMilli(),
		OAuthClientID: tok.ClientID,
		Enabled:       true,
		Weight:        1,
		Concurrency:   maxPerAccount,
		ProxyURL:      proxyURL,
		Status:        store.AccountStatusReady,
	}
	if err := m.store.CreateAccount(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

// LinkChatGPTCredential attaches verified OAuth output to an explicitly selected
// account without replacing its Codex credential or management configuration.
func (m *Manager) LinkChatGPTCredential(ctx context.Context, a *store.Account, tok *oauth.Token) error {
	if a == nil || tok == nil || tok.Access == "" || tok.Refresh == "" {
		return errors.New("accounts: missing ChatGPT credential")
	}
	if err := oauth.ValidateChatGPTAccessToken(tok.Access); err != nil {
		return err
	}
	if err := oauth.ValidateChatGPTClientID(tok.ClientID); err != nil {
		return err
	}
	if tok.AccountID != "" {
		return errors.New("accounts: Codex credentials cannot be used as ChatGPT credentials")
	}
	if a.Email != "" && tok.Email != "" && !strings.EqualFold(a.Email, tok.Email) {
		return errors.New("accounts: ChatGPT email does not match the target account")
	}
	lock := m.refreshLock(a.ID)
	lock.Lock()
	defer lock.Unlock()
	email := tok.Email
	if email == "" {
		email = a.Email
	}
	return m.store.AttachChatGPTCredential(ctx, a.ID, store.ChatGPTCredential{Email: email, Plan: tok.PlanType, Access: tok.Access, Refresh: tok.Refresh, IDToken: tok.IDToken, ClientID: tok.ClientID, ExpiresAt: tok.ExpiresAt.UnixMilli()})
}

// LinkCodexCredential attaches the optional quota/catalog credential.
func (m *Manager) LinkCodexCredential(ctx context.Context, a *store.Account, tok *oauth.Token) error {
	if a == nil || tok == nil || tok.Access == "" {
		return errors.New("accounts: missing token")
	}
	if strings.TrimSpace(tok.Refresh) == "" {
		return errors.New("accounts: Codex credential is missing a refresh token")
	}
	lock := m.refreshLock(a.ID)
	lock.Lock()
	defer lock.Unlock()
	codexAccountID := tok.AccountID
	if codexAccountID == "" {
		if extracted, err := oauth.AccountIDFromToken(tok.Access); err == nil {
			codexAccountID = extracted
		}
	}
	if strings.TrimSpace(codexAccountID) == "" {
		return errors.New("accounts: Codex credential has no account id")
	}
	if err := m.store.SaveCodexCredential(ctx, a.ID, tok.Access, tok.Refresh, tok.IDToken, tok.ExpiresAt.UnixMilli(), codexAccountID); err != nil {
		return err
	}
	a.CodexAccessToken = tok.Access
	if tok.Refresh != "" {
		a.CodexRefreshToken = tok.Refresh
	}
	a.CodexIDToken = tok.IDToken
	a.CodexExpiresAt = tok.ExpiresAt.UnixMilli()
	a.CodexAccountID = codexAccountID
	return nil
}

// UnlinkCodexCredential detaches the optional Codex credential and clears any cached
// quota state that came from it.
func (m *Manager) UnlinkCodexCredential(ctx context.Context, a *store.Account) error {
	if a == nil {
		return errors.New("accounts: missing account")
	}
	lock := m.refreshLock(a.ID)
	lock.Lock()
	defer lock.Unlock()
	if err := m.store.ClearCodexCredential(ctx, a.ID); err != nil {
		return err
	}
	a.CodexAccessToken, a.CodexRefreshToken, a.CodexIDToken, a.CodexAccountID = "", "", "", ""
	a.CodexExpiresAt = 0
	// The cached snapshot came from the Codex credential that no longer exists.
	a.QuotaJSON = ""
	a.QuotaUpdatedAt = 0
	a.QuotaError = ""
	return nil
}

func (m *Manager) findByAccountID(ctx context.Context, accountID string) (*store.Account, error) {
	all, err := m.store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range all {
		if a.AccountID == accountID {
			return a, nil
		}
	}
	return nil, nil
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// StartRefresher runs a background loop that keeps tokens fresh.
func (m *Manager) StartRefresher(ctx context.Context, interval time.Duration, concurrency int) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if concurrency <= 0 {
		concurrency = 2
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.refreshDue(ctx, concurrency)
			}
		}
	}()
}

func (m *Manager) refreshDue(ctx context.Context, concurrency int) {
	deadline := time.Now().Add(m.refreshMargin).UnixMilli()
	due, err := m.store.ListAccountsDueForRefresh(ctx, deadline)
	if err != nil {
		m.logger.Error("listing refresh candidates failed", "error", err)
		return
	}
	if len(due) == 0 {
		return
	}

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, a := range due {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(acc *store.Account) {
			defer wg.Done()
			defer func() { <-sem }()
			tctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			if err := m.Refresh(tctx, acc); err != nil {
				m.logger.Debug("scheduled refresh failed", "account", acc.Name, "error", err)
			}
		}(a)
	}
	wg.Wait()
}

// ValidateProxy checks a proxy URL for an account.
func ValidateProxy(raw string) error { return egress.Validate(raw) }

func (m *Manager) tryAcquireOrdered(ctx context.Context, key *store.APIKey, requestedModel string, ordered []*store.Account, hint *requestAffinity) (*store.Account, func(), error) {
	for _, chosen := range ordered {
		if !m.tryReserve(chosen) {
			continue
		}
		release := m.pinnedRelease(chosen.ID)
		if err := ctx.Err(); err != nil {
			release()
			return nil, nil, err
		}
		// Selection reads may have begun while all slots were occupied. A
		// revocation can commit before tryReserve succeeds; validate again
		// after owning the slot rather than forwarding that older snapshot.
		fresh, err := m.revalidateReservation(ctx, key, chosen.ID, requestedModel)
		if err != nil {
			release()
			return nil, nil, err
		}
		if hint.save(ctx, m, fresh.ID) {
			// Even best-effort cache IO may wait. Never let a revocation or
			// credential rotation during that wait return an older account.
			fresh, err = m.revalidateReservation(ctx, key, chosen.ID, requestedModel)
			if err != nil {
				release()
				return nil, nil, err
			}
		}
		return fresh, release, nil
	}
	return nil, nil, nil
}

func (m *Manager) specificEligibleAccount(ctx context.Context, accountID int64, requestedModel string) (*store.Account, error) {
	a, err := m.store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if a == nil || !a.Enabled || a.CooldownUntil > m.nowTime().UnixMilli() {
		return nil, ErrNoAccounts
	}
	switch a.Status {
	case store.AccountStatusExpired, store.AccountStatusInvalid, store.AccountStatusBanned, "quota_exhausted":
		return nil, ErrNoAccounts
	}
	m.mu.Lock()
	interval := m.requestInterval.Milliseconds()
	m.mu.Unlock()
	if interval > 0 && a.LastStartedAt > 0 && m.nowTime().UnixMilli()-a.LastStartedAt < interval {
		return nil, ErrNoAccounts
	}
	models, err := m.store.DisabledModelsForAccount(ctx, a.ID, nil)
	if err != nil {
		return nil, err
	}
	for _, model := range models {
		if model == strings.TrimSpace(requestedModel) {
			return nil, fmt.Errorf("%w: %s", ErrModelUnavailable, model)
		}
	}
	return a, nil
}

func healthyAccounts(all []*store.Account, accountID int64, includeWaiting bool, interval, now int64) (healthy []*store.Account, ids []int64) {
	for _, a := range all {
		if a == nil || !a.Enabled || (accountID > 0 && a.ID != accountID) {
			continue
		}
		if !includeWaiting && accountReadyAt(a, interval) > now {
			continue
		}
		switch a.Status {
		case store.AccountStatusExpired, store.AccountStatusInvalid, store.AccountStatusBanned, "quota_exhausted":
			continue
		}
		healthy = append(healthy, a)
		ids = append(ids, a.ID)
	}
	return healthy, ids
}

func modelIsDisabled(model string, disabled []string) bool {
	blocked := false
	for _, banned := range disabled {
		if model != "" && banned == model {
			blocked = true
			break
		}
	}
	return blocked
}

func (m *Manager) updateFromToken(ctx context.Context, existing *store.Account, tok *oauth.Token, proxyURL string) (*store.Account, error) {
	existing.AccessToken = tok.Access
	if tok.Refresh != "" {
		existing.RefreshToken = tok.Refresh
	}
	existing.IDToken = tok.IDToken
	existing.ExpiresAt = tok.ExpiresAt.UnixMilli()
	existing.PlanType = tok.PlanType
	existing.Email = tok.Email
	if tok.ClientID != "" {
		existing.OAuthClientID = tok.ClientID
	}
	existing.Status = store.AccountStatusReady
	existing.LastError = ""
	existing.Enabled = true
	if proxyURL != "" {
		existing.ProxyURL = proxyURL
	}
	if err := m.store.UpdateAccount(ctx, existing); err != nil {
		return nil, err
	}
	if err := m.store.UpdateAccountCredentials(ctx, existing.ID, existing.AccessToken, tok.Refresh, existing.ExpiresAt, existing.Status, ""); err != nil {
		return nil, err
	}
	return existing, nil
}

func excludeAccounts(candidates []*store.Account, excluded map[int64]bool) []*store.Account {
	filtered := candidates[:0]
	for _, a := range candidates {
		if !excluded[a.ID] {
			filtered = append(filtered, a)
		}
	}
	return filtered
}
