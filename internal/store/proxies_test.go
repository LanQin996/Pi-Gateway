package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestSavedProxyPersistenceBindingsAndDeleteProtection(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "proxies.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	p := &Proxy{Name: "local proxy", URL: "http://user:password@127.0.0.1:18080"}
	if err := s.CreateProxy(ctx, p); err != nil {
		t.Fatal(err)
	}
	a := &Account{Name: "bound", Enabled: true, ProxyID: &p.ID, ProxyURL: "stale"}
	if err := s.CreateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	assertBinding := func(wantURL string, wantID *int64) {
		t.Helper()
		got, err := s.GetAccount(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ProxyURL != wantURL || (wantID == nil) != (got.ProxyID == nil) || (wantID != nil && *got.ProxyID != *wantID) {
			t.Fatalf("unexpected persistent binding: id=%v url=%q", got.ProxyID, got.ProxyURL)
		}
	}
	assertBinding(p.URL, &p.ID)
	if err := s.DeleteProxy(ctx, p.ID); !errors.Is(err, ErrProxyInUse) {
		t.Fatalf("delete reference protection: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM proxies WHERE id=?", p.ID); err == nil {
		t.Fatal("FK must protect even direct SQL deletion")
	}
	p.URL = "socks5://new-user:new-password@127.0.0.1:19090"
	if err := s.UpdateProxy(ctx, p); err != nil {
		t.Fatal(err)
	}
	assertBinding(p.URL, &p.ID)
	// A stale management/credential snapshot cannot undo the authoritative URL.
	if err := s.UpdateAccountManagementFields(ctx, a.ID, AccountManagementFields{Name: "renamed", Enabled: true, Weight: 1, Concurrency: 3, ProxyURL: "http://old:old@localhost:1"}); err != nil {
		t.Fatal(err)
	}
	assertBinding(p.URL, &p.ID)
	if err := s.SetProxyAccounts(ctx, p.ID, []int64{a.ID, 999999}); err == nil {
		t.Fatal("unknown account must reject the complete assignment")
	}
	assertBinding(p.URL, &p.ID)
	test := &ProxyTest{Success: true, LatencyMS: 12, Status: 200, TestedAt: NowMS()}
	if err := s.UpdateProxyTest(ctx, p.ID, p.URL, test); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateProxyTest(ctx, p.ID, "http://old:1", test); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stale connection test persisted: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	assertBinding(p.URL, &p.ID)
	got, err := s.GetProxy(ctx, p.ID)
	if err != nil || got == nil || got.URL != p.URL || got.AccountCount != 1 || got.LastTest == nil || got.LastTest.LatencyMS != 12 {
		t.Fatalf("proxy did not survive restart: %+v %v", got, err)
	}
	// Clearing requires an explicit assignment update; only then is deletion safe.
	if err := s.SetProxyAccounts(ctx, p.ID, []int64{}); err != nil {
		t.Fatal(err)
	}
	assertBinding("", nil)
	if err := s.DeleteProxy(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetProxy(ctx, p.ID); err != nil || got != nil {
		t.Fatalf("proxy not deleted: %+v %v", got, err)
	}
}

func TestSavedProxyMigrationPreservesLegacyDirectAndCustomEndpoints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the existing schema: all release columns except proxy_id.
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO accounts(name,proxy_url,created_at,updated_at) VALUES('legacy','http://old:secret@localhost:1234',1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := s.GetAccount(context.Background(), 1)
	if err != nil || a == nil || a.ProxyID != nil || a.ProxyURL != "http://old:secret@localhost:1234" {
		t.Fatalf("legacy account changed: %+v %v", a, err)
	}
}
