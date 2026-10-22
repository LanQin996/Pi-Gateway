package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pi-gateway/internal/config"
	"pi-gateway/internal/store"
)

// This covers the real admin -> Manager -> release/manifest -> SQLite -> public
// catalog path. Model entries are supplied only by the local Codex manifest.
func TestCodexAutoVersionAdminRefreshAndPublicModels(t *testing.T) {
	const latestURL = "https://api.github.com/repos/openai/codex/releases/latest"
	const version = "0.170.0"
	const mainUserAgent = "auto-catalog-primary-pi"
	const manifest = `{"models":[
		{"slug":"gpt-6-sol","display_name":"GPT-6 Sol","description":"Local Codex model","supported_reasoning_levels":[{"effort":"high","description":"Deep"}],"default_reasoning_level":"high","supports_parallel_tool_calls":false,"context_window":9007199254740993,"input_modalities":["text","image"],"future_extension":{"enabled":false,"items":[],"nullable":null}},
		{"slug":"gpt-6.1-sol","display_name":"GPT-6.1 Sol","description":"Local Codex successor","supported_reasoning_levels":[{"effort":"xhigh"}],"default_reasoning_level":"xhigh","supports_parallel_tool_calls":true,"context_window":256000,"input_modalities":["text"],"default_reasoning_summary":null}
	]}`
	var expected struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	{
		err := json.Unmarshal([]byte(manifest), &expected)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	wantModels := make(map[string]map[string]json.RawMessage, len(expected.Models))
	for _, model := range expected.Models {
		var id string
		{
			err := json.Unmarshal(model["slug"], &id)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		wantModels[id] = model
	}

	var releaseCalls, codexCalls, primaryCalls, responseCalls, proxyCalls atomic.Int64
	checkHeaders := func(w http.ResponseWriter, r *http.Request, want map[string]string) bool {
		return serveCodexAutoVersionAdminRefreshAndPublicModelsRequest(w, r, want, &t)
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveCodexAutoVersionAdminRefreshAndPublicModelsRequest2(w, r, serveCodexAutoVersionAdminRefreshAndPublicModelsRequest2State{capturedT: &t, capturedVersion: version, capturedMainUserAgent: mainUserAgent, capturedManifest: manifest, capturedReleaseCalls: &releaseCalls, capturedCodexCalls: &codexCalls, capturedPrimaryCalls: &primaryCalls, capturedResponseCalls: &responseCalls, capturedCheckHeaders: &checkHeaders})
	}))
	t.Cleanup(target.Close)
	h := newProxyHarness(t, target.URL+"/v1", "sse")
	ctx := context.Background()
	h.admin.cfg.Models.CodexClientVersion = config.AutoCodexClientVersion
	h.admin.cfg.Models.CodexBaseURL = target.URL + "/codex"
	h.admin.cfg.Upstream.UserAgent = "configured-primary-pi"
	runtime := h.admin.settings.Get()
	runtime.UserAgent = mainUserAgent
	{
		err := h.admin.settings.Set(ctx, runtime)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	{
		err := h.db.SaveCodexCredential(ctx, h.account.ID, "local-codex-auto-token", "local-codex-auto-refresh", "", time.Now().Add(time.Hour).UnixMilli(), "local-codex-auto-account")
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	group := &store.AccountGroup{Name: "Codex-linked primary account", Enabled: true, AccountIDs: []int64{h.account.ID}}
	{
		err := h.db.CreateAccountGroup(ctx, group)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	key := &store.APIKey{Name: "scoped model reader", Key: "sk-local-auto-models", Enabled: true, GroupIDs: []int64{group.ID}}
	{
		err := h.db.CreateKey(ctx, key)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	proxy := localForwardProxy(t, target.URL, &proxyCalls, false)
	proxyResult := h.request(t, http.MethodPost, "/api/proxies", proxyPayload("auto version egress", proxy.URL), http.StatusCreated, true)
	h.request(t, http.MethodPatch, fmt.Sprintf("/api/accounts/%d", h.account.ID), fmt.Sprintf(`{"proxy_id":%d}`, savedProxyID(proxyResult)), http.StatusOK, true)
	account, err := h.db.GetAccount(ctx, h.account.ID)
	reportTestFailure(t, err != nil || account == nil, func() {
		t.Fatalf("read linked account: account=%v error=%v", account, err)
	})
	reportTestFailure(t, account.ProxyURL != proxy.URL || account.CodexAccessToken != "local-codex-auto-token" || len(account.SupplementalModels) != 0 || !reflect.DeepEqual(account.GroupIDs, key.GroupIDs), func() {
		t.Fatal("fixture must use a real linked credential and matching primary account/key groups without supplemental models")
	})

	// Rewrite exactly the fixed release URL on the Factory's selected cached
	// client. The actual HTTP transport still traverses the local account proxy.
	client, err := h.factory.HTTPClient(account.ProxyURL)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	base := client.Transport
	t.Cleanup(client.CloseIdleConnections)
	local, err := url.Parse(target.URL)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	client.Transport = modelTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		return verifyCodexAutoVersionAdminRefreshAndPublicModels(r, &t, latestURL, &base, &local)
	})
	// A resolver accidentally using the direct client must fail locally too,
	// rather than reaching GitHub or silently bypassing the selected proxy.
	direct, err := h.factory.HTTPClient("")
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	direct.Transport = modelTestRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Error("outbound request bypassed the account proxy")
		return nil, fmt.Errorf("test blocks direct outbound requests")
	})

	request := func(method, path, token, body string) (http.Header, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, h.server.URL+path, strings.NewReader(body))
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res, err := h.server.Client().Do(req)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		reportTestFailure(t, err != nil || res.StatusCode != http.StatusOK, func() {
			t.Fatalf("%s %s status=%d body=%s error=%v", method, path, res.StatusCode, raw, err)
		})
		return res.Header, raw
	}
	checkModels := func(models []store.CatalogModel) {
		t.Helper()
		reportTestFailure(t, len(models) != len(wantModels), func() {
			t.Fatalf("expected both auto-negotiated Codex models, got %+v", models)
		})
		seen := make(map[string]bool)
		for _, model := range models {
			want, exists := wantModels[model.ID]
			reportTestFailure(t, !exists || seen[model.ID] || model.Source != "upstream" || !reflect.DeepEqual(model.Origins, []string{store.ModelSourceCodex}), func() {
				t.Fatalf("lost Codex model identity/provenance: %+v", model)
			})
			seen[model.ID] = true
			reportTestFailure(t, len(model.Metadata) != len(want), func() {
				t.Fatalf("unexpected stored metadata fields for %s: %v", model.ID, model.Metadata)
			})
			assertCatalogMetadataJSON(t, model.Metadata, want)
		}
	}
	checkCatalog := func(catalog *store.ModelCatalog) {
		verifyCodexAutoVersionAdminRefreshAndPublicModels2(catalog, &t, &checkModels)
	}
	adminPath := fmt.Sprintf("/api/accounts/%d/models", h.account.ID)
	_, initial := request(http.MethodGet, adminPath, h.token, "")
	var empty store.ModelCatalog
	{
		err := json.Unmarshal(initial, &empty)
		reportTestFailure(t, err != nil || len(empty.Models) != 0 || empty.FetchedAt != 0 || releaseCalls.Load()+codexCalls.Load()+primaryCalls.Load() != 0, func() {
			t.Fatalf("unrefreshed catalog invented models or made an upstream request: %s error=%v", initial, err)
		})
	}
	for attempt := 0; attempt < 2; attempt++ {
		_, raw := request(http.MethodPost, adminPath+"/refresh", h.token, "")
		var refreshed store.ModelCatalog
		{
			err := json.Unmarshal(raw, &refreshed)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		checkCatalog(&refreshed)
	}
	stored, err := h.db.GetAccountModelCatalog(ctx, account.ID)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	checkCatalog(stored)
	_, raw := request(http.MethodGet, adminPath, h.token, "")
	var adminCatalog store.ModelCatalog
	{
		err := json.Unmarshal(raw, &adminCatalog)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	checkCatalog(&adminCatalog)
	reportTestFailure(t, !reflect.DeepEqual(stored, &adminCatalog), func() {
		t.Fatal("admin GET did not return the persisted merged source catalog")
	})

	for _, mode := range []struct{ query, field string }{{"", "data"}, {"?client_version=0.144.0", "models"}} {
		t.Run(mode.field, func(t *testing.T) {
			runCodexAutoVersionAdminRefreshAndPublicModelsCase(t, &wantModels, &key, &request, &mode)
		})
	}
	_, raw = request(http.MethodPost, "/v1/responses", key.Key, `{"model":"gpt-6.1-sol","input":"hi","stream":false}`)
	var response map[string]json.RawMessage
	{
		err := json.Unmarshal(raw, &response)
		reportTestFailure(t, err != nil || string(response["id"]) != `"local-primary-response"` || string(response["status"]) != `"completed"`, func() {
			t.Fatalf("generation did not preserve the primary ChatGPT route: %s error=%v", raw, err)
		})
	}
	reportTestFailure(t, releaseCalls.Load() != 1 || codexCalls.Load() != 2 || primaryCalls.Load() != 2 || responseCalls.Load() != 1 || proxyCalls.Load() != 6, func() {
		t.Fatalf("unexpected calls (release cache/public reads/proxy): release=%d codex=%d primary=%d response=%d proxy=%d", releaseCalls.Load(), codexCalls.Load(), primaryCalls.Load(), responseCalls.Load(), proxyCalls.Load())
	})
	reportTestFailure(t, h.admin.cfg.Models.CodexClientVersion != config.AutoCodexClientVersion || h.admin.cfg.Upstream.UserAgent != "configured-primary-pi" || h.admin.settings.Get().UserAgent != mainUserAgent, func() {
		t.Fatal("resolving a Codex manifest version mutated the shared primary configuration")
	})
}

// serveCodexAutoVersionAdminRefreshAndPublicModelsRequest exercises one isolated test phase with the original shared state.
func serveCodexAutoVersionAdminRefreshAndPublicModelsRequest(w http.ResponseWriter, r *http.Request, want map[string]string, capturedT **testing.T) bool {
	(*capturedT).Helper()
	for key, value := range want {
		if got := r.Header.Get(key); got != value {
			(*capturedT).Errorf("%s header %s=%q want=%q", r.URL.Path, key, got, value)
			http.Error(w, "unexpected client identity", http.StatusPreconditionFailed)
			return false
		}
	}
	return true
}

// verifyCodexAutoVersionAdminRefreshAndPublicModels exercises one isolated test phase with the original shared state.
func verifyCodexAutoVersionAdminRefreshAndPublicModels(r *http.Request, capturedT **testing.T, capturedLatestURL string, capturedBase *http.RoundTripper, capturedLocal **url.URL) (*http.Response, error) {
	(*capturedT).Helper()

	clone := r.Clone(r.Context())
	if r.URL.String() == capturedLatestURL {
		deadline, ok := r.Context().Deadline()
		reportTestFailure((*capturedT), !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second, func() {
			(*capturedT).Error("release lookup must have a live deadline bounded by five seconds")
		})
		clone.URL.Scheme, clone.URL.Host = (*capturedLocal).Scheme, (*capturedLocal).Host
		clone.Host = (*capturedLocal).Host
	}
	if clone.URL.Scheme != (*capturedLocal).Scheme || clone.URL.Host != (*capturedLocal).Host || clone.URL.User != nil {
		(*capturedT).Errorf("blocked non-local outbound request: %s", r.URL)
		return nil, fmt.Errorf("test blocks non-local outbound requests")
	}
	return (*capturedBase).RoundTrip(clone)
}

// verifyCodexAutoVersionAdminRefreshAndPublicModels2 exercises one isolated test phase with the original shared state.
func verifyCodexAutoVersionAdminRefreshAndPublicModels2(catalog *store.ModelCatalog, capturedT **testing.T, capturedCheckModels *func(models []store.CatalogModel)) {
	(*capturedT).Helper()
	reportTestFailure((*capturedT), catalog == nil || catalog.Error != "" || catalog.FetchedAt == 0 || catalog.AttemptedAt == 0 || len(catalog.SourceCatalogs) != 2, func() {
		(*capturedT).Fatalf("refresh did not persist a successful dual-source catalog: %+v", catalog)
	})
	(*capturedCheckModels)(catalog.Models)
	for _, source := range []string{store.ModelSourceChatGPT, store.ModelSourceCodex} {
		snapshot, exists := catalog.SourceCatalogs[source]
		reportTestFailure((*capturedT), !exists || snapshot.Skipped || snapshot.Error != "" || snapshot.FetchedAt == 0 || snapshot.AttemptedAt == 0, func() {
			(*capturedT).Fatalf("missing successful %s source: %+v", source, snapshot)
		})
		if source == store.ModelSourceCodex {
			(*capturedCheckModels)(snapshot.Models)
		} else {
			reportTestFailure((*capturedT), len(snapshot.Models) != 0, func() {
				(*capturedT).Fatal("Codex models must not be supplied by the primary fixture")
			})
		}
	}
}

// runCodexAutoVersionAdminRefreshAndPublicModelsCase exercises one isolated test phase with the original shared state.
func runCodexAutoVersionAdminRefreshAndPublicModelsCase(t *testing.T, capturedWantModels *map[string]map[string]json.RawMessage, capturedKey **store.APIKey, capturedRequest *func(method string, path string, token string, body string) (http.Header, []byte), capturedMode *struct {
	query string
	field string
}) {
	t.Helper()

	headers, raw := (*capturedRequest)(http.MethodGet, "/v1/models"+(*capturedMode).query, (*capturedKey).Key, "")
	reportTestFailure(t, headers.Get("Cache-Control") != "private, no-store" || headers.Get("X-Model-Catalog-Status") != "cached", func() {
		t.Fatalf("incorrect public catalog cache headers: %v", headers)
	})
	var envelope map[string]json.RawMessage
	{
		err := json.Unmarshal(raw, &envelope)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, (*capturedMode).field == "data" && (len(envelope) != 2 || string(envelope["object"]) != `"list"`) || (*capturedMode).field == "models" && len(envelope) != 1, func() {
		t.Fatalf("incorrect public models envelope: %s", raw)
	})
	var entries []map[string]json.RawMessage
	{
		err := json.Unmarshal(envelope[(*capturedMode).field], &entries)
		reportTestFailure(t, err != nil || len(entries) != 2, func() {
			t.Fatalf("both auto-negotiated Codex models must be public: %s error=%v", raw, err)
		})
	}
	seen := make(map[string]bool)
	for _, entry := range entries {
		var id string
		{
			err := json.Unmarshal(entry["id"], &id)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		want, exists := (*capturedWantModels)[id]
		reportTestFailure(t, !exists || seen[id] || string(entry["source"]) != `"upstream"` || string(entry["origins"]) != `["codex"]` || string(entry["object"]) != `"model"`, func() {
			t.Fatalf("public model identity/provenance changed: %s", raw)
		})
		seen[id] = true
		assertCatalogMetadataJSON(t, entry, want)
	}
	for _, secret := range []string{"local-model-token", "local-codex-auto-token", "local-codex-auto-refresh", "local-codex-auto-account"} {
		reportTestFailure(t, strings.Contains(string(raw), secret), func() {
			t.Fatalf("public catalog exposed credential %q", secret)
		})
	}
}

type serveCodexAutoVersionAdminRefreshAndPublicModelsRequest2State struct {
	capturedT             **testing.T
	capturedVersion       string
	capturedMainUserAgent string
	capturedManifest      string
	capturedReleaseCalls  *atomic.Int64
	capturedCodexCalls    *atomic.Int64
	capturedPrimaryCalls  *atomic.Int64
	capturedResponseCalls *atomic.Int64
	capturedCheckHeaders  *func(w http.ResponseWriter, r *http.Request, want map[string]string) bool
}

// serveCodexAutoVersionAdminRefreshAndPublicModelsRequest2 exercises one isolated test phase with the original shared state.
func serveCodexAutoVersionAdminRefreshAndPublicModelsRequest2(w http.ResponseWriter, r *http.Request, state serveCodexAutoVersionAdminRefreshAndPublicModelsRequest2State) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/repos/openai/codex/releases/latest":
		(*state.capturedReleaseCalls).Add(1)
		if r.Method != http.MethodGet || r.URL.RawQuery != "" {
			(*state.capturedT).Errorf("unexpected release lookup: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !(*state.capturedCheckHeaders)(w, r, map[string]string{"Authorization": "", "ChatGPT-Account-Id": "", "Originator": "", "Version": "", "Cookie": "", "Proxy-Authorization": ""}) {
			return
		}
		fmt.Fprint(w, `{"tag_name":"rust-v0.170.0","name":"0.170.0","draft":false,"prerelease":false}`)
	case "/codex/models":
		(*state.capturedCodexCalls).Add(1)
		if r.Method != http.MethodGet || r.URL.RawQuery != "client_version="+state.capturedVersion {
			(*state.capturedT).Errorf("Codex manifest requires the resolved release version: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		if !(*state.capturedCheckHeaders)(w, r, map[string]string{
			"Authorization": "Bearer local-codex-auto-token", "ChatGPT-Account-Id": "local-codex-auto-account",
			"User-Agent": "codex-tui/" + state.capturedVersion, "Originator": "codex_cli_rs", "Version": state.capturedVersion,
			"Accept": "application/json", "OpenAI-Beta": "", "X-Stainless-Lang": "", "Proxy-Authorization": "",
		}) {
			return
		}
		fmt.Fprint(w, state.capturedManifest)
	case "/v1/models", "/v1/responses":
		serveAutoCatalogPrimary(w, r, state)
	default:
		(*state.capturedT).Errorf("unexpected local upstream path: %s", r.URL)
		w.WriteHeader(http.StatusNotFound)
	}
}

func serveAutoCatalogPrimary(w http.ResponseWriter, r *http.Request, state serveCodexAutoVersionAdminRefreshAndPublicModelsRequest2State) {
	if !(*state.capturedCheckHeaders)(w, r, map[string]string{
		"Authorization": "Bearer local-model-token", "User-Agent": state.capturedMainUserAgent,
		"ChatGPT-Account-Id": "", "Originator": "", "Version": "", "OpenAI-Beta": "",
		"X-Stainless-Lang": "js", "X-Stainless-Package-Version": "7.19.0", "Proxy-Authorization": "",
	}) {
		return
	}
	if r.URL.RawQuery != "" {
		(*state.capturedT).Errorf("primary request inherited Codex query: %s", r.URL)
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	if r.URL.Path == "/v1/models" {
		(*state.capturedPrimaryCalls).Add(1)
		if r.Method != http.MethodGet {
			(*state.capturedT).Errorf("primary catalog method=%s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		fmt.Fprint(w, `{"data":[]}`)
		return
	}
	(*state.capturedResponseCalls).Add(1)
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.Method != http.MethodPost || string(body["model"]) != `"gpt-6.1-sol"` {
		(*state.capturedT).Errorf("unexpected primary response request: method=%s body=%s error=%v", r.Method, body, err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"local-primary-response\",\"status\":\"completed\",\"output\":[]}}\n\n")
}
