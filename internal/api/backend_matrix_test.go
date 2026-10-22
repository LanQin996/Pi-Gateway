package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"pi-gateway/internal/session"
	"pi-gateway/internal/store"
)

// This is the CI entry point for the real service matrix. Supplying either
// service opts into the entire matrix: partial configuration and broken service
// protocols are errors, never skips or a fallback to in-process substitutes.
func TestBackendMatrixIntegration(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PI_GATEWAY_TEST_POSTGRES_DSN"))
	redisURL := strings.TrimSpace(os.Getenv("PI_GATEWAY_TEST_REDIS_URL"))
	if dsn == "" && redisURL == "" {
		t.Skip("real backend matrix requires PI_GATEWAY_TEST_POSTGRES_DSN and PI_GATEWAY_TEST_REDIS_URL")
	}
	reportTestFailure(t, dsn == "" || redisURL == "", func() {
		t.Fatal("both PI_GATEWAY_TEST_POSTGRES_DSN and PI_GATEWAY_TEST_REDIS_URL must be configured")
	})
	for _, tc := range []struct {
		name, database string
		redis          bool
	}{
		{"sqlite-memory", "SQLite", false},
		{"sqlite-redis", "SQLite", true},
		{"postgres-memory", "PostgreSQL", false},
		{"postgres-redis", "PostgreSQL", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := matrixDatabase(t, tc.database, dsn)
			url := ""
			if tc.redis {
				url = redisURL
			}
			matrixContract(t, opts, url)
		})
	}
}

// Local feedback exercises the exact same contract, but intentionally has a
// distinct name: miniredis is not evidence of a real Redis integration run.
func TestBackendMatrixLocalContract(t *testing.T) {
	for _, cache := range []string{"memory", "miniredis"} {
		t.Run("SQLite+"+cache, func(t *testing.T) {
			url := ""
			if cache == "miniredis" {
				url = "redis://" + miniredis.RunT(t).Addr() + "/0"
			}
			matrixContract(t, matrixDatabase(t, "SQLite", ""), url)
		})
	}
}

func matrixNonce(t *testing.T) string {
	t.Helper()
	var nonce [16]byte
	{
		_, err := rand.Read(nonce[:])
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	return hex.EncodeToString(nonce[:])
}

func matrixDatabase(t *testing.T, database, dsn string) store.Options {
	t.Helper()
	if database == "SQLite" {
		dir := t.TempDir()
		// SQLite WAL handles may remain delete-pending briefly on Windows.
		t.Cleanup(func() {
			var err error
			for attempt := 0; attempt < 50; attempt++ {
				if err = os.RemoveAll(dir); err == nil {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Errorf("remove matrix SQLite directory: %v", err)
		})
		return store.Options{Driver: "sqlite", Path: filepath.Join(dir, "matrix.db")}
	}
	cfg, err := pgx.ParseConfig(dsn)
	reportTestFailure(t, err != nil, func() {
		t.Fatal("invalid PostgreSQL integration DSN")
	})
	cfg.ConnectTimeout = 5 * time.Second
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = make(map[string]string)
	}
	cfg.RuntimeParams["statement_timeout"] = "10000"
	cfg.RuntimeParams["lock_timeout"] = "5000"
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() {
		{
			err := admin.Close()
			reportTestFailure(t, err != nil, func() {
				t.Errorf("close PostgreSQL schema connection: %v", err)
			})
		}
	})
	schema := "api_matrix_" + matrixNonce(t)
	quoted := pgx.Identifier{schema}.Sanitize()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	{
		_, err := admin.ExecContext(ctx, "CREATE SCHEMA "+quoted)
		reportTestFailure(t, err != nil, func() {
			t.Fatalf("create private PostgreSQL schema: %v", err)
		})
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Never clean caller-owned tables or another schema, even on failure.
		{
			_, err := admin.ExecContext(ctx, "DROP SCHEMA "+quoted+" CASCADE")
			reportTestFailure(t, err != nil, func() {
				t.Errorf("drop private PostgreSQL schema: %v", err)
			})
		}
	})
	cfg.RuntimeParams["search_path"] = schema
	registered := stdlib.RegisterConnConfig(cfg)
	t.Cleanup(func() { stdlib.UnregisterConnConfig(registered) })
	return store.Options{Driver: "postgres", DSN: registered}
}

type matrixSession interface {
	session.Store
	Close() error
}

func matrixSessionFactory(t *testing.T, redisURL, deploymentID string) func() matrixSession {
	t.Helper()
	namespace := "pi-gateway:api-matrix:" + deploymentID + ":" + matrixNonce(t)
	opts := session.Options{Namespace: namespace, TTL: 2 * time.Minute, CommitTimeout: 3 * time.Second}
	if redisURL != "" {
		parsed, err := redis.ParseURL(redisURL)
		reportTestFailure(t, err != nil, func() {
			t.Fatal("invalid Redis integration URL")
		})
		parsed.DialTimeout = 3 * time.Second
		parsed.ReadTimeout = 3 * time.Second
		parsed.WriteTimeout = 3 * time.Second
		parsed.PoolTimeout = 3 * time.Second
		parsed.ContextTimeoutEnabled = true
		parsed.MaxRetries = -1
		// The cleanup client is separate from the owned session clients, which
		// are deliberately closed/reopened to test persistence across clients.
		cleanup := redis.NewClient(parsed)
		t.Cleanup(func() { verifyMatrixSessionFactory(&t, &namespace, &cleanup) })
	}
	return func() matrixSession {
		var sessions matrixSession
		if redisURL == "" {
			sessions = session.NewMemory(opts)
		} else {
			var err error
			sessions, err = session.NewRedis(session.RedisOptions{URL: redisURL, Options: opts})
			reportTestFailure(t, err != nil, func() {
				t.Fatalf("open Redis session store: %v", err)
			})
		}
		t.Cleanup(func() {
			{
				err := sessions.Close()
				reportTestFailure(t, err != nil, func() {
					t.Errorf("close matrix session store: %v", err)
				})
			}
		})
		return sessions
	}
}

type matrixRows struct {
	keyID, accountID, groupID int64
	captureID                 int64
	deletedKeyID              int64
	deletedAccountID          int64
	deletedGroupID            int64
}

func matrixContract(t *testing.T, opts store.Options, redisURL string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.OpenOptions(opts) // A fresh database/schema runs real migrations.
	reportTestFailure(t, err != nil, func() {
		t.Fatalf("initial migration: %v", err)
	})
	t.Cleanup(func() {
		if st != nil {
			{
				err := st.Close()
				reportTestFailure(t, err != nil, func() {
					t.Errorf("close matrix SQL store: %v", err)
				})
			}
		}
	})
	deploymentID, err := st.DeploymentID(ctx)
	reportTestFailure(t, err != nil || !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(deploymentID), func() {
		t.Fatalf("persist deployment UUID: %q %v", deploymentID, err)
	})
	var migrationVersion int64
	if opts.Driver == "postgres" {
		var schema string
		{
			err := st.DB().QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema)
			reportTestFailure(t, err != nil || !strings.HasPrefix(schema, "api_matrix_"), func() {
				t.Fatalf("PostgreSQL escaped private search_path: %q %v", schema, err)
			})
		}
		var count int
		{
			err := st.QueryRowContext(ctx, "SELECT MAX(version),COUNT(*) FROM schema_migrations").Scan(&migrationVersion, &count)
			reportTestFailure(t, err != nil || migrationVersion <= 0 || count != 1, func() {
				t.Fatalf("initial PostgreSQL migration: version=%d count=%d error=%v", migrationVersion, count, err)
			})
		}
	}
	reopen := func() { verifyMatrixContract(&t, &opts, &ctx, &st, &err, &deploymentID, &migrationVersion) }
	reopen()
	openSessions := matrixSessionFactory(t, redisURL, deploymentID)
	sessions := openSessions()
	var rows matrixRows
	if !t.Run("CRUD-HTTP-aggregation-SSE-ledger-capture", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, deltaSSE)
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, usageTerminalSSE)
		})
		h := newHarnessWithStore(t, handler, "sse", st, sessions)
		t.Cleanup(h.dataPlane.Close)
		rows = matrixCRUD(t, h)
		for _, stream := range []bool{false, true} {
			body := map[string]any{"model": "gpt-5.5", "input": []any{gatewayUser("matrix HTTP")}, "stream": stream}
			status, headers, raw := matrixHTTP(t, h, h.key, body)
			reportTestFailure(t, status != http.StatusOK || matrixHTTPResponseID(t, headers, raw, stream) != "resp_usage_1", func() {
				t.Fatalf("HTTP stream=%t protocol response: %d %s", stream, status, raw)
			})
		}
		rows.captureID = matrixLedgerAndCapture(t, h, rows)
	}) {
		return
	}
	reportTestFailure(t, !rowsValid(rows), func() {
		t.Fatal("matrix CRUD fixture did not finish")
	})
	var stale session.Record
	if !t.Run("WS-two-turns-reconnect-HTTP-bridge-current-policy", func(t *testing.T) {
		stale = matrixWSContract(t, st, sessions, rows)
	}) {
		return
	}
	// Restore genuine completed metadata after orderly shutdown to model the
	// stale cache left by a crash. This must NOT restore a live upstream socket.
	stale.Version = 0
	{
		err := sessions.RecordCompleted(ctx, stale)
		reportTestFailure(t, err != nil, func() {
			t.Fatalf("persist stale crash metadata: %v", err)
		})
	}
	{
		err := sessions.Close()
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reopen()
	restarted := openSessions()
	if redisURL != "" {
		got, err := restarted.Resolve(ctx, stale.KeyID, stale.ResponseID)
		reportTestFailure(t, err != nil || got.ConnectionID != stale.ConnectionID || got.InstanceID != stale.InstanceID, func() {
			t.Fatalf("Redis record not retained across owned client reopen: %+v %v", got, err)
		})
	} else {
		{
			_, err := restarted.Resolve(ctx, stale.KeyID, stale.ResponseID)
			reportTestFailure(t, !errors.Is(err, session.ErrMiss), func() {
				t.Fatalf("new memory store retained old process state: %v", err)
			})
		}
		// Even if stale metadata is restored, memory is not a socket authority.
		{
			err := restarted.RecordCompleted(ctx, stale)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
	}
	t.Run("reopen-data-retained-dead-connection-never-revived", func(t *testing.T) { runMatrixContractCase(t, &st, &rows, &stale, &restarted) })
}

func rowsValid(rows matrixRows) bool {
	return rows.keyID > 0 && rows.accountID > 0 && rows.groupID > 0 && rows.captureID > 0
}

func matrixCRUD(t *testing.T, h *testHarness) matrixRows {
	t.Helper()
	ctx := context.Background()
	key, err := h.store.GetKeyByValue(ctx, h.key)
	reportTestFailure(t, err != nil || key == nil, func() {
		t.Fatalf("read API key: %+v %v", key, err)
	})
	group := &store.AccountGroup{Name: "matrix-group", Enabled: true, AccountIDs: []int64{h.accountID}}
	{
		err := h.store.CreateAccountGroup(ctx, group)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	group.Name, group.Notes = "matrix-group-updated", "retained after reopen"
	group.DisabledModels = []string{"blocked-model"}
	{
		err := h.store.UpdateAccountGroup(ctx, group)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	key.Name, key.Label, key.GroupIDs = "matrix-key-updated", "matrix", []int64{group.ID}
	{
		err := h.store.UpdateKey(ctx, key)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	name := "matrix-account-updated"
	{
		err := h.store.PatchAccountManagementFields(ctx, h.accountID, store.AccountManagementPatch{Name: &name})
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	account := &store.Account{Name: "disposable", Enabled: false, Weight: 1, Concurrency: 1}
	{
		err := h.store.CreateAccount(ctx, account)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	account.Name = "disposable-updated"
	{
		err := h.store.UpdateAccount(ctx, account)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	{
		got, err := h.store.GetAccount(ctx, account.ID)
		reportTestFailure(t, err != nil || got == nil || got.Name != account.Name, func() {
			t.Fatalf("account CRUD read: %+v %v", got, err)
		})
	}
	otherGroup := &store.AccountGroup{Name: "disposable-group", Enabled: true, AccountIDs: []int64{account.ID}}
	{
		err := h.store.CreateAccountGroup(ctx, otherGroup)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	otherKey := &store.APIKey{Name: "disposable-key", Enabled: true, GroupIDs: []int64{otherGroup.ID}}
	{
		err := h.store.CreateKey(ctx, otherKey)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	{
		keys, err := h.store.ListKeys(ctx)
		reportTestFailure(t, err != nil || len(keys) != 2, func() {
			t.Fatalf("list keys: count=%d err=%v", len(keys), err)
		})
	}
	{
		accounts, err := h.store.ListAccounts(ctx)
		reportTestFailure(t, err != nil || len(accounts) != 2, func() {
			t.Fatalf("list accounts: count=%d err=%v", len(accounts), err)
		})
	}
	{
		groups, err := h.store.ListAccountGroups(ctx)
		reportTestFailure(t, err != nil || len(groups) != 2, func() {
			t.Fatalf("list groups: count=%d err=%v", len(groups), err)
		})
	}
	{
		err := h.store.DeleteKey(ctx, otherKey.ID)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	{
		err := h.store.DeleteAccountGroup(ctx, otherGroup.ID)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	{
		err := h.store.DeleteAccount(ctx, account.ID)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	rows := matrixRows{keyID: key.ID, accountID: h.accountID, groupID: group.ID, deletedKeyID: otherKey.ID, deletedAccountID: account.ID, deletedGroupID: otherGroup.ID}
	matrixAssertRows(t, h.store, rows)
	return rows
}

func matrixAssertRows(t *testing.T, st *store.Store, rows matrixRows) {
	t.Helper()
	ctx := context.Background()
	key, err := st.GetKey(ctx, rows.keyID)
	reportTestFailure(t, err != nil || key == nil || key.Name != "matrix-key-updated" || key.Label != "matrix" || !key.Enabled || !reflect.DeepEqual(key.GroupIDs, []int64{rows.groupID}), func() {
		t.Fatalf("retained API key: %+v %v", key, err)
	})
	account, err := st.GetAccount(ctx, rows.accountID)
	reportTestFailure(t, err != nil || account == nil || account.Name != "matrix-account-updated" || !account.Enabled, func() {
		t.Fatalf("retained account: %+v %v", account, err)
	})
	group, err := st.GetAccountGroup(ctx, rows.groupID)
	reportTestFailure(t, err != nil || group == nil || group.Name != "matrix-group-updated" || group.Notes != "retained after reopen" || !group.Enabled || !reflect.DeepEqual(group.AccountIDs, []int64{rows.accountID}) || !reflect.DeepEqual(group.DisabledModels, []string{"blocked-model"}), func() {
		t.Fatalf("retained group: %+v %v", group, err)
	})
	{
		got, err := st.GetKey(ctx, rows.deletedKeyID)
		reportTestFailure(t, err != nil || got != nil, func() {
			t.Fatalf("deleted key returned: %+v %v", got, err)
		})
	}
	{
		got, err := st.GetAccount(ctx, rows.deletedAccountID)
		reportTestFailure(t, err != nil || got != nil, func() {
			t.Fatalf("deleted account returned: %+v %v", got, err)
		})
	}
	{
		got, err := st.GetAccountGroup(ctx, rows.deletedGroupID)
		reportTestFailure(t, err != nil || got != nil, func() {
			t.Fatalf("deleted group returned: %+v %v", got, err)
		})
	}
	if rows.captureID > 0 {
		capture, err := st.GetCapture(ctx, rows.captureID)
		reportTestFailure(t, err != nil || capture == nil || capture.ResponseID != "resp_usage_1" || capture.PromptTokens != 100 || capture.CompletionTokens != 20 || len(capture.ResponseFrames) == 0 || capture.RequestBody == "", func() {
			t.Fatalf("retained capture payload: %+v %v", capture, err)
		})
		records, total, err := st.ListUsageRecords(ctx, store.UsageFilter{Model: "gpt-5.5", Outcome: "succeeded", Limit: 10})
		reportTestFailure(t, err != nil || total != 2 || len(records) != 2, func() {
			t.Fatalf("retained HTTP usage ledger: total=%d rows=%d err=%v", total, len(records), err)
		})
	}
}

func matrixHTTP(t *testing.T, h *testHarness, key string, body map[string]any) (int, http.Header, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/responses", bytes.NewReader(raw))
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Session_id", "same-client-session")
	client := &http.Client{Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	return resp.StatusCode, resp.Header.Clone(), payload
}

func matrixHTTPResponseID(t *testing.T, headers http.Header, raw []byte, stream bool) string {
	t.Helper()
	if !stream {
		var response struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		reportTestFailure(t, !strings.Contains(headers.Get("Content-Type"), "application/json") || json.Unmarshal(raw, &response) != nil || response.Status != "completed" || response.ID == "", func() {
			t.Fatalf("invalid aggregated HTTP response: %s", raw)
		})
		return response.ID
	}
	reportTestFailure(t, !strings.Contains(headers.Get("Content-Type"), "text/event-stream"), func() {
		t.Fatalf("SSE Content-Type = %q", headers.Get("Content-Type"))
	})
	var id string
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	for _, frame := range strings.Split(text, "\n\n") {
		var lines []string
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data:") {
				lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		data := strings.TrimSpace(strings.Join(lines, "\n"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event map[string]any
		{
			err := json.Unmarshal([]byte(data), &event)
			reportTestFailure(t, err != nil, func() {
				t.Fatalf("invalid SSE JSON event: %v", err)
			})
		}
		if event["type"] == "response.completed" {
			id = gatewayResponseID(t, event)
		}
	}
	reportTestFailure(t, id == "", func() {
		t.Fatalf("SSE has no completed response: %s", raw)
	})
	return id
}

func matrixLedgerAndCapture(t *testing.T, h *testHarness, rows matrixRows) int64 {
	t.Helper()
	var records []store.UsageRecord
	var captures []*store.Capture
	gatewayAwait(t, func() bool {
		var err error
		records, _, err = h.store.ListUsageRecords(context.Background(), store.UsageFilter{Model: "gpt-5.5", Outcome: "succeeded", Limit: 10})
		reportTestFailure(t, err != nil, func() {
			t.Fatalf("read protocol usage ledger: %v", err)
		})
		captures, _, err = h.store.ListCaptures(context.Background(), store.CaptureFilter{Limit: 10, IncludePayloads: true})
		reportTestFailure(t, err != nil, func() {
			t.Fatalf("read protocol captures: %v", err)
		})
		return len(records) == 2 && len(captures) == 2
	}, "HTTP handlers did not persist both ledger/capture rows")
	for _, rec := range records {
		reportTestFailure(t, rec.APIKeyID != rows.keyID || rec.AccountID != rows.accountID || rec.APIKeyName != "matrix-key-updated" || rec.AccountName != "matrix-account-updated" || rec.InputTokens == nil || *rec.InputTokens != 100 || rec.OutputTokens == nil || *rec.OutputTokens != 20 || rec.TotalTokens == nil || *rec.TotalTokens != 120 || rec.CachedTokens == nil || *rec.CachedTokens != 80 || rec.CostMicros == nil || *rec.CostMicros <= 0, func() {
			t.Fatalf("protocol ledger attribution/usage/cost: %+v", rec)
		})
	}
	for _, capture := range captures {
		reportTestFailure(t, capture.AccountID != rows.accountID || capture.Outcome != store.OutcomeOK || capture.Status != http.StatusOK || capture.ResponseID != "resp_usage_1" || capture.TotalTokens != 120 || capture.RequestBody == "", func() {
			t.Fatalf("protocol capture: %+v", capture)
		})
		terminal := false
		for _, frame := range capture.ResponseFrames {
			terminal = terminal || frame.Type == "response.completed"
		}
		reportTestFailure(t, !terminal, func() {
			t.Fatal("capture lost upstream terminal event")
		})
		for _, header := range capture.RequestHeaders {
			reportTestFailure(t, strings.EqualFold(header.Name, "Authorization") && strings.Contains(header.Value, "Bearer "+h.key), func() {
				t.Fatal("capture persisted unmasked API key")
			})
		}
	}
	return captures[0].ID
}

func matrixResolve(t *testing.T, sessions session.Store, keyID int64, responseID string) *session.Record {
	t.Helper()
	var record *session.Record
	gatewayAwait(t, func() bool {
		var err error
		record, err = sessions.Resolve(context.Background(), keyID, responseID)
		if errors.Is(err, session.ErrMiss) {
			return false
		}
		reportTestFailure(t, err != nil, func() {
			t.Fatalf("read actual continuation backend: %v", err)
		})
		return record != nil
	}, "completed protocol response was not committed to continuation backend")
	reportTestFailure(t, record.KeyID != keyID || record.Scope != session.ScopeConnectionLocal || record.ConnectionID == "" || record.InstanceID == "", func() {
		t.Fatalf("invalid connection-local metadata: %+v", record)
	})
	return record
}

func matrixWSContract(t *testing.T, st *store.Store, sessions matrixSession, rows matrixRows) session.Record {
	t.Helper()
	ctx := context.Background()
	b := newGatewayContinuationBackend()
	h := newHarnessWithStore(t, b, "websocket-cached", st, sessions)
	// Register after the harness so downstream cancellation/wait precedes SQL,
	// upstream and session closure, including a fatal assertion mid-protocol.
	t.Cleanup(h.dataPlane.Close)
	conn := dialGatewayContinuation(t, h, h.key)
	first := gatewayResponseID(t, gatewayExchange(t, conn, gatewayCreate([]any{gatewayUser("one")}, "", "same-client-session")))
	firstTurn := gatewayObservedTurn(t, b)
	matrixResolve(t, sessions, rows.keyID, first)
	second := gatewayResponseID(t, gatewayExchange(t, conn, gatewayCreate([]any{gatewayUser("two")}, first, "same-client-session")))
	secondTurn := gatewayObservedTurn(t, b)
	reportTestFailure(t, first == second || secondTurn.connection != firstTurn.connection || secondTurn.body["previous_response_id"] != first, func() {
		t.Fatal("two WS turns did not use the same upstream continuation")
	})
	matrixResolve(t, sessions, rows.keyID, second)
	foreign := &store.APIKey{Name: "foreign-key", Enabled: true, GroupIDs: []int64{rows.groupID}}
	{
		err := st.CreateKey(ctx, foreign)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	other := dialGatewayContinuation(t, h, foreign.Key)
	{
		code := gatewayErrorCode(t, gatewayExchange(t, other, gatewayCreate([]any{gatewayUser("steal")}, second, "same-client-session")))
		reportTestFailure(t, code != "previous_response_not_found", func() {
			t.Fatalf("cross-key same-session WS request: %s", code)
		})
	}
	{
		_, err := sessions.Resolve(ctx, foreign.ID, second)
		reportTestFailure(t, !errors.Is(err, session.ErrWrongOwner), func() {
			t.Fatalf("session backend failed owner binding: %v", err)
		})
	}
	foreignBody := gatewayCreate([]any{gatewayUser("steal")}, second, "same-client-session")
	status, _, raw := matrixHTTP(t, h, foreign.Key, foreignBody)
	reportTestFailure(t, status != http.StatusBadRequest || !bytes.Contains(raw, []byte("previous_response_not_found")) || b.generations.Load() != 2, func() {
		t.Fatalf("cross-key same-session HTTP request reached upstream: %d %s", status, raw)
	})
	_ = other.Close()
	_ = conn.Close()
	gatewayAwait(t, func() bool {
		h.dataPlane.lifecycle.mu.Lock()
		defer h.dataPlane.lifecycle.mu.Unlock()
		return len(h.dataPlane.lifecycle.active) == 0
	}, "disconnected WS handlers did not finish")
	reconnected := dialGatewayContinuation(t, h, h.key)
	previous := gatewayResponseID(t, gatewayExchange(t, reconnected, gatewayCreate([]any{gatewayUser("reconnect delta")}, second, "same-client-session")))
	resumed := gatewayObservedTurn(t, b)
	reportTestFailure(t, resumed.connection != firstTurn.connection || resumed.body["previous_response_id"] != second || len(resumed.body["input"].([]any)) != 1, func() {
		t.Fatalf("explicit reconnect lost original socket/delta: %+v", resumed)
	})
	for _, stream := range []bool{false, true} {
		body := gatewayCreate([]any{gatewayUser("WS to HTTP")}, previous, "same-client-session")
		body["stream"] = stream
		status, headers, raw := matrixHTTP(t, h, h.key, body)
		reportTestFailure(t, status != http.StatusOK, func() {
			t.Fatalf("WS to HTTP continuation: %d %s", status, raw)
		})
		turn := gatewayObservedTurn(t, b)
		reportTestFailure(t, turn.connection != firstTurn.connection || turn.body["previous_response_id"] != previous, func() {
			t.Fatal("HTTP bridge did not keep original WS continuation")
		})
		previous = matrixHTTPResponseID(t, headers, raw, stream)
		matrixResolve(t, sessions, rows.keyID, previous)
	}
	last := gatewayResponseID(t, gatewayExchange(t, reconnected, gatewayCreate([]any{gatewayUser("HTTP to WS")}, previous, "same-client-session")))
	turn := gatewayObservedTurn(t, b)
	reportTestFailure(t, turn.connection != firstTurn.connection || turn.body["previous_response_id"] != previous || b.connections.Load() != 1 || b.httpCalls.Load() != 0, func() {
		t.Fatal("HTTP to WS bridge opened a replacement or HTTP fallback")
	})
	matrixResolve(t, sessions, rows.keyID, last)
	_ = reconnected.Close()
	matrixCurrentPolicy(t, h, b, sessions, rows)
	// A fresh, completed chain supplies authentic ownership metadata for the
	// restart test, independent from chains invalidated by policy rejection.
	final := dialGatewayContinuation(t, h, h.key)
	id := gatewayResponseID(t, gatewayExchange(t, final, gatewayCreate([]any{gatewayUser("before restart")}, "", "same-client-session")))
	gatewayObservedTurn(t, b)
	return *matrixResolve(t, sessions, rows.keyID, id)
}

func matrixCurrentPolicy(t *testing.T, h *testHarness, b *gatewayContinuationBackend, sessions session.Store, rows matrixRows) {
	t.Helper()
	ctx := context.Background()
	for _, policy := range []string{"disabled-key", "disabled-account", "disabled-group", "group-model", "account-model"} {
		conn := dialGatewayContinuation(t, h, h.key)
		id := gatewayResponseID(t, gatewayExchange(t, conn, gatewayCreate([]any{gatewayUser("before " + policy)}, "", "")))
		gatewayObservedTurn(t, b)
		matrixResolve(t, sessions, rows.keyID, id)
		key, err := h.store.GetKey(ctx, rows.keyID)
		reportTestFailure(t, err != nil || key == nil, func() {
			t.Fatalf("policy key read: %v", err)
		})
		group, err := h.store.GetAccountGroup(ctx, rows.groupID)
		reportTestFailure(t, err != nil || group == nil, func() {
			t.Fatalf("policy group read: %v", err)
		})
		no, yes := false, true
		var restore func() error
		switch policy {
		case "disabled-key":
			key.Enabled = false
			err = h.store.UpdateKey(ctx, key)
			restore = func() error { key.Enabled = true; return h.store.UpdateKey(ctx, key) }
		case "disabled-account":
			err = h.store.PatchAccountManagementFields(ctx, rows.accountID, store.AccountManagementPatch{Enabled: &no})
			restore = func() error {
				return h.store.PatchAccountManagementFields(ctx, rows.accountID, store.AccountManagementPatch{Enabled: &yes})
			}
		case "disabled-group":
			group.Enabled = false
			err = h.store.UpdateAccountGroup(ctx, group)
			restore = func() error { group.Enabled = true; return h.store.UpdateAccountGroup(ctx, group) }
		case "group-model":
			group.DisabledModels = []string{"blocked-model", "test-model"}
			err = h.store.UpdateAccountGroup(ctx, group)
			restore = func() error {
				group.DisabledModels = []string{"blocked-model"}
				return h.store.UpdateAccountGroup(ctx, group)
			}
		case "account-model":
			err = h.store.PatchAccountManagementFields(ctx, rows.accountID, store.AccountManagementPatch{DisabledModels: []string{"test-model"}})
			restore = func() error {
				return h.store.PatchAccountManagementFields(ctx, rows.accountID, store.AccountManagementPatch{DisabledModels: []string{}})
			}
		}
		reportTestFailure(t, err != nil, func() {
			t.Fatalf("change current %s: %v", policy, err)
		})
		before := b.generations.Load()
		body := gatewayCreate([]any{gatewayUser("must reject changed policy")}, id, "")
		event := gatewayExchange(t, conn, body)
		gatewayErrorCode(t, event) // Authentication errors need not have a code.
		problem := event["error"].(map[string]any)
		reportTestFailure(t, problem["message"] == nil || problem["type"] == nil, func() {
			t.Fatalf("%s returned no structured WS rejection: %v", policy, event)
		})
		delete(body, "previous_response_id")
		status, _, raw := matrixHTTP(t, h, h.key, body)
		wantStatus := http.StatusServiceUnavailable
		if policy == "disabled-key" {
			wantStatus = http.StatusUnauthorized
		} else if strings.Contains(policy, "model") {
			wantStatus = http.StatusForbidden
			reportTestFailure(t, !bytes.Contains(raw, []byte("model_disabled")), func() {
				t.Fatalf("current %s did not return model_disabled: %s", policy, raw)
			})
		}
		reportTestFailure(t, status != wantStatus || b.generations.Load() != before, func() {
			t.Fatalf("current %s bypassed HTTP/WS authorization: status=%d want=%d body=%s", policy, status, wantStatus, raw)
		})
		{
			err := restore()
			reportTestFailure(t, err != nil, func() {
				t.Fatalf("restore %s: %v", policy, err)
			})
		}
		_ = conn.Close()
		gatewayAwait(t, func() bool { return h.dataPlane.accounts.Inflight(rows.accountID) == 0 }, "policy rejection leaked account slot")
	}
}

// verifyMatrixSessionFactory exercises one isolated test phase with the original shared state.
func verifyMatrixSessionFactory(capturedT **testing.T, capturedNamespace *string, capturedCleanup **redis.Client) {
	(*capturedT).Helper()

	defer func() {
		{
			err := (*capturedCleanup).Close()
			reportTestFailure((*capturedT), err != nil, func() {
				(*capturedT).Errorf("close Redis cleanup client: %v", err)
			})
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	prefix := (*capturedNamespace) + ":"
	var cursor uint64
	var keys []string
	for {
		batch, next, err := (*capturedCleanup).Scan(ctx, cursor, prefix+"*", 100).Result()
		if err != nil {
			(*capturedT).Errorf("scan private Redis namespace: %v", err)
			return
		}
		for _, key := range batch {
			if !strings.HasPrefix(key, prefix) {
				(*capturedT).Error("Redis cleanup escaped private namespace")
				return
			}
			keys = append(keys, key)
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	if len(keys) != 0 {
		{
			err := (*capturedCleanup).Del(ctx, keys...).Err()
			reportTestFailure((*capturedT), err != nil, func() {
				(*capturedT).Errorf("delete private Redis keys: %v", err)
			})
		}
	}
}

// verifyMatrixContract exercises one isolated test phase with the original shared state.
func verifyMatrixContract(capturedT **testing.T, capturedOpts *store.Options, capturedCtx *context.Context, capturedSt **store.Store, capturedErr *error, capturedDeploymentID *string, capturedMigrationVersion *int64) {
	(*capturedT).Helper()
	{
		err := (*capturedSt).Close()
		reportTestFailure((*capturedT), err != nil, func() {
			(*capturedT).Fatal(err)
		})
	}
	(*capturedSt), (*capturedErr) = store.OpenOptions((*capturedOpts))
	reportTestFailure((*capturedT), (*capturedErr) != nil, func() {
		(*capturedT).Fatalf("reopen/migrate store: %v", (*capturedErr))
	})
	id, err := (*capturedSt).DeploymentID((*capturedCtx))
	reportTestFailure((*capturedT), err != nil || id != (*capturedDeploymentID), func() {
		(*capturedT).Fatalf("deployment UUID changed across Open: %q %v", id, err)
	})
	if (*capturedOpts).Driver == "postgres" {
		var version int64
		var count int
		{
			err := (*capturedSt).QueryRowContext((*capturedCtx), "SELECT MAX(version),COUNT(*) FROM schema_migrations").Scan(&version, &count)
			reportTestFailure((*capturedT), err != nil || version != (*capturedMigrationVersion) || count != 1, func() {
				(*capturedT).Fatalf("migration was not idempotent: version=%d count=%d error=%v", version, count, err)
			})
		}
	}
}

// runMatrixContractCase exercises one isolated test phase with the original shared state.
func runMatrixContractCase(t *testing.T, capturedSt **store.Store, capturedRows *matrixRows, capturedStale *session.Record, capturedRestarted *matrixSession) {
	t.Helper()

	matrixAssertRows(t, (*capturedSt), (*capturedRows))
	b := newGatewayContinuationBackend()
	h := newHarnessWithStore(t, b, "websocket-cached", (*capturedSt), (*capturedRestarted))
	t.Cleanup(h.dataPlane.Close)
	for _, stream := range []bool{false, true} {
		body := gatewayCreate([]any{gatewayUser("cannot resurrect")}, (*capturedStale).ResponseID, "same-client-session")
		body["stream"] = stream
		status, _, raw := matrixHTTP(t, h, h.key, body)
		reportTestFailure(t, status != http.StatusBadRequest || !bytes.Contains(raw, []byte("previous_response_not_found")), func() {
			t.Fatalf("old connection accepted over HTTP: %d %s", status, raw)
		})
	}
	conn := dialGatewayContinuation(t, h, h.key)
	{
		code := gatewayErrorCode(t, gatewayExchange(t, conn, gatewayCreate([]any{gatewayUser("cannot resurrect")}, (*capturedStale).ResponseID, "same-client-session")))
		reportTestFailure(t, code != "previous_response_not_found", func() {
			t.Fatalf("old connection accepted over WS: %s", code)
		})
	}
	reportTestFailure(t, b.generations.Load() != 0 || b.connections.Load() != 0 || b.httpCalls.Load() != 0, func() {
		t.Fatal("restarted gateway replayed a dead connection-local record")
	})
	// Keep fake response IDs globally unique across the two gateway instances;
	// this mirrors provider-issued IDs and avoids idempotent cache collisions.
	b.generations.Add(100)
	id := gatewayResponseID(t, gatewayExchange(t, conn, gatewayCreate([]any{gatewayUser("fresh after restart")}, "", "same-client-session")))
	gatewayObservedTurn(t, b)
	fresh := matrixResolve(t, (*capturedRestarted), (*capturedRows).keyID, id)
	reportTestFailure(t, fresh.InstanceID == (*capturedStale).InstanceID || fresh.ConnectionID == (*capturedStale).ConnectionID, func() {
		t.Fatal("new chain reused old instance/socket ownership")
	})
}
