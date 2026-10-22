package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

func hasLegacyKeyColumns(ctx context.Context, conn *sql.Conn) (bool, error) {
	var n int
	err := conn.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('api_keys') WHERE name IN ('scope','account_ids')`).Scan(&n)
	return n != 0, err
}

// A SQLite snapshot includes committed WAL pages, unlike copying the main file.
// The migration connection holds BEGIN IMMEDIATE, preventing concurrent writers;
// this separate reader snapshots the database before any schema changes are made.
func (s *Store) backupBeforeKeyMigration(ctx context.Context) error {
	if s.path == "" || s.path == ":memory:" {
		return nil
	}
	stamp := time.Now().Unix()
	var path string
	for {
		path = fmt.Sprintf("%s.bak-%d", s.path, stamp)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			break
		} else if err != nil {
			return err
		}
		stamp++
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM main INTO ?`, path); err != nil {
		return fmt.Errorf("store: backup before key migration: %w", err)
	}
	return nil
}

func migrateLegacyKeyGroups(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `SELECT id, account_ids FROM api_keys WHERE scope='list' AND trim(account_ids)<>'' ORDER BY id`)
	if err != nil {
		return err
	}
	type binding struct {
		keyID int64
		ids   []int64
	}
	var bindings []binding
	for rows.Next() {
		var b binding
		var raw string
		if err := rows.Scan(&b.keyID, &raw); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(raw), &b.ids); err != nil {
			rows.Close()
			return fmt.Errorf("key %d has invalid legacy account_ids: %w", b.keyID, err)
		}
		b.ids = uniqueIDs(b.ids)
		if len(b.ids) > 0 {
			bindings = append(bindings, b)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	now := NowMS()
	for _, b := range bindings {
		parts := make([]string, len(b.ids))
		for i, id := range b.ids {
			parts[i] = strconv.FormatInt(id, 10)
		}
		name := fmt.Sprintf("migrated:%x", sha256.Sum256([]byte(strings.Join(parts, ","))))
		if _, err := conn.ExecContext(ctx, `INSERT INTO account_groups(name,created_at,updated_at) VALUES (?,?,?) ON CONFLICT(name) DO NOTHING`, name, now, now); err != nil {
			return err
		}
		var groupID int64
		if err := conn.QueryRowContext(ctx, `SELECT id FROM account_groups WHERE name=?`, name).Scan(&groupID); err != nil {
			return err
		}
		for _, id := range b.ids {
			if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO account_group_accounts(group_id,account_id) VALUES (?,?)`, groupID, id); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO api_key_groups(api_key_id,group_id) VALUES (?,?)`, b.keyID, groupID); err != nil {
			return err
		}
	}
	// Preserve the high-water mark too: deleted key IDs may still be referenced
	// by usage and charge history, and must not be reused after rebuilding.
	var sequence int64
	if err := conn.QueryRowContext(ctx, `SELECT coalesce(max(seq),0) FROM sqlite_sequence WHERE name='api_keys'`).Scan(&sequence); err != nil {
		return err
	}
	// Keep the canonical schema in db.go. Rebuilding recreates the UNIQUE(key)
	// autoindex; explicit indexes are recreated after the old table is dropped.
	start := strings.Index(schemaSQL, "CREATE TABLE IF NOT EXISTS api_keys (")
	definition := schemaSQL[start:]
	definition = definition[:strings.Index(definition, ");")+2]
	definition = strings.Replace(definition, "CREATE TABLE IF NOT EXISTS api_keys", "CREATE TABLE api_keys_new", 1)
	if _, err := conn.ExecContext(ctx, definition); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO api_keys_new (`+keyColumns+`) SELECT `+keyColumns+` FROM api_keys`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `DROP TABLE api_keys; ALTER TABLE api_keys_new RENAME TO api_keys; CREATE INDEX idx_api_keys_key ON api_keys(key)`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE sqlite_sequence SET seq=max(seq,?) WHERE name='api_keys'`, sequence); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO sqlite_sequence(name,seq) SELECT 'api_keys',? WHERE NOT EXISTS(SELECT 1 FROM sqlite_sequence WHERE name='api_keys')`, sequence); err != nil {
		return err
	}
	return nil
}

func uniqueIDs(ids []int64) []int64 {
	out := append([]int64{}, ids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	n := 0
	for _, id := range out {
		if n == 0 || out[n-1] != id {
			out[n] = id
			n++
		}
	}
	return out[:n]
}
