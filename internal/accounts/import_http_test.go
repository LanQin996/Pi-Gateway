package accounts_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-gateway/internal/accounts"
	"pi-gateway/internal/admin"
	"pi-gateway/internal/config"
	"pi-gateway/internal/egress"
	"pi-gateway/internal/oauth"
	"pi-gateway/internal/store"
)

type importRoundTripper func(*http.Request) (*http.Response, error)

func (f importRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Tests live here because the audit permits editing admin/accounts.go but not
// other admin files. Exercise the actual authenticated management routes rather
// than duplicating the import handler in a test.
func importHarness(t *testing.T, transport http.RoundTripper) (*store.Store, func(map[string]string) *httptest.ResponseRecorder) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "import.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// TempDir cleanup on Windows fails while SQLite side files (-wal/-shm) are
	// still present, so drop them after the store closes (cleanups run LIFO).
	t.Cleanup(func() {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			for attempt := 0; attempt < 50; attempt++ {
				err := os.Remove(dbPath + suffix)
				if err == nil || os.IsNotExist(err) {
					break
				}
				if attempt == 49 {
					t.Errorf("remove SQLite fixture: %v", err)
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	})
	t.Cleanup(func() { _ = st.Close() })
	factory := egress.NewFactory(egress.Options{})
	httpClient, err := factory.HTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	httpClient.Transport = transport
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := accounts.New(st, oauth.NewClient(nil), factory, accounts.Options{Logger: logger})
	cfg := &config.Config{}
	cfg.Admin.Username, cfg.Admin.Password, cfg.Admin.SessionTTLMinute = "audit", "test-password", 5
	server := admin.New(admin.Options{Config: cfg, Store: st, Accounts: manager, Factory: factory, Logger: logger})
	if err := server.BootstrapPassword(context.Background()); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	server.Routes(mux, nil)
	login := httptest.NewRecorder()
	mux.ServeHTTP(login, httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"audit","password":"test-password"}`)))
	if login.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", login.Code, login.Body.String())
	}
	var session struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &session); err != nil || session.Token == "" {
		t.Fatalf("invalid login session: %v", err)
	}
	return st, func(body map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/accounts", bytes.NewReader(payload))
		request.Header.Set("Authorization", "Bearer "+session.Token)
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		return response
	}
}

func importJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestImportHTTPRejectsCodexCredential(t *testing.T) {
	st, submit := importHarness(t, importRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("rejected imports must not contact any token endpoint")
		return nil, nil
	}))
	for _, tc := range []struct {
		name, kind, clientID, access string
	}{
		{"codex_shape_declared_chatgpt", "chatgpt", "issued-client", importJWT(t, map[string]any{oauth.JWTClaimPath: map[string]any{"chatgpt_account_id": "codex-account"}})},
		{"legacy_client", "chatgpt", oauth.ClientID, "opaque"},
		{"registration_placeholder", "chatgpt", oauth.ChatGPTDynamicClientID, "opaque"},
		{"missing_client_id", "chatgpt", "", "opaque"},
		{"explicit_codex_type", "codex", "issued-client", "opaque"},
		{"missing_type", "", "issued-client", "opaque"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := submit(map[string]string{
				"credential_type": tc.kind, "oauth_client_id": tc.clientID,
				"access_token": tc.access, "refresh_token": "codex-refresh",
			})
			if response.Code != http.StatusBadRequest {
				t.Fatalf("import = %d: %s; want 400", response.Code, response.Body.String())
			}
		})
	}
	rows, err := st.ListAccounts(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatalf("rejected imports changed accounts: count=%d err=%v", len(rows), err)
	}
}

func TestImportHTTPAcceptsVerifiedChatGPTCredential(t *testing.T) {
	calls := 0
	idToken := importJWT(t, map[string]any{"email": "import@example.com"})
	st, submit := importHarness(t, importRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != oauth.ChatGPTTokenURL || r.Method != http.MethodPost {
			t.Fatalf("wrong token endpoint: %s %s", r.Method, r.URL)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{
			"client_id": "issued-client", "refresh_token": "submitted-refresh",
			"resource": oauth.ChatGPTResource, "grant_type": "refresh_token",
		} {
			if got := r.PostForm.Get(key); got != want {
				t.Errorf("form[%s] = %q, want %q", key, got, want)
			}
		}
		payload, _ := json.Marshal(map[string]any{
			"access_token": "verified-chatgpt-access", "refresh_token": "rotated-chatgpt-refresh",
			"scope": oauth.ChatGPTScope, "id_token": idToken, "expires_in": 3600,
		})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(payload)), Header: http.Header{}}, nil
	}))
	// Even a fresh supplied token is not trusted; only provider-returned tokens
	// may be written. A refresh-only import is supported as well.
	for _, access := range []string{importJWT(t, map[string]any{"exp": time.Now().Add(time.Hour).Unix()}), ""} {
		response := submit(map[string]string{
			"credential_type": "chatgpt", "oauth_client_id": "issued-client",
			"refresh_token": "submitted-refresh", "access_token": access,
		})
		if response.Code != http.StatusOK {
			t.Fatalf("import = %d: %s", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "verified-chatgpt-access") || strings.Contains(response.Body.String(), "rotated-chatgpt-refresh") {
			t.Fatal("import response exposed credentials")
		}
	}
	rows, err := st.ListAccounts(context.Background())
	if err != nil || len(rows) != 1 || calls != 2 {
		t.Fatalf("accounts=%d token calls=%d err=%v", len(rows), calls, err)
	}
	got := rows[0]
	if got.AccessToken != "verified-chatgpt-access" || got.RefreshToken != "rotated-chatgpt-refresh" || got.OAuthClientID != "issued-client" || got.CodexLinked() {
		t.Fatal("import did not store only the verified ChatGPT credential")
	}
}

func TestImportHTTPRefreshFailureDoesNotPersist(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"invalid_grant", http.StatusBadRequest, `{"error":"invalid_grant"}`},
		{"missing_direct_scope", http.StatusOK, `{"access_token":"codex-opaque","refresh_token":"codex-refresh","scope":"openid","id_token":"id","expires_in":3600}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, submit := importHarness(t, importRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: http.Header{}}, nil
			}))
			existing := &store.Account{Name: "existing", AccountID: "chatgpt:existing", AccessToken: "keep-chatgpt", RefreshToken: "keep-refresh", Enabled: true}
			if err := st.CreateAccount(context.Background(), existing); err != nil {
				t.Fatal(err)
			}
			response := submit(map[string]string{
				"credential_type": "chatgpt", "oauth_client_id": "issued-client",
				"access_token": "untrusted-opaque", "refresh_token": "invalid-refresh",
			})
			if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "initial ChatGPT refresh failed") {
				t.Fatalf("failed import = %d: %s; want explicit 502", response.Code, response.Body.String())
			}
			rows, err := st.ListAccounts(context.Background())
			if err != nil || len(rows) != 1 || rows[0].AccessToken != "keep-chatgpt" || rows[0].RefreshToken != "keep-refresh" {
				t.Fatalf("failed import mutated accounts: count=%d err=%v", len(rows), err)
			}
		})
	}
}
