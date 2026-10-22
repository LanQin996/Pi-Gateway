package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pi-gateway/internal/config"
	"pi-gateway/internal/rules"
	"pi-gateway/internal/rulesruntime"
	"pi-gateway/internal/store"
)

func ruleHTTP(t *testing.T, handler http.Handler, method, path, body string, status int) map[string]json.RawMessage {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer rules-test")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	reportTestFailure(t, w.Code != status, func() {
		t.Fatalf("%s %s status=%d want=%d body=%s", method, path, w.Code, status, w.Body.String())
	})
	var out map[string]json.RawMessage
	{
		err := json.Unmarshal(w.Body.Bytes(), &out)
		reportTestFailure(t, err != nil, func() {
			t.Fatalf("response JSON: %v: %s", err, w.Body.String())
		})
	}
	return out
}

func newRulesAdmin(t *testing.T, migrateEmpty bool) (*store.Store, *http.ServeMux) {
	t.Helper()
	st := newAdminTestStore(t)
	if migrateEmpty {
		_, err := st.InitializeRules(context.Background(), func(context.Context, []*store.MiddlewareRow) ([]*store.RuleRow, error) {
			return []*store.RuleRow{}, nil
		}, rulesruntime.ValidateSnapshot)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
		// Complete startup upgrades, then deliberately empty the set. Later admin
		// requests must honor the deletion rather than reseeding defaults.
		{
			err := rulesruntime.New(st).Ensure(t.Context())
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		snapshot, err := st.LoadRuleSet(t.Context())
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
		changes := []store.RuleChange{}
		for _, row := range snapshot.Rules {
			changes = append(changes, store.RuleChange{Kind: "delete", ID: row.ID, ExpectedRevision: row.Revision})
		}
		{
			_, err := st.PublishRules(t.Context(), nil, changes, rulesruntime.ValidateSnapshot)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
	}
	{
		err := st.CreateAdminSession(t.Context(), "rules-test", "admin", "", time.Now().Add(time.Hour).UnixMilli())
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	s := &Server{store: st, cfg: &config.Config{Admin: config.AdminConfig{Username: "admin"}}}
	mux := http.NewServeMux()
	s.Routes(mux, nil)
	return st, mux
}

func httpRuleJSON(name string, priority int) string {
	return fmt.Sprintf(`{"schema_version":1,"name":%q,"description":"admin test","enabled":true,"priority":%d,"phase":"request","when":{"op":"always"},"actions":[],"stop_after_match":false,"on_error":"abort"}`, name, priority)
}

func TestRulesAdminBackfillsOldInstallation(t *testing.T) {
	st, mux := newRulesAdmin(t, false)
	before, err := st.InitializeRules(t.Context(), func(context.Context, []*store.MiddlewareRow) ([]*store.RuleRow, error) {
		return []*store.RuleRow{{ID: "existing", Rule: json.RawMessage(httpRuleJSON("existing", 10)), Source: "user"}}, nil
	}, rulesruntime.ValidateSnapshot)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	out := ruleHTTP(t, mux, "GET", "/api/rules", "", 200)
	var definitions []rules.Rule
	{
		err := json.Unmarshal(out["rules"], &definitions)
		reportTestFailure(t, err != nil || len(definitions) != 6, func() {
			t.Fatalf("old installation list=%s err=%v", out["rules"], err)
		})
	}
	filter, err := st.GetRule(t.Context(), "default-drop-image-generation")
	version, versionErr := st.RuleSetVersion(t.Context())
	reportTestFailure(t, err != nil || versionErr != nil || filter == nil || !filter.Enabled || version != before.Version+2, func() {
		t.Fatalf("backfill failed: filter=%+v version=%d err=%v/%v", filter, version, err, versionErr)
	})
	ruleHTTP(t, mux, "DELETE", "/api/rules/"+filter.ID+"?expected_revision=1", "", 200)
	out = ruleHTTP(t, mux, "GET", "/api/rules", "", 200)
	{
		err := json.Unmarshal(out["rules"], &definitions)
		reportTestFailure(t, err != nil || len(definitions) != 5, func() {
			t.Fatalf("admin request resurrected deleted default: %s err=%v", out["rules"], err)
		})
	}
	for _, definition := range definitions {
		reportTestFailure(t, definition.ID == filter.ID, func() {
			t.Fatal("deleted image rule resurrected")
		})
	}
}

func createHTTPRule(t *testing.T, st *store.Store, mux http.Handler, name string, priority int) rules.Rule {
	t.Helper()
	version, err := st.RuleSetVersion(context.Background())
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	out := ruleHTTP(t, mux, "POST", "/api/rules", fmt.Sprintf(`{"expected_version":%d,"rule":%s}`, version, httpRuleJSON(name, priority)), http.StatusCreated)
	var rule rules.Rule
	{
		err := json.Unmarshal(out["rule"], &rule)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, rule.ID == "" || rule.Revision != 1 || rule.CreatedAt == "", func() {
		t.Fatalf("metadata missing: %+v", rule)
	})
	return rule
}

func TestRulesAdminCRUDVersionConflictsAndAuth(t *testing.T) {
	st, mux := newRulesAdmin(t, true)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/rules/schema", nil))
	reportTestFailure(t, w.Code != http.StatusUnauthorized, func() {
		t.Fatalf("schema bypassed auth: %d", w.Code)
	})
	first := createHTTPRule(t, st, mux, "repeated name", 50)
	second := createHTTPRule(t, st, mux, "repeated name", 10)
	out := ruleHTTP(t, mux, "GET", "/api/rules?search=repeated&page=2&page_size=1", "", 200)
	var total int
	var page []rules.Rule
	json.Unmarshal(out["total"], &total)
	json.Unmarshal(out["rules"], &page)
	reportTestFailure(t, total != 2 || len(page) != 1 || page[0].ID != first.ID, func() {
		t.Fatalf("page=%s", out["rules"])
	})
	out = ruleHTTP(t, mux, "GET", "/api/rules/"+second.ID, "", 200)
	var got rules.Rule
	{
		err := json.Unmarshal(out["rule"], &got)
		reportTestFailure(t, err != nil || got.ID != second.ID, func() {
			t.Fatalf("get: %+v %v", got, err)
		})
	}
	ruleHTTP(t, mux, "PUT", "/api/rules/"+first.ID, fmt.Sprintf(`{"expected_revision":1,"rule":%s}`, httpRuleJSON("edited", 50)), 200)
	ruleHTTP(t, mux, "PUT", "/api/rules/"+first.ID, fmt.Sprintf(`{"expected_revision":1,"rule":%s}`, httpRuleJSON("lost update", 50)), 409)
	ruleHTTP(t, mux, "PUT", "/api/rules/"+first.ID, fmt.Sprintf(`{"rule":%s}`, httpRuleJSON("missing revision", 50)), 400)
	ruleHTTP(t, mux, "POST", "/api/rules", fmt.Sprintf(`{"expected_version":0,"rule":%s}`, httpRuleJSON("stale create", 50)), 409)
	out = ruleHTTP(t, mux, "POST", "/api/rules/"+first.ID+"/duplicate", `{"expected_revision":2}`, 201)
	var cloned rules.Rule
	json.Unmarshal(out["rule"], &cloned)
	reportTestFailure(t, cloned.ID == first.ID || cloned.Revision != 1 || cloned.Name != "edited (copy)", func() {
		t.Fatalf("duplicate=%+v", cloned)
	})
	ruleHTTP(t, mux, "DELETE", "/api/rules/"+first.ID+"?expected_revision=1", "", 409)
	ruleHTTP(t, mux, "DELETE", "/api/rules/"+first.ID+"?expected_revision=2", "", 200)
	ruleHTTP(t, mux, "GET", "/api/rules/"+first.ID, "", 404)
	var audits int
	{
		err := st.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM audit_events WHERE action LIKE 'rule.%'`).Scan(&audits)
		reportTestFailure(t, err != nil || audits != 5, func() {
			t.Fatalf("audit count=%d err=%v", audits, err)
		})
	}
}

func TestRulesAdminBatchReorderAtomicityAndTrueNeighbor(t *testing.T) {
	st, mux := newRulesAdmin(t, true)
	a := createHTTPRule(t, st, mux, "visible-a", 0)
	b := createHTTPRule(t, st, mux, "hidden-b", 0)
	c := createHTTPRule(t, st, mux, "visible-c", 10)
	payload := fmt.Sprintf(`{"enabled":false,"items":[{"id":%q,"expected_revision":1},{"id":%q,"expected_revision":2}]}`, a.ID, b.ID)
	ruleHTTP(t, mux, "POST", "/api/rules/batch", payload, 409)
	row, _ := st.GetRule(context.Background(), a.ID)
	reportTestFailure(t, row == nil || !row.Enabled || row.Revision != 1, func() {
		t.Fatalf("partial batch=%+v", row)
	})
	payload = fmt.Sprintf(`{"enabled":false,"items":[{"id":%q,"expected_revision":1},{"id":%q,"expected_revision":1}]}`, a.ID, b.ID)
	ruleHTTP(t, mux, "POST", "/api/rules/batch", payload, 200)
	version, _ := st.RuleSetVersion(context.Background())
	payload = fmt.Sprintf(`{"id":%q,"expected_revision":1,"direction":"up","expected_version":%d}`, c.ID, version)
	ruleHTTP(t, mux, "POST", "/api/rules/reorder", payload, 200)
	snapshot, _ := st.LoadRuleSet(context.Background())
	reportTestFailure(t, len(snapshot.Rules) != 3 || snapshot.Rules[0].ID != a.ID || snapshot.Rules[1].ID != c.ID || snapshot.Rules[2].ID != b.ID, func() {
		t.Fatalf("move used a filtered neighbor: %+v", snapshot.Rules)
	})
	ruleHTTP(t, mux, "POST", "/api/rules/reorder", payload, 409)
	payload = fmt.Sprintf(`{"items":[{"id":%q,"expected_revision":2,"order_index":100},{"id":%q,"expected_revision":3,"order_index":200,"priority":0}]}`, c.ID, b.ID)
	ruleHTTP(t, mux, "POST", "/api/rules/reorder", payload, 200)
}

func TestRulesAdminSchemaValidationSimulationNoSideEffects(t *testing.T) {
	st, mux := newRulesAdmin(t, true)
	schema := ruleHTTP(t, mux, "GET", "/api/rules/schema", "", 200)
	reportTestFailure(t, len(schema["actions"]) == 0 || len(schema["conditions"]) == 0, func() {
		t.Fatalf("static schema route shadowed: %v", schema)
	})
	valid := `{"schema_version":1,"name":"rewrite","enabled":true,"priority":0,"phase":"request","when":{"op":"always"},"actions":[{"id":"model","type":"rewrite_model","params":{"model":"new-model"}}],"on_error":"abort"}`
	out := ruleHTTP(t, mux, "POST", "/api/rules/validate", `{"rule":`+valid+`}`, 200)
	reportTestFailure(t, string(out["valid"]) != "true", func() {
		t.Fatalf("validation=%v", out)
	})
	ruleHTTP(t, mux, "POST", "/api/rules/validate", `{"rule":{"name":"bad","unknown":true}}`, 400)
	ruleHTTP(t, mux, "POST", "/api/rules/validate", `{"rule":`+valid+`,"unknown":true}`, 400)
	ruleHTTP(t, mux, "POST", "/api/rules/validate", `{"rule":`+valid+`} {}`, 400)
	before, _ := st.RuleSetVersion(context.Background())
	out = ruleHTTP(t, mux, "POST", "/api/rules/simulate", `{"rule":`+valid+`,"phase":"request","input":{"body":{"model":"old-model","input":[]},"model":"old-model"}}`, 200)
	var result rules.Result
	{
		err := json.Unmarshal(out["result"], &result)
		reportTestFailure(t, err != nil || result.Model != "new-model" || !result.Changed || len(result.Traces) == 0, func() {
			t.Fatalf("simulation=%s err=%v", out["result"], err)
		})
	}
	after, _ := st.RuleSetVersion(context.Background())
	reportTestFailure(t, before != after, func() {
		t.Fatalf("simulation published version %d -> %d", before, after)
	})
	for _, table := range []string{"usage_records", "captures", "audit_events", "accounts"} {
		var count int
		{
			err := st.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM `+table).Scan(&count)
			reportTestFailure(t, err != nil || count != 0, func() {
				t.Fatalf("simulation wrote %s: count=%d err=%v", table, count, err)
			})
		}
	}
}

func TestRulesAdminInvalidCandidateAndServerManagedMetadata(t *testing.T) {
	st, mux := newRulesAdmin(t, true)
	rule := createHTTPRule(t, st, mux, "safe", 10)
	before, _ := st.RuleSetVersion(context.Background())
	invalid := `{"schema_version":1,"name":"bad","phase":"request","when":{"op":"regex","value":"["},"actions":[]}`
	ruleHTTP(t, mux, "PUT", "/api/rules/"+rule.ID, fmt.Sprintf(`{"expected_revision":1,"rule":%s}`, invalid), 400)
	after, _ := st.RuleSetVersion(context.Background())
	reportTestFailure(t, before != after, func() {
		t.Fatal("invalid update published")
	})
	doc := strings.TrimSuffix(httpRuleJSON("safe", 10), "}") + `,"id":"attacker-id","revision":999,"order_index":999,"legacy_name":"rewrite_model","source":"legacy","created_at":"2000-01-01T00:00:00Z"}`
	out := ruleHTTP(t, mux, "PUT", "/api/rules/"+rule.ID, fmt.Sprintf(`{"expected_revision":1,"rule":%s}`, doc), 200)
	var saved rules.Rule
	{
		err := json.Unmarshal(out["rule"], &saved)
		reportTestFailure(t, err != nil || saved.ID != rule.ID || saved.Revision != 2 || saved.OrderIndex != rule.OrderIndex || saved.Source != "user" || saved.LegacyName != "" || saved.CreatedAt != rule.CreatedAt, func() {
			t.Fatalf("metadata tampering=%+v %v", saved, err)
		})
	}
}

func TestRulesAdminLegacyBridgeAndConflict(t *testing.T) {
	st, mux := newRulesAdmin(t, false)
	ctx := context.Background()
	{
		err := st.UpsertMiddleware(ctx, &store.MiddlewareRow{Name: "rewrite_model", Enabled: true, OrderIndex: 20, Config: `{"model":"first"}`})
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	ruleHTTP(t, mux, "GET", "/api/rules", "", 200)
	snapshot, _ := st.LoadRuleSet(ctx)
	var migrated *store.RuleRow
	for _, row := range snapshot.Rules {
		if row.LegacyName == "rewrite_model" {
			migrated = row
		}
	}
	reportTestFailure(t, migrated == nil, func() {
		t.Fatal("legacy row not migrated")
	})
	ruleHTTP(t, mux, "PUT", "/api/middlewares/rewrite_model", `{"config":"{\"model\":\"second\"}"}`, 409)
	ruleHTTP(t, mux, "PUT", "/api/middlewares/rewrite_model", `{"expected_revision":1,"config":"{\"model\":\"second\"}"}`, 200)
	old, _ := st.GetMiddleware(ctx, "rewrite_model")
	updated, _ := st.GetRule(ctx, migrated.ID)
	reportTestFailure(t, old.Config != `{"model":"second"}` || updated.Revision != 2 || !strings.Contains(string(updated.Rule), "second"), func() {
		t.Fatalf("legacy PUT was not atomically mirrored: %+v %+v", old, updated)
	})
	doc := `{"schema_version":1,"name":"custom","enabled":true,"priority":20,"phase":"request","when":{"op":"always"},"actions":[{"id":"one","type":"rewrite_model","params":{"model":"third"}},{"id":"two","type":"set_reasoning","params":{"effort":"high"}}],"on_error":"abort"}`
	ruleHTTP(t, mux, "PUT", "/api/rules/"+migrated.ID, `{"expected_revision":2,"rule":`+doc+`}`, 200)
	ruleHTTP(t, mux, "PUT", "/api/middlewares/rewrite_model", `{"expected_revision":3,"enabled":false}`, 409)
	saved, _ := st.GetRule(ctx, migrated.ID)
	reportTestFailure(t, saved.Revision != 3 || !saved.Enabled, func() {
		t.Fatal("incompatible legacy PUT overwrote new rule")
	})
	ruleHTTP(t, mux, "DELETE", "/api/rules/"+migrated.ID+"?expected_revision=3", "", 200)
	ruleHTTP(t, mux, "PUT", "/api/middlewares/rewrite_model", `{"expected_revision":3,"enabled":true}`, 409)
}

func TestRulesAdminLegacyRepairBeforeMigration(t *testing.T) {
	st, mux := newRulesAdmin(t, false)
	ctx := context.Background()
	{
		err := st.UpsertMiddleware(ctx, &store.MiddlewareRow{Name: "block_prompt", Enabled: true, Config: `{"pattern":"["}`})
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	ruleHTTP(t, mux, "GET", "/api/rules", "", 409)
	ruleHTTP(t, mux, "GET", "/api/middlewares", "", 200)
	ruleHTTP(t, mux, "PUT", "/api/middlewares/block_prompt", `{"config":"{\"pattern\":\"\"}"}`, 200)
	ruleHTTP(t, mux, "GET", "/api/rules", "", 200)
}
