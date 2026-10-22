package store

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Version 3 was already recorded before cooldown/retry columns were added.
// Opening such a database must upgrade it rather than return early at version 3.
func TestPostgresV3CooldownMigration(t *testing.T) {
	dsn := os.Getenv("PI_GATEWAY_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PI_GATEWAY_TEST_POSTGRES_DSN is not set")
	}
	cfg, err := pgx.ParseConfig(dsn)
	requireDataCondition(t, err == nil, "invalid PostgreSQL test configuration")
	admin := stdlib.OpenDB(*cfg)
	defer admin.Close()
	schema := "cooldown_test_" + strings.TrimPrefix(GenerateKey(), "sk-pi-")
	{
		_, err := admin.Exec(`CREATE SCHEMA ` + schema)
		requireDataTrue(t, err == nil, err)
	}
	defer func() {
		{
			_, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
			reportStoreTestFailure(t, err != nil, func() {
				t.Error(err)
			})
		}
	}()
	cfg.RuntimeParams["search_path"] = schema
	registered := stdlib.RegisterConnConfig(cfg)
	defer stdlib.UnregisterConnConfig(registered)
	opts := Options{Driver: "postgres", DSN: registered}
	ctx := context.Background()
	first, err := OpenOptions(opts)
	requireDataNoError(t, err)
	defer first.Close()
	account := &Account{Name: "preserved", AccountID: "preserved-account", AccessToken: "fixture-token", Enabled: true}
	requireDataNoError(t, first.CreateAccount(ctx, account))
	group := &AccountGroup{Name: "preserved-group", Enabled: true, AccountIDs: []int64{account.ID}}
	requireDataNoError(t, first.CreateAccountGroup(ctx, group))
	// Emulate the deployed version 3 schema, preserving user data and membership.
	for _, statement := range []string{
		`ALTER TABLE accounts DROP COLUMN cooldown_429_seconds`,
		`ALTER TABLE account_groups DROP COLUMN switch_on_429`,
		`DELETE FROM schema_migrations`,
		`INSERT INTO schema_migrations(version,applied_at) VALUES(3,1)`,
	} {
		{
			_, err := first.ExecContext(ctx, statement)
			requireDataTrue(t, err == nil, err)
		}
	}
	{
		_, err := first.ListAccounts(ctx)
		requireDataTruef(t, !(err == nil || !strings.Contains(err.Error(), "cooldown_429_seconds")), "legacy fixture did not reproduce missing cooldown column: %v", err)
	}
	requireDataNoError(t, first.Close())
	for pass := 0; pass < 2; pass++ {
		s, err := OpenOptions(opts)
		requireDataTruef(t, err == nil, "upgrade/reopen %d: %v", pass, err)
		defer s.Close()
		accounts, err := s.ListAccounts(ctx)
		reportStoreTestFailure(t, err != nil || len(accounts) != 1, func() {
			t.Fatalf("upgraded account query: count=%d err=%v", len(accounts), err)
		})
		a := accounts[0]
		wantCooldown := -1
		if pass == 1 {
			wantCooldown = 75
		}
		requireDataTruef(t, !(a.ID != account.ID || a.Name != account.Name || a.AccessToken != account.AccessToken || a.Cooldown429Seconds != wantCooldown || len(a.GroupIDs) != 1 || a.GroupIDs[0] != group.ID), "upgrade/reopen changed account state: %+v", a)
		groups, err := s.ListAccountGroups(ctx)
		requireDataTruef(t, !(err != nil || len(groups) != 1 || groups[0].Name != group.Name || groups[0].SwitchOn429 != "inherit"), "upgraded group query: %+v err=%v", groups, err)
		var version, count int64
		{
			err := s.DB().QueryRow(`SELECT MAX(version),COUNT(*) FROM schema_migrations`).Scan(&version, &count)
			requireDataTruef(t, !(err != nil || version != postgresSchemaVersion || count != 2), "upgrade ledger version=%d count=%d err=%v", version, count, err)
		}
		if pass == 0 {
			value := 75
			requireDataNoError(t, s.PatchAccountManagementFields(ctx, account.ID, AccountManagementPatch{Cooldown429Seconds: &value}))
		}
		requireDataNoError(t, s.Close())
	}
}
