package accounts

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pi-gateway/internal/config"
	"pi-gateway/internal/egress"
	"pi-gateway/internal/oauth"
	"pi-gateway/internal/store"
)

func TestParseModelCatalogShapesAndLimits(t *testing.T) {
	for _, tt := range []struct{ name, body, id, nameWant string }{
		{"openai", `{"data":[{"id":"model-one","name":"One"},{"id":"model-one"},{"id":"model-two","display_name":"Two","description":"Desc"}]}`, "model-one", "One"},
		{"chatgpt", `{"models":[{"slug":"model-one","title":"One"}]}`, "model-one", "One"},
		{"name", `{"models":[{"name":"model-one","display_name":"One"}]}`, "model-one", "One"},
		{"nested", `{"data":{"models":[{"id":"model-one","displayName":"One"}]}}`, "model-one", "One"},
		{"bare", `[{"model":"model-one","name":"One"}]`, "model-one", "One"},
		{"string", `["model-one"]`, "model-one", "model-one"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseModelCatalog([]byte(tt.body))
			if err != nil || len(got) == 0 || got[0].ID != tt.id || got[0].Name != tt.nameWant {
				t.Fatalf("models=%+v err=%v", got, err)
			}
			if tt.name == "openai" && (len(got) != 2 || got[1].Description != "Desc") {
				t.Fatal("deduplication/description lost")
			}
		})
	}
	if got, err := parseModelCatalog([]byte(`{"data":[]}`)); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("real empty catalog rejected: %+v %v", got, err)
	}
	for _, body := range []string{`null`, `{}`, `{"error":{"message":"sensitive"}}`, `{"data":[],"error":"sensitive"}`, `{"models":[{}]}`, `{"models":[123]}`, `{"models":[{"id":"bad\nmodel"}]}`, `{"models":[]}{}`, `{"models":[{"id":"` + strings.Repeat("a", 257) + `"}]}`} {
		if _, err := parseModelCatalog([]byte(body)); err == nil {
			t.Fatalf("accepted bad catalog %s", body)
		}
	}
}

func TestParseModelCatalogPreservesCompleteModelMetadata(t *testing.T) {
	const entry = `{"id":" rich-model ","name":"Original name","display_name":" Display name ","description":null,"source":"provider-value","metadata":{"provider_nested":true},"supported_reasoning_levels":[{"effort":"future-level","description":"Provider level","extra":false}],"default_reasoning_level":"future-level","supports_reasoning_summary_parameter":false,"default_reasoning_summary":null,"context_window":9007199254740993123456789,"max_output_tokens":0,"input_modalities":[],"output_modalities":["text"],"supports_parallel_tool_calls":false,"unknown":{"nested":[null,false,[],{"large":18446744073709551617}]},"nothing":null,"empty":[]}`
	var want map[string]json.RawMessage
	if err := json.Unmarshal([]byte(entry), &want); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`[` + entry + `]`,
		`{"models":[` + entry + `],"envelope_secret":"must-not-copy"}`,
		`{"data":{"models":[` + entry + `]},"error":false}`,
	} {
		models, err := parseModelCatalog([]byte(body))
		if err != nil || len(models) != 1 {
			t.Fatalf("parse=%+v err=%v", models, err)
		}
		model := models[0]
		if model.ID != "rich-model" || model.Name != "Display name" || model.Description != "" || model.Source != "" {
			t.Fatalf("normalized identity changed: %+v", model)
		}
		if !reflect.DeepEqual(model.Metadata, want) {
			t.Fatalf("model metadata lost fields or precision: got=%v want=%v", model.Metadata, want)
		}
		raw, err := json.Marshal(models)
		if err != nil {
			t.Fatal(err)
		}
		var roundTrip []store.CatalogModel
		if err := json.Unmarshal(raw, &roundTrip); err != nil || !reflect.DeepEqual(roundTrip[0].Metadata, want) {
			t.Fatalf("JSON roundtrip changed model object: %s err=%v", raw, err)
		}
	}
}

func TestParseModelCatalogDisplayNormalizationDoesNotTruncateMetadata(t *testing.T) {
	name := strings.Repeat("模", 200)
	description := strings.Repeat("述", 1500)
	entry, err := json.Marshal(map[string]string{"slug": " normal ", "title": name, "description": description})
	if err != nil {
		t.Fatal(err)
	}
	models, err := parseModelCatalog(append(append([]byte("["), entry...), ']'))
	if err != nil || len(models) != 1 {
		t.Fatalf("parse=%+v err=%v", models, err)
	}
	model := models[0]
	if model.ID != "normal" || len(model.Name) > 512 || len(model.Description) > 4096 {
		t.Fatalf("display limits changed: %+v", model)
	}
	if catalogString(model.Metadata, "title") != name || catalogString(model.Metadata, "description") != description || string(model.Metadata["slug"]) != `" normal "` {
		t.Fatal("normalized display truncated original metadata")
	}
	// String entries and minimally described upstream models get no guessed capabilities.
	plain, err := parseModelCatalog([]byte(`["string-model",{"id":"plain-model"}]`))
	if err != nil || len(plain) != 2 || plain[0].Metadata != nil || len(plain[1].Metadata) != 1 {
		t.Fatalf("plain upstream model fabricated metadata: %+v err=%v", plain, err)
	}
}

func TestParseModelCatalogEnvelopeAndCountBounds(t *testing.T) {
	for _, body := range []string{
		`{"models":null}`,
		`{"data":{"data":{"data":{"models":[]}}}}`,
		`{"models":[],"error":[]}`,
		`{"models":[],"error":0}`,
		`[null]`, `[true]`, `[[]]`,
		`[` + strings.Repeat(`"same-model",`, maxCatalogModels) + `"same-model"]`,
	} {
		if _, err := parseModelCatalog([]byte(body)); err == nil {
			t.Fatalf("accepted invalid shape or over-limit catalog: %.100s", body)
		}
	}
	for _, body := range []string{
		`{"data":{"data":{"models":[]}}}`,
		`{"models":[],"error":null}`,
		`{"models":[],"error":false}`,
		`{"models":[],"error":""}`,
	} {
		if models, err := parseModelCatalog([]byte(body)); err != nil || models == nil || len(models) != 0 {
			t.Fatalf("rejected valid empty model list %s: %+v err=%v", body, models, err)
		}
	}
}

func TestModelCatalogRefreshThroughProxyPrimaryTokenAndFailurePreservation(t *testing.T) {
	var status atomic.Int64
	status.Store(200)
	var targetCalls, codexCalls, proxyCalls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		testModelCatalogRefreshThroughProxyPrimaryTokenAndFailurePreservationPhase1(w, r, &t, &status, &targetCalls, &codexCalls)
	}))
	defer target.Close()
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		reportTestFailure(t, r.Header.Get("Proxy-Authorization") == "", func() {
			t.Error("proxy credentials were not used")
		})
		forward := r.Clone(r.Context())
		forward.RequestURI = ""
		forward.Header.Del("Proxy-Authorization")
		resp, err := transport.RoundTrip(forward)
		if err != nil {
			t.Errorf("proxy forwarding: %v", err)
			w.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()
	st := newAccountsTestStore(t)
	factory := egress.NewFactory(egress.Options{})
	manager := New(st, oauth.NewClient(nil), factory, Options{})
	a := &store.Account{Name: "selected", AccessToken: "primary-secret", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), ProxyURL: strings.Replace(proxy.URL, "://", "://proxy-user:proxy-password@", 1), Enabled: true}
	requireDataNoError(t, st.CreateAccount(context.Background(), a))
	requireDataNoError(t, st.SaveCodexCredential(context.Background(), a.ID, "codex-secret", "codex-refresh", "", time.Now().Add(time.Hour).UnixMilli(), "codex-id"))
	a, _ = st.GetAccount(context.Background(), a.ID)
	cfg := config.Default()
	cfg.Upstream.BaseURL = target.URL + "/v1"
	cfg.Models.CodexBaseURL = target.URL + "/codex"
	cfg.Models.CodexClientVersion = config.DefaultCodexClientVersion
	first, err := manager.RefreshModelCatalog(context.Background(), a, cfg, "catalog-test-pi")
	requireDataTruef(t, !(err != nil || first == nil || len(first.Models) != 1 || first.Models[0].ID != "real-model" || first.FetchedAt == 0 || first.Error != ""), "refresh=%+v err=%v", first, err)
	for key, want := range map[string]string{
		"context_window":                       "9007199254740993123456789",
		"supported_reasoning_levels":           "[]",
		"supports_reasoning_summary_parameter": "false",
		"unknown":                              `{"nested":[null,false,[]]}`,
	} {
		reportTestFailure(t, string(first.Models[0].Metadata[key]) != want, func() {
			t.Fatalf("refresh lost metadata %s=%s want=%s", key, first.Models[0].Metadata[key], want)
		})
	}
	status.Store(403)
	second, err := manager.RefreshModelCatalog(context.Background(), a, cfg, "catalog-test-pi")
	requireDataTruef(t, !(err == nil || second == nil || second.Error == "" || second.FetchedAt != first.FetchedAt || len(second.Models) != 1), "error refresh destroyed cache: %+v err=%v", second, err)
	reportTestFailure(t, !reflect.DeepEqual(second.Models[0].Metadata, first.Models[0].Metadata), func() {
		t.Fatalf("failed refresh changed metadata: got=%v want=%v", second.Models[0].Metadata, first.Models[0].Metadata)
	})
	for _, secret := range []string{"primary-secret", "codex-secret", "proxy-password"} {
		requireDataCondition(t, !(strings.Contains(second.Error, secret) || strings.Contains(err.Error(), secret)), "catalog error leaked secret")
	}
	reportTestFailure(t, targetCalls.Load() != 2 || codexCalls.Load() != 2 || proxyCalls.Load() != 4, func() {
		t.Fatalf("wrong outbound path: primary=%d codex=%d proxy=%d", targetCalls.Load(), codexCalls.Load(), proxyCalls.Load())
	})
}

// catalogRoundTrip rewrites only the token endpoint to a local test server;
// the underlying real HTTP transport still traverses the selected local proxy.
type catalogRoundTrip func(*http.Request) (*http.Response, error)

func (f catalogRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestModelCatalogPrimaryRefreshTraversesSelectedProxy(t *testing.T) {
	var refreshes, modelCalls, proxyCalls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/accounts/oauth/token" {
			refreshes.Add(1)
			if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "primary-refresh" || r.Form.Get("client_id") != "issued-client" || r.Form.Get("resource") != oauth.ChatGPTResource {
				t.Errorf("wrong primary refresh form: %v %v", r.Form, err)
			}
			fmt.Fprint(w, `{"access_token":"rotated-primary","refresh_token":"rotated-refresh","expires_in":3600,"scope":"chatgpt.tokens.use.direct"}`)
			return
		}
		modelCalls.Add(1)
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer rotated-primary" {
			t.Errorf("catalog did not use refreshed primary credential: %s %v", r.URL.Path, r.Header)
		}
		fmt.Fprint(w, `{"data":[{"id":"actual-model"}]}`)
	}))
	defer target.Close()
	targetURL, _ := url.Parse(target.URL)
	tr := &http.Transport{Proxy: nil}
	defer tr.CloseIdleConnections()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		forward := r.Clone(r.Context())
		forward.RequestURI = ""
		resp, err := tr.RoundTrip(forward)
		if err != nil {
			t.Error(err)
			w.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()
	st := newAccountsTestStore(t)
	factory := egress.NewFactory(egress.Options{})
	httpClient, err := factory.HTTPClient(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	baseTransport := httpClient.Transport
	httpClient.Transport = catalogRoundTrip(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		if r.URL.Host == "auth.openai.com" {
			endpoint := *r.URL
			endpoint.Scheme, endpoint.Host = targetURL.Scheme, targetURL.Host
			clone.URL = &endpoint
			clone.Host = targetURL.Host
		}
		return baseTransport.RoundTrip(clone)
	})
	manager := New(st, oauth.NewClient(nil), factory, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	a := &store.Account{Name: "refresh", AccessToken: "expired-primary", RefreshToken: "primary-refresh", OAuthClientID: "issued-client", ExpiresAt: 1, ProxyURL: proxy.URL}
	if err := st.CreateAccount(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Upstream.BaseURL = target.URL + "/v1"
	got, err := manager.RefreshModelCatalog(context.Background(), a, cfg, "")
	if err != nil || got == nil || len(got.Models) != 1 || refreshes.Load() != 1 || modelCalls.Load() != 1 || proxyCalls.Load() != 2 {
		t.Fatalf("wrong refresh path: %+v %v refresh=%d model=%d proxy=%d", got, err, refreshes.Load(), modelCalls.Load(), proxyCalls.Load())
	}
}

// This integration regression intentionally covers the shared primary refresh
// lock: diagnostics must stay cancelable while a background refresh owns it.
func TestModelCatalogCancellationWhileWaitingForCredential(t *testing.T) {
	st := newAccountsTestStore(t)
	manager := New(st, oauth.NewClient(nil), egress.NewFactory(egress.Options{}), Options{})
	a := &store.Account{Name: "credential wait", AccessToken: "primary", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	if err := st.CreateAccount(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	lock := manager.refreshLock(a.ID)
	lock.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := manager.RefreshModelCatalog(ctx, a, config.Default(), "")
		done <- err
	}()
	select {
	case err := <-done:
		lock.Unlock()
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("canceled credential wait returned %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		lock.Unlock()
		<-done
		t.Fatal("model catalog cancellation remained blocked on a primary credential refresh lock")
	}
}

func TestModelCatalogRefreshBoundsCancellationAndRejectsRedirects(t *testing.T) {
	for _, mode := range []string{"oversize", "invalid", "http200error", "cancel", "timeout", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{}, 1)
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				started <- struct{}{}
				switch mode {
				case "oversize":
					fmt.Fprint(w, strings.Repeat(" ", maxCatalogBytes+1))
				case "invalid":
					fmt.Fprint(w, "not JSON")
				case "http200error":
					fmt.Fprint(w, `{"error":{"message":"sensitive-bearer"}}`)
				case "cancel", "timeout":
					<-r.Context().Done()
				case "redirect":
					w.Header().Set("Location", "/must-not-follow")
					w.WriteHeader(302)
				}
			}))
			defer target.Close()
			st := newAccountsTestStore(t)
			manager := New(st, oauth.NewClient(nil), egress.NewFactory(egress.Options{}), Options{})
			a := &store.Account{Name: mode, AccessToken: "primary-secret", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
			requireDataNoError(t, st.CreateAccount(context.Background(), a))
			cfg := config.Default()
			cfg.Upstream.BaseURL = target.URL + "/v1"
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if mode == "cancel" {
				go func() { <-started; cancel() }()
			}
			got, err := manager.RefreshModelCatalog(ctx, a, cfg, "")
			requireDataTruef(t, !(err == nil || got == nil || got.Error == "" || got.FetchedAt != 0 || len(got.Models) != 0 || got.Models == nil), "bad response accepted: catalog=%+v err=%v", got, err)
			cached, err := st.GetAccountModelCatalog(context.Background(), a.ID)
			requireDataTruef(t, !(err != nil || cached == nil || cached.Error != got.Error || cached.AttemptedAt == 0), "failed attempt not persisted: %+v %v", cached, err)
		})
	}
}

// A canceled network context must not cancel the local snapshot read: otherwise
// database/sql may retire the only :memory: connection, destroying the schema
// before the detached save. Starting canceled makes this contract deterministic
// rather than relying only on the concurrent HTTP-cancellation regression above.
func TestModelCatalogCanceledSourcesKeepSnapshotsForPersistence(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) { checkCanceledCatalogSourcesPreserveSnapshots(t, mode) })
	}
}

func TestModelCatalogRefreshDecodesCompressedJSON(t *testing.T) {
	const valid = `{"data":[{"id":"compressed-model","name":"Compressed model"}]}`
	for _, tc := range []struct {
		name, encoding, body, wantError string
		corrupt                         bool
	}{
		{name: "gzip", encoding: "gzip", body: valid},
		{name: "deflate", encoding: "deflate", body: valid},
		{name: "identity", encoding: "identity", body: valid},
		{name: "gzip_invalid_json", encoding: "gzip", body: "not-json primary-secret", wantError: "invalid JSON"},
		{name: "gzip_corrupt", encoding: "gzip", body: "broken gzip primary-secret", corrupt: true, wantError: "could not be decompressed"},
		{name: "gzip_expansion_limit", encoding: "gzip", body: `{"data":[{"id":"large","description":"` + strings.Repeat("x", maxCatalogBytes) + `"}]}`, wantError: "exceeds size limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire bytes.Buffer
			var compressor io.WriteCloser
			switch {
			case tc.corrupt || tc.encoding == "identity":
				wire.WriteString(tc.body)
			case tc.encoding == "gzip":
				compressor = gzip.NewWriter(&wire)
			case tc.encoding == "deflate":
				compressor = zlib.NewWriter(&wire)
			}
			if compressor != nil {
				{
					_, err := io.WriteString(compressor, tc.body)
					requireDataTrue(t, !(err != nil), err)
				}
				requireDataNoError(t, compressor.Close())
			}
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reportTestFailure(t, r.Header.Get("Accept-Encoding") != "gzip, deflate", func() {
					t.Errorf("catalog compression negotiation changed: %q", r.Header.Get("Accept-Encoding"))
				})
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Encoding", tc.encoding)
				_, _ = w.Write(wire.Bytes())
			}))
			defer target.Close()
			st, err := store.Open(":memory:")
			requireDataNoError(t, err)
			st.DB().SetMaxOpenConns(1)
			defer st.Close()
			ctx := context.Background()
			a := &store.Account{Name: "compressed catalog", AccessToken: "primary-secret", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Enabled: true}
			requireDataNoError(t, st.CreateAccount(ctx, a))
			previous := &store.ModelCatalog{Models: []store.CatalogModel{{ID: "cached-model", Name: "Cached model"}}, FetchedAt: 123, AttemptedAt: 123}
			requireDataNoError(t, st.SaveAccountModelCatalog(ctx, a.ID, previous))
			manager := New(st, oauth.NewClient(nil), egress.NewFactory(egress.Options{}), Options{})
			cfg := config.Default()
			cfg.Upstream.BaseURL = target.URL + "/v1"
			got, err := manager.RefreshModelCatalog(ctx, a, cfg, "catalog-test-pi")
			if tc.wantError != "" {
				reportTestFailure(t, err == nil || !strings.Contains(err.Error(), tc.wantError), func() {
					t.Fatalf("error=%v, want %q", err, tc.wantError)
				})
				requireDataTruef(t, !(got == nil || got.FetchedAt != previous.FetchedAt || len(got.Models) != 1 || got.Models[0].ID != "cached-model"), "decode failure destroyed successful cache: %+v", got)
				requireDataCondition(t, !(strings.Contains(got.Error, "primary-secret")), "decode error leaked response body")
				return
			}
			requireDataTruef(t, !(err != nil || got == nil || len(got.Models) != 1 || got.Models[0].ID != "compressed-model" || got.Models[0].Name != "Compressed model" || got.Error != ""), "compressed catalog=%+v error=%v", got, err)
		})
	}
}

func TestParseModelCatalogDuplicatesFillOnlyAbsentMetadata(t *testing.T) {
	models, err := parseModelCatalog([]byte(`{"data":[
		"string-first",
		{"id":"string-first","display_name":"Detailed label","context_window":65536},
		{"id":"object-first","name":"First label","default_reasoning_level":null,"supports_parallel_tool_calls":false,"supported_reasoning_levels":[],"future":{"first":true}},
		{"id":"object-first","name":"Second label","default_reasoning_level":"high","supports_parallel_tool_calls":true,"supported_reasoning_levels":[{"effort":"high"}],"future":{"second":true},"max_context_window":9007199254740993}
	]}`))
	if err != nil || len(models) != 2 {
		t.Fatalf("duplicate catalog=%+v error=%v", models, err)
	}
	if models[0].Name != "Detailed label" || string(models[0].Metadata["context_window"]) != "65536" {
		t.Fatalf("string entry discarded its duplicate's metadata: %+v", models[0])
	}
	if models[1].Name != "First label" {
		t.Fatalf("duplicate replaced the first label: %+v", models[1])
	}
	for field, want := range map[string]string{
		"default_reasoning_level":      "null",
		"supports_parallel_tool_calls": "false",
		"supported_reasoning_levels":   "[]",
		"future":                       `{"first":true}`,
		"max_context_window":           "9007199254740993",
	} {
		if got := string(models[1].Metadata[field]); got != want {
			t.Errorf("metadata[%q]=%s want=%s", field, got, want)
		}
	}
}

func checkCanceledCatalogSourcesPreserveSnapshots(t *testing.T, mode string) {
	st := newAccountsTestStore(t)
	base := context.Background()
	expires := time.Now().Add(time.Hour).UnixMilli()
	a := &store.Account{Name: mode, AccessToken: "primary-secret", ExpiresAt: expires}
	requireDataNoError(t, st.CreateAccount(base, a))
	requireDataNoError(t, st.SaveCodexCredential(base, a.ID, "codex-secret", "codex-refresh", "", expires, "codex-id"))
	var err error
	a, err = st.GetAccount(base, a.ID)
	requireDataTruef(t, !(err != nil || a == nil), "load linked account: %v", err)
	sources := []string{store.ModelSourceChatGPT, store.ModelSourceCodex}
	seed, err := st.BeginAccountModelCatalogRefresh(base, a.ID)
	requireDataNoError(t, err)
	initial := make(map[string]store.ModelCatalogSourceResult)
	for _, source := range sources {
		initial[source] = store.ModelCatalogSourceResult{
			CredentialFingerprint: store.ModelCatalogCredentialFingerprint(a, source),
			Catalog:               store.ModelCatalogSource{FetchedAt: store.NowMS(), Models: []store.CatalogModel{{ID: source + "-kept"}}},
		}
	}
	{
		_, err := st.SaveAccountModelCatalogRefresh(base, a.ID, seed, initial)
		requireDataTrue(t, !(err != nil), err)
	}
	before, err := st.GetAccountModelCatalog(base, a.ID)
	requireDataTruef(t, !(err != nil || before == nil || len(before.Models) != 2), "seed catalog: %+v %v", before, err)
	attempt, err := st.BeginAccountModelCatalogRefresh(base, a.ID)
	requireDataNoError(t, err)
	factory := egress.NewFactory(egress.Options{})
	client, err := factory.HTTPClient("")
	requireDataNoError(t, err)
	var networkCalls atomic.Int32
	client.Transport = catalogRoundTrip(func(*http.Request) (*http.Response, error) {
		networkCalls.Add(1)
		return nil, fmt.Errorf("canceled source must not send network requests")
	})
	manager := New(st, oauth.NewClient(nil), factory, Options{})
	ctx, cancel := context.WithCancel(base)
	cancel()
	wantError := "model catalog request canceled"
	if mode == "deadline" {
		ctx, cancel = context.WithDeadline(base, time.Unix(1, 0))
		defer cancel()
		wantError = "model catalog request timed out"
	}
	cfg := config.Default()
	results := make(map[string]store.ModelCatalogSourceResult)
	for _, source := range sources {
		result := manager.fetchCatalogSource(ctx, a.ID, source, attempt.AttemptedAt, cfg, "")
		requireDataTruef(t, !(result.CredentialFingerprint != initial[source].CredentialFingerprint || result.CredentialFingerprint == ""), "%s lost its snapshot when the network context ended", source)
		reportTestFailure(t, result.Catalog.Error != wantError || result.Catalog.FetchedAt != 0 || result.Catalog.Skipped, func() {
			t.Fatalf("%s ignored cancellation or misreported the result: %+v", source, result.Catalog)
		})
		results[source] = result
	}
	reportTestFailure(t, networkCalls.Load() != 0, func() {
		t.Fatalf("canceled sources sent %d network requests", networkCalls.Load())
	})
	applied, err := st.SaveAccountModelCatalogRefresh(base, a.ID, attempt, results)
	requireDataTruef(t, !(err != nil), "cancellation damaged the store before the detached save: %v", err)
	for _, source := range sources {
		requireDataTruef(t, !(!applied[source]), "%s cancellation status was not persisted", source)
	}
	after, err := st.GetAccountModelCatalog(base, a.ID)
	requireDataTruef(t, !(err != nil || after == nil || !reflect.DeepEqual(after.Models, before.Models)), "cancellation destroyed the last good catalogs: before=%+v after=%+v err=%v", before, after, err)
	for _, source := range sources {
		{
			got := after.SourceCatalogs[source]
			requireDataTruef(t, !(got.Error != wantError || got.FetchedAt != before.SourceCatalogs[source].FetchedAt || got.AttemptedAt != attempt.AttemptedAt), "%s failure status or last-success timestamp changed: %+v", source, got)
		}
	}
}

// testModelCatalogRefreshThroughProxyPrimaryTokenAndFailurePreservationPhase1 exercises one isolated test phase with the original shared state.
func testModelCatalogRefreshThroughProxyPrimaryTokenAndFailurePreservationPhase1(w http.ResponseWriter, r *http.Request, capturedT **testing.T, capturedStatus *atomic.Int64, capturedTargetCalls *atomic.Int64, capturedCodexCalls *atomic.Int64) {
	if r.URL.Path == "/codex/models" {
		(*capturedCodexCalls).Add(1)
		reportTestFailure((*capturedT), r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer codex-secret" ||
			r.Header.Get("ChatGPT-Account-Id") != "codex-id" || r.Header.Get("User-Agent") != "codex-tui/"+config.DefaultCodexClientVersion ||
			r.Header.Get("Version") != config.DefaultCodexClientVersion || r.Header.Get("Originator") != codexCatalogOriginator ||
			r.Header.Get("Proxy-Authorization") != "" || r.URL.Query().Get("client_version") != config.DefaultCodexClientVersion, func() {
			(*capturedT).Errorf("incorrect local Codex request: %s %s headers=%v", r.Method, r.URL, r.Header)
		})
		w.WriteHeader(int((*capturedStatus).Load()))
		if (*capturedStatus).Load() == http.StatusOK {
			fmt.Fprint(w, `{"models":[]}`)
		} else {
			fmt.Fprint(w, `{"error":"primary-secret codex-secret proxy-password"}`)
		}
		return
	}
	(*capturedTargetCalls).Add(1)
	reportTestFailure((*capturedT), r.Method != "GET" || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer primary-secret", func() {
		(*capturedT).Errorf("incorrect catalog target/credential: method=%s path=%s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
	})
	reportTestFailure((*capturedT), r.Header.Get("User-Agent") != "catalog-test-pi" || r.Header.Get("ChatGPT-Account-Id") != "" || r.Header.Get("OpenAI-Beta") != "" || r.Header.Get("Proxy-Authorization") != "", func() {
		(*capturedT).Errorf("wrong primary ChatGPT headers: %v", r.Header)
	})
	w.WriteHeader(int((*capturedStatus).Load()))
	if (*capturedStatus).Load() != 200 {
		fmt.Fprint(w, `{"error":"primary-secret codex-secret proxy-password"}`)
		return
	}
	fmt.Fprint(w, `{"data":[{"id":"real-model","display_name":"Live Model","supported_reasoning_levels":[],"supports_reasoning_summary_parameter":false,"context_window":9007199254740993123456789,"unknown":{"nested":[null,false,[]]}},{"id":"real-model","unknown":"duplicate-must-not-win"}]}`)
}
