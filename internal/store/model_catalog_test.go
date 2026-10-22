package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestModelCatalogPersistsFailureAndConcurrentAttemptOrdering(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.db")
	s, err := Open(path)
	requireDataNoError(t, err)
	t.Cleanup(func() {
		_ = s.Close()
		// Windows can defer removal of SQLite's just-closed WAL files. Retry
		// only fixture cleanup, bounded to 200ms; never suppress final failure.
		var cleanupErr error
		for attempt := 0; attempt < 10; attempt++ {
			cleanupErr = os.RemoveAll(dir)
			if cleanupErr == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Errorf("remove catalog database fixture: %v", cleanupErr)
	})
	a := &Account{Name: "catalog", Enabled: true}
	requireDataNoError(t, s.CreateAccount(ctx, a))
	get := func() *ModelCatalog {
		t.Helper()
		got, err := s.GetAccountModelCatalog(ctx, a.ID)
		requireDataTruef(t, !(err != nil || got == nil), "catalog=%+v err=%v", got, err)
		return got
	}
	{
		got := get()
		requireDataTruef(t, !(got.Models == nil || len(got.Models) != 0 || got.FetchedAt != 0), "new account has fabricated catalog: %+v", got)
	}
	{
		got, err := s.GetAccountModelCatalog(ctx, a.ID+1)
		requireDataTruef(t, !(err != nil || got != nil), "missing account catalog=%+v err=%v", got, err)
	}
	save := func(c *ModelCatalog) {
		t.Helper()
		requireDataNoError(t, s.SaveAccountModelCatalog(ctx, a.ID, c))
	}
	save(&ModelCatalog{Models: []CatalogModel{{ID: "model-real", Name: "Real model", Description: "live"}}, FetchedAt: 100, AttemptedAt: 90})
	save(&ModelCatalog{Models: []CatalogModel{{ID: "must-not-replace"}}, FetchedAt: 999, AttemptedAt: 110, Error: "HTTP 403"})
	{
		got := get()
		requireDataTruef(t, !(len(got.Models) != 1 || got.Models[0].ID != "model-real" || got.FetchedAt != 100 || got.AttemptedAt != 110 || got.Error != "HTTP 403"), "failure destroyed successful data: %+v", got)
	}
	// Older successful fetch can improve old data but cannot clear a newer error.
	before := catalogRevisionForTest(t, s)
	save(&ModelCatalog{Models: []CatalogModel{{ID: "model-new", Name: "New"}}, FetchedAt: 105, AttemptedAt: 100})
	{
		got := get()
		requireDataTruef(t, !(got.Models[0].ID != "model-new" || got.FetchedAt != 105 || got.Error != "HTTP 403" || got.AttemptedAt != 110), "out-of-order success lost recent failure: %+v", got)
	}
	{
		got := get().SourceCatalogs[ModelSourceChatGPT]
		requireDataTruef(t, !(got.Models[0].ID != "model-new" || got.FetchedAt != 105 || got.AttemptedAt != 110 || got.Error != "HTTP 403"), "out-of-order success diverged from source snapshot: %+v", got)
	}
	requireDataCondition(t, catalogRevisionForTest(t, s) != before, "out-of-order successful snapshot did not invalidate revision")
	save(&ModelCatalog{Models: []CatalogModel{{ID: "stale"}}, FetchedAt: 99, AttemptedAt: 80})
	{
		got := get()
		requireDataTrue(t, got.Models[0].ID == "model-new", "stale success rolled catalog back")
	}
	requireDataNoError(t, s.Close())
	s, err = Open(path)
	requireDataNoError(t, err)
	{
		got := get()
		requireDataTruef(t, !(got.Models[0].ID != "model-new" || got.Error != "HTTP 403"), "snapshot did not persist across reopen: %+v", got)
	}
	// A genuine empty successful list is authoritative, unlike a failed fetch.
	save(&ModelCatalog{FetchedAt: 120, AttemptedAt: 120})
	{
		got := get()
		requireDataTruef(t, !(got.Models == nil || len(got.Models) != 0 || got.Error != ""), "empty success not stored: %+v", got)
	}
	requireDataNoError(t, s.DeleteAccount(ctx, a.ID))
	save(&ModelCatalog{FetchedAt: 130, AttemptedAt: 130})
	var rows int
	{
		err := s.DB().QueryRow("SELECT COUNT(*) FROM account_model_catalog WHERE account_id=?", a.ID).Scan(&rows)
		requireDataTruef(t, !(err != nil || rows != 0), "deleted account left/recreated catalog: rows=%d err=%v", rows, err)
	}
}

func TestModelCatalogManualMergeKeepsUpstreamPersistenceSeparate(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	s.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = s.Close() })
	a := &Account{Name: "manual", Enabled: true, SupplementalModels: []string{" shared ", "manual-only", "shared", " "}}
	if err := s.CreateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	get := func(want []CatalogModel, fetched, attempted int64, wantError string) *ModelCatalog {
		t.Helper()
		return assertManualCatalogSnapshot(t, s, ctx, a.ID, &ModelCatalog{Models: want, FetchedAt: fetched, AttemptedAt: attempted, Error: wantError})
	}
	save := func(catalog *ModelCatalog) {
		t.Helper()
		if err := s.SaveAccountModelCatalog(ctx, a.ID, catalog); err != nil {
			t.Fatal(err)
		}
	}
	manual := []CatalogModel{NewManualCatalogModel("manual-only"), NewManualCatalogModel("shared")}
	get(manual, 0, 0, "")
	var rows int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM account_model_catalog").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("read fabricated upstream snapshot: rows=%d err=%v", rows, err)
	}
	save(&ModelCatalog{AttemptedAt: 90, Error: "initial failure"})
	get(manual, 0, 90, "initial failure")
	save(&ModelCatalog{Models: []CatalogModel{
		{ID: " shared ", Name: "Upstream name", Description: "upstream description"},
		{ID: "shared", Name: "Duplicate"}, {ID: "upstream-only", Name: "Upstream only"},
	}, FetchedAt: 100, AttemptedAt: 100})
	merged := []CatalogModel{
		{ID: "shared", Name: "Upstream name", Description: "upstream description", Source: "upstream", Origins: []string{ModelSourceChatGPT}},
		{ID: "upstream-only", Name: "Upstream only", Source: "upstream", Origins: []string{ModelSourceChatGPT}},
		NewManualCatalogModel("manual-only"),
	}
	snapshot := get(merged, 100, 100, "")
	// Even an accidental save of a merged read cannot persist artificial models.
	save(snapshot)
	assertOnlyUpstreamCatalogPersisted(t, s, a.ID)
	save(&ModelCatalog{Models: []CatalogModel{{ID: "failed-result"}}, FetchedAt: 999, AttemptedAt: 110, Error: "HTTP 403"})
	get(merged, 100, 110, "HTTP 403")
	// Upstream removal changes the duplicate's source back to manual.
	save(&ModelCatalog{FetchedAt: 120, AttemptedAt: 120})
	get(manual, 120, 120, "")
	if err := s.PatchAccountManagementFields(ctx, a.ID, AccountManagementPatch{SupplementalModels: []string{"shared"}}); err != nil {
		t.Fatal(err)
	}
	get(manual[1:], 120, 120, "")
	if err := s.PatchAccountManagementFields(ctx, a.ID, AccountManagementPatch{SupplementalModels: []string{}}); err != nil {
		t.Fatal(err)
	}
	get([]CatalogModel{}, 120, 120, "")
}

func assertManualCatalogSnapshot(t *testing.T, s *Store, ctx context.Context, accountID int64, want *ModelCatalog) *ModelCatalog {
	t.Helper()
	got, err := s.GetAccountModelCatalog(ctx, accountID)
	if err != nil || got == nil {
		t.Fatalf("catalog=%+v err=%v", got, err)
	}
	if !reflect.DeepEqual(got.Models, want.Models) || got.FetchedAt != want.FetchedAt || got.AttemptedAt != want.AttemptedAt || got.Error != want.Error {
		t.Fatalf("catalog=%+v want models=%+v fetched=%d attempted=%d error=%q", got, want.Models, want.FetchedAt, want.AttemptedAt, want.Error)
	}
	return got
}

func assertOnlyUpstreamCatalogPersisted(t *testing.T, s *Store, accountID int64) {
	t.Helper()
	var raw string
	if err := s.DB().QueryRow("SELECT models_json FROM account_model_catalog WHERE account_id=?", accountID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var persisted []CatalogModel
	if err := json.Unmarshal([]byte(raw), &persisted); err != nil || len(persisted) != 2 {
		t.Fatalf("persisted=%s err=%v", raw, err)
	}
	for _, model := range persisted {
		if model.Source != "" || model.ID == "manual-only" {
			t.Fatalf("manual merge contaminated upstream snapshot: %s", raw)
		}
	}
}

func TestModelCatalogInvalidMetadataRejectedBeforeNoOp(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	a := &Account{Name: "invalid-catalog"}
	requireDataNoError(t, s.CreateAccount(ctx, a))
	requireDataNoError(t, s.SaveAccountModelCatalog(ctx, a.ID, &ModelCatalog{
		Models: []CatalogModel{{ID: "good"}}, FetchedAt: 100, AttemptedAt: 100,
	}))
	before, err := s.GetAccountModelCatalog(ctx, a.ID)
	requireDataNoError(t, err)
	revision := catalogRevisionForTest(t, s)
	for _, tc := range []struct {
		name    string
		id      int64
		catalog ModelCatalog
	}{
		{"stale", a.ID, ModelCatalog{FetchedAt: 99, AttemptedAt: 99}},
		{"equal-time", a.ID, ModelCatalog{FetchedAt: 100, AttemptedAt: 100}},
		{"failed", a.ID, ModelCatalog{AttemptedAt: 110, Error: "failed fetch"}},
		{"manual-filtered", a.ID, ModelCatalog{FetchedAt: 110, AttemptedAt: 110, Models: []CatalogModel{{Source: "manual"}}}},
		{"codex-filtered", a.ID, ModelCatalog{FetchedAt: 110, AttemptedAt: 110, Models: []CatalogModel{{Origins: []string{ModelSourceCodex}}}}},
		{"missing-account", a.ID + 1, ModelCatalog{FetchedAt: 110, AttemptedAt: 110}},
		{"active-refresh", a.ID, ModelCatalog{FetchedAt: 110, AttemptedAt: 110}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testModelCatalogInvalidMetadataRejectedBeforeNoOpCase1(t, a, before, ctx, revision, s, tc)
		})
	}
}

func TestModelCatalogLegacyWritesPreserveSourcesAndRefreshFences(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	a := &Account{Name: "dual-catalog", AccessToken: "chatgpt-test-token", CodexAccessToken: "codex-test-token"}
	requireDataNoError(t, s.CreateAccount(ctx, a))
	requireDataNoError(t, s.SaveCodexCredential(ctx, a.ID, a.CodexAccessToken, "", "", 0, ""))
	attempt, err := s.BeginAccountModelCatalogRefresh(ctx, a.ID)
	requireDataNoError(t, err)
	at := attempt.AttemptedAt
	results := map[string]ModelCatalogSourceResult{}
	for _, source := range modelCatalogSources() {
		results[source] = ModelCatalogSourceResult{
			Catalog:               ModelCatalogSource{Models: []CatalogModel{{ID: source + "-only", Metadata: map[string]json.RawMessage{"source": json.RawMessage(`"` + source + `"`)}}}, FetchedAt: at},
			CredentialFingerprint: ModelCatalogCredentialFingerprint(a, source),
		}
	}
	applied, err := s.SaveAccountModelCatalogRefresh(ctx, a.ID, attempt, results)
	requireDataTruef(t, !(err != nil || !applied[ModelSourceChatGPT] || !applied[ModelSourceCodex]), "initial source refresh=%v err=%v", applied, err)
	get := func() *ModelCatalog {
		t.Helper()
		got, err := s.GetAccountModelCatalog(ctx, a.ID)
		requireDataTruef(t, !(err != nil || got == nil), "catalog=%+v err=%v", got, err)
		return got
	}
	save := func(catalog *ModelCatalog) {
		t.Helper()
		requireDataNoError(t, s.SaveAccountModelCatalog(ctx, a.ID, catalog))
	}
	before := get()
	revision := catalogRevisionForTest(t, s)
	// A completed tokenized refresh still rejects older work, even if it
	// claims a later successful fetch timestamp.
	save(&ModelCatalog{Models: []CatalogModel{{ID: "stale"}}, FetchedAt: at + 100, AttemptedAt: at - 1})
	{
		got := get()
		requireDataTruef(t, !(!reflect.DeepEqual(got, before) || catalogRevisionForTest(t, s) != revision), "legacy write bypassed completed refresh fence: %+v", got)
	}
	// Equal-time calls keep legacy invalidation but cannot change either source.
	save(&ModelCatalog{Models: []CatalogModel{{ID: "ambiguous"}}, FetchedAt: at + 100, AttemptedAt: at})
	{
		got := get()
		requireDataTruef(t, !(!reflect.DeepEqual(got, before) || catalogRevisionForTest(t, s) == revision), "equal-time write replaced snapshot or reused revision: %+v", got)
	}
	// Saving a merged read later must not reclassify Codex-only models.
	merged := get()
	merged.FetchedAt, merged.AttemptedAt = at+10, at+10
	save(merged)
	got := get()
	codex := before.SourceCatalogs[ModelSourceCodex]
	chatgpt := got.SourceCatalogs[ModelSourceChatGPT]
	requireDataTruef(t, !(!reflect.DeepEqual(got.SourceCatalogs[ModelSourceCodex], codex) || len(chatgpt.Models) != 1 || chatgpt.Models[0].ID != "chatgpt-only"), "legacy merged write contaminated source snapshots: %+v", got)
	save(&ModelCatalog{Models: []CatalogModel{{ID: "failed-result"}}, FetchedAt: at + 999, AttemptedAt: at + 30, Error: "HTTP 403"})
	got = get()
	failed := got.SourceCatalogs[ModelSourceChatGPT]
	requireDataTruef(t, !(!reflect.DeepEqual(got.SourceCatalogs[ModelSourceCodex], codex) || !reflect.DeepEqual(failed.Models, chatgpt.Models) || failed.FetchedAt != at+10 || failed.AttemptedAt != at+30 || failed.Error != "HTTP 403"), "legacy failure destroyed a source snapshot: %+v", got)
	// After the tokenized watermark, legacy success/error clocks remain
	// independent without changing the other source.
	save(&ModelCatalog{Models: []CatalogModel{{ID: "chatgpt-new"}}, FetchedAt: at + 25, AttemptedAt: at + 20})
	got = get()
	late := got.SourceCatalogs[ModelSourceChatGPT]
	requireDataTruef(t, !(!reflect.DeepEqual(got.SourceCatalogs[ModelSourceCodex], codex) || len(late.Models) != 1 || late.Models[0].ID != "chatgpt-new" || late.FetchedAt != at+25 || late.AttemptedAt != at+30 || late.Error != "HTTP 403"), "late legacy success lost source data or newer error: %+v", got)
	// Later legacy attempts cannot revive work at or before a consumed fence.
	before = got
	revision = catalogRevisionForTest(t, s)
	for _, attempted := range []int64{at - 1, at} {
		save(&ModelCatalog{Models: []CatalogModel{{ID: "fenced"}}, FetchedAt: at + 100, AttemptedAt: attempted})
		{
			got := get()
			requireDataTruef(t, !(!reflect.DeepEqual(got, before) || catalogRevisionForTest(t, s) != revision), "later legacy attempts weakened consumed refresh fence: %+v", got)
		}
	}
	// An active tokenized refresh takes precedence even over a legacy writer
	// with a larger timestamp.
	{
		_, err := s.BeginAccountModelCatalogRefresh(ctx, a.ID)
		requireDataTrue(t, err == nil, err)
	}
	before = get()
	revision = catalogRevisionForTest(t, s)
	save(&ModelCatalog{Models: []CatalogModel{{ID: "must-not-overwrite"}}, FetchedAt: at + 200, AttemptedAt: at + 200})
	{
		got := get()
		requireDataTruef(t, !(!reflect.DeepEqual(got, before) || catalogRevisionForTest(t, s) != revision), "legacy write bypassed active refresh fence: %+v", got)
	}
}

func TestManualCatalogModelReasoningDefaultsAreIndependent(t *testing.T) {
	first := NewManualCatalogModel("manual-one")
	second := NewManualCatalogModel("manual-two")
	if first.ID != "manual-one" || first.Name != first.ID || first.Source != "manual" {
		t.Fatalf("incorrect manual identity: %+v", first)
	}
	var levels []struct {
		Effort      string `json:"effort"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(first.Metadata["supported_reasoning_levels"], &levels); err != nil {
		t.Fatal(err)
	}
	var efforts []string
	for _, level := range levels {
		efforts = append(efforts, level.Effort)
		if level.Description == "" {
			t.Fatal("manual reasoning level has no description")
		}
	}
	if !reflect.DeepEqual(efforts, []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("manual reasoning levels: %v", efforts)
	}
	for key, want := range map[string]string{
		"default_reasoning_level":              `"medium"`,
		"supports_reasoning_summary_parameter": `true`,
		"default_reasoning_summary":            `"auto"`,
	} {
		if string(first.Metadata[key]) != want {
			t.Fatalf("manual %s=%s want %s", key, first.Metadata[key], want)
		}
	}
	if len(first.Metadata) != 4 {
		t.Fatalf("manual model fabricated additional capabilities: %v", first.Metadata)
	}
	// Defaults must not share either a mutable map or RawMessage backing arrays.
	first.Metadata["default_reasoning_level"][1] = 'X'
	delete(first.Metadata, "default_reasoning_summary")
	if string(second.Metadata["default_reasoning_level"]) != `"medium"` || string(second.Metadata["default_reasoning_summary"]) != `"auto"` {
		t.Fatalf("manual defaults alias across models: %v", second.Metadata)
	}
}

func TestModelCatalogMetadataPersistenceAndLegacyJSON(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:")
	requireDataNoError(t, err)
	s.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = s.Close() })
	a := &Account{Name: "metadata", SupplementalModels: []string{"rich", "manual"}}
	requireDataNoError(t, s.CreateAccount(ctx, a))
	// Write an actual old-format cache rather than producing it via the new type.
	const legacy = `[{"id":"legacy","name":"Legacy","description":"old cache"}]`
	{
		_, err := s.ExecContext(ctx, `INSERT INTO account_model_catalog(account_id,models_json,fetched_at,attempted_at,error) VALUES(?,?,?,?,?)`, a.ID, legacy, 10, 10, "")
		requireDataTrue(t, err == nil, err)
	}
	old, err := s.GetAccountModelCatalog(ctx, a.ID)
	requireDataTruef(t, !(err != nil || old == nil || len(old.Models) != 3 || old.Models[0].ID != "legacy" || old.Models[0].Metadata != nil), "legacy cache incompatible: %+v err=%v", old, err)
	metadata := map[string]json.RawMessage{
		"id":                                   json.RawMessage(`" rich "`),
		"supported_reasoning_levels":           json.RawMessage(`[]`),
		"default_reasoning_level":              json.RawMessage(`null`),
		"supports_reasoning_summary_parameter": json.RawMessage(`false`),
		"context_window":                       json.RawMessage(`9007199254740993123456789`),
		"input_modalities":                     json.RawMessage(`["text","image"]`),
		"unknown":                              json.RawMessage(`{"nested":[null,false,[],{"large":18446744073709551617}]}`),
	}
	model := CatalogModel{ID: "rich", Name: "Rich", Metadata: metadata}
	requireDataNoError(t, s.SaveAccountModelCatalog(ctx, a.ID, &ModelCatalog{Models: []CatalogModel{model}, FetchedAt: 20, AttemptedAt: 20}))
	for _, failed := range []bool{false, true} {
		if failed {
			requireDataNoError(t, s.SaveAccountModelCatalog(ctx, a.ID, &ModelCatalog{AttemptedAt: 30, Error: "HTTP 403"}))
		}
		got, err := s.GetAccountModelCatalog(ctx, a.ID)
		requireDataTruef(t, !(err != nil || got == nil || len(got.Models) != 2), "metadata snapshot=%+v err=%v", got, err)
		requireDataTruef(t, !(!reflect.DeepEqual(got.Models[0].Metadata, metadata) || got.Models[0].Source != "upstream" || got.Models[1].Source != "manual" || got.FetchedAt != 20), "merge or persistence changed upstream metadata: %+v", got)
		// A model also manually listed must not inherit any manual defaults.
		{
			_, exists := got.Models[0].Metadata["default_reasoning_summary"]
			requireDataTrue(t, !(exists), "manual defaults contaminated upstream metadata")
		}
		if !failed {
			requireDataNoError(t, s.SaveAccountModelCatalog(ctx, a.ID, got))
		}
	}
	var persisted string
	requireDataNoError(t, s.QueryRowContext(ctx, `SELECT models_json FROM account_model_catalog WHERE account_id=?`, a.ID).Scan(&persisted))
	var models []CatalogModel
	{
		err := json.Unmarshal([]byte(persisted), &models)
		requireDataTruef(t, !(err != nil || len(models) != 1 || models[0].Source != "" || !reflect.DeepEqual(models[0].Metadata, metadata)), "persisted metadata/manual isolation: %s err=%v", persisted, err)
	}
}

func testModelCatalogInvalidMetadataRejectedBeforeNoOpCase1(t *testing.T, a *Account, before *ModelCatalog, ctx context.Context, revision string, s *Store, tc struct {
	name    string
	id      int64
	catalog ModelCatalog
}) {
	if tc.name == "active-refresh" {
		{
			_, err := s.BeginAccountModelCatalogRefresh(ctx, a.ID)
			requireDataTrue(t, err == nil, err)
		}
	}
	if len(tc.catalog.Models) == 0 {
		tc.catalog.Models = []CatalogModel{{}}
	}
	tc.catalog.Models[0].ID = "invalid"
	tc.catalog.Models[0].Metadata = map[string]json.RawMessage{"bad": json.RawMessage(`not-json`)}
	{
		err := s.SaveAccountModelCatalog(ctx, tc.id, &tc.catalog)
		requireDataTrue(t, err != nil, "invalid metadata bypassed validation")
	}
	requireDataCondition(t, catalogRevisionForTest(t, s) == revision, "invalid metadata changed catalog revision")
	{
		got, err := s.GetAccountModelCatalog(ctx, a.ID)
		requireDataTruef(t, !(err != nil || !reflect.DeepEqual(got, before)), "invalid metadata changed source snapshots: %+v err=%v", got, err)
	}
}
