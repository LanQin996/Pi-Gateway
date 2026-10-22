package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

type AccountGroup struct {
	SwitchOn429 string `json:"switch_on_429"`

	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	Enabled        bool     `json:"enabled"`
	Notes          string   `json:"notes"`
	AccountIDs     []int64  `json:"account_ids"`
	DisabledModels []string `json:"disabled_models"`
	CreatedAt      int64    `json:"created_at"`
	UpdatedAt      int64    `json:"updated_at"`
}

func scanAccountGroup(row interface{ Scan(...any) error }) (*AccountGroup, error) {
	var g AccountGroup
	var enabled int
	var modelsJSON string
	if err := row.Scan(&g.ID, &g.Name, &enabled, &g.Notes, &g.CreatedAt, &g.UpdatedAt, &modelsJSON, &g.SwitchOn429); err != nil {
		return nil, err
	}
	models, err := decodeDisabledModels(modelsJSON)
	if err != nil {
		return nil, err
	}
	g.Enabled = enabled == 1
	g.AccountIDs = []int64{}
	g.DisabledModels = models
	return &g, nil
}

func (s *Store) ListAccountGroups(ctx context.Context) ([]AccountGroup, error) {
	rows, err := s.QueryContext(ctx, `SELECT id,name,enabled,notes,created_at,updated_at,disabled_models,switch_on_429 FROM account_groups ORDER BY id`)
	if err != nil {
		return nil, err
	}
	out := []AccountGroup{}
	for rows.Next() {
		g, err := scanAccountGroup(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, *g)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].AccountIDs, err = s.accountGroupIDs(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) GetAccountGroup(ctx context.Context, id int64) (*AccountGroup, error) {
	g, err := scanAccountGroup(s.QueryRowContext(ctx, `SELECT id,name,enabled,notes,created_at,updated_at,disabled_models,switch_on_429 FROM account_groups WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	g.AccountIDs, err = s.accountGroupIDs(ctx, id)
	return g, err
}

func (s *Store) accountGroupIDs(ctx context.Context, id int64) ([]int64, error) {
	return queryIDs(ctx, s, `SELECT account_id FROM account_group_accounts WHERE group_id=? ORDER BY account_id`, id)
}

func (s *Store) CreateAccountGroup(ctx context.Context, g *AccountGroup) error {
	if g == nil {
		return errors.New("store: nil account group")
	}
	if g.SwitchOn429 == "" {
		g.SwitchOn429 = "inherit"
	}
	if err := validateSwitchOn429(g.SwitchOn429); err != nil {
		return err
	}
	g.Name = strings.TrimSpace(g.Name)
	g.Notes = strings.TrimSpace(g.Notes)
	if g.Name == "" {
		return fmt.Errorf("%w: group name cannot be empty", ErrInvalidPolicy)
	}
	models, err := NormalizeDisabledModels(g.DisabledModels)
	if err != nil {
		return err
	}
	g.DisabledModels = models
	g.AccountIDs = uniqueIDs(g.AccountIDs)
	if g.AccountIDs == nil {
		g.AccountIDs = []int64{}
	}
	modelsJSON, _ := json.Marshal(models)
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if g.CreatedAt == 0 {
		g.CreatedAt = NowMS()
	}
	g.UpdatedAt = NowMS()
	id, err := s.insertID(ctx, tx, `INSERT INTO account_groups(name,enabled,notes,created_at,updated_at,disabled_models,switch_on_429) VALUES (?,?,?,?,?,?,?)`, g.Name, boolToInt(g.Enabled), g.Notes, g.CreatedAt, g.UpdatedAt, string(modelsJSON), g.SwitchOn429)
	if err != nil {
		return groupWriteError(err)
	}
	g.ID = id
	if err := ensureUniqueGroupName(ctx, tx, g.Name, g.ID); err != nil {
		return err
	}
	if err := replaceGroupAccounts(ctx, tx, g.ID, g.AccountIDs); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpdateAccountGroup(ctx context.Context, g *AccountGroup) error {
	if g == nil {
		return errors.New("store: nil account group")
	}
	return s.PatchAccountGroup(ctx, g.ID, AccountGroupPatch{
		Name: &g.Name, Enabled: &g.Enabled, Notes: &g.Notes, SwitchOn429: &g.SwitchOn429,
		AccountIDs: g.AccountIDs, DisabledModels: g.DisabledModels,
	})
}

// AccountGroupPatch preserves omitted fields; non-nil empty slices clear lists.
type AccountGroupPatch struct {
	SwitchOn429 *string

	Name           *string
	Enabled        *bool
	Notes          *string
	AccountIDs     []int64
	DisabledModels []string
}

func (s *Store) PatchAccountGroup(ctx context.Context, id int64, patch AccountGroupPatch) error {
	sets, args, err := accountGroupUpdate(patch)
	if err != nil {
		return err
	}
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	args = append(args, id)
	res, err := tx.ExecContext(ctx, "UPDATE account_groups SET "+strings.Join(sets, ",")+" WHERE id=?", args...)
	if err != nil {
		return groupWriteError(err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return sql.ErrNoRows
	}
	if patch.Name != nil {
		if err := ensureUniqueGroupName(ctx, tx, strings.TrimSpace(*patch.Name), id); err != nil {
			return err
		}
	}
	if patch.AccountIDs != nil {
		if err := replaceGroupAccounts(ctx, tx, id, patch.AccountIDs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteAccountGroup(ctx context.Context, id int64) error {
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, query := range []string{`DELETE FROM account_groups WHERE id=?`, `DELETE FROM account_group_accounts WHERE group_id=?`, `DELETE FROM api_key_groups WHERE group_id=?`} {
		if _, err := tx.ExecContext(ctx, query, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SetAccountGroupAccounts(ctx context.Context, groupID int64, accountIDs []int64) error {
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Acquire a write lock before validation, avoiding a read-to-write upgrade race.
	res, err := tx.ExecContext(ctx, `UPDATE account_groups SET updated_at=? WHERE id=?`, NowMS(), groupID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return sql.ErrNoRows
	}
	if err := replaceGroupAccounts(ctx, tx, groupID, accountIDs); err != nil {
		return err
	}
	return tx.Commit()
}

func replaceGroupAccounts(ctx context.Context, tx *storeTx, groupID int64, ids []int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM account_group_accounts WHERE group_id=?`, groupID); err != nil {
		return err
	}
	for _, id := range uniqueIDs(ids) {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM accounts WHERE id=?`, id).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: unknown account", ErrInvalidPolicy)
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_group_accounts(group_id,account_id) VALUES (?,?)`, groupID, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) EligibleAccountsForKey(ctx context.Context, key *APIKey) ([]*Account, error) {
	var keyID int64
	if key != nil {
		keyID = key.ID
	}
	rows, err := s.QueryContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE enabled=1 AND (
  NOT EXISTS(SELECT 1 FROM api_key_groups WHERE api_key_id=?) OR
  NOT EXISTS(SELECT 1 FROM account_group_accounts WHERE account_id=accounts.id) OR
  EXISTS(SELECT 1 FROM account_group_accounts ga JOIN account_groups g ON g.id=ga.group_id
   JOIN api_key_groups kg ON kg.group_id=g.id WHERE kg.api_key_id=? AND g.enabled=1 AND ga.account_id=accounts.id)
 ) ORDER BY id`, keyID, keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) ListKeyGroupIDs(ctx context.Context, apiKeyID int64) ([]int64, error) {
	return queryIDs(ctx, s, `SELECT group_id FROM api_key_groups WHERE api_key_id=? ORDER BY group_id`, apiKeyID)
}

func (s *Store) SetKeyGroups(ctx context.Context, apiKeyID int64, groupIDs []int64) error {
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE api_keys SET updated_at=? WHERE id=?`, NowMS(), apiKeyID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return sql.ErrNoRows
	}
	if err := replaceKeyGroups(ctx, tx, apiKeyID, groupIDs); err != nil {
		return err
	}
	return tx.Commit()
}

func replaceKeyGroups(ctx context.Context, tx *storeTx, apiKeyID int64, ids []int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM api_key_groups WHERE api_key_id=?`, apiKeyID); err != nil {
		return err
	}
	for _, id := range uniqueIDs(ids) {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM account_groups WHERE id=?`, id).Scan(&exists); err != nil {
			return fmt.Errorf("store: account group %d: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO api_key_groups(api_key_id,group_id) VALUES (?,?)`, apiKeyID, id); err != nil {
			return err
		}
	}
	return nil
}

var (
	ErrInvalidPolicy     = errors.New("invalid account policy")
	ErrGroupNameConflict = errors.New("an account group with this name already exists")
)

// NormalizeDisabledModels bounds management input and keeps stable, unique IDs.
func NormalizeDisabledModels(models []string) ([]string, error) {
	if len(models) > 512 {
		return nil, fmt.Errorf("%w: at most 512 disabled models", ErrInvalidPolicy)
	}
	seen := map[string]bool{}
	out := []string{}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if len(model) > 256 || strings.ContainsAny(model, "\x00\r\n") {
			return nil, fmt.Errorf("%w: model identifiers must be at most 256 bytes and contain no control lines", ErrInvalidPolicy)
		}
		if !seen[model] {
			seen[model] = true
			out = append(out, model)
		}
	}
	sort.Strings(out)
	return out, nil
}

func decodeDisabledModels(raw string) ([]string, error) {
	// encoding/json silently decodes null array entries into empty strings for
	// []string, which would discard a corrupt restriction and fail open.
	var values []*string
	if err := json.Unmarshal([]byte(raw), &values); err != nil || values == nil {
		return nil, fmt.Errorf("store: invalid persisted model restrictions")
	}
	models := make([]string, len(values))
	for i, value := range values {
		if value == nil {
			return nil, fmt.Errorf("store: invalid persisted model restrictions")
		}
		models[i] = *value
	}
	return NormalizeDisabledModels(models)
}

func decodeInheritedModelRestrictions(raw string) ([]InheritedModelRestriction, error) {
	var groups []struct {
		Name   string          `json:"name"`
		Models json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal([]byte(raw), &groups); err != nil {
		return nil, err
	}
	byModel := map[string][]string{}
	for _, g := range groups {
		models, err := decodeDisabledModels(string(g.Models))
		if err != nil {
			return nil, err
		}
		for _, model := range models {
			byModel[model] = append(byModel[model], g.Name)
		}
	}
	out := make([]InheritedModelRestriction, 0, len(byModel))
	for model, names := range byModel {
		sort.Strings(names)
		out = append(out, InheritedModelRestriction{Model: model, GroupNames: names})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out, nil
}

// DisabledModelsForAccount merges account, enabled membership-group and enabled
// key-group restrictions. It always reads current bindings, never a stale key snapshot.
// Every database/decoding failure is returned to the caller (fail closed).
func (s *Store) DisabledModelsForAccount(ctx context.Context, accountID int64, key *APIKey) ([]string, error) {
	var keyID int64
	if key != nil {
		keyID = key.ID
	}
	rows, err := s.QueryContext(ctx, `SELECT disabled_models FROM accounts WHERE id=?
	 UNION ALL SELECT g.disabled_models FROM account_groups g WHERE g.enabled=1 AND
	 EXISTS(SELECT 1 FROM accounts WHERE id=?) AND (EXISTS(SELECT 1 FROM account_group_accounts ga WHERE ga.group_id=g.id AND ga.account_id=?)
	 OR EXISTS(SELECT 1 FROM api_key_groups kg WHERE kg.group_id=g.id AND kg.api_key_id=?))`, accountID, accountID, accountID, keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	out := []string{}
	count := 0
	for rows.Next() {
		count++
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		models, err := decodeDisabledModels(raw)
		if err != nil {
			return nil, err
		}
		for _, model := range models {
			if !seen[model] {
				seen[model] = true
				out = append(out, model)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, sql.ErrNoRows
	}
	sort.Strings(out)
	return out, nil
}

// DisabledModelsForAccounts merges the current database restrictions for each
// requested account, its enabled membership groups, and the key's enabled groups.
// Only key.ID is used; cached key group bindings are never trusted. Each batch of
// at most 500 distinct IDs uses one SQL statement, with no per-account queries.
// Any unknown ID returns nil, sql.ErrNoRows, matching the single-account method.
// Any malformed applicable policy fails the whole call closed (no partial map).
// Nil/empty input returns a non-nil empty map without consulting the database.
func (s *Store) DisabledModelsForAccounts(ctx context.Context, ids []int64, key *APIKey) (map[int64][]string, error) {
	ids = uniqueIDs(ids)
	out := make(map[int64][]string, len(ids))
	var keyID int64
	if key != nil {
		keyID = key.ID
	}
	for start := 0; start < len(ids); start += accountModelBatchSize {
		placeholders, args := accountModelBatchArgs(ids[start:min(start+accountModelBatchSize, len(ids))])
		args = append(args, keyID)
		rows, err := s.QueryContext(ctx, `WITH selected_accounts AS (
			SELECT id,disabled_models FROM accounts WHERE id IN (`+placeholders+`)
		)
		SELECT a.id,a.disabled_models FROM selected_accounts a
		UNION ALL SELECT a.id,g.disabled_models FROM selected_accounts a
			JOIN account_group_accounts ga ON ga.account_id=a.id
			JOIN account_groups g ON g.id=ga.group_id AND g.enabled=1
		UNION ALL SELECT a.id,g.disabled_models FROM selected_accounts a
			JOIN api_key_groups kg ON kg.api_key_id=?
			JOIN account_groups g ON g.id=kg.group_id AND g.enabled=1`, args...)
		if err != nil {
			return nil, err
		}
		err = readAccountModelRestrictions(rows, out)
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	if len(out) != len(ids) {
		return nil, sql.ErrNoRows
	}
	return out, nil
}

func readAccountModelRestrictions(rows *sql.Rows, out map[int64][]string) error {
	seen := make(map[int64]map[string]bool)
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		models, err := decodeDisabledModels(raw)
		if err != nil {
			return err
		}
		if seen[id] == nil {
			seen[id] = make(map[string]bool, len(models))
			out[id] = []string{}
		}
		for _, model := range models {
			if !seen[id][model] {
				seen[id][model] = true
				out[id] = append(out[id], model)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for id := range seen {
		sort.Strings(out[id])
	}
	return nil
}

func ensureUniqueGroupName(ctx context.Context, tx *storeTx, name string, self int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM account_groups WHERE id<>?`, self)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var existing string
		if err := rows.Scan(&existing); err != nil {
			return err
		}
		if strings.EqualFold(strings.TrimSpace(existing), name) {
			return ErrGroupNameConflict
		}
	}
	return rows.Err()
}

func groupWriteError(err error) error {
	var pgErr *pgconn.PgError
	if strings.Contains(err.Error(), "UNIQUE constraint failed: account_groups.name") ||
		(errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "account_groups_name_key") {
		return ErrGroupNameConflict
	}
	return err
}

func replaceAccountGroups(ctx context.Context, tx *storeTx, accountID int64, ids []int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM account_group_accounts WHERE account_id=?`, accountID); err != nil {
		return err
	}
	for _, id := range uniqueIDs(ids) {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM account_groups WHERE id=?`, id).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: unknown account group", ErrInvalidPolicy)
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_group_accounts(group_id,account_id) VALUES (?,?)`, id, accountID); err != nil {
			return err
		}
	}
	return nil
}

func queryIDs(ctx context.Context, db *Store, query string, id int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, query, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var value int64
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

func validateSwitchOn429(v string) error {
	switch v {
	case "inherit", "enabled", "disabled":
		return nil
	}
	return fmt.Errorf("%w: switch_on_429 must be inherit, enabled or disabled", ErrInvalidPolicy)
}

func accountGroupUpdate(patch AccountGroupPatch) ([]string, []any, error) {
	sets := []string{"updated_at=?"}
	args := []any{NowMS()}
	if patch.SwitchOn429 != nil {
		if err := validateSwitchOn429(*patch.SwitchOn429); err != nil {
			return nil, nil, err
		}
		sets = append(sets, "switch_on_429=?")
		args = append(args, *patch.SwitchOn429)
	}
	if patch.Name != nil {
		name := strings.TrimSpace(*patch.Name)
		if name == "" {
			return nil, nil, fmt.Errorf("%w: group name cannot be empty", ErrInvalidPolicy)
		}
		sets = append(sets, "name=?")
		args = append(args, name)
	}
	if patch.Enabled != nil {
		sets = append(sets, "enabled=?")
		args = append(args, boolToInt(*patch.Enabled))
	}
	if patch.Notes != nil {
		sets = append(sets, "notes=?")
		args = append(args, strings.TrimSpace(*patch.Notes))
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
	return sets, args, nil
}
