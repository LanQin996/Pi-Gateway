package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

const boundaryTableSQL = `CREATE TABLE dialect_boundary_values (value TEXT PRIMARY KEY)`
const boundarySelectSQL = `SELECT value FROM dialect_boundary_values WHERE value=?`
const boundaryInsertSQL = `INSERT INTO dialect_boundary_values(value) VALUES (?)`

// Run through the shared SQLite/PostgreSQL backend contract so all six wrappers
// must preserve argument binding, even for values containing SQL and placeholders.
func exerciseStoreSQLBoundaries(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	_, err := s.ExecContext(ctx, boundaryTableSQL)
	requireDataNoError(t, err)
	const value = `'); DROP TABLE dialect_boundary_values; -- ? $1 "`
	_, err = s.ExecContext(ctx, boundaryInsertSQL, value)
	requireDataNoError(t, err)
	var got string
	requireDataNoError(t, s.QueryRowContext(ctx, boundarySelectSQL, value).Scan(&got))
	if got != value {
		t.Fatalf("store query row changed bound value: %q", got)
	}
	rows, err := s.QueryContext(ctx, boundarySelectSQL, value)
	requireDataNoError(t, err)
	assertBoundaryRows(t, rows, value)
	tx, err := s.BeginTx(ctx, nil)
	requireDataNoError(t, err)
	defer tx.Rollback()
	const replacement = `?'); DELETE FROM dialect_boundary_values; -- $2`
	_, err = tx.ExecContext(ctx, `UPDATE dialect_boundary_values SET value=? WHERE value=?`, replacement, value)
	requireDataNoError(t, err)
	requireDataNoError(t, tx.QueryRowContext(ctx, boundarySelectSQL, replacement).Scan(&got))
	if got != replacement {
		t.Fatalf("transaction query row changed bound value: %q", got)
	}
	rows, err = tx.QueryContext(ctx, boundarySelectSQL, replacement)
	requireDataNoError(t, err)
	assertBoundaryRows(t, rows, replacement)
	requireDataNoError(t, tx.Commit())
	requireDataNoError(t, s.QueryRowContext(ctx, boundarySelectSQL, replacement).Scan(&got))
	if got != replacement {
		t.Fatalf("commit changed bound value: %q", got)
	}
	_, err = s.ExecContext(ctx, `DROP TABLE dialect_boundary_values`)
	requireDataNoError(t, err)
}

func assertBoundaryRows(t *testing.T, rows *sql.Rows, want string) {
	t.Helper()
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("missing bound value: %v", rows.Err())
	}
	var got string
	requireDataNoError(t, rows.Scan(&got))
	if got != want {
		t.Fatalf("query rows changed bound value: %q", got)
	}
	if rows.Next() {
		t.Fatal("bound predicate selected extra rows")
	}
	requireDataNoError(t, rows.Err())
}

func TestStoreTransactionOwnership(t *testing.T) {
	s := openTestStore(t)
	_, err := s.ExecContext(t.Context(), boundaryTableSQL)
	requireDataNoError(t, err)
	t.Run("early return", func(t *testing.T) { testStoreTransactionEarlyReturn(t, s) })
	t.Run("panic", func(t *testing.T) { testStoreTransactionPanic(t, s) })
	t.Run("canceled", func(t *testing.T) { testStoreTransactionCanceled(t, s) })
}

func testStoreTransactionEarlyReturn(t *testing.T, s *Store) {
	t.Helper()
	sentinel := errors.New("abort transaction")
	err := func() error {
		tx, err := s.BeginTx(t.Context(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(t.Context(), boundaryInsertSQL, t.Name()); err != nil {
			return err
		}
		return sentinel
	}()
	if !errors.Is(err, sentinel) {
		t.Fatalf("early return: %v", err)
	}
	assertBoundaryMissing(t, s)
}

func testStoreTransactionPanic(t *testing.T, s *Store) {
	t.Helper()
	sentinel := errors.New("panic transaction")
	func() {
		defer func() {
			if got := recover(); got != sentinel {
				t.Fatalf("panic changed: %v", got)
			}
		}()
		tx, err := s.BeginTx(t.Context(), nil)
		requireDataNoError(t, err)
		defer tx.Rollback()
		_, err = tx.ExecContext(t.Context(), boundaryInsertSQL, t.Name())
		requireDataNoError(t, err)
		panic(sentinel)
	}()
	assertBoundaryMissing(t, s)
}

func testStoreTransactionCanceled(t *testing.T, s *Store) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tx, err := s.BeginTx(ctx, nil)
	requireDataNoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, boundaryInsertSQL, t.Name())
	requireDataNoError(t, err)
	cancel()
	// Either this caller or database/sql's cancellation goroutine owns rollback.
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		t.Fatal(err)
	}
	assertBoundaryMissing(t, s)
	if tx, err := s.BeginTx(ctx, nil); err == nil {
		defer tx.Rollback()
		t.Fatal("canceled BeginTx succeeded")
	} else if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled BeginTx error: %v", err)
	}
}

func assertBoundaryMissing(t *testing.T, s *Store) {
	t.Helper()
	var got string
	if err := s.QueryRowContext(t.Context(), boundarySelectSQL, t.Name()).Scan(&got); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("uncommitted value escaped: %q err=%v", got, err)
	}
}
