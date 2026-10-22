package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

const (
	ModelSourceChatGPT = "chatgpt"
	ModelSourceCodex   = "codex"
)

// CatalogModel is an upstream-advertised or manually supplemented model. Listing
// a model is not proof that the account is authorized to generate with it.
type CatalogModel struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Source      string   `json:"source,omitempty"`
	Origins     []string `json:"origins,omitempty"`

	// Metadata retains the complete upstream model entry, not the response
	// envelope. RawMessage preserves unknown fields, nested values, explicit
	// null/false/empty arrays and numbers larger than float64 can represent.
	// Older snapshots without metadata remain readable.
	Metadata map[string]json.RawMessage `json:"metadata,omitempty"`
}

// NewManualCatalogModel supplies gateway reasoning defaults for a manually
// listed model. These are not verified upstream capabilities; Source identifies
// their origin. Context windows, modalities and tool support stay unspecified.
func NewManualCatalogModel(id string) CatalogModel {
	return CatalogModel{
		ID: id, Name: id, Source: "manual",
		Metadata: map[string]json.RawMessage{
			"supported_reasoning_levels":           json.RawMessage(`[{"effort":"none","description":"None"},{"effort":"minimal","description":"Minimal"},{"effort":"low","description":"Low"},{"effort":"medium","description":"Medium"},{"effort":"high","description":"High"},{"effort":"xhigh","description":"Extra high"},{"effort":"max","description":"Maximum"}]`),
			"default_reasoning_level":              json.RawMessage(`"medium"`),
			"supports_reasoning_summary_parameter": json.RawMessage(`true`),
			"default_reasoning_summary":            json.RawMessage(`"auto"`),
		},
	}
}

// ModelCatalog retains the last successful list and the most recent attempt.
// Timestamps use Unix milliseconds, consistent with the other store APIs.
type ModelCatalog struct {
	Models         []CatalogModel                `json:"models"`
	FetchedAt      int64                         `json:"fetched_at"`
	AttemptedAt    int64                         `json:"attempted_at"`
	Error          string                        `json:"error"`
	SourceCatalogs map[string]ModelCatalogSource `json:"source_catalogs,omitempty"`
}

// ModelCatalogSource is public catalog data only; request fences are stored
// separately and never decoded into this response type.
type ModelCatalogSource struct {
	Models      []CatalogModel `json:"models"`
	FetchedAt   int64          `json:"fetched_at"`
	AttemptedAt int64          `json:"attempted_at"`
	Error       string         `json:"error"`
	Skipped     bool           `json:"skipped,omitempty"`
	SkipReason  string         `json:"skip_reason,omitempty"`
}

// ModelCatalogRefreshAttempt identifies a single refresh independently of clock
// precision. It is an internal coordination DTO, not public catalog state.
type ModelCatalogRefreshAttempt struct {
	Token       string `json:"-"`
	AttemptedAt int64  `json:"-"`
}

// ModelCatalogSourceResult carries a hash of the credentials actually used after
// token refresh. The fingerprint is neither persisted nor exposed as catalog data.
type ModelCatalogSourceResult struct {
	Catalog               ModelCatalogSource `json:"-"`
	CredentialFingerprint string             `json:"-"`
}

// modelCatalogState is the private source_catalogs_json envelope. Only Sources
// reaches public responses; Pending contains opaque request IDs, never OAuth
// tokens, credential hashes, proxies or request headers.
type modelCatalogState struct {
	Sources map[string]ModelCatalogSource `json:"sources"`
	Pending map[string]modelCatalogFence  `json:"pending,omitempty"`
}

type modelCatalogFence struct {
	Token       string `json:"token,omitempty"`
	AttemptedAt int64  `json:"attempted_at"`
}

func modelCatalogSources() [2]string {
	return [2]string{ModelSourceChatGPT, ModelSourceCodex}
}

// ModelCatalogCredentialFingerprint identifies the credentials actually used by
// one source. An unlinked Codex account still has a stable, non-empty identity.
// No credential or fingerprint is written to model catalog storage.
func ModelCatalogCredentialFingerprint(a *Account, source string) string {
	if a == nil {
		return ""
	}
	parts := []string{source, strconv.FormatInt(a.ID, 10)}
	switch source {
	case ModelSourceChatGPT:
		parts = append(parts, a.AccessToken, a.RefreshToken, a.AccountID, a.OAuthClientID)
	case ModelSourceCodex:
		parts = append(parts, a.CodexAccessToken, a.CodexRefreshToken, a.CodexAccountID)
	default:
		return ""
	}
	// Length-delimited JSON avoids ambiguous concatenations, including tokens
	// containing delimiters. These are strings, so Marshal cannot fail.
	raw, _ := json.Marshal(parts)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// GetAccountModelCatalog merges the last successful upstream snapshots with the
// current manual models, or returns nil when the account does not exist.
// Reads never persist manual models or perform upstream requests.
func (s *Store) GetAccountModelCatalog(ctx context.Context, id int64) (*ModelCatalog, error) {
	out := &ModelCatalog{Models: []CatalogModel{}}
	var raw, sourcesRaw, supplementalJSON string
	err := s.QueryRowContext(ctx, `SELECT COALESCE(c.models_json,'[]'),
		COALESCE(c.fetched_at,0), COALESCE(c.attempted_at,0), COALESCE(c.error,''),
		a.supplemental_models, COALESCE(c.source_catalogs_json,'{}')
		FROM accounts a LEFT JOIN account_model_catalog c ON c.account_id=a.id
		WHERE a.id=?`, id).Scan(&raw, &out.FetchedAt, &out.AttemptedAt, &out.Error, &supplementalJSON, &sourcesRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: read account model catalog: %w", err)
	}
	if err := mergeAccountModelCatalogSources(out, raw, sourcesRaw, supplementalJSON); err != nil {
		return nil, err
	}
	return out, nil
}

// accountModelBatchSize bounds both backends below SQLite's legacy 999-variable
// limit, including the extra key ID used by restriction queries.
const accountModelBatchSize = 500

func accountModelBatchArgs(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

// GetAccountModelCatalogs reads one SQL statement per 500 distinct IDs, retaining
// all upstream metadata and the same manual defaults as GetAccountModelCatalog.
// Unknown IDs are omitted; nil/empty input returns a non-nil empty map without SQL.
// No partial result is returned on any database or decoding error.
func (s *Store) GetAccountModelCatalogs(ctx context.Context, ids []int64) (map[int64]*ModelCatalog, error) {
	ids = uniqueIDs(ids)
	out := make(map[int64]*ModelCatalog, len(ids))
	for start := 0; start < len(ids); start += accountModelBatchSize {
		placeholders, args := accountModelBatchArgs(ids[start:min(start+accountModelBatchSize, len(ids))])
		rows, err := s.QueryContext(ctx, `SELECT a.id, COALESCE(c.models_json,'[]'),
			COALESCE(c.fetched_at,0), COALESCE(c.attempted_at,0), COALESCE(c.error,''),
			a.supplemental_models, COALESCE(c.source_catalogs_json,'{}')
			FROM accounts a LEFT JOIN account_model_catalog c ON c.account_id=a.id
			WHERE a.id IN (`+placeholders+`) ORDER BY a.id`, args...)
		if err != nil {
			return nil, fmt.Errorf("store: read account model catalogs: %w", err)
		}
		err = readAccountModelCatalogs(rows, out)
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func readAccountModelCatalogs(rows *sql.Rows, out map[int64]*ModelCatalog) error {
	for rows.Next() {
		var id int64
		var raw, sourcesRaw, supplementalJSON string
		catalog := &ModelCatalog{Models: []CatalogModel{}}
		if err := rows.Scan(&id, &raw, &catalog.FetchedAt, &catalog.AttemptedAt, &catalog.Error, &supplementalJSON, &sourcesRaw); err != nil {
			return fmt.Errorf("store: scan account model catalog: %w", err)
		}
		if err := mergeAccountModelCatalogSources(catalog, raw, sourcesRaw, supplementalJSON); err != nil {
			return err
		}
		out[id] = catalog
	}
	return rows.Err()
}

// mergeAccountModelCatalog also supports callers decoding a pre-source snapshot.
func mergeAccountModelCatalog(out *ModelCatalog, raw, supplementalJSON string) error {
	return mergeAccountModelCatalogSources(out, raw, "{}", supplementalJSON)
}

func mergeAccountModelCatalogSources(out *ModelCatalog, raw, sourcesRaw, supplementalJSON string) error {
	state, err := decodeModelCatalogState(raw, sourcesRaw, out.FetchedAt, out.AttemptedAt, out.Error)
	if err != nil {
		return err
	}
	supplemental, err := decodeSupplementalModels(supplementalJSON)
	if err != nil {
		return err
	}
	*out = *catalogFromSources(state.Sources)
	seen := make(map[string]bool, len(out.Models)+len(supplemental))
	for _, model := range out.Models {
		seen[model.ID] = true
	}
	for _, id := range supplemental {
		if !seen[id] {
			out.Models = append(out.Models, NewManualCatalogModel(id))
			seen[id] = true
		}
	}
	return nil
}

func decodeModelCatalogState(raw, sourcesRaw string, fetched, attempted int64, lastError string) (*modelCatalogState, error) {
	// models_json remains part of the persisted catalog contract, including for
	// dual-source rows. Never hide a corrupt legacy projection behind valid sources.
	var legacy []CatalogModel
	if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
		return nil, fmt.Errorf("store: decode account model catalog: %w", err)
	}
	var state modelCatalogState
	if err := json.Unmarshal([]byte(sourcesRaw), &state); err != nil {
		return nil, fmt.Errorf("store: decode model catalog sources: %w", err)
	}
	if state.Sources == nil {
		// Accept both the private envelope and a direct source-name map. The
		// latter keeps hand-migrated/early incremental snapshots readable.
		var err error
		state.Sources, err = directCatalogSources(sourcesRaw)
		if err != nil {
			return nil, err
		}
	}
	if state.Sources == nil {
		// A default {} marks an old single-source snapshot. An explicit empty
		// sources object instead means a source was cleared; do not revive it.
		state.Sources = make(map[string]ModelCatalogSource)
		if fetched > 0 || attempted > 0 || lastError != "" {
			if fetched <= 0 {
				legacy = nil
			}
			state.Sources[ModelSourceChatGPT] = ModelCatalogSource{
				Models: legacy, FetchedAt: fetched, AttemptedAt: attempted, Error: lastError,
			}
		}
	}
	for _, source := range modelCatalogSources() {
		if catalog, exists := state.Sources[source]; exists {
			catalog.Models = normalizeSourceModels(catalog.Models, source)
			state.Sources[source] = catalog
		}
	}
	if state.Pending == nil {
		state.Pending = make(map[string]modelCatalogFence)
	}
	return &state, nil
}

func copyCatalogModel(model CatalogModel) CatalogModel {
	model.Origins = append([]string(nil), model.Origins...)
	if model.Metadata != nil {
		metadata := make(map[string]json.RawMessage, len(model.Metadata))
		for key, value := range model.Metadata {
			metadata[key] = append(json.RawMessage(nil), value...)
		}
		model.Metadata = metadata
	}
	return model
}

func normalizeSourceModels(models []CatalogModel, source string) []CatalogModel {
	out := make([]CatalogModel, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		model.ID = strings.TrimSpace(model.ID)
		if model.Source == "manual" || model.ID == "" || seen[model.ID] {
			continue
		}
		model = copyCatalogModel(model)
		model.Source = "upstream"
		model.Origins = []string{source}
		out = append(out, model)
		seen[model.ID] = true
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func catalogFromSources(sources map[string]ModelCatalogSource) *ModelCatalog {
	out := &ModelCatalog{Models: []CatalogModel{}, SourceCatalogs: make(map[string]ModelCatalogSource)}
	byID := make(map[string]CatalogModel)
	warnings := make([]string, 0, 2)
	for _, source := range modelCatalogSources() {
		catalog, exists := sources[source]
		if !exists {
			continue
		}
		catalog.Models = normalizeSourceModels(catalog.Models, source)
		out.SourceCatalogs[source] = catalog
		out.FetchedAt = max(out.FetchedAt, catalog.FetchedAt)
		out.AttemptedAt = max(out.AttemptedAt, catalog.AttemptedAt)
		if catalog.Error != "" {
			warnings = append(warnings, source+": "+catalog.Error)
		}
		for _, model := range catalog.Models {
			primary, exists := byID[model.ID]
			if !exists {
				byID[model.ID] = copyCatalogModel(model)
				continue
			}
			primary = mergeSourceCatalogModel(primary, model, source)
			byID[model.ID] = primary
		}
	}
	for _, model := range byID {
		out.Models = append(out.Models, model)
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].ID < out.Models[j].ID })
	out.Error = strings.Join(warnings, "; ")
	if len(out.SourceCatalogs) == 1 {
		// Preserve the legacy single-source error string.
		for _, catalog := range out.SourceCatalogs {
			out.Error = catalog.Error
		}
	}
	return out
}

const catalogRevisionSetting = "internal.catalog_revision"

// CatalogRevision is the cross-instance authoritative directory cache epoch.
// It is deliberately read from SQL each time and contains no credentials.
// Databases predating the first catalog mutation have the stable revision "0".
func (s *Store) CatalogRevision(ctx context.Context) (string, error) {
	var revision string
	err := s.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, catalogRevisionSetting).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return "0", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: read catalog revision: %w", err)
	}
	return revision, nil
}

// changeCatalogRevision must run in the very same transaction as the mutation:
// readers can never observe committed catalog data with its previous epoch.
// A random UUID prevents cache reuse across same-millisecond writes/restarts.
func changeCatalogRevision(ctx context.Context, tx *storeTx) error {
	revision, err := uuid.NewRandom()
	if err != nil {
		return fmt.Errorf("store: generate catalog revision: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		catalogRevisionSetting, revision.String(), NowMS())
	if err != nil {
		return fmt.Errorf("store: change catalog revision: %w", err)
	}
	return nil
}

// lockModelCatalogAccount serializes all catalog/credential mutations for an
// account, even before it has a catalog row. SQLite must acquire its write lock
// BEFORE reading, otherwise a deferred transaction could use a stale snapshot.
func lockModelCatalogAccount(ctx context.Context, tx *storeTx, id int64) (*Account, error) {
	if tx.store.driver != "postgres" {
		res, err := tx.ExecContext(ctx, `UPDATE accounts SET updated_at=updated_at WHERE id=?`, id)
		if err != nil {
			return nil, fmt.Errorf("store: lock model catalog account: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return nil, err
		} else if n == 0 {
			return nil, sql.ErrNoRows
		}
	}
	query := `SELECT id, account_id, access_token, refresh_token, id_token, oauth_client_id,
		codex_access_token, codex_refresh_token, codex_id_token, codex_account_id FROM accounts WHERE id=?`
	if tx.store.driver == "postgres" {
		query += " FOR UPDATE"
	}
	var a Account
	err := tx.QueryRowContext(ctx, query, id).Scan(&a.ID, &a.AccountID, &a.AccessToken, &a.RefreshToken,
		&a.IDToken, &a.OAuthClientID, &a.CodexAccessToken, &a.CodexRefreshToken, &a.CodexIDToken, &a.CodexAccountID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("store: lock model catalog credentials: %w", err)
	}
	return &a, nil
}

func readLockedModelCatalog(ctx context.Context, tx *storeTx, id int64) (*modelCatalogState, string, error) {
	query := `SELECT models_json, source_catalogs_json, fetched_at, attempted_at, error
		FROM account_model_catalog WHERE account_id=?`
	if tx.store.driver == "postgres" {
		query += " FOR UPDATE"
	}
	var raw, sourcesRaw, lastError string
	var fetched, attempted int64
	err := tx.QueryRowContext(ctx, query, id).Scan(&raw, &sourcesRaw, &fetched, &attempted, &lastError)
	if errors.Is(err, sql.ErrNoRows) {
		raw, sourcesRaw = "[]", "{}"
	} else if err != nil {
		return nil, "", fmt.Errorf("store: lock model catalog: %w", err)
	}
	state, err := decodeModelCatalogState(raw, sourcesRaw, fetched, attempted, lastError)
	if err != nil {
		return nil, "", err
	}
	before, err := json.Marshal(catalogFromSources(state.Sources))
	if err != nil {
		return nil, "", fmt.Errorf("store: encode previous model catalog: %w", err)
	}
	return state, string(before), nil
}

// writeLockedModelCatalog keeps models_json an ordinary model array for old
// readers. Pending-only writes do not invalidate the public directory cache.
func writeLockedModelCatalog(ctx context.Context, tx *storeTx, id int64, state *modelCatalogState, before string, bindingChanged bool) error {
	catalog := catalogFromSources(state.Sources)
	publicRaw, err := json.Marshal(catalog)
	if err != nil {
		return fmt.Errorf("store: encode model catalog: %w", err)
	}
	sourcesRaw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("store: encode model catalog sources: %w", err)
	}
	models := append([]CatalogModel{}, catalog.Models...)
	for i := range models {
		// Preserve the old upstream-only array representation. Public reads
		// restore Source while manual entries remain strictly read-time data.
		models[i].Source = ""
	}
	modelsRaw, err := json.Marshal(models)
	if err != nil {
		return fmt.Errorf("store: encode model catalog models: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO account_model_catalog
		(account_id, models_json, source_catalogs_json, fetched_at, attempted_at, error) VALUES(?,?,?,?,?,?)
		ON CONFLICT(account_id) DO UPDATE SET models_json=excluded.models_json,
		source_catalogs_json=excluded.source_catalogs_json, fetched_at=excluded.fetched_at,
		attempted_at=excluded.attempted_at, error=excluded.error`,
		id, string(modelsRaw), string(sourcesRaw), catalog.FetchedAt, catalog.AttemptedAt, catalog.Error)
	if err != nil {
		return fmt.Errorf("store: save model catalog sources: %w", err)
	}
	if bindingChanged || string(publicRaw) != before {
		return changeCatalogRevision(ctx, tx)
	}
	return nil
}

// BeginAccountModelCatalogRefresh fences both sources with the same opaque
// request token. It preserves previous snapshots/status and does not invalidate
// the public cache. Newer starts always supersede older work, even in one ms.
func (s *Store) BeginAccountModelCatalogRefresh(ctx context.Context, id int64) (*ModelCatalogRefreshAttempt, error) {
	token, err := uuid.NewRandom()
	if err != nil {
		return nil, fmt.Errorf("store: generate model catalog refresh token: %w", err)
	}
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := lockModelCatalogAccount(ctx, tx, id); err != nil {
		return nil, err
	}
	state, before, err := readLockedModelCatalog(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	attempt := &ModelCatalogRefreshAttempt{Token: token.String(), AttemptedAt: NowMS()}
	for _, source := range modelCatalogSources() {
		state.Pending[source] = modelCatalogFence{Token: attempt.Token, AttemptedAt: attempt.AttemptedAt}
	}
	if err := writeLockedModelCatalog(ctx, tx, id, state, before, false); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return attempt, nil
}

func applyModelCatalogSource(previous ModelCatalogSource, incoming ModelCatalogSource, source string, attempted int64) (ModelCatalogSource, error) {
	incoming.AttemptedAt = attempted
	if incoming.Error != "" || incoming.Skipped {
		incoming.Models = previous.Models
		incoming.FetchedAt = previous.FetchedAt
	} else if incoming.FetchedAt <= 0 {
		return ModelCatalogSource{}, errors.New("store: successful model catalog needs a fetched timestamp")
	}
	if !incoming.Skipped {
		incoming.SkipReason = ""
	}
	incoming.Models = normalizeSourceModels(incoming.Models, source)
	return incoming, nil
}

// SaveAccountModelCatalogRefresh applies independently fenced results against
// current locked credentials. A stale or rebound source cannot block the other
// source. Failed/skipped attempts preserve only that source's last good list.
func (s *Store) SaveAccountModelCatalogRefresh(ctx context.Context, id int64, attempt *ModelCatalogRefreshAttempt, results map[string]ModelCatalogSourceResult) (map[string]bool, error) {
	if attempt == nil || attempt.Token == "" || attempt.AttemptedAt <= 0 {
		return nil, errors.New("store: invalid model catalog refresh attempt")
	}
	applied := make(map[string]bool, len(results))
	for source := range results {
		applied[source] = false
	}
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	account, err := lockModelCatalogAccount(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	state, before, err := readLockedModelCatalog(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	changed := false
	for _, source := range modelCatalogSources() {
		result, exists := results[source]
		fence := state.Pending[source]
		if !exists || fence.Token != attempt.Token || fence.AttemptedAt != attempt.AttemptedAt ||
			result.CredentialFingerprint == "" || result.CredentialFingerprint != ModelCatalogCredentialFingerprint(account, source) {
			continue
		}
		catalog, err := applyModelCatalogSource(state.Sources[source], result.Catalog, source, fence.AttemptedAt)
		if err != nil {
			return nil, err
		}
		state.Sources[source] = catalog
		// Retain the time watermark for the legacy writer, but consume the
		// token so duplicate submissions cannot overwrite an applied result.
		state.Pending[source] = modelCatalogFence{AttemptedAt: fence.AttemptedAt}
		applied[source] = true
		changed = true
	}
	if changed {
		if err := writeLockedModelCatalog(ctx, tx, id, state, before, false); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return applied, nil
}

// SaveAccountModelCatalog is the legacy ChatGPT-only writer. It must not replace
// Codex state. Equal-time legacy writes follow transaction order, while active
// and consumed tokenized refreshes continue to protect their source snapshots.
func (s *Store) SaveAccountModelCatalog(ctx context.Context, id int64, catalog *ModelCatalog) error {
	if err := validateLegacyCatalog(id, catalog); err != nil {
		return err
	}
	attempted := catalog.AttemptedAt
	if attempted <= 0 {
		attempted = NowMS()
	}
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := lockModelCatalogAccount(ctx, tx, id); err != nil {
		// Preserve the old writer's harmless no-op on an account deleted
		// during a fetch. The generation-aware API returns sql.ErrNoRows.
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	state, before, err := readLockedModelCatalog(ctx, tx, id)
	if err != nil {
		return err
	}
	previous := state.Sources[ModelSourceChatGPT]
	fence := state.Pending[ModelSourceChatGPT]
	watermark := max(previous.AttemptedAt, fence.AttemptedAt)
	// Tokenized refreshes fence all older work. Legacy attempts instead track
	// the latest successful list and latest attempt independently: an older
	// success may improve the list without clearing a newer failure.
	if legacyCatalogSuperseded(catalog, previous, fence, attempted, watermark) {
		return tx.Commit()
	}
	if attempted == watermark && attempted <= fence.AttemptedAt {
		// Preserve legacy cache invalidation without letting a same-millisecond
		// legacy attempt overwrite a snapshot protected by a tokenized refresh.
		if err := changeCatalogRevision(ctx, tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	// A merged read can contain Codex-only models. They must never become
	// ChatGPT-owned through the compatibility entry point.
	models := legacyChatGPTModels(catalog.Models)
	incoming, err := applyModelCatalogSource(previous, ModelCatalogSource{
		Models: models, FetchedAt: catalog.FetchedAt, Error: catalog.Error,
	}, ModelSourceChatGPT, attempted)
	if err != nil {
		return err
	}
	if attempted < previous.AttemptedAt {
		incoming.AttemptedAt = previous.AttemptedAt
		incoming.Error = previous.Error
		incoming.Skipped = previous.Skipped
		incoming.SkipReason = previous.SkipReason
	}
	state.Sources[ModelSourceChatGPT] = incoming
	// Pending watermarks belong only to tokenized refreshes. Treating every
	// legacy failure as a fence would discard a late successful snapshot.
	// Identical legacy writes still invalidate caches when their clocks tie.
	if err := writeLockedModelCatalog(ctx, tx, id, state, before, attempted == watermark); err != nil {
		return err
	}
	return tx.Commit()
}

// clearCodexModelCatalog runs only after locking/updating the owning account in
// the same transaction. Removing its pending token also prevents unlink/relink
// ABA races even if the same OAuth credential is attached again.
func clearCodexModelCatalog(ctx context.Context, tx *storeTx, id int64, bindingChanged bool) error {
	state, before, err := readLockedModelCatalog(ctx, tx, id)
	if err != nil {
		return err
	}
	_, hadSource := state.Sources[ModelSourceCodex]
	_, hadPending := state.Pending[ModelSourceCodex]
	if !bindingChanged && !hadSource && !hadPending {
		return nil
	}
	delete(state.Sources, ModelSourceCodex)
	delete(state.Pending, ModelSourceCodex)
	return writeLockedModelCatalog(ctx, tx, id, state, before, bindingChanged)
}

func directCatalogSources(sourcesRaw string) (map[string]ModelCatalogSource, error) {
	var direct map[string]ModelCatalogSource
	if err := json.Unmarshal([]byte(sourcesRaw), &direct); err != nil {
		return nil, fmt.Errorf("store: decode model catalog sources: %w", err)
	}
	if _, hasChatGPT := direct[ModelSourceChatGPT]; hasChatGPT {
		return direct, nil
	} else if _, hasCodex := direct[ModelSourceCodex]; hasCodex {
		return direct, nil
	}
	return nil, nil
}

func mergeSourceCatalogModel(primary, model CatalogModel, source string) CatalogModel {
	// Never replace a present JSON key: false, null, zero, empty
	// arrays and nested objects all remain the primary source's value.
	_, hasName := primary.Metadata["name"]
	_, hasDisplayName := primary.Metadata["display_name"]
	if model.Name != "" && (primary.Name == "" || primary.Name == primary.ID) && !hasName && !hasDisplayName {
		primary.Name = model.Name
	}
	if _, hasDescription := primary.Metadata["description"]; primary.Description == "" && !hasDescription {
		primary.Description = model.Description
	}
	if primary.Metadata == nil && len(model.Metadata) > 0 {
		primary.Metadata = make(map[string]json.RawMessage, len(model.Metadata))
	}
	for key, value := range model.Metadata {
		if _, exists := primary.Metadata[key]; !exists {
			primary.Metadata[key] = append(json.RawMessage(nil), value...)
		}
	}
	primary.Origins = append(primary.Origins, source)
	return primary
}

func legacyChatGPTModels(incoming []CatalogModel) []CatalogModel {
	models := make([]CatalogModel, 0, len(incoming))
	for _, model := range incoming {
		if len(model.Origins) > 0 {
			chatgpt := false
			for _, origin := range model.Origins {
				chatgpt = chatgpt || origin == ModelSourceChatGPT
			}
			if !chatgpt {
				continue
			}
		}
		models = append(models, model)
	}
	return models
}

func legacyCatalogSuperseded(catalog *ModelCatalog, previous ModelCatalogSource, fence modelCatalogFence, attempted, watermark int64) bool {
	return fence.Token != "" ||
		(attempted < watermark && (attempted <= fence.AttemptedAt || catalog.Error != "" || catalog.FetchedAt <= previous.FetchedAt)) ||
		(catalog.Error == "" && catalog.FetchedAt < previous.FetchedAt)
}

func validateLegacyCatalog(id int64, catalog *ModelCatalog) error {
	if catalog == nil || id <= 0 {
		return errors.New("store: invalid account model catalog")
	}
	if catalog.Error == "" && catalog.FetchedAt <= 0 {
		return errors.New("store: successful model catalog needs a fetched timestamp")
	}
	// Validate raw metadata before any stale-attempt or missing-account no-op.
	// Invalid input must return an error, not success or a new cache revision.
	if _, err := json.Marshal(catalog.Models); err != nil {
		return fmt.Errorf("store: encode account model catalog: %w", err)
	}
	return nil
}
