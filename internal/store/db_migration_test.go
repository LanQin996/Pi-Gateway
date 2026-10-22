package store

import (
	"context"
	"database/sql"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestOpenAddsUsageRequestKindToOldDatabase(t *testing.T) {
	path := t.TempDir() + "/old-usage.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	oldSchema := strings.ReplaceAll(schemaSQL, " request_kind TEXT NOT NULL DEFAULT '',\n", "")
	for _, column := range []string{"reasoning_effort", "requested_service_tier", "service_tier", "billing_details"} {
		oldSchema = strings.ReplaceAll(oldSchema, " "+column+" TEXT NOT NULL DEFAULT '',\n", "")
	}
	if _, err := db.Exec(oldSchema); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO usage_records(request_id,model,outcome,started_at,completed_at) VALUES('historical','test','succeeded',1,2)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		records, _, err := st.ListUsageRecords(t.Context(), UsageFilter{})
		if err != nil || len(records) != 1 || records[0].RequestID != "historical" || records[0].RequestKind != "" || records[0].ReasoningEffort != "" || records[0].RequestedServiceTier != "" || records[0].ServiceTier != "" || records[0].BillingDetails != "" {
			st.Close()
			t.Fatalf("historical record changed during migration: %+v err=%v", records, err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenMigratesLegacyAccountsColumnsIdempotently(t *testing.T) {
	path := t.TempDir() + "/legacy.db"
	// Registered after t.TempDir, so it runs first: Windows refuses to remove a
	// directory while SQLite's WAL/journal side files are still present.
	t.Cleanup(func() {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			_ = os.Remove(path + suffix)
		}
	})
	db, err := sql.Open("sqlite", path)
	requireDataNoError(t, err)
	_, err = db.Exec(`CREATE TABLE accounts (
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
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO accounts(name, access_token, refresh_token, proxy_url, created_at, updated_at)
		VALUES ('legacy', 'old-access', 'old-refresh', 'http://127.0.0.1:8888', 10, 20)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	requireDataNoError(t, db.Close())

	// Independent pools exercise SQLite's file-level migration lock.
	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			s, err := Open(path)
			if err == nil {
				err = s.Close()
			}
			errs <- err
		}()
	}
	close(start)
	for i := 0; i < 2; i++ {
		{
			err := <-errs
			reportStoreTestFailure(t, err != nil, func() {
				t.Errorf("concurrent migration: %v", err)
			})
		}
	}
	reportStoreTestFailure(t, t.Failed(), func() {
		t.FailNow()
	})

	func() { verifyOpenMigratesLegacyAccountsColumnsIdempotently2(&t, &path) }()

	check, err := sql.Open("sqlite", path)
	requireDataNoError(t, err)
	defer check.Close()
	rows, err := check.Query("PRAGMA table_info(accounts)")
	requireDataNoError(t, err)
	defer rows.Close()
	want := map[string]bool{
		"quota_json": true, "quota_updated_at": true, "quota_error": true,
		"oauth_client_id": true, "upstream_protocol": true, "supplemental_models": true,
		"codex_access_token": true, "codex_refresh_token": true,
		"codex_id_token": true, "codex_expires_at": true, "codex_account_id": true,
	}
	for rows.Next() {
		var cid, notNull, pk int
		var name, ctype string
		var dflt any
		requireDataNoError(t, rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk))
		if want[name] {
			reportStoreTestFailure(t, notNull != 1 || dflt == nil, func() {
				t.Errorf("%s lost NOT NULL DEFAULT: %d, %v", name, notNull, dflt)
			})
		}
		delete(want, name)
	}
	requireDataNoError(t, rows.Err())
	requireDataTruef(t, len(want) == 0, "missing migrated columns: %v", want)
}

// verifyOpenMigratesLegacyAccountsColumnsIdempotently exercises one isolated test phase with the original shared state.
func verifyOpenMigratesLegacyAccountsColumnsIdempotently(capturedT **testing.T, capturedI *int, capturedS **Store, capturedCtx *context.Context, capturedA **Account) {
	(*capturedT).Helper()

	if (*capturedI) == 0 {
		if err := (*capturedS).PatchAccountManagementFields((*capturedCtx), (*capturedA).ID, AccountManagementPatch{SupplementalModels: []string{" manual-b ", "manual-a", "manual-b"}}); err != nil {
			(*capturedS).Close()
			(*capturedT).Fatal(err)
		}
	} else {
		catalog, err := (*capturedS).GetAccountModelCatalog((*capturedCtx), (*capturedA).ID)
		if err != nil || catalog == nil || catalog.FetchedAt != 0 || len(catalog.Models) != 2 || catalog.Models[0].Source != "manual" {
			(*capturedS).Close()
			(*capturedT).Fatalf("reopened manual catalog=%+v err=%v", catalog, err)
		}
	}
}

// verifyOpenMigratesLegacyAccountsColumnsIdempotently2 exercises one isolated test phase with the original shared state.
func verifyOpenMigratesLegacyAccountsColumnsIdempotently2(capturedT **testing.T, capturedPath *string) {
	(*capturedT).Helper()

	for i := 0; i < 2; i++ {
		s, err := Open((*capturedPath))
		reportStoreTestFailure((*capturedT), err != nil, func() {
			(*capturedT).Fatalf("migration pass %d: %v", i+1, err)
		})
		ctx := context.Background()
		a, err := s.GetAccount(ctx, 1)
		if err != nil || a == nil {
			s.Close()
			(*capturedT).Fatalf("legacy account=%+v err=%v", a, err)
		}
		wantModels := []string{}
		if i > 0 {
			wantModels = []string{"manual-a", "manual-b"}
		}
		if a.Name != "legacy" || a.AccessToken != "old-access" || a.RefreshToken != "old-refresh" || a.ProxyURL != "http://127.0.0.1:8888" || a.CreatedAt != 10 || !reflect.DeepEqual(a.SupplementalModels, wantModels) {
			s.Close()
			(*capturedT).Fatalf("migration/reopen changed legacy data or lost models: %+v", a)
		}
		func() { verifyOpenMigratesLegacyAccountsColumnsIdempotently(&(*capturedT), &i, &s, &ctx, &a) }()
		requireDataNoError((*capturedT), s.Close())
	}
}

const (
	migrationTestTable  = "api_keys"
	migrationTestColumn = "label"
)

// A nil connection proves invalid DDL is rejected before any database access.
func TestEnsureColumnsRejectsUnregisteredDDL(t *testing.T) {
	cases := []struct {
		name    string
		table   string
		columns map[string]string
	}{
		{"unregistered table", "unregistered", nil},
		{"injected table", "accounts); DROP TABLE accounts; --", nil},
		{"unregistered column", "accounts", map[string]string{"unregistered": "TEXT"}},
		{"injected column", "accounts", map[string]string{"name; DROP TABLE accounts; --": "TEXT"}},
		{"changed definition", migrationTestTable, map[string]string{migrationTestColumn: "TEXT"}},
		{"injected definition", migrationTestTable, map[string]string{migrationTestColumn: "TEXT; DROP TABLE accounts; --"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ensureColumns(t.Context(), nil, tc.table, tc.columns); err == nil {
				t.Fatal("unregistered migration DDL was accepted")
			}
		})
	}
}

func TestValidateMigrationColumnsAcceptsStaticRegistry(t *testing.T) {
	for table, columns := range additiveColumns() {
		if err := validateMigrationColumns(table, columns); err != nil {
			t.Fatalf("registered migration %s rejected: %v", table, err)
		}
	}
}

func TestEnsureColumnsAddsRegisteredColumnIdempotently(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	requireDataNoError(t, err)
	defer db.Close()
	conn, err := db.Conn(t.Context())
	requireDataNoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), "CREATE TABLE api_keys(id INTEGER PRIMARY KEY); INSERT INTO api_keys(id) VALUES(1)")
	requireDataNoError(t, err)
	for range 2 {
		requireDataNoError(t, ensureColumns(t.Context(), conn, migrationTestTable, map[string]string{migrationTestColumn: textEmptyColumn}))
	}
	var label string
	requireDataNoError(t, conn.QueryRowContext(t.Context(), "SELECT label FROM api_keys WHERE id=1").Scan(&label))
	if label != "" {
		t.Fatalf("registered migration changed default: %q", label)
	}
}
