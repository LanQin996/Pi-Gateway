package accounts

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pi-gateway/internal/config"
	"pi-gateway/internal/egress"
	"pi-gateway/internal/oauth"
	"pi-gateway/internal/store"
)

func TestScheduledModelCatalogRefreshUsesEnabledAccountsAndCurrentUserAgent(t *testing.T) {
	var primary, codex atomic.Int32
	var userAgent atomic.Value
	userAgent.Store("first-agent")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/codex/models" {
			codex.Add(1)
			if r.Header.Get("Authorization") != "Bearer codex-token" {
				t.Error("Codex catalog used another credential")
			}
		} else {
			primary.Add(1)
			if r.Header.Get("User-Agent") != userAgent.Load().(string) || r.Header.Get("Authorization") != "Bearer primary-token" {
				t.Error("primary catalog ignored the current user agent or credential")
			}
		}
		fmt.Fprint(w, `{"data":[{"id":"automatic-model"}]}`)
	}))
	defer target.Close()
	st := newAccountsTestStore(t)
	manager := New(st, oauth.NewClient(nil), egress.NewFactory(egress.Options{}), Options{})
	expires := time.Now().Add(time.Hour).UnixMilli()
	account := &store.Account{Name: "enabled", AccessToken: "primary-token", ExpiresAt: expires, Enabled: true}
	disabled := &store.Account{Name: "disabled", AccessToken: "must-not-use", ExpiresAt: expires, Enabled: false}
	for _, a := range []*store.Account{account, disabled} {
		if err := st.CreateAccount(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveCodexCredential(context.Background(), account.ID, "codex-token", "codex-refresh", "", expires, "codex-account"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Upstream.BaseURL = target.URL + "/v1"
	cfg.Models.CodexBaseURL = target.URL + "/codex"
	cfg.Models.CodexClientVersion = config.DefaultCodexClientVersion
	provider := func() string { return userAgent.Load().(string) }
	manager.refreshAllModelCatalogs(context.Background(), 1, cfg, provider, 0)
	userAgent.Store("updated-agent")
	manager.refreshAllModelCatalogs(context.Background(), 1, cfg, provider, 0)
	if primary.Load() != 2 || codex.Load() != 2 {
		t.Fatalf("wrong scheduled request counts: primary=%d codex=%d", primary.Load(), codex.Load())
	}
	snapshot, err := st.GetAccountModelCatalog(context.Background(), account.ID)
	if err != nil || snapshot == nil || len(snapshot.Models) != 1 || snapshot.Models[0].ID != "automatic-model" || snapshot.FetchedAt == 0 {
		t.Fatalf("automatic catalog was not persisted: %+v %v", snapshot, err)
	}
	disabledSnapshot, err := st.GetAccountModelCatalog(context.Background(), disabled.ID)
	if err != nil || disabledSnapshot == nil || disabledSnapshot.AttemptedAt != 0 {
		t.Fatalf("disabled account was refreshed: %+v %v", disabledSnapshot, err)
	}
}

func TestScheduledModelCatalogRefreshBoundsConcurrencyAndCancels(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{}, 4)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer target.Close()
	st := newAccountsTestStore(t)
	manager := New(st, oauth.NewClient(nil), egress.NewFactory(egress.Options{}), Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	for i := 0; i < 4; i++ {
		a := &store.Account{Name: fmt.Sprint(i), AccessToken: "primary", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Enabled: true}
		if err := st.CreateAccount(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	cfg.Upstream.BaseURL = target.URL + "/v1"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		manager.refreshAllModelCatalogs(ctx, 2, cfg, nil, 0)
		close(done)
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("scheduled workers did not start")
		}
	}
	// An overlapping sweep must return without creating any more requests.
	manager.refreshAllModelCatalogs(ctx, 2, cfg, nil, 0)
	if got := calls.Load(); got != 2 {
		t.Fatalf("scheduled refresh exceeded concurrency: %d", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduled refresh did not stop after cancellation")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("cancellation started queued accounts: %d", got)
	}
}

func TestScheduledModelCatalogRefreshLogsFailureAndKeepsCache(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer target.Close()
	st := newAccountsTestStore(t)
	var logs bytes.Buffer
	manager := New(st, oauth.NewClient(nil), egress.NewFactory(egress.Options{}), Options{Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	a := &store.Account{Name: "failure", AccessToken: "primary", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Enabled: true}
	if err := st.CreateAccount(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAccountModelCatalog(context.Background(), a.ID, &store.ModelCatalog{Models: []store.CatalogModel{{ID: "cached"}}, FetchedAt: 123, AttemptedAt: 123}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Upstream.BaseURL = target.URL + "/v1"
	manager.refreshAllModelCatalogs(context.Background(), 1, cfg, nil, time.Hour)
	snapshot, err := st.GetAccountModelCatalog(context.Background(), a.ID)
	if err != nil || snapshot == nil || len(snapshot.Models) != 1 || snapshot.Models[0].ID != "cached" || snapshot.Error == "" {
		t.Fatalf("failed automatic refresh lost cache or error: %+v %v", snapshot, err)
	}
	if !strings.Contains(logs.String(), "scheduled model catalog refresh failed") {
		t.Fatalf("automatic refresh failure was not logged: %s", logs.String())
	}
	logs.Reset()
	manager.refreshAllModelCatalogs(context.Background(), 1, cfg, nil, time.Hour)
	if logs.Len() != 0 {
		t.Fatalf("failed attempts were not throttled: %s", logs.String())
	}
}

func TestScheduledModelCatalogRefreshDiscoversNewAccountsAndCodexLinks(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"models":[]}`)
	}))
	defer target.Close()
	st := newAccountsTestStore(t)
	manager := New(st, oauth.NewClient(nil), egress.NewFactory(egress.Options{}), Options{})
	expires := time.Now().Add(time.Hour).UnixMilli()
	create := func(name string) *store.Account {
		a := &store.Account{Name: name, AccessToken: "primary", ExpiresAt: expires, Enabled: true}
		if err := st.CreateAccount(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	first := create("first")
	cfg := config.Default()
	cfg.Upstream.BaseURL = target.URL + "/v1"
	cfg.Models.CodexBaseURL = target.URL + "/codex"
	cfg.Models.CodexClientVersion = config.DefaultCodexClientVersion
	sweep := func() { manager.refreshAllModelCatalogs(context.Background(), 1, cfg, nil, time.Hour) }
	sweep()
	sweep()
	if calls.Load() != 1 {
		t.Fatal("fresh catalog was fetched again")
	}
	second := create("new-import")
	sweep()
	if calls.Load() != 2 {
		t.Fatal("newly imported account was not discovered")
	}
	if err := st.SaveCodexCredential(context.Background(), first.ID, "codex", "codex-refresh", "", expires, "codex-id"); err != nil {
		t.Fatal(err)
	}
	sweep()
	if calls.Load() != 4 {
		t.Fatalf("new Codex link was not synchronized promptly: requests=%d", calls.Load())
	}
	second.Enabled = false
	if err := st.UpdateAccount(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	sweep()
	if calls.Load() != 4 {
		t.Fatal("fresh or disabled accounts were fetched again")
	}
}

func TestModelCatalogRefresherDisabledDelayedAndUnique(t *testing.T) {
	manager := New(newAccountsTestStore(t), nil, nil, Options{})
	cfg := config.Default()
	manager.StartModelCatalogRefresher(context.Background(), 0, 1, cfg, nil)
	if manager.modelCatalogRefresherRunning {
		t.Fatal("disabled model catalog refresher started")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var providerCalls atomic.Int32
	manager.StartModelCatalogRefresher(ctx, 1, 1, cfg, func() string { providerCalls.Add(1); return "" })
	manager.StartModelCatalogRefresher(ctx, 1, 1, cfg, nil)
	manager.mu.Lock()
	running := manager.modelCatalogRefresherRunning
	manager.mu.Unlock()
	if !running || providerCalls.Load() != 0 {
		t.Fatal("refresher did not delay its first sweep")
	}
	cancel()
	deadline := time.After(time.Second)
	for {
		manager.mu.Lock()
		running = manager.modelCatalogRefresherRunning
		manager.mu.Unlock()
		if !running {
			break
		}
		select {
		case <-deadline:
			t.Fatal("refresher did not stop during startup delay")
		case <-time.After(time.Millisecond):
		}
	}
	if providerCalls.Load() != 0 {
		t.Fatal("canceled startup ran a sweep")
	}
}
