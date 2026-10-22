package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrRuleNotFound = errors.New("store: rule not found")
	ErrRuleConflict = errors.New("store: rule revision conflict")
	ErrRuleInvalid  = errors.New("store: invalid rule change")
)

// RuleConflict reports a stale row revision or ruleset version without overwriting it.
type RuleConflict struct {
	ID       string `json:"id,omitempty"`
	Expected int64  `json:"expected"`
	Actual   int64  `json:"actual"`
}

func (e *RuleConflict) Error() string {
	return fmt.Sprintf("%s: id=%q expected=%d actual=%d", ErrRuleConflict, e.ID, e.Expected, e.Actual)
}
func (e *RuleConflict) Unwrap() error { return ErrRuleConflict }

// RuleRow keeps the editable language separate from server-managed metadata.
// Rule contains only the editable AST; the indexed fields are derived from it.
// MarshalJSON presents one flat rule object to management clients.
type RuleRow struct {
	ID          string          `json:"id"`
	Rule        json.RawMessage `json:"-"`
	Revision    int64           `json:"revision"`
	Name        string          `json:"-"`
	Description string          `json:"-"`
	Phase       string          `json:"-"`
	Enabled     bool            `json:"-"`
	Priority    int             `json:"-"`
	OrderIndex  int64           `json:"order_index"`
	CreatedAt   int64           `json:"created_at"`
	UpdatedAt   int64           `json:"updated_at"`
	LegacyName  string          `json:"legacy_name,omitempty"`
	Source      string          `json:"source,omitempty"`
}

func (r RuleRow) MarshalJSON() ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(r.Rule, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, fmt.Errorf("%w: rule must be an object", ErrRuleInvalid)
	}
	for key, value := range map[string]any{
		"id": r.ID, "revision": r.Revision, "order_index": r.OrderIndex,
		"created_at": ruleTime(r.CreatedAt), "updated_at": ruleTime(r.UpdatedAt),
		"legacy_name": r.LegacyName, "source": r.Source,
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		fields[key] = encoded
	}
	return json.Marshal(fields)
}

func ruleTime(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}

// RuleSetSnapshot is read under one database snapshot, never management pagination.
// Its rules include disabled rules so publication validates the complete candidate.
type RuleSetSnapshot struct {
	Version        int64      `json:"version"`
	Rules          []*RuleRow `json:"rules"`
	LegacyMigrated bool       `json:"legacy_migrated"`
	UpdatedAt      int64      `json:"updated_at"`
	// ChangedIDs is populated only by publication, including implicit neighbors.
	ChangedIDs []string `json:"-"`
}

// RuleSetValidator must compile the complete candidate without external side effects.
// It runs while the publication lock is held; it must not query this Store (a
// single-connection SQLite database would deadlock). A nil validator is rejected.
type RuleSetValidator func(context.Context, *RuleSetSnapshot) error

type RuleListFilter struct {
	Search   string
	Phase    string
	Enabled  *bool
	Page     int
	PageSize int
	Offset   *int64
}

type RulePage struct {
	Rules    []*RuleRow `json:"rules"`
	Total    int64      `json:"total"`
	Page     int        `json:"page"`
	PageSize int        `json:"page_size"`
	Limit    int        `json:"limit"`
	Offset   int64      `json:"offset"`
	Version  int64      `json:"version"`
}

const ruleColumns = `id,rule_json,revision,name,description,phase,enabled,priority,order_index,created_at,updated_at,legacy_name,source`
const ruleOrder = `CASE phase WHEN 'client_request' THEN 0 WHEN 'request_normalize' THEN 1 WHEN 'request' THEN 2 WHEN 'request_finalize' THEN 3 WHEN 'upstream_headers' THEN 4 WHEN 'response_event' THEN 5 WHEN 'response_body' THEN 6 ELSE 7 END,priority,order_index,id`

type ruleScanner interface{ Scan(...any) error }

func scanRule(scanner ruleScanner) (*RuleRow, error) {
	var r RuleRow
	var raw string
	var enabled int
	if err := scanner.Scan(&r.ID, &raw, &r.Revision, &r.Name, &r.Description, &r.Phase, &enabled, &r.Priority, &r.OrderIndex, &r.CreatedAt, &r.UpdatedAt, &r.LegacyName, &r.Source); err != nil {
		return nil, err
	}
	r.Rule, r.Enabled = json.RawMessage(raw), enabled == 1
	return &r, nil
}

func (s *Store) RuleSetVersion(ctx context.Context) (int64, error) {
	var version int64
	err := s.QueryRowContext(ctx, `SELECT version FROM rule_set_metadata WHERE singleton=1`).Scan(&version)
	return version, err
}

func (s *Store) GetRule(ctx context.Context, id string) (*RuleRow, error) {
	r, err := scanRule(s.QueryRowContext(ctx, `SELECT `+ruleColumns+` FROM rules WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// LoadRuleSet reads both the publication version and ALL rows atomically.
func (s *Store) LoadRuleSet(ctx context.Context) (*RuleSetSnapshot, error) {
	tx, err := s.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	snapshot, err := loadRuleSetTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func loadRuleSetTx(ctx context.Context, tx *storeTx) (*RuleSetSnapshot, error) {
	out := &RuleSetSnapshot{Rules: []*RuleRow{}}
	var migrated int
	if err := tx.QueryRowContext(ctx, `SELECT version,legacy_migrated,updated_at FROM rule_set_metadata WHERE singleton=1`).Scan(&out.Version, &migrated, &out.UpdatedAt); err != nil {
		return nil, err
	}
	out.LegacyMigrated = migrated == 1
	rows, err := tx.QueryContext(ctx, `SELECT `+ruleColumns+` FROM rules ORDER BY `+ruleOrder)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out.Rules = append(out.Rules, r)
	}
	return out, rows.Err()
}

// ListRules uses a consistent count/page/version snapshot. Page limits never apply
// to LoadRuleSet or the validator used by publication.
func (s *Store) ListRules(ctx context.Context, filter RuleListFilter) (*RulePage, error) {
	if filter.Page < 1 {
		filter.Page = 1
	}
	if filter.PageSize < 1 {
		filter.PageSize = 50
	}
	if filter.PageSize > 200 {
		filter.PageSize = 200
	}
	if int64(filter.Page-1) > (1<<63-1)/int64(filter.PageSize) {
		return nil, fmt.Errorf("%w: page overflow", ErrRuleInvalid)
	}
	where, args := " WHERE 1=1", []any{}
	if search := strings.TrimSpace(filter.Search); search != "" {
		search = "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(strings.ToLower(search)) + "%"
		where += ` AND (LOWER(name) LIKE ? ESCAPE '\' OR LOWER(description) LIKE ? ESCAPE '\' OR LOWER(id) LIKE ? ESCAPE '\')`
		args = append(args, search, search, search)
	}
	if filter.Phase != "" {
		where += ` AND phase=?`
		args = append(args, filter.Phase)
	}
	if filter.Enabled != nil {
		where += ` AND enabled=?`
		args = append(args, boolToInt(*filter.Enabled))
	}
	tx, err := s.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	offset := int64(filter.Page-1) * int64(filter.PageSize)
	if filter.Offset != nil {
		if *filter.Offset < 0 {
			return nil, fmt.Errorf("%w: offset must not be negative", ErrRuleInvalid)
		}
		offset = *filter.Offset
	}
	out := &RulePage{Rules: []*RuleRow{}, Page: filter.Page, PageSize: filter.PageSize, Limit: filter.PageSize, Offset: offset}
	if err := tx.QueryRowContext(ctx, `SELECT version FROM rule_set_metadata WHERE singleton=1`).Scan(&out.Version); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rules`+where, args...).Scan(&out.Total); err != nil {
		return nil, err
	}
	pageArgs := append(append([]any{}, args...), filter.PageSize, offset)
	rows, err := tx.QueryContext(ctx, `SELECT `+ruleColumns+` FROM rules`+where+` ORDER BY `+ruleOrder+` LIMIT ? OFFSET ?`, pageArgs...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out.Rules = append(out.Rules, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// RuleChange is an atomic mutation. Existing rows always require their revision.
// Kind is create/update/delete/duplicate/patch/move_up/move_down. Patch is limited
// to enabled, priority and order_index; moves select the true phase-local neighbor.
// Duplicate uses ID as its source and NewID as its new ID.
type RuleChange struct {
	Kind             string
	ID               string
	NewID            string
	ExpectedRevision int64
	Rule             json.RawMessage
	Name             string
	Enabled          *bool
	Priority         *int
	OrderIndex       *int64
	LegacyName       string
	Source           string
	// Legacy mirrors a compatible legacy PUT in the same publication transaction.
	// It is accepted only for update of that row's own legacy_name association.
	Legacy *MiddlewareRow
}

// PublishRules serializes writers across processes using the singleton metadata
// row. Its first statement is a write, avoiding SQLite read-to-write upgrades.
// Validation failure rolls back every change, including the publication version.
func (s *Store) PublishRules(ctx context.Context, expectedVersion *int64, changes []RuleChange, validate RuleSetValidator) (*RuleSetSnapshot, error) {
	if validate == nil || len(changes) == 0 {
		return nil, fmt.Errorf("%w: changes and validator are required", ErrRuleInvalid)
	}
	tx, err := s.beginRulePublication(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	snapshot, err := loadRuleSetTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if expectedVersion != nil && *expectedVersion != snapshot.Version {
		return nil, &RuleConflict{Expected: *expectedVersion, Actual: snapshot.Version}
	}
	changed, deleted, err := applyRuleChanges(snapshot, changes)
	if err != nil {
		return nil, err
	}
	snapshot.ChangedIDs = make([]string, 0, len(changed)+len(deleted))
	for _, row := range changed {
		snapshot.ChangedIDs = append(snapshot.ChangedIDs, row.ID)
	}
	snapshot.ChangedIDs = append(snapshot.ChangedIDs, deleted...)
	snapshot.Version++
	snapshot.UpdatedAt = NowMS()
	if err := validate(ctx, snapshot); err != nil {
		return nil, err
	}
	if err := persistRuleChanges(ctx, tx, changed, deleted); err != nil {
		return nil, err
	}
	if err := persistRuleMirrors(ctx, tx, changes, changed); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE rule_set_metadata SET version=?,updated_at=? WHERE singleton=1`, snapshot.Version, snapshot.UpdatedAt); err != nil {
		return nil, err
	}
	if err := persistLanguageV2Marker(ctx, tx, snapshot); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *Store) beginRulePublication(ctx context.Context) (*storeTx, error) {
	// A pooled SQLite connection can encounter transient setup/write contention
	// before busy_timeout takes effect. Retry only lock acquisition, before any
	// publication reads or changes, so revision checks keep their normal meaning.
	for attempt := 0; ; attempt++ {
		tx, err := s.tryBeginRulePublication(ctx)
		if err == nil || s.driver != "sqlite" || !isSQLiteBusy(err) || attempt >= 5 {
			return tx, err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Store) tryBeginRulePublication(ctx context.Context) (*storeTx, error) {
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE rule_set_metadata SET version=version WHERE singleton=1`); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

type ruleChangeBatch struct {
	snapshot  *RuleSetSnapshot
	byID      map[string]*RuleRow
	seen      map[string]bool
	nextOrder int64
	now       int64
	count     int
	changed   []*RuleRow
	deleted   []string
}

func applyRuleChanges(snapshot *RuleSetSnapshot, changes []RuleChange) ([]*RuleRow, []string, error) {
	batch := ruleChangeBatch{snapshot: snapshot, byID: map[string]*RuleRow{}, seen: map[string]bool{}, now: NowMS(), count: len(changes), changed: []*RuleRow{}, deleted: []string{}}
	for _, row := range snapshot.Rules {
		batch.byID[row.ID] = row
		if row.OrderIndex >= batch.nextOrder {
			batch.nextOrder = row.OrderIndex + 1
		}
	}
	for _, change := range changes {
		if err := batch.apply(change); err != nil {
			return nil, nil, err
		}
	}
	snapshot.Rules = make([]*RuleRow, 0, len(batch.byID))
	for _, row := range batch.byID {
		snapshot.Rules = append(snapshot.Rules, row)
	}
	sortRules(snapshot.Rules)
	return batch.changed, batch.deleted, nil
}

func (batch *ruleChangeBatch) apply(change RuleChange) error {
	id := change.ID
	if change.Kind == "create" && id == "" {
		id = uuid.NewString()
	}
	if strings.TrimSpace(id) == "" || batch.seen[id] {
		return fmt.Errorf("%w: empty or repeated id %q", ErrRuleInvalid, id)
	}
	batch.seen[id] = true
	old := batch.byID[id]
	if err := validateRuleChangeRevision(id, old, change); err != nil {
		return err
	}
	switch change.Kind {
	case "move_up", "move_down":
		return batch.move(id, change.Kind == "move_up")
	case "delete":
		delete(batch.byID, id)
		batch.deleted = append(batch.deleted, id)
		return nil
	}
	row := &RuleRow{ID: id, Revision: 1, CreatedAt: batch.now, UpdatedAt: batch.now, OrderIndex: batch.nextOrder, LegacyName: change.LegacyName, Source: change.Source}
	if old != nil {
		*row = *old
		row.Rule = append(json.RawMessage(nil), old.Rule...)
		row.Revision++
		row.UpdatedAt = batch.now
	}
	if err := batch.updateRow(row, old, change); err != nil {
		return err
	}
	if err := applyRulePatch(row, change); err != nil {
		return err
	}
	if err := indexRule(row); err != nil {
		return err
	}
	batch.byID[row.ID] = row
	batch.changed = append(batch.changed, row)
	return nil
}

func validateRuleChangeRevision(id string, old *RuleRow, change RuleChange) error {
	if change.Kind == "create" {
		if old != nil {
			return &RuleConflict{ID: id, Expected: 0, Actual: old.Revision}
		}
		return nil
	}
	if old == nil {
		return fmt.Errorf("%w: %s", ErrRuleNotFound, id)
	}
	if change.ExpectedRevision < 1 {
		return fmt.Errorf("%w: expected_revision is required", ErrRuleInvalid)
	}
	if old.Revision != change.ExpectedRevision {
		return &RuleConflict{ID: id, Expected: change.ExpectedRevision, Actual: old.Revision}
	}
	return nil
}

func (batch *ruleChangeBatch) move(id string, up bool) error {
	if batch.count != 1 {
		return fmt.Errorf("%w: adjacent movement must be a single operation", ErrRuleInvalid)
	}
	moved, err := moveAdjacentRule(batch.snapshot.Rules, id, up, batch.now)
	if err != nil {
		return err
	}
	for _, row := range moved {
		batch.byID[row.ID] = row
		batch.changed = append(batch.changed, row)
	}
	return nil
}

func (batch *ruleChangeBatch) updateRow(row, old *RuleRow, change RuleChange) error {
	switch change.Kind {
	case "create":
		row.Rule = append(json.RawMessage(nil), change.Rule...)
		batch.nextOrder++
	case "update":
		row.Rule = append(json.RawMessage(nil), change.Rule...)
	case "patch":
		if change.Enabled == nil && change.Priority == nil && change.OrderIndex == nil {
			return fmt.Errorf("%w: empty patch", ErrRuleInvalid)
		}
	case "duplicate":
		return batch.duplicate(row, old, change)
	default:
		return fmt.Errorf("%w: unknown change kind %q", ErrRuleInvalid, change.Kind)
	}
	return nil
}

func (batch *ruleChangeBatch) duplicate(row, old *RuleRow, change RuleChange) error {
	row.ID = change.NewID
	if row.ID == "" {
		row.ID = uuid.NewString()
	}
	if batch.byID[row.ID] != nil || batch.seen[row.ID] {
		return fmt.Errorf("%w: duplicate destination id", ErrRuleConflict)
	}
	batch.seen[row.ID] = true
	row.Revision, row.OrderIndex, row.CreatedAt = 1, batch.nextOrder, batch.now
	row.LegacyName, row.Source = "", "duplicate:"+change.ID
	batch.nextOrder++
	name := change.Name
	if name == "" {
		name = old.Name + " (copy)"
	}
	return patchRuleJSON(row, "name", name)
}

func applyRulePatch(row *RuleRow, change RuleChange) error {
	if change.Enabled != nil {
		if err := patchRuleJSON(row, "enabled", *change.Enabled); err != nil {
			return err
		}
	}
	if change.Priority != nil {
		if err := patchRuleJSON(row, "priority", *change.Priority); err != nil {
			return err
		}
	}
	if change.OrderIndex != nil {
		if *change.OrderIndex < 0 || *change.OrderIndex > 9007199254740991 {
			return fmt.Errorf("%w: order_index must be a nonnegative safe integer", ErrRuleInvalid)
		}
		row.OrderIndex = *change.OrderIndex
	}
	return nil
}

// moveAdjacentRule operates on the complete execution order, not a filtered page.
// It exchanges adjacent priorities and normalizes positions in their two groups.
// This also handles duplicate sort keys without letting ID tie-breaking move a
// rule past additional neighbors. Other priorities/phases remain untouched.
func moveAdjacentRule(rows []*RuleRow, id string, up bool, now int64) ([]*RuleRow, error) {
	at := -1
	for i, row := range rows {
		if row.ID == id {
			at = i
			break
		}
	}
	neighbor := at + 1
	if up {
		neighbor = at - 1
	}
	if at < 0 || neighbor < 0 || neighbor >= len(rows) || rows[at].Phase != rows[neighbor].Phase {
		return nil, fmt.Errorf("%w: rule is already at the phase boundary", ErrRuleInvalid)
	}
	a, b := rows[at], rows[neighbor]
	ordered := append([]*RuleRow(nil), rows...)
	ordered[at], ordered[neighbor] = ordered[neighbor], ordered[at]
	return reorderAdjacentRules(ordered, a, b, now)
}

func patchRuleJSON(row *RuleRow, field string, value any) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(row.Rule, &fields); err != nil || fields == nil {
		return fmt.Errorf("%w: rule must be a JSON object", ErrRuleInvalid)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	fields[field] = encoded
	row.Rule, err = json.Marshal(fields)
	return err
}

func indexRule(row *RuleRow) error {
	var header = struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Phase       string `json:"phase"`
		Enabled     bool   `json:"enabled"`
		Priority    int    `json:"priority"`
	}{Phase: "request", Enabled: true, Priority: 100}
	raw := strings.TrimSpace(string(row.Rule))
	if len(raw) == 0 || raw[0] != '{' {
		return fmt.Errorf("%w: rule must be a JSON object", ErrRuleInvalid)
	}
	if err := json.Unmarshal(row.Rule, &header); err != nil {
		return fmt.Errorf("%w: %v", ErrRuleInvalid, err)
	}
	if strings.TrimSpace(header.Name) == "" {
		return fmt.Errorf("%w: name is required", ErrRuleInvalid)
	}
	if header.Phase != "request" && header.Phase != "response_event" && header.Phase != "response_body" && header.Phase != "client_request" && header.Phase != "request_normalize" && header.Phase != "request_finalize" && header.Phase != "upstream_headers" {
		return fmt.Errorf("%w: invalid phase", ErrRuleInvalid)
	}
	row.Name, row.Description, row.Phase = header.Name, header.Description, header.Phase
	row.Enabled, row.Priority = header.Enabled, header.Priority
	return nil
}

func sortRules(rows []*RuleRow) {
	phases := map[string]int{"client_request": 0, "request_normalize": 1, "request": 2, "request_finalize": 3, "upstream_headers": 4, "response_event": 5, "response_body": 6}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Phase != b.Phase {
			return phases[a.Phase] < phases[b.Phase]
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.OrderIndex != b.OrderIndex {
			return a.OrderIndex < b.OrderIndex
		}
		return a.ID < b.ID
	})
}

func persistRuleTx(ctx context.Context, tx *storeTx, r *RuleRow) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO rules (`+ruleColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
	 ON CONFLICT(id) DO UPDATE SET rule_json=excluded.rule_json,revision=excluded.revision,
	 name=excluded.name,description=excluded.description,phase=excluded.phase,enabled=excluded.enabled,
	 priority=excluded.priority,order_index=excluded.order_index,updated_at=excluded.updated_at,
	 legacy_name=excluded.legacy_name,source=excluded.source`,
		r.ID, string(r.Rule), r.Revision, r.Name, r.Description, r.Phase, boolToInt(r.Enabled), r.Priority, r.OrderIndex, r.CreatedAt, r.UpdatedAt, r.LegacyName, r.Source)
	return err
}

func persistLegacyRuleTx(ctx context.Context, tx *storeTx, row *MiddlewareRow) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO middlewares(name,enabled,order_index,config,updated_at) VALUES(?,?,?,?,?)
	 ON CONFLICT(name) DO UPDATE SET enabled=excluded.enabled,order_index=excluded.order_index,config=excluded.config,updated_at=excluded.updated_at`,
		row.Name, boolToInt(row.Enabled), row.OrderIndex, row.Config, NowMS())
	return err
}

// RepairLegacyMiddleware keeps the repair endpoint usable before migration while
// sharing its publication lock. If migration wins the race, no stale legacy-only
// write is accepted and the caller must retry through the migrated-rule adapter.
func (s *Store) RepairLegacyMiddleware(ctx context.Context, row *MiddlewareRow) error {
	if row == nil || row.Name == "" {
		return fmt.Errorf("%w: legacy row is required", ErrRuleInvalid)
	}
	tx, err := s.beginRulePublication(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var migrated int
	if err := tx.QueryRowContext(ctx, `SELECT legacy_migrated FROM rule_set_metadata WHERE singleton=1`).Scan(&migrated); err != nil {
		return err
	}
	if migrated != 0 {
		return fmt.Errorf("%w: legacy rules were migrated; reload before updating", ErrRuleConflict)
	}
	if err := persistLegacyRuleTx(ctx, tx, row); err != nil {
		return err
	}
	return tx.Commit()
}

// LegacyRuleConverter translates the original rows, including missing-row defaults.
// It must be pure. The raw rows are retained in rule_set_metadata for diagnostics.
type LegacyRuleConverter func(context.Context, []*MiddlewareRow) ([]*RuleRow, error)

// InitializeRules migrates exactly once, atomically with its durable marker. A
// deleted ruleset stays empty after restart. Concurrent callers share the same lock.
// New installations should run their existing seed before invoking this method.
func (s *Store) InitializeRules(ctx context.Context, convert LegacyRuleConverter, validate RuleSetValidator) (*RuleSetSnapshot, error) {
	if convert == nil || validate == nil {
		return nil, fmt.Errorf("%w: converter and validator are required", ErrRuleInvalid)
	}
	tx, err := s.beginRulePublication(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	snapshot, err := loadRuleSetTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if snapshot.LegacyMigrated {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return snapshot, nil
	}
	if snapshot.Version != 0 || len(snapshot.Rules) != 0 {
		return nil, fmt.Errorf("%w: cannot import legacy rows into an already published ruleset", ErrRuleConflict)
	}
	rows, err := tx.QueryContext(ctx, `SELECT name,enabled,order_index,config,updated_at FROM middlewares ORDER BY order_index,name`)
	if err != nil {
		return nil, err
	}
	legacy := []*MiddlewareRow{}
	for rows.Next() {
		row := &MiddlewareRow{}
		var enabled int
		if err := rows.Scan(&row.Name, &enabled, &row.OrderIndex, &row.Config, &row.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		row.Enabled = enabled == 1
		legacy = append(legacy, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	original, err := json.Marshal(legacy)
	if err != nil {
		return nil, err
	}
	converted, err := convert(ctx, legacy)
	if err != nil {
		return nil, err
	}
	changes := make([]RuleChange, 0, len(converted))
	for _, row := range converted {
		if row == nil {
			return nil, fmt.Errorf("%w: converter returned nil row", ErrRuleInvalid)
		}
		order := row.OrderIndex
		changes = append(changes, RuleChange{Kind: "create", ID: row.ID, Rule: row.Rule, OrderIndex: &order, LegacyName: row.LegacyName, Source: row.Source})
	}
	changed, _, err := applyRuleChanges(snapshot, changes)
	if err != nil {
		return nil, err
	}
	snapshot.Version, snapshot.LegacyMigrated, snapshot.UpdatedAt = 1, true, NowMS()
	if err := validate(ctx, snapshot); err != nil {
		return nil, err
	}
	for _, row := range changed {
		if err := persistRuleTx(ctx, tx, row); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE rule_set_metadata SET version=?,legacy_migrated=1,legacy_source=?,updated_at=? WHERE singleton=1`, snapshot.Version, string(original), snapshot.UpdatedAt); err != nil {
		return nil, err
	}
	if hasV2Marker(snapshot) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES('rules.language.v2','installed',?) ON CONFLICT(key) DO NOTHING`, NowMS()); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func hasV2Marker(snapshot *RuleSetSnapshot) bool {
	for _, r := range snapshot.Rules {
		if r.ID == "language-v2-installed" {
			return true
		}
	}
	return false
}

// RestoreRulesV2Marker repairs installations predating the durable setting.
// Keep this write out of LoadRuleSet's read-only snapshot transaction and
// serialize it with publication so the marker cannot race a rule deletion.
func (s *Store) RestoreRulesV2Marker(ctx context.Context) (bool, error) {
	tx, err := s.beginRulePublication(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var setting, marker int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key='rules.language.v2'`).Scan(&setting); err != nil {
		return false, err
	}
	if setting == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rules WHERE id='language-v2-installed'`).Scan(&marker); err != nil {
			return false, err
		}
	}
	installed := setting > 0 || marker > 0
	if setting == 0 && installed {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES('rules.language.v2','installed',?) ON CONFLICT(key) DO NOTHING`, NowMS()); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return installed, nil
}

func (s *Store) RulesV2Installed(ctx context.Context) (bool, error) {
	var n int
	err := s.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key='rules.language.v2'`).Scan(&n)
	return n > 0, err
}

func persistRuleChanges(ctx context.Context, tx *storeTx, changed []*RuleRow, deleted []string) error {
	for _, id := range deleted {
		if _, err := tx.ExecContext(ctx, `DELETE FROM rules WHERE id=?`, id); err != nil {
			return err
		}
	}
	for _, row := range changed {
		if err := persistRuleTx(ctx, tx, row); err != nil {
			return err
		}
	}
	return nil
}

func persistRuleMirrors(ctx context.Context, tx *storeTx, changes []RuleChange, changed []*RuleRow) error {
	for _, change := range changes {
		if change.Legacy == nil {
			continue
		}
		var target *RuleRow
		for _, row := range changed {
			if row.ID == change.ID {
				target = row
				break
			}
		}
		if change.Kind != "update" || target == nil || target.LegacyName == "" || target.LegacyName != change.Legacy.Name {
			return fmt.Errorf("%w: legacy mirror must match the updated migration rule", ErrRuleInvalid)
		}
		if err := persistLegacyRuleTx(ctx, tx, change.Legacy); err != nil {
			return err
		}
	}
	return nil
}

func reorderAdjacentRules(ordered []*RuleRow, a, b *RuleRow, now int64) ([]*RuleRow, error) {
	positions := map[int]int64{}
	changed := []*RuleRow{}
	for _, original := range ordered {
		if !ruleAffectedBySwap(original, a, b) {
			continue
		}
		row := *original
		if row.ID == a.ID {
			row.Priority = b.Priority
		} else if row.ID == b.ID {
			row.Priority = a.Priority
		}
		row.OrderIndex = positions[row.Priority]
		positions[row.Priority]++
		if row.ID != a.ID && row.ID != b.ID && row.OrderIndex == original.OrderIndex {
			continue
		}
		row.Revision++
		row.UpdatedAt = now
		if row.Priority != original.Priority {
			if err := patchRuleJSON(&row, "priority", row.Priority); err != nil {
				return nil, err
			}
		}
		changed = append(changed, &row)
	}
	return changed, nil
}

func ruleAffectedBySwap(original, a, b *RuleRow) bool {
	return original.Phase == a.Phase && (original.Priority == a.Priority || original.Priority == b.Priority)
}

func persistLanguageV2Marker(ctx context.Context, tx *storeTx, snapshot *RuleSetSnapshot) error {
	if hasV2Marker(snapshot) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES('rules.language.v2','installed',?) ON CONFLICT(key) DO NOTHING`, NowMS()); err != nil {
			return err
		}
	}
	return nil
}
