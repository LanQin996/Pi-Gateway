package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Account status values.
const (
	AccountStatusUnknown = "unknown"
	AccountStatusReady   = "ready"
	AccountStatusExpired = "expired"
	AccountStatusInvalid = "invalid"
	AccountStatusBanned  = "banned"
)

// ErrCodexCredentialChanged means an in-flight Codex refresh or quota write lost
// the compare-and-swap race against unlink/relink.
var ErrCodexCredentialChanged = errors.New("store: Codex credential changed")

// Account is a ChatGPT subscription account plus its OAuth credentials.
type Account struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Email         string `json:"email"`
	AccountID     string `json:"account_id"`
	PlanType      string `json:"plan_type"`
	AccessToken   string `json:"-"`
	RefreshToken  string `json:"-"`
	IDToken       string `json:"-"`
	ExpiresAt     int64  `json:"expires_at"`
	Enabled       bool   `json:"enabled"`
	Weight        int    `json:"weight"`
	Concurrency   int    `json:"concurrency"`
	ProxyURL      string `json:"proxy_url"`
	ProxyID       *int64 `json:"proxy_id"`
	Status        string `json:"status"`
	LastError     string `json:"last_error"`
	LastRefreshAt int64  `json:"last_refresh_at"`
	LastUsedAt    int64  `json:"last_used_at"`
	RequestCount  int64  `json:"request_count"`
	ErrorCount    int64  `json:"error_count"`
	// QuotaJSON holds the last quota snapshot (see internal/quota.Snapshot).
	QuotaJSON      string `json:"-"`
	QuotaUpdatedAt int64  `json:"quota_updated_at"`
	QuotaError     string `json:"quota_error"`

	// OAuthClientID is the client id OpenAI issued for this account's Sign in with
	// ChatGPT registration. Refreshing the credential requires it.
	OAuthClientID string `json:"-"`
	// UpstreamProtocol selects the upstream transport for model traffic: sse | ws.
	// Empty means the global default.
	UpstreamProtocol string `json:"upstream_protocol"`

	// Optional Codex credential for quota reads and Codex model catalog sync.
	// Generation continues to use the primary ChatGPT credential.
	CodexAccessToken  string `json:"-"`
	CodexRefreshToken string `json:"-"`
	CodexIDToken      string `json:"-"`
	CodexExpiresAt    int64  `json:"codex_expires_at"`
	CodexAccountID    string `json:"codex_account_id"`

	// Scheduling state is persisted separately from management/credential edits.
	ConsecutiveFailures int     `json:"consecutive_failures"`
	Cooldown429Seconds  int     `json:"cooldown_429_seconds"`
	CooldownUntil       int64   `json:"cooldown_until"`
	CooldownKind        string  `json:"cooldown_kind"`
	LastStartedAt       int64   `json:"last_started_at"`
	EWMAFirstOutputMS   float64 `json:"ewma_first_output_ms"`
	EWMAFailureRateBP   int     `json:"ewma_failure_rate_bp"`

	GroupIDs                   []int64                     `json:"group_ids"`
	DisabledModels             []string                    `json:"disabled_models"`
	SupplementalModels         []string                    `json:"supplemental_models"`
	InheritedModelRestrictions []InheritedModelRestriction `json:"inherited_model_restrictions"`

	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// InheritedModelRestriction names the enabled groups that prohibit a model.
type InheritedModelRestriction struct {
	Model      string   `json:"model"`
	GroupNames []string `json:"group_names"`
}

// CodexLinked reports whether the optional credential required by quota reads
// and Codex model catalog synchronization is attached.
func (a *Account) CodexLinked() bool {
	return a != nil && strings.TrimSpace(a.CodexRefreshToken) != ""
}

const accountColumns = `id, name, email, account_id, plan_type, access_token, refresh_token, id_token,
	expires_at, enabled, weight, concurrency, proxy_url, status, last_error, last_refresh_at,
	last_used_at, request_count, error_count, quota_json, quota_updated_at, quota_error,
	proxy_id, oauth_client_id, upstream_protocol,
	codex_access_token, codex_refresh_token, codex_id_token, codex_expires_at, codex_account_id,
	cooldown_429_seconds, consecutive_failures, cooldown_until, cooldown_kind, last_started_at, ewma_first_output_ms, ewma_failure_rate_bp,
	created_at, updated_at, disabled_models, supplemental_models,
	(SELECT COALESCE(json_group_array(group_id), '[]') FROM
	 (SELECT group_id FROM account_group_accounts WHERE account_id=accounts.id ORDER BY group_id)),
	(SELECT COALESCE(json_group_array(json_object('name',g.name,'models',json(g.disabled_models))), '[]')
	 FROM account_groups g JOIN account_group_accounts ga ON ga.group_id=g.id
	 WHERE ga.account_id=accounts.id AND g.enabled=1)`

func scanAccount(row interface{ Scan(...any) error }) (*Account, error) {
	var a Account
	var enabled int
	var modelsJSON, supplementalJSON, groupIDsJSON, inheritedJSON string
	err := row.Scan(&a.ID, &a.Name, &a.Email, &a.AccountID, &a.PlanType, &a.AccessToken, &a.RefreshToken,
		&a.IDToken, &a.ExpiresAt, &enabled, &a.Weight, &a.Concurrency, &a.ProxyURL, &a.Status,
		&a.LastError, &a.LastRefreshAt, &a.LastUsedAt, &a.RequestCount, &a.ErrorCount,
		&a.QuotaJSON, &a.QuotaUpdatedAt, &a.QuotaError,
		&a.ProxyID, &a.OAuthClientID, &a.UpstreamProtocol,
		&a.CodexAccessToken, &a.CodexRefreshToken, &a.CodexIDToken, &a.CodexExpiresAt, &a.CodexAccountID,
		&a.Cooldown429Seconds, &a.ConsecutiveFailures, &a.CooldownUntil, &a.CooldownKind, &a.LastStartedAt, &a.EWMAFirstOutputMS, &a.EWMAFailureRateBP,
		&a.CreatedAt, &a.UpdatedAt, &modelsJSON, &supplementalJSON, &groupIDsJSON, &inheritedJSON)
	if err != nil {
		return nil, err
	}
	a.DisabledModels, err = decodeDisabledModels(modelsJSON)
	if err != nil {
		return nil, err
	}
	a.SupplementalModels, err = decodeSupplementalModels(supplementalJSON)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(groupIDsJSON), &a.GroupIDs); err != nil {
		return nil, fmt.Errorf("store: decode account groups: %w", err)
	}
	a.InheritedModelRestrictions, err = decodeInheritedModelRestrictions(inheritedJSON)
	if err != nil {
		return nil, err
	}
	a.Enabled = enabled == 1
	return &a, nil
}

// CreateAccount inserts a new account and returns it with the assigned id.
func (s *Store) CreateAccount(ctx context.Context, a *Account, cooldownOverride ...int) error {
	if a == nil {
		return errors.New("store: nil account")
	}
	models, err := NormalizeDisabledModels(a.DisabledModels)
	if err != nil {
		return err
	}
	a.DisabledModels = models
	supplemental, err := normalizeSupplementalModels(a.SupplementalModels)
	if err != nil {
		return err
	}
	a.SupplementalModels = supplemental
	a.GroupIDs = uniqueIDs(a.GroupIDs)
	if a.GroupIDs == nil {
		a.GroupIDs = []int64{}
	}
	a.InheritedModelRestrictions = []InheritedModelRestriction{}
	modelsJSON, _ := json.Marshal(models)
	supplementalJSON, _ := json.Marshal(supplemental)
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := NowMS()
	if a.CreatedAt == 0 {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	a.Cooldown429Seconds = -1
	if len(cooldownOverride) > 0 {
		if cooldownOverride[0] < -1 || cooldownOverride[0] > 86400 {
			return fmt.Errorf("%w: invalid cooldown override", ErrInvalidPolicy)
		}
		a.Cooldown429Seconds = cooldownOverride[0]
	}
	if a.Concurrency <= 0 {
		a.Concurrency = 3
	}
	if a.Weight <= 0 {
		a.Weight = 1
	}
	if a.Status == "" {
		a.Status = AccountStatusUnknown
	}

	id, err := s.insertID(ctx, tx, `INSERT INTO accounts
		(name, email, account_id, plan_type, access_token, refresh_token, id_token, expires_at,
		 enabled, weight, concurrency, proxy_url, proxy_id, status, last_error, created_at, updated_at,
		 oauth_client_id, upstream_protocol, disabled_models, supplemental_models,
		 codex_access_token,codex_refresh_token,codex_id_token,codex_expires_at,codex_account_id,cooldown_429_seconds)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.Name, a.Email, a.AccountID, a.PlanType, a.AccessToken, a.RefreshToken, a.IDToken, a.ExpiresAt,
		boolToInt(a.Enabled), a.Weight, a.Concurrency, a.ProxyURL, a.ProxyID, a.Status, a.LastError, a.CreatedAt, a.UpdatedAt,
		a.OAuthClientID, a.UpstreamProtocol, string(modelsJSON), string(supplementalJSON),
		a.CodexAccessToken, a.CodexRefreshToken, a.CodexIDToken, a.CodexExpiresAt, a.CodexAccountID, a.Cooldown429Seconds)
	if err != nil {
		return fmt.Errorf("store: create account: %w", err)
	}
	a.ID = id
	if err := replaceAccountGroups(ctx, tx, id, a.GroupIDs); err != nil {
		return err
	}
	if err := changeCatalogRevision(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateAccount persists the mutable fields of an account. It does not write
// supplemental models or catalogs; credentials, health and eligibility changes
// therefore do not invalidate the catalog-only cache (authorization is read live).
func (s *Store) UpdateAccount(ctx context.Context, a *Account) error {
	a.UpdatedAt = NowMS()
	_, err := s.ExecContext(ctx, `UPDATE accounts SET
		name=?, email=?, account_id=?, plan_type=?, access_token=?, refresh_token=?, id_token=?,
		expires_at=?, enabled=?, weight=?, concurrency=?, proxy_url=?, status=?, last_error=?, updated_at=?,
		oauth_client_id=?, upstream_protocol=?
		WHERE id=?`,
		a.Name, a.Email, a.AccountID, a.PlanType, a.AccessToken, a.RefreshToken, a.IDToken,
		a.ExpiresAt, boolToInt(a.Enabled), a.Weight, a.Concurrency, a.ProxyURL, a.Status, a.LastError,
		a.UpdatedAt, a.OAuthClientID, a.UpstreamProtocol, a.ID)
	if err != nil {
		return fmt.Errorf("store: update account: %w", err)
	}
	return nil
}

// UpdateAccountManagementFields updates only fields exposed by the management
// PATCH surface. It deliberately excludes all access/refresh/id tokens, expiry,
// and oauth_client_id so a stale admin snapshot cannot overwrite a live rotation.
// The admin package should call this method instead of UpdateAccount.
type AccountManagementFields struct {
	Name             string
	Enabled          bool
	Weight           int
	Concurrency      int
	ProxyURL         string
	UpstreamProtocol string
}

func (s *Store) UpdateAccountManagementFields(ctx context.Context, id int64, fields AccountManagementFields, proxyIDs ...*int64) error {
	name, enabled, weight, concurrency := fields.Name, fields.Enabled, fields.Weight, fields.Concurrency
	proxyURL, upstreamProtocol := fields.ProxyURL, fields.UpstreamProtocol
	now := NowMS()
	query := `UPDATE accounts SET name=?, enabled=?, weight=?, concurrency=?, upstream_protocol=?, updated_at=?`
	args := []any{name, boolToInt(enabled), weight, concurrency, upstreamProtocol, now}
	// The variadic argument is the compatibility-preserving presence bit: no
	// argument means proxy fields were omitted and must not be written.
	if len(proxyIDs) > 0 {
		query += `, proxy_url=?, proxy_id=?`
		args = append(args, proxyURL, proxyIDs[0])
	}
	query += ` WHERE id=?`
	args = append(args, id)
	res, err := s.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("store: update account management fields: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// AccountProxyPatch is an explicit, atomic replacement of both proxy fields.
// Nil on AccountManagementPatch means neither column may be written.
type AccountProxyPatch struct {
	ID  *int64
	URL string
}

// AccountManagementPatch updates only submitted management fields, never tokens
// or scheduling state. Nil lists preserve policy; empty non-nil lists clear it.
type AccountManagementPatch struct {
	Cooldown429Seconds *int

	Name               *string
	Enabled            *bool
	Weight             *int
	Concurrency        *int
	UpstreamProtocol   *string
	Proxy              *AccountProxyPatch
	GroupIDs           []int64
	DisabledModels     []string
	SupplementalModels []string
}

// normalizeSupplementalModels uses the same identifier limits as model policy.
func normalizeSupplementalModels(models []string) ([]string, error) {
	if len(models) > 512 {
		return nil, fmt.Errorf("%w: at most 512 supplemental models", ErrInvalidPolicy)
	}
	return NormalizeDisabledModels(models)
}

func decodeSupplementalModels(raw string) ([]string, error) {
	var models []string
	if err := json.Unmarshal([]byte(raw), &models); err != nil || models == nil {
		return nil, errors.New("store: invalid persisted supplemental models")
	}
	return normalizeSupplementalModels(models)
}

func (s *Store) PatchAccountManagementFields(ctx context.Context, id int64, patch AccountManagementPatch) error {
	sets, args, err := accountManagementUpdate(patch)
	if err != nil {
		return err
	}
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	args = append(args, id)
	res, err := tx.ExecContext(ctx, "UPDATE accounts SET "+strings.Join(sets, ",")+" WHERE id=?", args...)
	if err != nil {
		return fmt.Errorf("store: patch account: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return sql.ErrNoRows
	}
	if patch.GroupIDs != nil {
		if err := replaceAccountGroups(ctx, tx, id, patch.GroupIDs); err != nil {
			return err
		}
	}
	if patch.SupplementalModels != nil {
		if err := changeCatalogRevision(ctx, tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpdateAccountCredentials writes refreshed tokens.
func (s *Store) UpdateAccountCredentials(ctx context.Context, id int64, access, refresh string, expiresAt int64, status, lastErr string) error {
	_, err := s.ExecContext(ctx, `UPDATE accounts SET
		access_token=?, refresh_token=CASE WHEN ?='' THEN refresh_token ELSE ? END,
		expires_at=?, status=?, last_error=?, last_refresh_at=?, updated_at=?
		WHERE id=?`,
		access, refresh, refresh, expiresAt, status, lastErr, NowMS(), NowMS(), id)
	if err != nil {
		return fmt.Errorf("store: update credentials: %w", err)
	}
	return nil
}

// AttachChatGPTCredential preserves management policy, Codex linkage and usage.
// An explicitly authorized attachment activates a credential-only placeholder.
type ChatGPTCredential struct {
	Email     string
	Plan      string
	Access    string
	Refresh   string
	IDToken   string
	ClientID  string
	ExpiresAt int64
}

func (s *Store) AttachChatGPTCredential(ctx context.Context, id int64, credential ChatGPTCredential) error {
	email, plan, access, refresh := credential.Email, credential.Plan, credential.Access, credential.Refresh
	idToken, clientID, expiresAt := credential.IDToken, credential.ClientID, credential.ExpiresAt
	result, err := s.ExecContext(ctx, `UPDATE accounts SET
	 enabled=CASE WHEN access_token='' THEN 1 ELSE enabled END,
	 email=?,plan_type=?,access_token=?,refresh_token=?,id_token=?,oauth_client_id=?,
	 expires_at=?,status=?,last_error='',updated_at=? WHERE id=?`,
		email, plan, access, refresh, idToken, clientID, expiresAt, AccountStatusReady, NowMS(), id)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SaveCodexCredential explicitly attaches/replaces the Codex credential used for
// quota and model catalogs. Even a same-identity relink invalidates its pending
// catalog generation and old snapshot in the credential transaction.
func (s *Store) SaveCodexCredential(ctx context.Context, id int64, access, refresh, idToken string, expiresAt int64, codexAccountID string) error {
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := lockModelCatalogAccount(ctx, tx, id); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE accounts SET
		codex_access_token=?, codex_refresh_token=?, codex_id_token=?, codex_expires_at=?, codex_account_id=?,
		updated_at=?
		WHERE id=?`,
		access, refresh, idToken, expiresAt, codexAccountID, NowMS(), id)
	if err != nil {
		return fmt.Errorf("store: save codex credential: %w", err)
	}
	if err := clearCodexModelCatalog(ctx, tx, id, true); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateCodexCredentialsIfCurrent atomically rotates the Codex credential only if
// expectedRefresh is still attached. This is the CAS guard that makes unlink safe.
func (s *Store) UpdateCodexCredentialsIfCurrent(ctx context.Context, id int64, expectedRefresh, access, refresh string, expiresAt int64, codexAccountID string) error {
	if strings.TrimSpace(expectedRefresh) == "" {
		return ErrCodexCredentialChanged
	}
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := lockModelCatalogAccount(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCodexCredentialChanged
	}
	if err != nil {
		return err
	}
	if current.CodexRefreshToken != expectedRefresh {
		return ErrCodexCredentialChanged
	}
	result, err := tx.ExecContext(ctx, `UPDATE accounts SET
		codex_access_token=?, codex_refresh_token=CASE WHEN ?='' THEN codex_refresh_token ELSE ? END,
		codex_expires_at=?, codex_account_id=?, updated_at=?
		WHERE id=? AND codex_refresh_token=?`,
		access, refresh, refresh, expiresAt, codexAccountID, NowMS(), id, expectedRefresh)
	if err != nil {
		return fmt.Errorf("store: compare-and-swap Codex credentials: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("store: compare-and-swap Codex credentials count: %w", err)
	} else if affected != 1 {
		return ErrCodexCredentialChanged
	}
	// Normal token renewal keeps both snapshots and the current generation.
	// A changed account identity instead requires a new catalog request.
	if current.CodexAccountID != codexAccountID {
		if err := clearCodexModelCatalog(ctx, tx, id, true); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ClearCodexCredential detaches the optional Codex credential.
func (s *Store) ClearCodexCredential(ctx context.Context, id int64) error {
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := lockModelCatalogAccount(ctx, tx, id)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE accounts SET
		codex_access_token='', codex_refresh_token='', codex_id_token='', codex_expires_at=0, codex_account_id='',
		quota_json='', quota_updated_at=0, quota_error='',
		updated_at=?
		WHERE id=?`, NowMS(), id)
	if err != nil {
		return fmt.Errorf("store: clear codex credential: %w", err)
	}
	bindingChanged := current.CodexAccessToken != "" || current.CodexRefreshToken != "" ||
		current.CodexIDToken != "" || current.CodexAccountID != ""
	if err := clearCodexModelCatalog(ctx, tx, id, bindingChanged); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateAccountOAuthClientID stores the issued client id for the ChatGPT credential.
func (s *Store) UpdateAccountOAuthClientID(ctx context.Context, id int64, clientID string) error {
	_, err := s.ExecContext(ctx, `UPDATE accounts SET oauth_client_id=?, updated_at=? WHERE id=?`,
		clientID, NowMS(), id)
	if err != nil {
		return fmt.Errorf("store: update oauth client id: %w", err)
	}
	return nil
}

// MarkAccountUsed bumps usage counters after a successful request.
func (s *Store) MarkAccountUsed(ctx context.Context, id int64, failed bool) error {
	now := NowMS()
	_, err := s.ExecContext(ctx, `UPDATE accounts SET
		last_used_at=?, request_count=request_count+1,
		error_count=error_count + CASE WHEN ?=1 THEN 1 ELSE 0 END,
		updated_at=?
		WHERE id=?`, now, boolToInt(failed), now, id)
	if err != nil {
		return fmt.Errorf("store: mark account used: %w", err)
	}
	return nil
}

// SetAccountStatus records a status transition (and optionally an error message).
func (s *Store) SetAccountStatus(ctx context.Context, id int64, status, lastErr string) error {
	_, err := s.ExecContext(ctx, `UPDATE accounts SET status=?, last_error=?, updated_at=? WHERE id=?`,
		status, lastErr, NowMS(), id)
	if err != nil {
		return fmt.Errorf("store: set account status: %w", err)
	}
	return nil
}

// RecoverAccount clears health failures so the administrator can retry the
// existing credential. It does not enable disabled accounts or alter quota,
// credentials, lifetime usage, model policies, or in-flight concurrency.
func (s *Store) RecoverAccount(ctx context.Context, id int64) error {
	result, err := s.ExecContext(ctx, `UPDATE accounts SET status=?,last_error='',
 consecutive_failures=0,cooldown_until=0,cooldown_kind='',ewma_failure_rate_bp=0,
 updated_at=? WHERE id=?`, AccountStatusReady, NowMS(), id)
	if err != nil {
		return fmt.Errorf("store: recover account: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("store: account not found")
	}
	return nil
}

// SaveAccountQuotaIfCurrent persists quota only while the same Codex refresh token
// remains attached. An unlink or relink causes a compare-and-swap failure.
func (s *Store) SaveAccountQuotaIfCurrent(ctx context.Context, id int64, expectedRefresh, snapshotJSON, planType, quotaErr string) error {
	if strings.TrimSpace(expectedRefresh) == "" {
		return ErrCodexCredentialChanged
	}
	now := NowMS()
	result, err := s.ExecContext(ctx, `UPDATE accounts SET
		quota_json=?, quota_updated_at=?, quota_error=?,
		plan_type=CASE WHEN ?='' THEN plan_type ELSE ? END,
		updated_at=?
		WHERE id=? AND codex_refresh_token=?`, snapshotJSON, now, quotaErr, planType, planType, now, id, expectedRefresh)
	if err != nil {
		return fmt.Errorf("store: compare-and-swap account quota: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("store: compare-and-swap account quota count: %w", err)
	} else if affected != 1 {
		return ErrCodexCredentialChanged
	}
	return nil
}

// GetAccount loads a single account by id.
func (s *Store) GetAccount(ctx context.Context, id int64) (*Account, error) {
	row := s.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id=?`, id)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get account: %w", err)
	}
	return a, nil
}

// FindAccountByAccountID loads the account owning a ChatGPT account id.
func (s *Store) FindAccountByAccountID(ctx context.Context, accountID string) (*Account, error) {
	if accountID == "" {
		return nil, nil
	}
	row := s.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE account_id=? LIMIT 1`, accountID)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: find account by account id: %w", err)
	}
	return a, nil
}

// ListAccounts returns all accounts ordered by id.
func (s *Store) ListAccounts(ctx context.Context) ([]*Account, error) {
	rows, err := s.QueryContext(ctx, `SELECT `+accountColumns+` FROM accounts ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list accounts: %w", err)
	}
	defer rows.Close()

	var out []*Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan account: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListEnabledAccounts returns enabled accounts ordered by id.
func (s *Store) ListEnabledAccounts(ctx context.Context) ([]*Account, error) {
	rows, err := s.QueryContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE enabled=1 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list enabled accounts: %w", err)
	}
	defer rows.Close()

	var out []*Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan account: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAccountsDueForRefresh returns accounts whose access token expires before the given deadline.
func (s *Store) ListAccountsDueForRefresh(ctx context.Context, deadlineMS int64) ([]*Account, error) {
	rows, err := s.QueryContext(ctx, `SELECT `+accountColumns+`
		FROM accounts
		WHERE enabled=1 AND refresh_token <> '' AND status IN (?, ?, ?) AND expires_at <= ?
		ORDER BY expires_at`, AccountStatusReady, AccountStatusUnknown, AccountStatusExpired, deadlineMS)
	if err != nil {
		return nil, fmt.Errorf("store: list refresh candidates: %w", err)
	}
	defer rows.Close()

	var out []*Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan account: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAccount removes an account by id.
func (s *Store) DeleteAccount(ctx context.Context, id int64) error {
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	changed := false
	for _, query := range []string{
		`DELETE FROM accounts WHERE id=?`,
		`DELETE FROM account_group_accounts WHERE account_id=?`,
		`DELETE FROM account_model_catalog WHERE account_id=?`,
	} {
		res, err := tx.ExecContext(ctx, query, id)
		if err != nil {
			return fmt.Errorf("store: delete account: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n > 0 {
			changed = true
		}
	}
	if changed {
		if err := changeCatalogRevision(ctx, tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func accountManagementUpdate(patch AccountManagementPatch) ([]string, []any, error) {
	sets := []string{"updated_at=?"}
	args := []any{NowMS()}
	if patch.Name != nil {
		name := strings.TrimSpace(*patch.Name)
		if name == "" {
			return nil, nil, fmt.Errorf("%w: account name cannot be empty", ErrInvalidPolicy)
		}
		sets = append(sets, "name=?")
		args = append(args, name)
	}
	if patch.Enabled != nil {
		sets = append(sets, "enabled=?")
		args = append(args, boolToInt(*patch.Enabled))
	}
	var err error
	sets, args, err = appendAccountCapacityUpdate(sets, args, patch)
	if err != nil {
		return nil, nil, err
	}
	if patch.UpstreamProtocol != nil {
		sets = append(sets, "upstream_protocol=?")
		args = append(args, *patch.UpstreamProtocol)
	}
	if patch.Proxy != nil {
		sets = append(sets, "proxy_url=?", "proxy_id=?")
		args = append(args, patch.Proxy.URL, patch.Proxy.ID)
	}
	if patch.DisabledModels != nil {
		models, err := NormalizeDisabledModels(patch.DisabledModels)
		if err != nil {
			return nil, nil, err
		}
		raw, _ := json.Marshal(models)
		sets = append(sets, "disabled_models=?")
		args = append(args, string(raw))
	}
	if patch.SupplementalModels != nil {
		models, err := normalizeSupplementalModels(patch.SupplementalModels)
		if err != nil {
			return nil, nil, err
		}
		raw, _ := json.Marshal(models)
		sets = append(sets, "supplemental_models=?")
		args = append(args, string(raw))
	}
	return sets, args, nil
}

func appendAccountCapacityUpdate(sets []string, args []any, patch AccountManagementPatch) ([]string, []any, error) {
	if patch.Weight != nil {
		if *patch.Weight < 1 {
			return nil, nil, fmt.Errorf("%w: weight must be at least 1", ErrInvalidPolicy)
		}
		sets = append(sets, "weight=?")
		args = append(args, *patch.Weight)
	}
	if patch.Cooldown429Seconds != nil {
		if *patch.Cooldown429Seconds < -1 || *patch.Cooldown429Seconds > 604800 {
			return nil, nil, fmt.Errorf("%w: cooldown_429_seconds must be between -1 and 604800", ErrInvalidPolicy)
		}
		sets = append(sets, "cooldown_429_seconds=?")
		args = append(args, *patch.Cooldown429Seconds)
	}
	if patch.Concurrency != nil {
		if *patch.Concurrency < 1 {
			return nil, nil, fmt.Errorf("%w: concurrency must be at least 1", ErrInvalidPolicy)
		}
		sets = append(sets, "concurrency=?")
		args = append(args, *patch.Concurrency)
	}
	return sets, args, nil
}
