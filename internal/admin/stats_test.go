package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pi-gateway/internal/store"
)

func newAdminTestStore(t *testing.T) *store.Store {
	t.Helper()
	// Handler tests exercise real SQLite without needing on-disk persistence.
	// Store tests separately cover reopening files. A single connection keeps
	// the in-memory schema shared and avoids Windows WAL deletion races.
	db, err := store.Open(":memory:")
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	db.DB().SetMaxOpenConns(1)
	db.DB().SetMaxIdleConns(1)
	// LIFO closes server/flow users registered by callers before the database.
	t.Cleanup(func() {
		{
			err := db.Close()
			reportTestFailure(t, err != nil, func() {
				t.Errorf("close admin test database: %v", err)
			})
		}
	})
	return db
}

func ptr(v int64) *int64 { return &v }

// seedUsage writes one finished ledger row.
type usageSeed struct {
	model, outcome                       string
	input, cached, output, ttft, latency int64
	costMicros                           *int64
}

func seedUsage(t *testing.T, db *store.Store, seed usageSeed) {
	model, outcome := seed.model, seed.outcome
	input, cached, output, ttft, latency, costMicros := seed.input, seed.cached, seed.output, seed.ttft, seed.latency, seed.costMicros
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixMilli()
	id, err := db.StartUsageRecord(ctx, &store.UsageRecord{
		RequestID: model + outcome + time.Now().Format(time.RFC3339Nano),
		Model:     model,
		Outcome:   "running",
		StartedAt: now,
	})
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	rec := &store.UsageRecord{
		RequestID:    "final",
		Model:        model,
		Outcome:      outcome,
		InputTokens:  ptr(input),
		CachedTokens: ptr(cached),
		OutputTokens: ptr(output),
		TotalTokens:  ptr(input + output),
		FirstTokenMS: ttft,
		LatencyMS:    latency,
		CostMicros:   costMicros,
		CostSource:   "builtin",
		StartedAt:    now,
		CompletedAt:  now + latency,
	}
	{
		err := db.FinishUsageRecord(ctx, id, rec)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
}

func getJSON(t *testing.T, s *Server, handler func(http.ResponseWriter, *http.Request), url string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	handler(w, httptest.NewRequest("GET", url, nil))
	out := map[string]any{}
	if w.Body.Len() > 0 {
		{
			err := json.Unmarshal(w.Body.Bytes(), &out)
			reportTestFailure(t, err != nil, func() {
				t.Fatalf("decode %s: %v (%s)", url, err, w.Body.String())
			})
		}
	}
	return w.Code, out
}

// getJSONWindow requests a window that ends in the future.
//
// The ledger uses the half-open interval [start, end), and the default end is
// "now" — a row written in the same millisecond as the query is therefore
// legitimately excluded. Tests pin an explicit future end so the fixtures are
// always inside the window.
func getJSONWindow(t *testing.T, s *Server, handler func(http.ResponseWriter, *http.Request), path string) (int, map[string]any) {
	t.Helper()
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return getJSON(t, s, handler, path+sep+"end="+itoa(time.Now().Add(time.Hour).UnixMilli()))
}

func TestStatsSummaryReportsFrozenMetrics(t *testing.T) {
	db := newAdminTestStore(t)
	seedUsage(t, db, usageSeed{model: "gpt-5.5", outcome: "succeeded", input: 100, cached: 80, output: 20, ttft: 10, latency: 1010, costMicros: ptr(1_500_000)})
	seedUsage(t, db, usageSeed{model: "gpt-5.5", outcome: "failed", input: 50, cached: 0, output: 0, ttft: 0, latency: 500, costMicros: nil})
	s := &Server{store: db}

	code, body := getJSONWindow(t, s, s.handleStatsSummary, "/api/stats/summary")
	reportTestFailure(t, code != 200, func() {
		t.Fatalf("status = %d: %v", code, body)
	})
	{
		got := body["request_count"].(float64)
		reportTestFailure(t, got != 2, func() {
			t.Errorf("request_count = %v, want 2", got)
		})
	}
	{
		got := body["success_count"].(float64)
		reportTestFailure(t, got != 1, func() {
			t.Errorf("success_count = %v, want 1", got)
		})
	}
	{
		got := body["failure_count"].(float64)
		reportTestFailure(t, got != 1, func() {
			t.Errorf("failure_count = %v, want 1", got)
		})
	}
	{
		got := body["input_tokens"].(float64)
		reportTestFailure(t, got != 150, func() {
			t.Errorf("input_tokens = %v, want 150", got)
		})
	}
	{
		got := body["cached_tokens"].(float64)
		reportTestFailure(t, got != 80, func() {
			t.Errorf("cached_tokens = %v, want 80", got)
		})
	}
	// 80 cached of 150 input.
	{
		got := body["cached_token_rate"].(float64)
		reportTestFailure(t, got < 0.53 || got > 0.54, func() {
			t.Errorf("cached_token_rate = %v, want ~0.5333", got)
		})
	}
	// One of two eligible requests was a cache hit.
	{
		got := body["cache_hit_request_rate"].(float64)
		reportTestFailure(t, got != 0.5, func() {
			t.Errorf("cache_hit_request_rate = %v, want 0.5", got)
		})
	}
	{
		got := body["total_cost_micros"].(float64)
		reportTestFailure(t, got != 1_500_000, func() {
			t.Errorf("total_cost_micros = %v, want 1500000", got)
		})
	}
	{
		got := body["total_cost_usd"].(float64)
		reportTestFailure(t, got != 1.5, func() {
			t.Errorf("total_cost_usd = %v, want 1.5", got)
		})
	}
	// The second row has output_tokens 0, so only the first contributes TPS.
	{
		got := body["output_tps_p50"].(float64)
		reportTestFailure(t, got <= 0, func() {
			t.Errorf("output_tps_p50 = %v, want > 0", got)
		})
	}
}

func TestStatsEndpointsOnEmptyWarehouse(t *testing.T) {
	db := newAdminTestStore(t)
	s := &Server{store: db}

	cases := map[string]func(http.ResponseWriter, *http.Request){
		"summary":  s.handleStatsSummary,
		"trend":    s.handleStatsTrend,
		"models":   s.handleStatsModels,
		"keys":     s.handleStatsKeys,
		"accounts": s.handleStatsAccounts,
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			code, body := getJSONWindow(t, s, handler, "/api/stats/"+name)
			reportTestFailure(t, code != 200, func() {
				t.Fatalf("status = %d: %v", code, body)
			})
		})
	}
}

func TestStatsRejectsInvalidRange(t *testing.T) {
	db := newAdminTestStore(t)
	s := &Server{store: db}
	for _, tc := range []struct {
		url     string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"/api/stats/summary?start=abc", s.handleStatsSummary},
		{"/api/stats/summary?end=nope", s.handleStatsSummary},
		{"/api/stats/summary?start=1000&end=500", s.handleStatsSummary},
		{"/api/stats/trend?bucket=week", s.handleStatsTrend},
	} {
		code, _ := getJSON(t, s, tc.handler, tc.url)
		reportTestFailure(t, code != 400, func() {
			t.Errorf("%s: status = %d, want 400", tc.url, code)
		})
	}
}

func TestStatsModelShareUsesRequestCount(t *testing.T) {
	db := newAdminTestStore(t)
	for i := 0; i < 3; i++ {
		seedUsage(t, db, usageSeed{model: "gpt-5.5", outcome: "succeeded", input: 10, cached: 0, output: 5, ttft: 5, latency: 100, costMicros: nil})
	}
	seedUsage(t, db, usageSeed{model: "gpt-5.4", outcome: "succeeded", input: 10, cached: 0, output: 5, ttft: 5, latency: 100, costMicros: nil})
	s := &Server{store: db}

	code, body := getJSONWindow(t, s, s.handleStatsModels, "/api/stats/models")
	reportTestFailure(t, code != 200, func() {
		t.Fatalf("status = %d", code)
	})
	items := body["items"].([]any)
	reportTestFailure(t, len(items) != 2, func() {
		t.Fatalf("items = %d, want 2", len(items))
	})
	first := items[0].(map[string]any)
	reportTestFailure(t, first["key"] != "gpt-5.5", func() {
		t.Fatalf("first model = %v, want gpt-5.5 (most requests)", first["key"])
	})
	{
		got := first["share"].(float64)
		reportTestFailure(t, got != 0.75, func() {
			t.Errorf("share = %v, want 0.75 (3 of 4 requests)", got)
		})
	}
}

func TestUsageRecordsPaginationAndFilters(t *testing.T) {
	db := newAdminTestStore(t)
	for i := 0; i < 5; i++ {
		seedUsage(t, db, usageSeed{model: "gpt-5.5", outcome: "succeeded", input: 10, cached: 0, output: 5, ttft: 5, latency: 100, costMicros: nil})
	}
	seedUsage(t, db, usageSeed{model: "gpt-5.4", outcome: "failed", input: 10, cached: 0, output: 5, ttft: 5, latency: 100, costMicros: nil})
	s := &Server{store: db}

	code, body := getJSONWindow(t, s, s.handleUsageRecords, "/api/usage/records?limit=2")
	reportTestFailure(t, code != 200, func() {
		t.Fatalf("status = %d", code)
	})
	{
		got := body["total"].(float64)
		reportTestFailure(t, got != 6, func() {
			t.Errorf("total = %v, want 6", got)
		})
	}
	{
		got := len(body["records"].([]any))
		reportTestFailure(t, got != 2, func() {
			t.Errorf("page size = %d, want 2", got)
		})
	}

	code, body = getJSONWindow(t, s, s.handleUsageRecords, "/api/usage/records?model=gpt-5.4")
	reportTestFailure(t, code != 200, func() {
		t.Fatalf("status = %d", code)
	})
	{
		got := body["total"].(float64)
		reportTestFailure(t, got != 1, func() {
			t.Errorf("filtered total = %v, want 1", got)
		})
	}

	// An oversized limit is clamped rather than rejected.
	code, body = getJSONWindow(t, s, s.handleUsageRecords, "/api/usage/records?limit=99999")
	reportTestFailure(t, code != 200, func() {
		t.Fatalf("status = %d", code)
	})
	{
		got := body["limit"].(float64)
		reportTestFailure(t, got != usageDefaultLimit, func() {
			t.Errorf("limit = %v, want %d", got, usageDefaultLimit)
		})
	}

	{
		code, _ := getJSONWindow(t, s, s.handleUsageRecords, "/api/usage/records?limit=-3")
		reportTestFailure(t, code != 400, func() {
			t.Errorf("negative limit: status = %d, want 400", code)
		})
	}
}

func TestUsageRecordsCompactionAndThroughput(t *testing.T) {
	db := newAdminTestStore(t)
	ctx := t.Context()
	id, err := db.StartUsageRecord(ctx, &store.UsageRecord{RequestID: "compact", RequestKind: "compaction", Model: "test"})
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	{
		err := db.FinishUsageRecord(ctx, id, &store.UsageRecord{Outcome: "succeeded", OutputTokens: ptr(100), FirstTokenMS: 500, LatencyMS: 2500})
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	seedUsage(t, db, usageSeed{model: "missing-output", outcome: "failed", input: 0, cached: 0, output: 0, ttft: 0, latency: 100, costMicros: nil})
	code, body := getJSONWindow(t, &Server{store: db}, (&Server{store: db}).handleUsageRecords, "/api/usage/records")
	reportTestFailure(t, code != 200, func() {
		t.Fatalf("status=%d", code)
	})
	for _, raw := range body["records"].([]any) {
		row := raw.(map[string]any)
		if row["request_id"] == "compact" {
			reportTestFailure(t, row["request_kind"] != "compaction" || row["output_tps"] != float64(50), func() {
				t.Fatalf("compaction or TPS lost: %+v", row)
			})
		} else {
			reportTestFailure(t, row["output_tps"] != nil, func() {
				t.Fatalf("missing output fabricated TPS: %+v", row)
			})
		}
	}
}

func TestAccountGroupsCRUD(t *testing.T) {
	db := newAdminTestStore(t)
	ctx := context.Background()
	acct := &store.Account{Name: "a1", AccountID: "acct-1", Enabled: true, Weight: 1, Concurrency: 1, Status: store.AccountStatusReady}
	{
		err := db.CreateAccount(ctx, acct)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	s := &Server{store: db}

	// Create
	w := httptest.NewRecorder()
	body := `{"name":"team-a","account_ids":[` + itoa(acct.ID) + `]}`
	s.handleCreateAccountGroup(w, httptest.NewRequest("POST", "/api/account-groups", strings.NewReader(body)))
	reportTestFailure(t, w.Code != 200, func() {
		t.Fatalf("create: status %d: %s", w.Code, w.Body.String())
	})

	// Duplicate name conflicts.
	w = httptest.NewRecorder()
	s.handleCreateAccountGroup(w, httptest.NewRequest("POST", "/api/account-groups", strings.NewReader(`{"name":"team-a"}`)))
	reportTestFailure(t, w.Code != 409, func() {
		t.Fatalf("duplicate name: status %d, want 409", w.Code)
	})

	// Unknown account is rejected.
	w = httptest.NewRecorder()
	s.handleCreateAccountGroup(w, httptest.NewRequest("POST", "/api/account-groups", strings.NewReader(`{"name":"team-b","account_ids":[99999]}`)))
	reportTestFailure(t, w.Code != 400, func() {
		t.Fatalf("unknown account: status %d, want 400", w.Code)
	})

	// List
	w = httptest.NewRecorder()
	s.handleListAccountGroups(w, httptest.NewRequest("GET", "/api/account-groups", nil))
	reportTestFailure(t, w.Code != 200, func() {
		t.Fatalf("list: status %d", w.Code)
	})
	var listed struct {
		Groups []store.AccountGroup `json:"groups"`
	}
	{
		err := json.Unmarshal(w.Body.Bytes(), &listed)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, len(listed.Groups) != 1 || len(listed.Groups[0].AccountIDs) != 1, func() {
		t.Fatalf("groups = %+v", listed.Groups)
	})
	id := listed.Groups[0].ID

	// Replace members
	w = httptest.NewRecorder()
	putReq := httptest.NewRequest("PUT", "/api/account-groups/1/accounts", strings.NewReader(`{"account_ids":[]}`))
	putReq.SetPathValue("id", itoa(id))
	s.handleSetAccountGroupAccounts(w, putReq)
	reportTestFailure(t, w.Code != 200, func() {
		t.Fatalf("set members: status %d: %s", w.Code, w.Body.String())
	})

	// Delete
	w = httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/api/account-groups/1", nil)
	req.SetPathValue("id", itoa(id))
	s.handleDeleteAccountGroup(w, req)
	reportTestFailure(t, w.Code != 200, func() {
		t.Fatalf("delete: status %d: %s", w.Code, w.Body.String())
	})
}

func TestListKeysReportsGroupAndAccountNames(t *testing.T) {
	db := newAdminTestStore(t)
	ctx := context.Background()
	a1 := &store.Account{Name: "a1", AccountID: "acct-1", Enabled: true, Weight: 1, Concurrency: 1, Status: store.AccountStatusReady}
	a2 := &store.Account{Name: "a2", AccountID: "acct-2", Enabled: true, Weight: 1, Concurrency: 1, Status: store.AccountStatusReady}
	for _, a := range []*store.Account{a1, a2} {
		{
			err := db.CreateAccount(ctx, a)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
	}
	g := &store.AccountGroup{Name: "team-a", Enabled: true}
	{
		err := db.CreateAccountGroup(ctx, g)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	{
		err := db.SetAccountGroupAccounts(ctx, g.ID, []int64{a2.ID})
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}

	scoped := &store.APIKey{Name: "scoped", Key: "sk-pi-scoped", Enabled: true, GroupIDs: []int64{g.ID}}
	unscoped := &store.APIKey{Name: "everything", Key: "sk-pi-all", Enabled: true}
	for _, k := range []*store.APIKey{scoped, unscoped} {
		{
			err := db.CreateKey(ctx, k)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
	}

	s := &Server{store: db}
	w := httptest.NewRecorder()
	s.handleListKeys(w, httptest.NewRequest("GET", "/api/keys", nil))
	reportTestFailure(t, w.Code != 200, func() {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	})
	var out struct {
		Keys []struct {
			Name         string   `json:"name"`
			GroupNames   []string `json:"group_names"`
			AccountNames []string `json:"account_names"`
		} `json:"keys"`
	}
	{
		err := json.Unmarshal(w.Body.Bytes(), &out)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	byName := map[string]struct {
		GroupNames   []string
		AccountNames []string
	}{}
	for _, k := range out.Keys {
		byName[k.Name] = struct {
			GroupNames   []string
			AccountNames []string
		}{k.GroupNames, k.AccountNames}
	}
	{
		got := byName["scoped"]
		reportTestFailure(t, len(got.GroupNames) != 1 || got.GroupNames[0] != "team-a", func() {
			t.Errorf("scoped groups = %v", got.GroupNames)
		})
	}
	{
		got := byName["scoped"]
		reportTestFailure(t, len(got.AccountNames) != 2 || got.AccountNames[0] != "a1" || got.AccountNames[1] != "a2", func() {
			t.Errorf("scoped accounts = %v, want public a1 plus grouped a2", got.AccountNames)
		})
	}
	{
		got := byName["everything"]
		reportTestFailure(t, len(got.AccountNames) != 2, func() {
			t.Errorf("unscoped accounts = %v, want both accounts", got.AccountNames)
		})
	}
}
