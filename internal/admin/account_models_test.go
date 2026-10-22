package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"pi-gateway/internal/accounts"
	"pi-gateway/internal/config"
	"pi-gateway/internal/egress"
	"pi-gateway/internal/oauth"
	"pi-gateway/internal/settings"
	"pi-gateway/internal/store"
	"pi-gateway/internal/upstream"
)

func newModelAdmin(t *testing.T, target string) (*Server, *store.Account) {
	t.Helper()
	// Handler fixtures do not need files; persistence/reopen is covered by the
	// store suite. One connection keeps the in-memory database shared by calls.
	st, err := store.Open(":memory:")
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	st.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = st.Close() })
	// Keep the linked credential for generation-isolation tests, but never send
	// its fake token to the default public Codex endpoint during catalog refresh.
	codex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reportTestFailure(t, r.Method != http.MethodGet || r.URL.Path != "/codex/models" ||
			r.Header.Get("Authorization") != "Bearer secret-codex" || r.Header.Get("ChatGPT-Account-Id") != "codex-account" ||
			r.URL.Query().Get("client_version") != config.DefaultCodexClientVersion ||
			r.Header.Get("Originator") != "codex_cli_rs" || r.Header.Get("Version") != config.DefaultCodexClientVersion ||
			r.Header.Get("User-Agent") != "codex-tui/"+config.DefaultCodexClientVersion || r.Header.Get("Accept") != "application/json", func() {
			t.Errorf("unexpected local Codex catalog request: %s %s headers=%v", r.Method, r.URL, r.Header)
		})
		// Primary-only fixtures exercise partial success and total failure;
		// explicit dual-source tests install their own successful Codex handler.
		http.Error(w, "local Codex catalog unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(codex.Close)
	cfg := config.Default()
	cfg.Models.CodexClientVersion = config.DefaultCodexClientVersion
	cfg.Upstream.BaseURL = target + "/v1"
	cfg.Upstream.UserAgent = "model-test-pi"
	cfg.Models.CodexBaseURL = codex.URL + "/codex"
	factory := egress.NewFactory(egress.Options{ConnectTimeout: time.Second, ResponseHeaderTimeout: time.Second})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := accounts.New(st, oauth.NewClient(nil), factory, accounts.Options{Logger: logger})
	holder := settings.New(st, cfg)
	runtimeSettings := holder.Get()
	runtimeSettings.ConcurrencyWaitTimeoutSeconds = 1
	manager.SetSettings(runtimeSettings)
	client := upstream.New(upstream.Config{SSEURL: cfg.UpstreamSSEURL(), WSURL: cfg.UpstreamWSURL(), Pool: true, ConnectTimeout: time.Second, IdleTimeout: time.Second, Logger: logger}, factory)
	t.Cleanup(client.Close)
	a := &store.Account{Name: "chosen", AccountID: "main-account", AccessToken: "selected-primary", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Enabled: true, Status: store.AccountStatusReady, Concurrency: 1}
	{
		err := st.CreateAccount(context.Background(), a)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	{
		err := st.SaveCodexCredential(context.Background(), a.ID, "secret-codex", "secret-codex-refresh", "", time.Now().Add(time.Hour).UnixMilli(), "codex-account")
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	{
		err := st.CreateAccount(context.Background(), &store.Account{Name: "must-never-select", AccessToken: "other-account-token", Enabled: true, Status: store.AccountStatusReady})
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	return New(Options{Config: cfg, Store: st, Accounts: manager, Settings: holder, Upstream: client, Factory: factory, Logger: logger}), a
}

func modelAdminRequest(t *testing.T, handler http.HandlerFunc, ctx context.Context, id int64, method, body string) (*httptest.ResponseRecorder, accountModelTestResult) {
	t.Helper()
	r := httptest.NewRequest(method, "/", strings.NewReader(body)).WithContext(ctx)
	r.SetPathValue("id", strconv.FormatInt(id, 10))
	w := httptest.NewRecorder()
	handler(w, r)
	var result accountModelTestResult
	{
		err := json.Unmarshal(w.Body.Bytes(), &result)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	return w, result
}

func TestAccountModelTestPrimaryAccountPiRequestAndRealProxy(t *testing.T) {
	for _, protocol := range []string{"sse", "ws"} {
		t.Run(protocol, func(t *testing.T) { runAccountModelTestPrimaryAccountPiRequestAndRealProxyCase(t, &protocol) })
	}
}

func TestAccountModelTestRejectsStreamFailuresAndLimits(t *testing.T) {
	for _, protocol := range []string{"sse", "ws"} {
		for _, tc := range []struct {
			name, event string
			status      int
		}{
			{"error", `{"type":"error","error":{"message":"selected-primary secret-codex proxy-password"}}`, 200},
			{"untyped_error", `{"error":{"message":"selected-primary"}}`, 200},
			{"failed", `{"type":"response.failed","response":{"status":"failed","error":{"message":"secret-codex"}}}`, 200},
			{"incomplete", `{"type":"response.incomplete","response":{"status":"incomplete"}}`, 200},
			{"completed_failed", `{"type":"response.completed","response":{"status":"failed"}}`, 200},
			{"completed_missing_status", `{"type":"response.completed","response":{"id":"not-proof"}}`, 200},
			{"completed_error", `{"type":"response.completed","response":{"status":"completed","error":{"message":"selected-primary"}}}`, 200},
			{"done_not_complete", `{"type":"response.done","response":{"status":"incomplete"}}`, 200},
			{"truncated", `{"type":"response.output_text.delta","delta":"partial"}`, 200},
			{"oversize_output", `{"type":"response.output_text.delta","delta":"` + strings.Repeat("x", maxModelTestOutput+1) + `"}`, 200},
			{"http_error", `{"error":"selected-primary proxy-password"}`, 403},
		} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) { runAccountModelTestRejectsStreamFailuresAndLimitsCase(t, &protocol, &tc) })
		}
	}
}

func TestAccountModelTestCancellationAndTimeoutReleaseSlots(t *testing.T) {
	for _, protocol := range []string{"sse", "ws"} {
		for _, mode := range []string{"cancel", "timeout"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) { runAccountModelTestCancellationAndTimeoutReleaseSlotsCase(t, &protocol, &mode) })
		}
	}
}

func TestAccountModelTestValidationHealthPolicyAndConcurrencyMakeNoRequest(t *testing.T) {
	var calls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer target.Close()
	s, a := newModelAdmin(t, target.URL)
	for _, payload := range []string{`{}`, `{"model":"","message":"Hi"}`, `{"model":"chosen-model","message":" "}`, `{"model":"chosen-model","message":"Hi"}{}`, `{"model":"chosen-model","message":"Hi","access_token":"bad"}`, `{"model":"chosen-model","message":"` + strings.Repeat("x", maxModelTestMessage+1) + `"}`} {
		w, result := modelAdminRequest(t, s.handleTestAccountModel, context.Background(), a.ID, "POST", payload)
		reportTestFailure(t, w.Code != 400 || result.OK, func() {
			t.Fatalf("bad request accepted: %d %s", w.Code, w.Body.String())
		})
	}
	{
		w, _ := modelAdminRequest(t, s.handleTestAccountModel, context.Background(), a.ID, "GET", `{"model":"chosen-model","message":"Hi"}`)
		reportTestFailure(t, w.Code != 405, func() {
			t.Fatal("GET triggered model test")
		})
	}
	payload := `{"model":"chosen-model","message":"Hi"}`
	// Apply restrictions through the database fixture to independently assert
	// the production policy check, including inherited group restrictions.
	{
		_, err := s.store.DB().Exec("UPDATE accounts SET disabled_models='[\"chosen-model\"]' WHERE id=?", a.ID)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	w, _ := modelAdminRequest(t, s.handleTestAccountModel, context.Background(), a.ID, "POST", payload)
	reportTestFailure(t, w.Code != 403, func() {
		t.Fatalf("disabled model allowed: %d %s", w.Code, w.Body.String())
	})
	{
		_, err := s.store.DB().Exec("UPDATE accounts SET disabled_models='[]' WHERE id=?", a.ID)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	group := &store.AccountGroup{Name: "restrict model diagnostics", Enabled: true, DisabledModels: []string{"chosen-model"}}
	{
		err := s.store.CreateAccountGroup(context.Background(), group)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	{
		err := s.store.SetAccountGroupAccounts(context.Background(), group.ID, []int64{a.ID})
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	w, _ = modelAdminRequest(t, s.handleTestAccountModel, context.Background(), a.ID, "POST", payload)
	reportTestFailure(t, w.Code != 403, func() {
		t.Fatalf("inherited model restriction allowed: %d %s", w.Code, w.Body.String())
	})
	{
		err := s.store.DeleteAccountGroup(context.Background(), group.ID)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	_, release, err := s.accounts.AcquireSpecific(context.Background(), a.ID, "chosen-model")
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	w, _ = modelAdminRequest(t, s.handleTestAccountModel, context.Background(), a.ID, "POST", payload)
	release()
	reportTestFailure(t, w.Code != 409, func() {
		t.Fatalf("busy account rotated/requested: %d %s", w.Code, w.Body.String())
	})
	for _, state := range []string{store.AccountStatusInvalid, store.AccountStatusBanned, "quota_exhausted"} {
		{
			err := s.store.SetAccountStatus(context.Background(), a.ID, state, "")
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		w, _ := modelAdminRequest(t, s.handleTestAccountModel, context.Background(), a.ID, "POST", payload)
		reportTestFailure(t, w.Code != 409, func() {
			t.Fatalf("unhealthy account allowed: %s %d", state, w.Code)
		})
	}
	{
		_, err := s.store.DB().Exec("UPDATE accounts SET status='ready', enabled=0 WHERE id=?", a.ID)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	w, _ = modelAdminRequest(t, s.handleTestAccountModel, context.Background(), a.ID, "POST", payload)
	reportTestFailure(t, w.Code != 409, func() {
		t.Fatalf("disabled account allowed: %d %s", w.Code, w.Body.String())
	})
	// A linked Codex credential is never a fallback for a missing main token.
	{
		_, err := s.store.DB().Exec("UPDATE accounts SET enabled=1, access_token='', refresh_token='' WHERE id=?", a.ID)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	w, _ = modelAdminRequest(t, s.handleTestAccountModel, context.Background(), a.ID, "POST", payload)
	reportTestFailure(t, w.Code != 502 || strings.Contains(w.Body.String(), "secret-codex"), func() {
		t.Fatalf("Codex-only account accepted or leaked token: %d %s", w.Code, w.Body.String())
	})
	reportTestFailure(t, calls.Load() != 0 || s.accounts.Inflight(a.ID) != 0, func() {
		t.Fatalf("rejected diagnostic made outbound request or leaked slot: %d", calls.Load())
	})
}

func TestAccountModelsGETReadOnlyAndRefreshFailureKeepsList(t *testing.T) {
	var calls atomic.Int64
	var fail atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		reportTestFailure(t, r.Method != "GET" || r.URL.Path != "/v1/models", func() {
			t.Errorf("unexpected generation/proxy request: %s %s", r.Method, r.URL.Path)
		})
		if fail.Load() {
			w.WriteHeader(401)
			fmt.Fprint(w, "selected-primary proxy-password")
			return
		}
		fmt.Fprint(w, `{"models":[{"id":"actual-model","display_name":"Display name"}]}`)
	}))
	defer target.Close()
	s, a := newModelAdmin(t, target.URL)
	get := func(handler http.HandlerFunc, method string, want int) store.ModelCatalog {
		t.Helper()
		w, _ := modelAdminRequest(t, handler, context.Background(), a.ID, method, "")
		reportTestFailure(t, w.Code != want, func() {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		})
		var catalog store.ModelCatalog
		{
			err := json.Unmarshal(w.Body.Bytes(), &catalog)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		return catalog
	}
	{
		got := get(s.handleAccountModels, "GET", 200)
		reportTestFailure(t, got.Models == nil || len(got.Models) != 0 || calls.Load() != 0, func() {
			t.Fatalf("opening dialog fetched/generated: %+v calls=%d", got, calls.Load())
		})
	}
	first := get(s.handleRefreshAccountModels, "POST", 200)
	reportTestFailure(t, len(first.Models) != 1 || first.FetchedAt == 0, func() {
		t.Fatalf("missing real catalog: %+v", first)
	})
	fail.Store(true)
	failed := get(s.handleRefreshAccountModels, "POST", 502)
	cached := get(s.handleAccountModels, "GET", 200)
	reportTestFailure(t, len(cached.Models) != 1 || cached.FetchedAt != first.FetchedAt || cached.Error == "" || cached.Error != failed.Error || calls.Load() != 2, func() {
		t.Fatalf("refresh failure hidden/lost cache: %+v attempts=%d", cached, calls.Load())
	})
}

func TestAccountModelDiagnosticTransportAndCompletedOutput(t *testing.T) {
	for _, tc := range []struct{ account, global, want string }{
		{"ws", "sse", "websocket-cached"}, {"sse", "websocket", "sse"}, {"", "websocket", "websocket"},
		{"", "websocket-cached", "websocket-cached"}, {"", "auto", "auto"}, {"", "passthrough", "sse"},
	} {
		{
			got := diagnosticTransport(tc.account, tc.global)
			reportTestFailure(t, got != tc.want, func() {
				t.Errorf("transport(%q,%q)=%q want=%q", tc.account, tc.global, got, tc.want)
			})
		}
	}
	result := accountModelTestResult{}
	collector := modelTestCollector{started: time.Now(), result: &result}
	var data map[string]any
	_ = json.Unmarshal([]byte(`{"response":{"status":"completed","output":[{"content":[{"type":"output_text","text":"terminal only"}]}]}}`), &data)
	{
		err := collector.onEvent(&upstream.Event{Type: "response.completed", Data: data})
		reportTestFailure(t, err != errModelTestComplete || !collector.completed || collector.output.String() != "terminal only" || result.FirstTokenMS == nil, func() {
			t.Fatalf("terminal text not collected: err=%v output=%s", err, collector.output.String())
		})
	}
}

func TestAccountModelCatalogDualSourceLocalContracts(t *testing.T) {
	for _, tc := range []struct {
		name                string
		linked              bool
		chatGPT, codex      int
		wantStatus          int
		wantChat, wantCodex string
	}{
		{"both_success", true, 200, 200, 200, "chatgpt-new", "codex-new"},
		{"chatgpt_failure", true, 503, 200, 200, "chatgpt-old", "codex-new"},
		{"codex_failure", true, 200, 503, 200, "chatgpt-new", "codex-old"},
		{"both_failure", true, 503, 503, 502, "chatgpt-old", "codex-old"},
		{"codex_unlinked", false, 200, 200, 200, "chatgpt-new", ""},
		{"unlinked_primary_failure", false, 503, 200, 502, "chatgpt-old", ""},
	} {
		t.Run(tc.name, func(t *testing.T) { runAccountModelCatalogDualSourceLocalContractsCase(t, &tc) })
	}
}

// runAccountModelTestPrimaryAccountPiRequestAndRealProxyCase exercises one isolated test phase with the original shared state.
func runAccountModelTestPrimaryAccountPiRequestAndRealProxyCase(t *testing.T, capturedProtocol *string) {
	t.Helper()

	var requests, proxyHits atomic.Int64
	var lastSession string
	const message = "  A custom question\nnot the default Hi  "
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveAccountModelTestPrimaryAccountPiRequestAndRealProxyRequest(w, r, &t, &capturedProtocol, &requests, &lastSession, message)
	}))
	defer target.Close()
	proxy := localForwardProxy(t, target.URL, &proxyHits, false)
	s, a := newModelAdmin(t, target.URL)
	a.UpstreamProtocol = (*capturedProtocol)
	a.ProxyURL = strings.Replace(proxy.URL, "://", "://proxy-user:proxy-password@", 1)
	{
		err := s.store.UpdateAccount(context.Background(), a)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	payload, _ := json.Marshal(map[string]string{"model": "chosen-model", "message": message})
	for i := 0; i < 2; i++ {
		w, result := modelAdminRequest(t, s.handleTestAccountModel, context.Background(), a.ID, "POST", string(payload))
		wantTransport := "sse"
		if (*capturedProtocol) == "ws" {
			wantTransport = "websocket-cached"
		}
		reportTestFailure(t, w.Code != 200 || !result.OK || result.Model != "chosen-model" || result.Output != "Hello world" || result.FirstTokenMS == nil || result.Transport != wantTransport, func() {
			t.Fatalf("bad test result: %d %s", w.Code, w.Body.String())
		})
		reportTestFailure(t, s.accounts.Inflight(a.ID) != 0 || s.upstream.PoolSize() != 0, func() {
			t.Fatal("diagnostic retained slot or cached conversation")
		})
	}
	reportTestFailure(t, requests.Load() != 2 || proxyHits.Load() != 2, func() {
		t.Fatalf("test bypassed proxy or duplicated request: upstream=%d proxy=%d", requests.Load(), proxyHits.Load())
	})
	var usageRows, audits int
	{
		err := s.store.DB().QueryRow("SELECT COUNT(*) FROM usage_records").Scan(&usageRows)
		reportTestFailure(t, err != nil || usageRows != 0, func() {
			t.Fatalf("diagnostic falsely billed a business request: %d %v", usageRows, err)
		})
	}
	{
		err := s.store.DB().QueryRow("SELECT COUNT(*) FROM audit_events WHERE action='account.model_test'").Scan(&audits)
		reportTestFailure(t, err != nil || audits != 2, func() {
			t.Fatalf("missing diagnostic audit: %d %v", audits, err)
		})
	}
}

// runAccountModelTestRejectsStreamFailuresAndLimitsCase exercises one isolated test phase with the original shared state.
func runAccountModelTestRejectsStreamFailuresAndLimitsCase(t *testing.T, capturedProtocol *string, capturedTc *struct {
	name   string
	event  string
	status int
}) {
	t.Helper()

	var calls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if (*capturedTc).status != 200 {
			w.WriteHeader((*capturedTc).status)
			fmt.Fprint(w, (*capturedTc).event)
			return
		}
		if (*capturedProtocol) == "ws" {
			ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer ws.Close()
			_, _, _ = ws.ReadMessage()
			_ = ws.WriteMessage(websocket.TextMessage, []byte((*capturedTc).event))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(upstream.FormatSSEFrame([]byte((*capturedTc).event)))
	}))
	defer target.Close()
	s, a := newModelAdmin(t, target.URL)
	a.UpstreamProtocol = (*capturedProtocol)
	// Error bodies now retain real details, so use an actual configured
	// proxy credential rather than treating an arbitrary word as secret.
	var proxyHits atomic.Int64
	proxy := localForwardProxy(t, target.URL, &proxyHits, false)
	a.ProxyURL = strings.Replace(proxy.URL, "://", "://proxy-user:proxy-password@", 1)
	{
		err := s.store.UpdateAccount(context.Background(), a)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	w, result := modelAdminRequest(t, s.handleTestAccountModel, context.Background(), a.ID, "POST", `{"model":"chosen-model","message":"Hi"}`)
	reportTestFailure(t, w.Code != 502 || result.OK || result.Error == "" || calls.Load() != 1 || s.accounts.Inflight(a.ID) != 0 || len(result.Output) > maxModelTestOutput, func() {
		t.Fatalf("invalid upstream stream accepted/retried/leaked slot: %d %s calls=%d", w.Code, w.Body.String(), calls.Load())
	})
	for _, secret := range []string{"selected-primary", "secret-codex", "proxy-password"} {
		reportTestFailure(t, strings.Contains(w.Body.String(), secret), func() {
			t.Fatal("response disclosed upstream credential/error payload")
		})
	}
}

// runAccountModelTestCancellationAndTimeoutReleaseSlotsCase exercises one isolated test phase with the original shared state.
func runAccountModelTestCancellationAndTimeoutReleaseSlotsCase(t *testing.T, capturedProtocol *string, capturedMode *string) {
	t.Helper()

	started, closed := make(chan struct{}), make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(closed)
		if (*capturedProtocol) == "ws" {
			ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer ws.Close()
			_, _, _ = ws.ReadMessage()
			close(started)
			_, _, _ = ws.ReadMessage()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer target.Close()
	s, a := newModelAdmin(t, target.URL)
	a.UpstreamProtocol = (*capturedProtocol)
	{
		err := s.store.UpdateAccount(context.Background(), a)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if (*capturedMode) == "cancel" {
		go func() { <-started; cancel() }()
	}
	w, result := modelAdminRequest(t, s.handleTestAccountModel, ctx, a.ID, "POST", `{"model":"chosen-model","message":"Hi"}`)
	reportTestFailure(t, w.Code != 408 || result.OK || s.accounts.Inflight(a.ID) != 0 || s.upstream.PoolSize() != 0, func() {
		t.Fatalf("cancel failed: %d %s", w.Code, w.Body.String())
	})
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("upstream connection remained open after cancellation")
	}
}

// runAccountModelCatalogDualSourceLocalContractsCase exercises one isolated test phase with the original shared state.
func runAccountModelCatalogDualSourceLocalContractsCase(t *testing.T, capturedTc *struct {
	name       string
	linked     bool
	chatGPT    int
	codex      int
	wantStatus int
	wantChat   string
	wantCodex  string
}) {
	var second atomic.Bool
	var chatCalls, codexCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveAccountModelCatalogDualSourceLocalContractsRequest(w, r, &t, &capturedTc, &second, &chatCalls, &codexCalls)
	}))
	defer target.Close()
	s, account := newModelAdmin(t, target.URL)
	s.cfg.Models.CodexBaseURL = target.URL + "/codex"
	ctx := context.Background()
	if !(*capturedTc).linked {
		{
			err := s.store.ClearCodexCredential(ctx, account.ID)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
	}
	{
		err := s.store.PatchAccountManagementFields(ctx, account.ID, store.AccountManagementPatch{SupplementalModels: []string{"shared", "manual-only"}})
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	refresh := func(wantStatus int) store.ModelCatalog {
		t.Helper()
		w, _ := modelAdminRequest(t, s.handleRefreshAccountModels, ctx, account.ID, http.MethodPost, "")
		reportTestFailure(t, w.Code != wantStatus, func() {
			t.Fatalf("refresh status=%d want=%d body=%s", w.Code, wantStatus, w.Body.String())
		})
		for _, secret := range []string{"selected-primary", "secret-codex", "must-not-leak"} {
			reportTestFailure(t, strings.Contains(w.Body.String(), secret), func() {
				t.Fatalf("catalog response leaked %q", secret)
			})
		}
		var catalog store.ModelCatalog
		{
			err := json.Unmarshal(w.Body.Bytes(), &catalog)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		return catalog
	}
	first := refresh(http.StatusOK)
	reportTestFailure(t, first.Error != "", func() {
		t.Fatalf("initial local refresh failed: %s", first.Error)
	})
	second.Store(true)
	got := refresh((*capturedTc).wantStatus)
	models := make(map[string]store.CatalogModel)
	for _, model := range got.Models {
		models[model.ID] = model
	}
	wantCount := 3
	if (*capturedTc).linked {
		wantCount++
	}
	reportTestFailure(t, len(got.Models) != wantCount || len(models) != wantCount || models[(*capturedTc).wantChat].ID == "" || ((*capturedTc).wantCodex != "" && models[(*capturedTc).wantCodex].ID == "") || models["manual-only"].Source != "manual", func() {
		t.Fatalf("source replacement/cache retention/manual precedence: %+v", got.Models)
	})
	shared := models["shared"]
	wantOrigins := store.ModelSourceChatGPT
	if (*capturedTc).linked {
		wantOrigins += "," + store.ModelSourceCodex
		reportTestFailure(t, len(got.SourceCatalogs[store.ModelSourceCodex].Models) != 2 || string(shared.Metadata["context_window"]) != "128000" || string(got.SourceCatalogs[store.ModelSourceCodex].Models[1].Metadata["supports_parallel_tool_calls"]) != "true", func() {
			t.Fatal("Codex metadata was lost or its source snapshot was overwritten")
		})
	}
	reportTestFailure(t, shared.Source != "upstream" || shared.Name != "ChatGPT name" || strings.Join(shared.Origins, ",") != wantOrigins || string(shared.Metadata["supports_parallel_tool_calls"]) != "false" || string(shared.Metadata["supported_reasoning_levels"]) != "[]", func() {
		t.Fatalf("merged model lost primary capabilities or origins: %+v", shared)
	})
	reportTestFailure(t, (got.SourceCatalogs[store.ModelSourceChatGPT].Error != "") != ((*capturedTc).chatGPT != 200) || (got.SourceCatalogs[store.ModelSourceCodex].Error != "") != ((*capturedTc).linked && (*capturedTc).codex != 200), func() {
		t.Fatalf("incorrect per-source errors: %+v", got.SourceCatalogs)
	})
	reportTestFailure(t, (got.Error != "") != ((*capturedTc).chatGPT != 200 || ((*capturedTc).linked && (*capturedTc).codex != 200)), func() {
		t.Fatalf("partial/complete failure was hidden: %q", got.Error)
	})
	func() {
		verifyAccountModelCatalogDualSourceLocalContracts(&t, &capturedTc, &chatCalls, &codexCalls, &got)
	}()
}

// serveAccountModelTestPrimaryAccountPiRequestAndRealProxyRequest exercises one isolated test phase with the original shared state.
func serveAccountModelTestPrimaryAccountPiRequestAndRealProxyRequest(w http.ResponseWriter, r *http.Request, capturedT **testing.T, capturedCapturedProtocol **string, capturedRequests *atomic.Int64, capturedLastSession *string, capturedMessage string) {
	(*capturedT).Helper()

	(*capturedRequests).Add(1)
	reportTestFailure((*capturedT), r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer selected-primary" || r.Header.Get("User-Agent") != "model-test-pi" || r.Header.Get("Proxy-Authorization") != "", func() {
		(*capturedT).Errorf("incorrect primary account/headers: %s %v", r.URL.Path, r.Header)
	})
	var body map[string]any
	var ws *websocket.Conn
	if (*(*capturedCapturedProtocol)) == "ws" {
		reportTestFailure((*capturedT), r.Header.Get("ChatGPT-Account-Id") != "main-account", func() {
			(*capturedT).Errorf("ws used wrong account identity: %q", r.Header.Get("ChatGPT-Account-Id"))
		})
		var err error
		ws, err = (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			(*capturedT).Error(err)
			return
		}
		defer ws.Close()
		if err := ws.ReadJSON(&body); err != nil {
			(*capturedT).Error(err)
			return
		}
		reportTestFailure((*capturedT), body["type"] != "response.create", func() {
			(*capturedT).Errorf("wrong ws request frame: %v", body)
		})
	} else {
		reportTestFailure((*capturedT), r.Method != http.MethodPost || r.Header.Get("ChatGPT-Account-Id") != "" || r.Header.Get("OpenAI-Beta") != "", func() {
			(*capturedT).Errorf("SSE did not use primary header builder: %v", r.Header)
		})
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			(*capturedT).Error(err)
			return
		}
	}
	session := r.Header.Get("X-Client-Request-Id")
	reportTestFailure((*capturedT), !strings.HasPrefix(session, "admin-test-") || session == (*capturedLastSession), func() {
		(*capturedT).Errorf("not a fresh isolated diagnostic session: %q", session)
	})
	(*capturedLastSession) = session
	reportTestFailure((*capturedT), body["model"] != "chosen-model" || body["store"] != false || body["prompt_cache_key"] != nil || body["previous_response_id"] != nil, func() {
		(*capturedT).Errorf("wrong Pi body: %v", body)
	})
	input, _ := body["input"].([]any)
	if len(input) != 1 {
		(*capturedT).Errorf("wrong input: %v", body)
		return
	}
	content := input[0].(map[string]any)["content"].([]any)
	reportTestFailure((*capturedT), content[0].(map[string]any)["text"] != capturedMessage, func() {
		(*capturedT).Errorf("message was substituted/trimmed: %v", content)
	})
	frames := []string{`{"type":"response.output_text.delta","delta":"Hello "}`, `{"type":"response.output_text.delta","delta":"world"}`, `{"type":"response.completed","response":{"id":"diagnostic-response","status":"completed"}}`}
	func() {
		verifyAccountModelTestPrimaryAccountPiRequestAndRealProxy(&w, &capturedT, &ws, &frames)
	}()
}

// serveAccountModelCatalogDualSourceLocalContractsRequest exercises one isolated test phase with the original shared state.
func serveAccountModelCatalogDualSourceLocalContractsRequest(w http.ResponseWriter, r *http.Request, capturedT **testing.T, capturedCapturedTc **struct {
	name       string
	linked     bool
	chatGPT    int
	codex      int
	wantStatus int
	wantChat   string
	wantCodex  string
}, capturedSecond *atomic.Bool, capturedChatCalls *atomic.Int32, capturedCodexCalls *atomic.Int32) {
	(*capturedT).Helper()

	status, suffix := http.StatusOK, "old"
	if (*capturedSecond).Load() {
		suffix = "new"
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v1/models":
		(*capturedChatCalls).Add(1)
		reportTestFailure((*capturedT), r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer selected-primary" || r.Header.Get("ChatGPT-Account-Id") != "" ||
			r.Header.Get("User-Agent") != "model-test-pi" || r.Header.Get("Originator") != "" || r.Header.Get("Version") != "" || r.URL.Query().Has("client_version"), func() {
			(*capturedT).Error("ChatGPT catalog used the wrong method, credential or primary identity")
		})
		if (*capturedSecond).Load() {
			status = (*(*capturedCapturedTc)).chatGPT
		}
		w.WriteHeader(status)
		if status != http.StatusOK {
			fmt.Fprint(w, `{"error":"selected-primary must-not-leak"}`)
			return
		}
		fmt.Fprintf(w, `{"data":[{"id":"shared","name":"ChatGPT name","supports_parallel_tool_calls":false,"supported_reasoning_levels":[]},{"id":"chatgpt-%s"}]}`, suffix)
	case "/codex/models":
		(*capturedCodexCalls).Add(1)
		reportTestFailure((*capturedT), r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer secret-codex" || r.Header.Get("ChatGPT-Account-Id") != "codex-account" ||
			r.URL.Query().Get("client_version") != config.DefaultCodexClientVersion || r.Header.Get("Version") != config.DefaultCodexClientVersion ||
			r.Header.Get("User-Agent") != "codex-tui/"+config.DefaultCodexClientVersion || r.Header.Get("Originator") != "codex_cli_rs" || r.Header.Get("Accept") != "application/json", func() {
			(*capturedT).Error("Codex catalog used the wrong method, credential, identity or version")
		})
		if (*capturedSecond).Load() {
			status = (*(*capturedCapturedTc)).codex
		}
		w.WriteHeader(status)
		if status != http.StatusOK {
			fmt.Fprint(w, `{"error":"secret-codex must-not-leak"}`)
			return
		}
		fmt.Fprintf(w, `{"models":[{"slug":"shared","display_name":"Codex name","supports_parallel_tool_calls":true,"supported_reasoning_levels":[{"effort":"high"}],"context_window":128000},{"slug":"codex-%s"}]}`, suffix)
	default:
		(*capturedT).Errorf("unexpected upstream route: %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// verifyAccountModelTestPrimaryAccountPiRequestAndRealProxy exercises one isolated test phase with the original shared state.
func verifyAccountModelTestPrimaryAccountPiRequestAndRealProxy(capturedW *http.ResponseWriter, capturedCapturedT ***testing.T, capturedWs **websocket.Conn, capturedFrames *[]string) {
	if (*capturedWs) != nil {
		for _, frame := range *capturedFrames {
			_ = (*capturedWs).WriteMessage(websocket.TextMessage, []byte(frame))
		}
		// The diagnostic must close the connection, even when cached WS
		// transport is selected for the account.
		_ = (*capturedWs).SetReadDeadline(time.Now().Add(time.Second))
		_, _, err := (*capturedWs).ReadMessage()
		reportTestFailure((*(*capturedCapturedT)), websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseAbnormalClosure), func() {
			(*(*capturedCapturedT)).Errorf("diagnostic connection not closed: %v", err)
		})
	} else {
		(*capturedW).Header().Set("Content-Type", "text/event-stream")
		for _, frame := range *capturedFrames {
			_, _ = (*capturedW).Write(upstream.FormatSSEFrame([]byte(frame)))
		}
	}
}

// verifyAccountModelCatalogDualSourceLocalContracts exercises one isolated test phase with the original shared state.
func verifyAccountModelCatalogDualSourceLocalContracts(capturedT **testing.T, capturedCapturedTc **struct {
	name       string
	linked     bool
	chatGPT    int
	codex      int
	wantStatus int
	wantChat   string
	wantCodex  string
}, capturedChatCalls *atomic.Int32, capturedCodexCalls *atomic.Int32, capturedGot *store.ModelCatalog) {
	(*capturedT).Helper()

	wantCodexCalls := int32(2)
	if !(*(*capturedCapturedTc)).linked {
		wantCodexCalls = 0
		{
			source := (*capturedGot).SourceCatalogs[store.ModelSourceCodex]
			reportTestFailure((*capturedT), !source.Skipped || source.SkipReason != "codex_not_linked" || len(source.Models) != 0, func() {
				(*capturedT).Fatalf("unlinked Codex was not skipped: %+v", source)
			})
		}
	}
	reportTestFailure((*capturedT), (*capturedChatCalls).Load() != 2 || (*capturedCodexCalls).Load() != wantCodexCalls, func() {
		(*capturedT).Fatalf("unexpected real requests: chatgpt=%d codex=%d", (*capturedChatCalls).Load(), (*capturedCodexCalls).Load())
	})
}
