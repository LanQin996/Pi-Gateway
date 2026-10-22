package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	dataplane "pi-gateway/internal/api"
	"pi-gateway/internal/store"
)

func TestAdminSupplementalModelsPatchValidationAndAtomicity(t *testing.T) {
	ctx := context.Background()
	s, a := newModelAdmin(t, "http://127.0.0.1:1")
	group := &store.AccountGroup{Name: "manual group", Enabled: true}
	{
		err := s.store.CreateAccountGroup(ctx, group)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	readAccount := func() *store.Account {
		t.Helper()
		got, err := s.store.GetAccount(ctx, a.ID)
		reportTestFailure(t, err != nil || got == nil, func() {
			t.Fatalf("account=%+v err=%v", got, err)
		})
		return got
	}
	out := policyRequest(t, s.handleListAccounts, "GET", 0, "", 200)
	var listed []map[string]json.RawMessage
	{
		err := json.Unmarshal(out["accounts"], &listed)
		reportTestFailure(t, err != nil || len(listed) == 0, func() {
			t.Fatalf("account list=%s err=%v", out["accounts"], err)
		})
	}
	for _, account := range listed {
		reportTestFailure(t, string(account["supplemental_models"]) != "[]", func() {
			t.Fatalf("new account supplemental_models=%s", account["supplemental_models"])
		})
	}
	out = policyRequest(t, s.handleUpdateAccount, "PATCH", a.ID, fmt.Sprintf(`{"name":" Manual account ","group_ids":[%d],"disabled_models":["manual-a"],"supplemental_models":[" manual-z ","manual-a","manual-z"," "]}`, group.ID), 200)
	var view store.Account
	{
		err := json.Unmarshal(out["account"], &view)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, view.Name != "Manual account" || !reflect.DeepEqual(view.SupplementalModels, []string{"manual-a", "manual-z"}) || !reflect.DeepEqual(view.GroupIDs, []int64{group.ID}) || !reflect.DeepEqual(view.DisabledModels, []string{"manual-a"}), func() {
		t.Fatalf("patch contract=%+v", view)
	})
	reportTestFailure(t, strings.Contains(string(out["account"]), "selected-primary") || strings.Contains(string(out["account"]), "secret-codex"), func() {
		t.Fatal("manual model patch leaked credentials")
	})
	policyRequest(t, s.handleUpdateAccount, "PATCH", a.ID, `{"weight":2}`, 200)
	policyRequest(t, s.handleUpdateAccount, "PATCH", a.ID, `{"supplemental_models":null}`, 200)
	before := readAccount()
	reportTestFailure(t, before.Weight != 2 || !reflect.DeepEqual(before.SupplementalModels, view.SupplementalModels), func() {
		t.Fatalf("omitted/null supplemental models changed: %+v", before)
	})
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"length", []string{strings.Repeat("x", 257)}},
		{"count", make([]string, 513)},
		{"nul", []string{"bad\x00model"}},
		{"newline", []string{"bad\nmodel"}},
		{"carriage-return", []string{"bad\rmodel"}},
		{"not-array", "manual-model"},
		{"non-string", []int{42}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"name": "must not save", "disabled_models": []string{}, "supplemental_models": tc.value})
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			policyRequest(t, s.handleUpdateAccount, "PATCH", a.ID, string(body), 400)
			{
				got := readAccount()
				reportTestFailure(t, !reflect.DeepEqual(got, before), func() {
					t.Fatalf("invalid patch changed account: got=%+v before=%+v", got, before)
				})
			}
		})
	}
	// Missing membership fails after the UPDATE; all management fields roll back.
	policyRequest(t, s.handleUpdateAccount, "PATCH", a.ID, `{"name":"must rollback","group_ids":[999999],"proxy_url":"http://127.0.0.1:9999","disabled_models":[],"supplemental_models":["must-not-persist"]}`, 400)
	{
		got := readAccount()
		reportTestFailure(t, !reflect.DeepEqual(got, before), func() {
			t.Fatalf("transaction partially committed: got=%+v before=%+v", got, before)
		})
	}
	models := make([]string, 512)
	for i := range models {
		models[i] = fmt.Sprintf("manual-%03d", i)
	}
	models[0] = strings.Repeat("x", 256)
	body, err := json.Marshal(map[string]any{"supplemental_models": models})
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	policyRequest(t, s.handleUpdateAccount, "PATCH", a.ID, string(body), 200)
	{
		got := readAccount()
		reportTestFailure(t, len(got.SupplementalModels) != 512, func() {
			t.Fatalf("valid maximum-sized model list rejected: %d", len(got.SupplementalModels))
		})
	}
	out = policyRequest(t, s.handleUpdateAccount, "PATCH", a.ID, `{"supplemental_models":[]}`, 200)
	var raw map[string]json.RawMessage
	{
		err := json.Unmarshal(out["account"], &raw)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, string(raw["supplemental_models"]) != "[]" || readAccount().SupplementalModels == nil, func() {
		t.Fatalf("explicit clear must return/store []: %s", out["account"])
	})
}

func TestAdminSupplementalModelsPatchDoesNotRestoreConcurrentProxyOrCredentials(t *testing.T) {
	ctx := context.Background()
	s, a := newModelAdmin(t, "http://127.0.0.1:1")
	proxy := &store.Proxy{Name: "manual proxy", URL: "http://proxy:secret@127.0.0.1:1"}
	{
		err := s.store.CreateProxy(ctx, proxy)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	{
		err := s.store.SetAccountProxy(ctx, a.ID, proxy.ID, proxy.URL)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	body := &policyInterleavedReader{reader: strings.NewReader(`{"name":"after","supplemental_models":["manual-model"]}`), before: func() {
		{
			err := s.store.SetProxyAccounts(ctx, proxy.ID, []int64{})
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		{
			err := s.store.UpdateAccountCredentials(ctx, a.ID, "rotated-access", "rotated-refresh", 987654321, store.AccountStatusExpired, "rotated-error")
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		{
			err := s.store.UpdateAccountOAuthClientID(ctx, a.ID, "rotated-client")
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		{
			err := s.store.SaveCodexCredential(ctx, a.ID, "rotated-codex-access", "rotated-codex-refresh", "rotated-codex-id", 12345, "rotated-codex-account")
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		no := false
		{
			err := s.store.PatchAccountManagementFields(ctx, a.ID, store.AccountManagementPatch{Enabled: &no, DisabledModels: []string{"concurrent-ban"}})
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
	}}
	r := httptest.NewRequest("PATCH", "/api/accounts", body)
	r.SetPathValue("id", fmt.Sprint(a.ID))
	w := httptest.NewRecorder()
	s.handleUpdateAccount(w, r)
	reportTestFailure(t, w.Code != 200, func() {
		t.Fatalf("manual model patch: %d %s", w.Code, w.Body.String())
	})
	got, err := s.store.GetAccount(ctx, a.ID)
	reportTestFailure(t, err != nil || got == nil, func() {
		t.Fatalf("account=%+v err=%v", got, err)
	})
	reportTestFailure(t, got.Name != "after" || !reflect.DeepEqual(got.SupplementalModels, []string{"manual-model"}) || got.ProxyID != nil || got.ProxyURL != "" || got.Enabled || !reflect.DeepEqual(got.DisabledModels, []string{"concurrent-ban"}), func() {
		t.Fatalf("manual model patch restored stale management fields: %+v", got)
	})
	reportTestFailure(t, got.AccessToken != "rotated-access" || got.RefreshToken != "rotated-refresh" || got.ExpiresAt != 987654321 || got.Status != store.AccountStatusExpired || got.LastError != "rotated-error" || got.OAuthClientID != "rotated-client" || got.CodexAccessToken != "rotated-codex-access" || got.CodexRefreshToken != "rotated-codex-refresh" || got.CodexIDToken != "rotated-codex-id" || got.CodexExpiresAt != 12345 || got.CodexAccountID != "rotated-codex-account", func() {
		t.Fatalf("manual model patch restored stale credentials: %+v", got)
	})
}

func TestAdminSupplementalModelsCatalogRefreshAndImmediateRemoval(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if fail.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"shared","name":"Real upstream name"}]}`))
	}))
	defer target.Close()
	s, a := newModelAdmin(t, target.URL)
	policyRequest(t, s.handleUpdateAccount, "PATCH", a.ID, `{"supplemental_models":["shared","manual-only"]}`, 200)
	readCatalog := func(handler http.HandlerFunc, method string, status int) *store.ModelCatalog {
		t.Helper()
		r := httptest.NewRequest(method, "/api/accounts/models", nil)
		r.SetPathValue("id", fmt.Sprint(a.ID))
		w := httptest.NewRecorder()
		handler(w, r)
		reportTestFailure(t, w.Code != status, func() {
			t.Fatalf("catalog status=%d want=%d body=%s", w.Code, status, w.Body.String())
		})
		var catalog store.ModelCatalog
		{
			err := json.Unmarshal(w.Body.Bytes(), &catalog)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		return &catalog
	}
	initial := readCatalog(s.handleAccountModels, "GET", 200)
	reportTestFailure(t, initial.FetchedAt != 0 || initial.AttemptedAt != 0 || len(initial.Models) != 2 || initial.Models[0].Source != "manual" || calls.Load() != 0, func() {
		t.Fatalf("manual GET fabricated upstream activity: %+v calls=%d", initial, calls.Load())
	})
	success := readCatalog(s.handleRefreshAccountModels, "POST", 200)
	want := []store.CatalogModel{
		{ID: "shared", Name: "Real upstream name", Source: "upstream", Origins: []string{store.ModelSourceChatGPT}, Metadata: map[string]json.RawMessage{
			"id": json.RawMessage(`"shared"`), "name": json.RawMessage(`"Real upstream name"`),
		}},
		store.NewManualCatalogModel("manual-only"),
	}
	reportTestFailure(t, success.FetchedAt == 0 || !reflect.DeepEqual(success.Models, want) || calls.Load() != 1, func() {
		t.Fatalf("upstream success lost manual/source precedence: %+v calls=%d", success, calls.Load())
	})
	fail.Store(true)
	failure := readCatalog(s.handleRefreshAccountModels, "POST", 502)
	reportTestFailure(t, failure.FetchedAt != success.FetchedAt || failure.Error == "" || !reflect.DeepEqual(failure.Models, want) || calls.Load() != 2, func() {
		t.Fatalf("upstream failure lost manual/success data: %+v calls=%d", failure, calls.Load())
	})
	policyRequest(t, s.handleUpdateAccount, "PATCH", a.ID, `{"supplemental_models":[]}`, 200)
	removed := readCatalog(s.handleAccountModels, "GET", 200)
	reportTestFailure(t, !reflect.DeepEqual(removed.Models, want[:1]) || removed.FetchedAt != success.FetchedAt || removed.Error != failure.Error || calls.Load() != 2, func() {
		t.Fatalf("manual deletion was not immediately local-only: %+v calls=%d", removed, calls.Load())
	})
}

// Exercise the complete refresh -> SQLite JSON snapshot -> admin/public catalog
// path, rather than constructing CatalogModel directly and bypassing parsing.
func TestCatalogMetadataSurvivesRefreshStorageAndPublicFormats(t *testing.T) {
	const nativeJSON = `{
		"id":"rich-model", "slug":"rich-model", "model":"rich-model",
		"object":"model", "owned_by":"upstream-vendor",
		"name":"Native wire name", "display_name":"UI display name",
		"description":"A complete model description",
		"supported_reasoning_levels":[{"effort":"low","description":"Fast"},{"effort":"xhigh","description":"Deep"}],
		"default_reasoning_level":"xhigh", "default_reasoning_summary":null,
		"supports_reasoning_summary_parameter":false,
		"context_window":250000, "max_context_window":9007199254740993,
		"input_modalities":["text","image"], "supports_parallel_tool_calls":false,
		"experimental_supported_tools":[],
		"base_instructions":"<script>display as text only</script>",
		"metadata":{"vendor":{"revision":9007199254740993}},
		"future_extension":{"nested":{"number":9007199254740993},"enabled":false,"items":[],"nullable":null}
	}`
	var expected map[string]json.RawMessage
	{
		err := json.Unmarshal([]byte(nativeJSON), &expected)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	var calls atomic.Int32
	var fail atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveCatalogMetadataSurvivesRefreshStorageAndPublicFormatsRequest(w, r, &t, nativeJSON, &calls, &fail)
	}))
	defer target.Close()
	s, account := newModelAdmin(t, target.URL)
	ctx := context.Background()
	policyRequest(t, s.handleUpdateAccount, "PATCH", account.ID, `{"supplemental_models":["custom-model"]}`, http.StatusOK)
	readAdmin := func(handler http.HandlerFunc, method string, status int) store.ModelCatalog {
		t.Helper()
		w, _ := modelAdminRequest(t, handler, ctx, account.ID, method, "")
		reportTestFailure(t, w.Code != status, func() {
			t.Fatalf("admin catalog status=%d want=%d body=%s", w.Code, status, w.Body.String())
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
	initial := readAdmin(s.handleAccountModels, http.MethodGet, http.StatusOK)
	reportTestFailure(t, len(initial.Models) != 1 || initial.Models[0].Source != "manual" || calls.Load() != 0, func() {
		t.Fatalf("manual catalog fabricated an upstream request: %+v calls=%d", initial, calls.Load())
	})
	first := readAdmin(s.handleRefreshAccountModels, http.MethodPost, http.StatusOK)
	reportTestFailure(t, len(first.Models) != 2 || first.Models[0].Source != "upstream", func() {
		t.Fatalf("unexpected refreshed models: %+v", first.Models)
	})
	assertCatalogMetadataJSON(t, first.Models[0].Metadata, expected)
	fail.Store(true)
	failed := readAdmin(s.handleRefreshAccountModels, http.MethodPost, http.StatusBadGateway)
	cached := readAdmin(s.handleAccountModels, http.MethodGet, http.StatusOK)
	reportTestFailure(t, failed.Error == "" || cached.Error != failed.Error || cached.FetchedAt != first.FetchedAt || len(cached.Models) != 2 || calls.Load() != 2, func() {
		t.Fatalf("failed refresh did not retain the snapshot: %+v calls=%d", cached, calls.Load())
	})
	assertCatalogMetadataJSON(t, cached.Models[0].Metadata, expected)

	key := &store.APIKey{Name: "catalog reader", Key: store.GenerateKey(), Enabled: true}
	{
		err := s.store.CreateKey(ctx, key)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	runtime := s.settings.Get()
	runtime.ModelMappings = map[string]string{"client-alias": "rich-model"}
	{
		err := s.settings.Set(ctx, runtime)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	public := dataplane.New(dataplane.Options{Config: s.cfg, Store: s.store, Accounts: s.accounts, Upstream: s.upstream, Settings: s.settings, Logger: s.logger})
	t.Cleanup(public.Close)
	mux := http.NewServeMux()
	public.Routes(mux)
	for _, mode := range []struct{ query, field string }{{"", "data"}, {"?client_version=0.144.0", "models"}} {
		t.Run(mode.field, func(t *testing.T) {
			runCatalogMetadataSurvivesRefreshStorageAndPublicFormatsCase(t, &expected, &key, &mux, &mode)
		})
	}
	reportTestFailure(t, calls.Load() != 2, func() {
		t.Fatalf("reading public catalogs made extra upstream requests: %d", calls.Load())
	})
}

func assertCatalogMetadataJSON(t *testing.T, got, want map[string]json.RawMessage) {
	t.Helper()
	decode := func(raw json.RawMessage) any {
		t.Helper()
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		{
			err := decoder.Decode(&value)
			reportTestFailure(t, err != nil, func() {
				t.Fatalf("invalid metadata JSON %s: %v", raw, err)
			})
		}
		return value
	}
	for field, expected := range want {
		actual, exists := got[field]
		if !exists {
			t.Errorf("metadata field %q disappeared", field)
			continue
		}
		reportTestFailure(t, !reflect.DeepEqual(decode(actual), decode(expected)), func() {
			t.Errorf("metadata field %q changed: got=%s want=%s", field, actual, expected)
		})
	}
}

// serveCatalogMetadataSurvivesRefreshStorageAndPublicFormatsRequest exercises one isolated test phase with the original shared state.
func serveCatalogMetadataSurvivesRefreshStorageAndPublicFormatsRequest(w http.ResponseWriter, r *http.Request, capturedT **testing.T, capturedNativeJSON string, capturedCalls *atomic.Int32, capturedFail *atomic.Bool) {
	(*capturedT).Helper()

	(*capturedCalls).Add(1)
	if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
		(*capturedT).Errorf("unexpected catalog request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if (*capturedFail).Load() {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"selected-primary secret-codex"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"account":{"access_token":"envelope-secret"},"models":[%s]}`, capturedNativeJSON)
}

// runCatalogMetadataSurvivesRefreshStorageAndPublicFormatsCase exercises one isolated test phase with the original shared state.
func runCatalogMetadataSurvivesRefreshStorageAndPublicFormatsCase(t *testing.T, capturedExpected *map[string]json.RawMessage, capturedKey **store.APIKey, capturedMux **http.ServeMux, capturedMode *struct {
	query string
	field string
}) {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, "/v1/models"+(*capturedMode).query, nil)
	r.Header.Set("Authorization", "Bearer "+(*capturedKey).Key)
	w := httptest.NewRecorder()
	(*capturedMux).ServeHTTP(w, r)
	reportTestFailure(t, w.Code != http.StatusOK, func() {
		t.Fatalf("public catalog status=%d body=%s", w.Code, w.Body.String())
	})
	reportTestFailure(t, w.Header().Get("Cache-Control") != "private, no-store", func() {
		t.Fatalf("account-scoped metadata must not enter a shared cache: %v", w.Header())
	})
	var envelope map[string]json.RawMessage
	{
		err := json.Unmarshal(w.Body.Bytes(), &envelope)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	var entries []map[string]json.RawMessage
	{
		err := json.Unmarshal(envelope[(*capturedMode).field], &entries)
		reportTestFailure(t, err != nil || len(entries) != 3, func() {
			t.Fatalf("public models=%s error=%v", w.Body.String(), err)
		})
	}
	for _, entry := range entries {
		var id string
		{
			err := json.Unmarshal(entry["id"], &id)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		if id == "custom-model" {
			var levels []struct {
				Effort string `json:"effort"`
			}
			{
				err := json.Unmarshal(entry["supported_reasoning_levels"], &levels)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			efforts := make([]string, 0, len(levels))
			for _, level := range levels {
				efforts = append(efforts, level.Effort)
			}
			reportTestFailure(t, !reflect.DeepEqual(efforts, []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}) || string(entry["default_reasoning_level"]) != `"medium"` || string(entry["default_reasoning_summary"]) != `"auto"`, func() {
				t.Fatalf("custom model lost reasoning defaults: %s", w.Body.String())
			})
			continue
		}
		reportTestFailure(t, id != "rich-model" && id != "client-alias", func() {
			t.Fatalf("unexpected public model %q", id)
		})
		want := make(map[string]json.RawMessage, len((*capturedExpected)))
		for field, value := range *capturedExpected {
			want[field] = value
		}
		identity, err := json.Marshal(id)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
		want["id"], want["slug"], want["model"] = identity, identity, identity
		assertCatalogMetadataJSON(t, entry, want)
	}
	for _, secret := range []string{"envelope-secret", "selected-primary", "secret-codex", "main-account", "other-account-token"} {
		reportTestFailure(t, strings.Contains(w.Body.String(), secret), func() {
			t.Fatalf("public metadata leaked account/envelope data: %s", secret)
		})
	}
}
