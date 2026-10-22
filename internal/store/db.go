// Package store implements SQLite and PostgreSQL persistence.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // Registers the PostgreSQL database/sql driver.
	_ "modernc.org/sqlite"             // Registers the SQLite database/sql driver.
)

// Store wraps a database/sql pool and exposes typed data access.
type Store struct {
	db     *sql.DB
	path   string
	driver string
}

// Options selects a persistence backend. DSN overrides Path for SQLite;
// PostgreSQL requires DSN and never interprets Path as a connection string.
type Options struct {
	Driver string
	DSN    string
	Path   string
	// Zero connection limits retain the default pool of eight connections.
	MaxOpenConns int
	MaxIdleConns int
	// Zero lifetime keeps connections indefinitely. Negative values are invalid.
	ConnMaxLifetime time.Duration
}

// Open opens (creating if needed) the SQLite database at path and applies the schema.
func Open(path string) (*Store, error) {
	return OpenOptions(Options{Path: path})
}

// OpenOptions opens the selected backend and atomically applies its schema.
func OpenOptions(opts Options) (*Store, error) {
	if opts.MaxOpenConns < 0 || opts.MaxIdleConns < 0 || opts.ConnMaxLifetime < 0 {
		return nil, errors.New("store: connection pool options must not be negative")
	}
	driver, err := normalizeStoreDriver(opts.Driver)
	if err != nil {
		return nil, err
	}
	path, dsn, sqlDriver, err := storeDataSource(opts, driver)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(sqlDriver, dsn)
	if err != nil {
		if driver == "postgres" {
			return nil, postgresConnectionError("open", err)
		}
		return nil, fmt.Errorf("store: open %s database: %w", driver, err)
	}
	// A modest default keeps SQLite read concurrency without contention storms.
	maxOpen, maxIdle := opts.MaxOpenConns, opts.MaxIdleConns
	if maxOpen == 0 {
		maxOpen = 8
	}
	if maxIdle == 0 {
		maxIdle = 8
	}
	memorySQLite := driver == "sqlite" && sqliteMemorySource(path)
	lifetime := opts.ConnMaxLifetime
	if memorySQLite {
		// A memory database belongs to one connection; expiring it destroys its schema.
		maxOpen, maxIdle, lifetime = 1, 1, 0
	}
	migrationMaxOpen := maxOpen
	if driver == "sqlite" && !memorySQLite {
		// Legacy migration holds one connection while VACUUM INTO snapshots
		// committed WAL pages through another. A user limit of one must not deadlock.
		migrationMaxOpen = max(maxOpen, 2)
	}
	db.SetMaxOpenConns(migrationMaxOpen)
	db.SetMaxIdleConns(min(maxIdle, migrationMaxOpen))
	db.SetConnMaxLifetime(0) // Migration connections must not expire mid-backup.
	backupPath := ""
	if driver == "sqlite" {
		backupPath = sqliteBackupPath(path)
	}
	s := &Store{db: db, path: backupPath, driver: driver}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	// Restore the requested runtime limits only after all migration work finishes.
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(min(maxIdle, maxOpen))
	db.SetConnMaxLifetime(lifetime)
	return s, nil
}

// sqliteBackupPath strips connection parameters from on-disk snapshot filenames.
func sqliteBackupPath(dsn string) string {
	if sqliteMemorySource(dsn) {
		return ""
	}
	if strings.HasPrefix(dsn, "file:") {
		if u, err := url.Parse(dsn); err == nil {
			path := u.Opaque
			if path == "" {
				path = u.Path
			}
			if strings.HasPrefix(path, "/") && filepath.VolumeName(path[1:]) != "" {
				path = path[1:]
			}
			return filepath.FromSlash(path)
		}
	}
	return strings.SplitN(dsn, "?", 2)[0]
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the native handle for health checks and backend-specific operations.
// Portable SQL must use Store.ExecContext/QueryContext/QueryRowContext instead.
func (s *Store) DB() *sql.DB { return s.db }

const schemaSQL = `
CREATE TABLE IF NOT EXISTS admin_sessions (
 token_hash TEXT PRIMARY KEY,
 username TEXT NOT NULL,
 credential_version TEXT NOT NULL,
 expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_admin_sessions_expiry ON admin_sessions(expires_at);
CREATE TABLE IF NOT EXISTS settings (
 key TEXT PRIMARY KEY,
 value TEXT NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS proxies (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 name TEXT NOT NULL,
 url TEXT NOT NULL,
 last_test_json TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS accounts (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 name TEXT NOT NULL,
 email TEXT NOT NULL DEFAULT '',
 account_id TEXT NOT NULL DEFAULT '',
 plan_type TEXT NOT NULL DEFAULT '',
 access_token TEXT NOT NULL DEFAULT '',
 refresh_token TEXT NOT NULL DEFAULT '',
 id_token TEXT NOT NULL DEFAULT '',
 expires_at INTEGER NOT NULL DEFAULT 0,
 enabled INTEGER NOT NULL DEFAULT 1,
 weight INTEGER NOT NULL DEFAULT 1,
 concurrency INTEGER NOT NULL DEFAULT 3,
 proxy_url TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL DEFAULT 'unknown',
 last_error TEXT NOT NULL DEFAULT '',
 last_refresh_at INTEGER NOT NULL DEFAULT 0,
 last_used_at INTEGER NOT NULL DEFAULT 0,
 request_count INTEGER NOT NULL DEFAULT 0,
 error_count INTEGER NOT NULL DEFAULT 0,
 quota_json TEXT NOT NULL DEFAULT '',
 quota_updated_at INTEGER NOT NULL DEFAULT 0,
 quota_error TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_account_id ON accounts(account_id) WHERE account_id <> '';
CREATE TABLE IF NOT EXISTS account_model_catalog (
 account_id INTEGER PRIMARY KEY,
 models_json TEXT NOT NULL DEFAULT '[]',
 source_catalogs_json TEXT NOT NULL DEFAULT '{}',
 fetched_at INTEGER NOT NULL DEFAULT 0,
 attempted_at INTEGER NOT NULL DEFAULT 0,
 error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS api_keys (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 name TEXT NOT NULL,
 key TEXT NOT NULL UNIQUE,
 enabled INTEGER NOT NULL DEFAULT 1,
 strategy TEXT NOT NULL DEFAULT '',
 transport TEXT NOT NULL DEFAULT '',
 label TEXT NOT NULL DEFAULT '',
 max_concurrency INTEGER NOT NULL DEFAULT 0,
 requests_per_minute INTEGER NOT NULL DEFAULT 0,
 daily_limit_usd REAL NOT NULL DEFAULT 0,
 weekly_limit_usd REAL NOT NULL DEFAULT 0,
 last_used_at INTEGER NOT NULL DEFAULT 0,
 request_count INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_api_keys_key ON api_keys(key);
CREATE TABLE IF NOT EXISTS usage_records (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 request_id TEXT NOT NULL,
 api_key_id INTEGER NOT NULL DEFAULT 0,
 api_key_name TEXT NOT NULL DEFAULT '',
 account_id INTEGER NOT NULL DEFAULT 0,
 account_name TEXT NOT NULL DEFAULT '',
 model TEXT NOT NULL DEFAULT '',
 client_transport TEXT NOT NULL DEFAULT '',
 request_kind TEXT NOT NULL DEFAULT '',
 reasoning_effort TEXT NOT NULL DEFAULT '',
 requested_service_tier TEXT NOT NULL DEFAULT '',
 service_tier TEXT NOT NULL DEFAULT '',
 billing_details TEXT NOT NULL DEFAULT '',
 upstream_transport TEXT NOT NULL DEFAULT '',
 attempt_index INTEGER NOT NULL DEFAULT 0,
 outcome TEXT NOT NULL DEFAULT 'running',
 status_code INTEGER NOT NULL DEFAULT 0,
 error_code TEXT NOT NULL DEFAULT '',
 error_message TEXT NOT NULL DEFAULT '',
 upstream_send_state TEXT NOT NULL DEFAULT '',
 input_tokens INTEGER,
 cached_tokens INTEGER,
 cache_write_tokens INTEGER,
 output_tokens INTEGER,
 reasoning_tokens INTEGER,
 total_tokens INTEGER,
 connect_ms INTEGER NOT NULL DEFAULT 0,
 headers_ms INTEGER NOT NULL DEFAULT 0,
 first_event_ms INTEGER NOT NULL DEFAULT 0,
 first_token_ms INTEGER NOT NULL DEFAULT 0,
 latency_ms INTEGER NOT NULL DEFAULT 0,
 cost_micros INTEGER,
 cost_source TEXT NOT NULL DEFAULT '',
 price_version TEXT NOT NULL DEFAULT '',
 session_id TEXT NOT NULL DEFAULT '',
 started_at INTEGER NOT NULL,
 completed_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_usage_started ON usage_records(started_at DESC);
CREATE INDEX IF NOT EXISTS idx_usage_model ON usage_records(model, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_usage_key ON usage_records(api_key_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_usage_account ON usage_records(account_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_usage_outcome ON usage_records(outcome, id DESC);
CREATE TABLE IF NOT EXISTS official_usage_sync (
 workspace_id TEXT PRIMARY KEY,
 synced_at INTEGER NOT NULL DEFAULT 0,
 attempted_at INTEGER NOT NULL DEFAULT 0,
 error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS official_daily_usage (
 workspace_id TEXT NOT NULL,
 day TEXT NOT NULL,
 credits REAL,
 uncached_input_tokens INTEGER,
 cached_input_tokens INTEGER,
 output_tokens INTEGER,
 total_tokens INTEGER,
 users INTEGER NOT NULL DEFAULT 0,
 threads INTEGER NOT NULL DEFAULT 0,
 turns INTEGER NOT NULL DEFAULT 0,
 settled INTEGER NOT NULL DEFAULT 0,
 raw TEXT NOT NULL DEFAULT '{}',
 synced_at INTEGER NOT NULL,
 PRIMARY KEY(workspace_id, day)
);
CREATE TABLE IF NOT EXISTS account_groups (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 name TEXT NOT NULL UNIQUE,
 enabled INTEGER NOT NULL DEFAULT 1,
 notes TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS account_group_accounts (
 group_id INTEGER NOT NULL,
 account_id INTEGER NOT NULL,
 PRIMARY KEY (group_id, account_id)
);
CREATE TABLE IF NOT EXISTS api_key_groups (
 api_key_id INTEGER NOT NULL,
 group_id INTEGER NOT NULL,
 PRIMARY KEY (api_key_id, group_id)
);
CREATE TABLE IF NOT EXISTS key_charge_events (
 request_id TEXT PRIMARY KEY,
 api_key_id INTEGER NOT NULL,
 cost_micros INTEGER NOT NULL,
 created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS key_budget_windows (
 api_key_id INTEGER NOT NULL,
 period TEXT NOT NULL,
 window_start INTEGER NOT NULL,
 used_micros INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY (api_key_id, period)
);
CREATE TABLE IF NOT EXISTS captures (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 account_id INTEGER NOT NULL DEFAULT 0,
 account_name TEXT NOT NULL DEFAULT '',
 api_key_id INTEGER NOT NULL DEFAULT 0,
 api_key_name TEXT NOT NULL DEFAULT '',
 client_transport TEXT NOT NULL DEFAULT '',
 upstream_transport TEXT NOT NULL DEFAULT '',
 url TEXT NOT NULL DEFAULT '',
 model TEXT NOT NULL DEFAULT '',
 session_id TEXT NOT NULL DEFAULT '',
 status INTEGER NOT NULL DEFAULT 0,
 outcome TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 duration_ms INTEGER NOT NULL DEFAULT 0,
 ttfb_ms INTEGER NOT NULL DEFAULT 0,
 request_headers TEXT NOT NULL DEFAULT '[]',
 request_body TEXT NOT NULL DEFAULT '',
 request_bytes INTEGER NOT NULL DEFAULT 0,
 response_headers TEXT NOT NULL DEFAULT '[]',
 response_frames TEXT NOT NULL DEFAULT '[]',
 rule_traces TEXT NOT NULL DEFAULT '[]',
 rules_version INTEGER NOT NULL DEFAULT 0,
 rules_trace_truncated INTEGER NOT NULL DEFAULT 0,
 rules_trace_omitted INTEGER NOT NULL DEFAULT 0,
 response_text TEXT NOT NULL DEFAULT '',
 response_id TEXT NOT NULL DEFAULT '',
 prompt_tokens INTEGER NOT NULL DEFAULT 0,
 completion_tokens INTEGER NOT NULL DEFAULT 0,
 total_tokens INTEGER NOT NULL DEFAULT 0,
 truncated INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_captures_account ON captures(account_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_captures_created ON captures(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_captures_outcome ON captures(outcome, id DESC);
CREATE TABLE IF NOT EXISTS middlewares (
 name TEXT PRIMARY KEY,
 enabled INTEGER NOT NULL DEFAULT 1,
 order_index INTEGER NOT NULL DEFAULT 0,
 config TEXT NOT NULL DEFAULT '{}',
 updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS rules (
 id TEXT PRIMARY KEY,
 rule_json TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK (revision > 0),
 name TEXT NOT NULL,
 description TEXT NOT NULL DEFAULT '',
 phase TEXT NOT NULL,
 enabled INTEGER NOT NULL DEFAULT 1,
 priority INTEGER NOT NULL DEFAULT 0,
 order_index INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL,
 legacy_name TEXT NOT NULL DEFAULT '',
 source TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_rules_order ON rules(phase,priority,order_index,id);
CREATE INDEX IF NOT EXISTS idx_rules_legacy ON rules(legacy_name);
CREATE TABLE IF NOT EXISTS rule_set_metadata (
 singleton INTEGER PRIMARY KEY CHECK (singleton=1),
 version INTEGER NOT NULL DEFAULT 0,
 legacy_migrated INTEGER NOT NULL DEFAULT 0,
 legacy_source TEXT NOT NULL DEFAULT '[]',
 updated_at INTEGER NOT NULL DEFAULT 0
);
INSERT INTO rule_set_metadata(singleton) VALUES (1) ON CONFLICT(singleton) DO NOTHING;
CREATE TABLE IF NOT EXISTS audit_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 action TEXT NOT NULL,
 detail TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_events(created_at DESC);
`

// migrate retries transient SQLite setup contention between starting processes.
func (s *Store) migrate() error {
	if s.driver == "postgres" {
		return s.migratePostgres()
	}
	const attempts = 6
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		err := s.migrateOnce()
		if err == nil {
			return nil
		}
		if !isSQLiteBusy(err) {
			return err
		}
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
	}
	return lastErr
}

func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked")
}

func (s *Store) migrateOnce() error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: SQLite migration connection: %w", err)
	}
	defer conn.Close()
	// Keep creation and additions under SQLite's cross-process RESERVED write lock.
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("store: begin migration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	legacyKeys, err := hasLegacyKeyColumns(ctx, conn)
	if err != nil {
		return err
	}
	if legacyKeys {
		if err := s.backupBeforeKeyMigration(ctx); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	for table, want := range additiveColumns() {
		if err := ensureColumns(ctx, conn, table, want); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, proxyBindingSchema); err != nil {
		return fmt.Errorf("store: migrate proxy bindings: %w", err)
	}
	if legacyKeys {
		if err := migrateLegacyKeyGroups(ctx, conn); err != nil {
			return fmt.Errorf("store: migrate key groups: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("store: commit migration: %w", err)
	}
	committed = true
	return nil
}

// Columns added after the first release preserve existing data on both backends.
func additiveColumns() map[string]map[string]string {
	return map[string]map[string]string{
		"api_keys": {
			"label":               textEmptyColumn,
			"max_concurrency":     integerZeroColumn,
			"requests_per_minute": integerZeroColumn,
			"daily_limit_usd":     realZeroColumn,
			"weekly_limit_usd":    realZeroColumn,
		},
		"captures": {
			"rule_traces":           textArrayColumn,
			"rules_version":         integerZeroColumn,
			"rules_trace_truncated": integerZeroColumn,
			"rules_trace_omitted":   integerZeroColumn,
		},
		"usage_records":         {"request_kind": textEmptyColumn, "reasoning_effort": textEmptyColumn, "requested_service_tier": textEmptyColumn, "service_tier": textEmptyColumn, "billing_details": textEmptyColumn},
		"account_model_catalog": {"source_catalogs_json": "TEXT NOT NULL DEFAULT '{}'"},
		"account_groups":        {"disabled_models": textArrayColumn, "switch_on_429": "TEXT NOT NULL DEFAULT 'inherit'"},
		"accounts": {
			"cooldown_429_seconds": "INTEGER NOT NULL DEFAULT -1",
			"disabled_models":      textArrayColumn,
			"supplemental_models":  textArrayColumn,
			"proxy_id":             "INTEGER REFERENCES proxies(id) ON DELETE RESTRICT",
			"consecutive_failures": integerZeroColumn,
			"cooldown_until":       integerZeroColumn,
			"cooldown_kind":        textEmptyColumn,
			"last_started_at":      integerZeroColumn,
			"ewma_first_output_ms": realZeroColumn,
			"ewma_failure_rate_bp": integerZeroColumn,
			"quota_json":           textEmptyColumn,
			"quota_updated_at":     integerZeroColumn,
			"quota_error":          textEmptyColumn,
			// Modern ChatGPT registration client id is required for token refresh.
			"oauth_client_id":   textEmptyColumn,
			"upstream_protocol": textEmptyColumn,
			// Optional Codex credentials are used for quota and model catalogs.
			"codex_access_token":  textEmptyColumn,
			"codex_refresh_token": textEmptyColumn,
			"codex_id_token":      textEmptyColumn,
			"codex_expires_at":    integerZeroColumn,
			"codex_account_id":    textEmptyColumn,
		},
	}
}

// ensureColumns adds missing SQLite columns under the migration write lock.
func ensureColumns(ctx context.Context, conn *sql.Conn, table string, want map[string]string) error {
	if err := validateMigrationColumns(table, want); err != nil {
		return err
	}
	rows, err := conn.QueryContext(ctx, `SELECT cid, name, type, "notnull", dflt_value, pk FROM pragma_table_info(?)`, table)
	if err != nil {
		return fmt.Errorf("store: inspect %s: %w", table, err)
	}
	existing := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan %s columns: %w", table, err)
		}
		existing[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for name, definition := range want {
		if existing[name] {
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, name, definition)
		if _, err := conn.ExecContext(ctx, stmt); err != nil && !isDuplicateColumnError(err) {
			return fmt.Errorf("store: add %s.%s: %w", table, name, err)
		}
	}
	return nil
}

func isDuplicateColumnError(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "duplicate column name")
}

// NowMS is the millisecond timestamp helper used across the store.
func NowMS() int64 { return time.Now().UnixMilli() }

// FromMS converts a stored millisecond timestamp to time.Time (zero for 0).
func FromMS(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func normalizeStoreDriver(value string) (string, error) {
	driver := strings.ToLower(strings.TrimSpace(value))
	switch driver {
	case "", "sqlite":
		return "sqlite", nil
	case "postgres", "postgresql", "pgx":
		return "postgres", nil
	default:
		return "", fmt.Errorf("store: unsupported database driver %q", value)
	}
}

func storeDataSource(opts Options, driver string) (string, string, string, error) {
	if driver == "postgres" {
		if strings.TrimSpace(opts.DSN) == "" {
			return "", "", "", errors.New("store: PostgreSQL requires a DSN")
		}
		return opts.Path, opts.DSN, "pgx", nil
	}
	path := opts.Path
	if opts.DSN != "" {
		path = opts.DSN
	}
	if strings.TrimSpace(path) == "" {
		return "", "", "", errors.New("store: empty database path")
	}
	if err := createSQLiteDirectory(path); err != nil {
		return "", "", "", err
	}
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	dsn := path + separator + "_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	return path, dsn, "sqlite", nil
}

func createSQLiteDirectory(path string) error {
	if strings.HasPrefix(path, "file:") || path == sqliteMemoryPath {
		return nil
	}
	dir := filepath.Dir(strings.SplitN(path, "?", 2)[0])
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("store: create data dir: %w", err)
	}
	return nil
}

// SQL identifiers and DDL cannot be bound as query parameters. Only the static
// migration registry may supply them; reject unregistered tables or definitions.
func validateMigrationColumns(table string, want map[string]string) error {
	approved, ok := additiveColumns()[table]
	if !ok {
		return fmt.Errorf("store: unregistered migration table %q", table)
	}
	for name, definition := range want {
		expected, exists := approved[name]
		if !exists || definition != expected {
			return fmt.Errorf("store: unregistered migration column %s.%s", table, name)
		}
	}
	return nil
}

func sqliteMemorySource(path string) bool {
	return strings.HasPrefix(path, sqliteMemoryPath) || strings.Contains(path, "mode=memory") || strings.HasPrefix(path, "file::memory:")
}
