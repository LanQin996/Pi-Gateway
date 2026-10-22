package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"pi-gateway/internal/egress"
	"pi-gateway/internal/session"
	"pi-gateway/internal/store"
	"pi-gateway/internal/upstream"
)

// Unlike wsUpstream, this backend stays open across turns and assigns an ID to
// every generation. Tests observe actual gateway handshakes and wire frames.
type gatewayTurn struct {
	connection int64
	id         string
	headers    http.Header
	body       map[string]any
}

type gatewayContinuationBackend struct {
	connections atomic.Int64
	generations atomic.Int64
	httpCalls   atomic.Int64
	turns       chan gatewayTurn
	output      []any
	hook        func(*websocket.Conn, gatewayTurn) bool
}

func newGatewayContinuationBackend() *gatewayContinuationBackend {
	return &gatewayContinuationBackend{turns: make(chan gatewayTurn, 64), output: []any{}}
}

func (b *gatewayContinuationBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		b.httpCalls.Add(1)
		http.Error(w, "unexpected HTTP fallback", http.StatusBadGateway)
		return
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	connection := b.connections.Add(1)
	for {
		var body map[string]any
		if err := conn.ReadJSON(&body); err != nil {
			return
		}
		turn := gatewayTurn{connection, fmt.Sprintf("gateway-response-%d", b.generations.Add(1)), r.Header.Clone(), body}
		b.turns <- turn
		if b.hook != nil && !b.hook(conn, turn) {
			return
		}
		if err := conn.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": turn.id}}); err != nil {
			return
		}
		if err := conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{
			"id": turn.id, "status": "completed", "output": b.output,
			"usage": map[string]any{"input_tokens": 3, "output_tokens": 1, "total_tokens": 4},
		}}); err != nil {
			return
		}
	}
}

func newGatewayContinuationHarness(t *testing.T, backend *gatewayContinuationBackend, sessions session.Store) *testHarness {
	t.Helper()
	h := newHarness(t, backend, "websocket-cached")
	if sessions == nil {
		memory := session.NewMemory(session.Options{})
		t.Cleanup(func() { _ = memory.Close() })
		sessions = memory
	}
	// Replace the unused client before the first request, not during a stream.
	h.dataPlane.upstream.Close()
	client := upstream.New(upstream.Config{
		SSEURL: h.baseConfig.UpstreamSSEURL(), WSURL: h.baseConfig.UpstreamWSURL(),
		Pool: true, Sessions: sessions, Logger: discardLogger(),
	}, egress.NewFactory(egress.Options{ConnectTimeout: time.Second, IdleTimeout: time.Second}))
	h.dataPlane.upstream = client
	t.Cleanup(client.Close)
	t.Cleanup(h.dataPlane.Close)
	return h
}

func dialGatewayContinuation(t *testing.T, h *testHarness, key string) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.server.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer " + key}})
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		t.Fatalf("gateway WS handshake: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func gatewayCreate(input []any, previous, hint string) map[string]any {
	body := map[string]any{"type": "response.create", "model": "test-model", "input": input}
	if previous != "" {
		body["previous_response_id"] = previous
	}
	if hint != "" {
		body["prompt_cache_key"] = hint
	}
	return body
}

func gatewayUser(text string) map[string]any {
	return map[string]any{"type": "message", "role": "user", "content": text}
}

func gatewayExchange(t *testing.T, conn *websocket.Conn, body map[string]any) map[string]any {
	t.Helper()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	{
		err := conn.WriteJSON(body)
		reportTestFailure(t, err != nil, func() {
			t.Fatalf("send gateway turn: %v", err)
		})
	}
	return gatewayTerminal(t, conn)
}

func gatewayTerminal(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var event map[string]any
		{
			err := conn.ReadJSON(&event)
			reportTestFailure(t, err != nil, func() {
				t.Fatalf("receive gateway turn: %v", err)
			})
		}
		switch event["type"] {
		case "response.completed", "response.done", "response.failed", "response.incomplete", "error":
			return event
		}
	}
}

func gatewayResponseID(t *testing.T, event map[string]any) string {
	t.Helper()
	response, ok := event["response"].(map[string]any)
	reportTestFailure(t, !ok || event["type"] != "response.completed", func() {
		t.Fatalf("expected completed response: %v", event)
	})
	id, _ := response["id"].(string)
	reportTestFailure(t, id == "", func() {
		t.Fatal("response is missing its ID")
	})
	return id
}

func gatewayErrorCode(t *testing.T, event map[string]any) string {
	t.Helper()
	problem, ok := event["error"].(map[string]any)
	reportTestFailure(t, !ok, func() {
		t.Fatalf("expected structured error: %v", event)
	})
	code, _ := problem["code"].(string)
	return code
}

func gatewayObservedTurn(t *testing.T, b *gatewayContinuationBackend) gatewayTurn {
	t.Helper()
	select {
	case turn := <-b.turns:
		return turn
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not receive a turn")
		return gatewayTurn{}
	}
}

func gatewayOnlySession(t *testing.T, h *testHarness) string {
	t.Helper()
	h.dataPlane.lifecycle.mu.Lock()
	defer h.dataPlane.lifecycle.mu.Unlock()
	reportTestFailure(t, len(h.dataPlane.lifecycle.active) != 1, func() {
		t.Fatalf("active downstreams = %d, want one", len(h.dataPlane.lifecycle.active))
	})
	for id := range h.dataPlane.lifecycle.active {
		return id
	}
	return ""
}

func gatewayAwait(t *testing.T, check func() bool, description string) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for !check() {
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal(description)
		}
	}
}

func gatewayHTTP(t *testing.T, h *testHarness, body map[string]any) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/responses", bytes.NewReader(raw))
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	req.Header.Set("Authorization", "Bearer "+h.key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	return resp.StatusCode, payload
}

func TestGatewayContinuationStablePrivateSessionAndToolDelta(t *testing.T) {
	b := newGatewayContinuationBackend()
	call := map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "lookup", "arguments": "{}"}
	b.output = []any{call}
	h := newGatewayContinuationHarness(t, b, nil)
	conn := dialGatewayContinuation(t, h, h.key)
	firstInput := []any{gatewayUser("first")}
	firstID := gatewayResponseID(t, gatewayExchange(t, conn, gatewayCreate(firstInput, "", "")))
	first := gatewayObservedTurn(t, b)
	privateID := gatewayOnlySession(t, h)
	result := map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "result"}
	secondInput := []any{gatewayUser("first"), call, result, gatewayUser("second")}
	gatewayResponseID(t, gatewayExchange(t, conn, gatewayCreate(secondInput, "", "")))
	second := gatewayObservedTurn(t, b)
	reportTestFailure(t, gatewayOnlySession(t, h) != privateID || second.connection != first.connection || b.connections.Load() != 1, func() {
		t.Fatal("same downstream did not keep its internal identity and upstream socket")
	})
	reportTestFailure(t, second.body["previous_response_id"] != firstID, func() {
		t.Fatalf("full-history optimization lost parent: %v", second.body)
	})
	delta, _ := second.body["input"].([]any)
	reportTestFailure(t, len(delta) != 2 || !reflect.DeepEqual(delta[0], result), func() {
		t.Fatalf("tool result/new input order or delta is wrong: %v", delta)
	})
	for _, turn := range []gatewayTurn{first, second} {
		for _, name := range []string{"session_id", "session-id", "x-session-id", "x-client-request-id"} {
			reportTestFailure(t, turn.headers.Get(name) != "", func() {
				t.Errorf("generated session leaked in %s", name)
			})
		}
		body, _ := json.Marshal(turn.body)
		headers, _ := json.Marshal(turn.headers)
		{
			_, exists := turn.body["prompt_cache_key"]
			reportTestFailure(t, exists || bytes.Contains(body, []byte(privateID)) || bytes.Contains(headers, []byte(privateID)), func() {
				t.Fatal("internal connection identity appeared on the upstream wire")
			})
		}
	}
	_ = conn.Close()
	gatewayAwait(t, func() bool {
		h.dataPlane.lifecycle.mu.Lock()
		defer h.dataPlane.lifecycle.mu.Unlock()
		return len(h.dataPlane.lifecycle.active) == 0
	}, "closed downstream handler was not released")
	next := dialGatewayContinuation(t, h, h.key)
	gatewayResponseID(t, gatewayExchange(t, next, gatewayCreate(firstInput, "", "")))
	third := gatewayObservedTurn(t, b)
	reportTestFailure(t, gatewayOnlySession(t, h) == privateID || third.connection == first.connection, func() {
		t.Fatal("an independent no-session downstream reused another connection's implicit chain")
	})
}

func TestGatewayContinuationSameHintIsolationAndReconnect(t *testing.T) {
	for _, differentKey := range []bool{false, true} {
		t.Run(fmt.Sprintf("different-key-%t", differentKey), func(t *testing.T) {
			b := newGatewayContinuationBackend()
			h := newGatewayContinuationHarness(t, b, nil)
			key2 := h.key
			if differentKey {
				key := &store.APIKey{Name: "other", Key: "sk-other-continuation", Enabled: true}
				{
					err := h.store.CreateKey(context.Background(), key)
					reportTestFailure(t, err != nil, func() {
						t.Fatal(err)
					})
				}
				key2 = key.Key
			}
			one := dialGatewayContinuation(t, h, h.key)
			two := dialGatewayContinuation(t, h, key2)
			id1 := gatewayResponseID(t, gatewayExchange(t, one, gatewayCreate([]any{gatewayUser("one")}, "", "same-hint")))
			turn1 := gatewayObservedTurn(t, b)
			id2 := gatewayResponseID(t, gatewayExchange(t, two, gatewayCreate([]any{gatewayUser("two")}, "", "same-hint")))
			turn2 := gatewayObservedTurn(t, b)
			reportTestFailure(t, turn1.connection == turn2.connection, func() {
				t.Fatal("independent downstreams with same hint shared mutable state")
			})
			if differentKey {
				event := gatewayExchange(t, two, gatewayCreate([]any{gatewayUser("steal")}, id1, "same-hint"))
				reportTestFailure(t, gatewayErrorCode(t, event) != "previous_response_not_found" || b.generations.Load() != 2, func() {
					t.Fatalf("foreign continuation was sent upstream: %v", event)
				})
			}
			_ = one.Close()
			reconnected := dialGatewayContinuation(t, h, h.key)
			newID := gatewayResponseID(t, gatewayExchange(t, reconnected, gatewayCreate([]any{gatewayUser("delta-one")}, id1, "new-hint")))
			resumed := gatewayObservedTurn(t, b)
			reportTestFailure(t, resumed.connection != turn1.connection || resumed.body["previous_response_id"] != id1 || len(resumed.body["input"].([]any)) != 1, func() {
				t.Fatalf("reconnect did not resume original socket with explicit delta: %v", resumed)
			})
			gatewayResponseID(t, gatewayExchange(t, two, gatewayCreate([]any{gatewayUser("delta-two")}, id2, "same-hint")))
			independent := gatewayObservedTurn(t, b)
			reportTestFailure(t, independent.connection != turn2.connection || independent.body["previous_response_id"] != id2, func() {
				t.Fatal("resuming first chain overwrote second chain")
			})
			reportTestFailure(t, gatewayErrorCode(t, gatewayExchange(t, reconnected, gatewayCreate([]any{gatewayUser("old")}, id1, ""))) != "previous_response_not_found", func() {
				t.Fatal("an old parent was accepted")
			})
			reportTestFailure(t, newID == id1 || b.generations.Load() != 4 || b.connections.Load() != 2 || b.httpCalls.Load() != 0, func() {
				t.Fatal("reconnect or stale rejection caused extra generation/handshake/HTTP fallback")
			})
		})
	}
}

func TestGatewayContinuationHTTPUsesOnlyLiveOriginalSocket(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream-%t", stream), func(t *testing.T) {
			b := newGatewayContinuationBackend()
			h := newGatewayContinuationHarness(t, b, nil)
			conn := dialGatewayContinuation(t, h, h.key)
			id := gatewayResponseID(t, gatewayExchange(t, conn, gatewayCreate([]any{gatewayUser("one")}, "", "")))
			first := gatewayObservedTurn(t, b)
			body := gatewayCreate([]any{gatewayUser("delta")}, id, "")
			delete(body, "type")
			body["stream"] = stream
			status, raw := gatewayHTTP(t, h, body)
			reportTestFailure(t, status != http.StatusOK || !bytes.Contains(raw, []byte("gateway-response-2")), func() {
				t.Fatalf("HTTP downstream cannot bridge to live WS: %d %s", status, raw)
			})
			second := gatewayObservedTurn(t, b)
			reportTestFailure(t, first.connection != second.connection || second.body["previous_response_id"] != id || b.httpCalls.Load() != 0, func() {
				t.Fatal("connection-local continuation was sent on a new upstream transport")
			})
			// A stale ID is rejected before headers/streaming start, for both APIs.
			status, raw = gatewayHTTP(t, h, body)
			reportTestFailure(t, status != http.StatusBadRequest || !bytes.Contains(raw, []byte("previous_response_not_found")) || b.generations.Load() != 2, func() {
				t.Fatalf("stale HTTP continuation was executed: %d %s", status, raw)
			})
			delete(body, "previous_response_id")
			body["input"] = []any{gatewayUser("one"), gatewayUser("delta"), gatewayUser("complete new chain")}
			status, raw = gatewayHTTP(t, h, body)
			reportTestFailure(t, status != http.StatusOK || b.generations.Load() != 3, func() {
				t.Fatalf("complete new chain did not execute exactly once: %d %s", status, raw)
			})
			third := gatewayObservedTurn(t, b)
			{
				_, present := third.body["previous_response_id"]
				reportTestFailure(t, present || len(third.body["input"].([]any)) != 3, func() {
					t.Fatal("new chain inherited a stale delta")
				})
			}
		})
	}
}

type gatewayResolveFailure struct {
	session.Store
	err error
}

func (s gatewayResolveFailure) Resolve(context.Context, int64, string) (*session.Record, error) {
	return nil, s.err
}

func TestGatewayContinuationUntrustedMetadataNeverReachesUpstream(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		code   string
		status int
	}{
		{"missing-or-expired", session.ErrMiss, "previous_response_not_found", 400},
		{"foreign-owner", session.ErrWrongOwner, "previous_response_not_found", 400},
		{"corrupt-or-oversized", session.ErrInvalid, "previous_response_not_found", 400},
		{"unavailable", session.ErrUnavailable, "session_state_unavailable", 503},
		{"read-timeout", context.DeadlineExceeded, "session_state_unavailable", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newGatewayContinuationBackend()
			memory := session.NewMemory(session.Options{})
			defer memory.Close()
			h := newGatewayContinuationHarness(t, b, gatewayResolveFailure{memory, tc.err})
			conn := dialGatewayContinuation(t, h, h.key)
			request := gatewayCreate([]any{gatewayUser("delta")}, "not-owned", "")
			{
				code := gatewayErrorCode(t, gatewayExchange(t, conn, request))
				reportTestFailure(t, code != tc.code, func() {
					t.Fatalf("WS error code = %q, want %q", code, tc.code)
				})
			}
			for _, stream := range []bool{false, true} {
				request["stream"] = stream
				status, raw := gatewayHTTP(t, h, request)
				reportTestFailure(t, status != tc.status || !bytes.Contains(raw, []byte(tc.code)), func() {
					t.Fatalf("HTTP error = %d %s, want %d %s", status, raw, tc.status, tc.code)
				})
			}
			reportTestFailure(t, b.generations.Load() != 0 || b.connections.Load() != 0 || b.httpCalls.Load() != 0, func() {
				t.Fatal("untrusted response ID reached upstream")
			})
			delete(request, "previous_response_id")
			gatewayResponseID(t, gatewayExchange(t, conn, request))
			reportTestFailure(t, b.generations.Load() != 1, func() {
				t.Fatal("optional session backend failure blocked an ordinary complete request")
			})
		})
	}
}

func TestGatewayContinuationCurrentPolicyAndIdentityRechecked(t *testing.T) {
	for _, mutation := range []string{"disable-key", "rotate-key", "delete-key", "disable-account", "model", "group", "credential", "proxy"} {
		t.Run(mutation, func(t *testing.T) {
			b := newGatewayContinuationBackend()
			h := newGatewayContinuationHarness(t, b, nil)
			conn := dialGatewayContinuation(t, h, h.key)
			id := gatewayResponseID(t, gatewayExchange(t, conn, gatewayCreate([]any{gatewayUser("one")}, "", "")))
			gatewayObservedTurn(t, b)
			ctx := context.Background()
			key, err := h.store.GetKeyByValue(ctx, h.key)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			switch mutation {
			case "disable-key":
				key.Enabled = false
				err = h.store.UpdateKey(ctx, key)
			case "rotate-key":
				_, err = h.store.ExecContext(ctx, `UPDATE api_keys SET key=? WHERE id=?`, "sk-replacement", key.ID)
			case "delete-key":
				err = h.store.DeleteKey(ctx, key.ID)
			case "disable-account":
				no := false
				err = h.store.PatchAccountManagementFields(ctx, h.accountID, store.AccountManagementPatch{Enabled: &no})
			case "model":
				err = h.store.PatchAccountManagementFields(ctx, h.accountID, store.AccountManagementPatch{DisabledModels: []string{"test-model"}})
			case "group":
				group := &store.AccountGroup{Name: "revoked", Enabled: false, AccountIDs: []int64{h.accountID}}
				err = h.store.CreateAccountGroup(ctx, group)
				if err == nil {
					key.GroupIDs = []int64{group.ID}
					err = h.store.UpdateKey(ctx, key)
				}
			case "credential":
				_, err = h.store.ExecContext(ctx, `UPDATE accounts SET access_token=? WHERE id=?`, fakeJWT(t, "changed-identity"), h.accountID)
			case "proxy":
				_, err = h.store.ExecContext(ctx, `UPDATE accounts SET proxy_url=? WHERE id=?`, "http://127.0.0.1:1", h.accountID)
			}
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			event := gatewayExchange(t, conn, gatewayCreate([]any{gatewayUser("delta")}, id, ""))
			gatewayErrorCode(t, event)
			reportTestFailure(t, b.generations.Load() != 1 || b.connections.Load() != 1 || b.httpCalls.Load() != 0, func() {
				t.Fatalf("changed %s bypassed continuation validation", mutation)
			})
			gatewayAwait(t, func() bool { return h.dataPlane.accounts.Inflight(h.accountID) == 0 }, "rejected continuation leaked an account slot")
		})
	}
}

func TestGatewayContinuationDisconnectAndShutdownDoNotCommitCreated(t *testing.T) {
	for _, action := range []string{"disconnect", "shutdown", "max-age"} {
		t.Run(action, func(t *testing.T) { runGatewayContinuationDisconnectAndShutdownDoNotCommitCreatedCase(t, &action) })
	}
}

// Exercise the authentication -> key admission -> account wait -> upstream path,
// not just Manager.Acquire. A previously authenticated queued implicit turn must
// not survive revocation of the key that owns the existing socket.
func TestGatewayContinuationQueuedImplicitTurnRechecksKey(t *testing.T) {
	for _, mutation := range []string{"disable", "rotate", "delete"} {
		t.Run(mutation, func(t *testing.T) {
			b := newGatewayContinuationBackend()
			h := newGatewayContinuationHarness(t, b, nil)
			ctx := context.Background()
			one := 1
			{
				err := h.store.PatchAccountManagementFields(ctx, h.accountID, store.AccountManagementPatch{Concurrency: &one})
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			conn := dialGatewayContinuation(t, h, h.key)
			gatewayResponseID(t, gatewayExchange(t, conn, gatewayCreate([]any{gatewayUser("one")}, "", "")))
			gatewayObservedTurn(t, b)
			key, err := h.store.GetKeyByValue(ctx, h.key)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			gatewayAwait(t, func() bool { return h.dataPlane.limiter.Active(key.ID) == 0 }, "first turn did not release key admission")
			_, release, err := h.dataPlane.accounts.AcquireSpecific(ctx, h.accountID, "test-model")
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			defer release()
			{
				err := conn.WriteJSON(gatewayCreate([]any{gatewayUser("one"), gatewayUser("two")}, "", ""))
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			gatewayAwait(t, func() bool { return h.dataPlane.limiter.Active(key.ID) == 1 }, "queued turn did not pass authentication and key admission")
			switch mutation {
			case "disable":
				key.Enabled = false
				err = h.store.UpdateKey(ctx, key)
			case "rotate":
				_, err = h.store.ExecContext(ctx, `UPDATE api_keys SET key=? WHERE id=?`, "sk-rotated-while-queued", key.ID)
			case "delete":
				err = h.store.DeleteKey(ctx, key.ID)
			}
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			release()
			gatewayErrorCode(t, gatewayTerminal(t, conn))
			reportTestFailure(t, b.generations.Load() != 1 || b.connections.Load() != 1 || b.httpCalls.Load() != 0, func() {
				t.Fatalf("queued implicit continuation survived key %s", mutation)
			})
			gatewayAwait(t, func() bool {
				return h.dataPlane.limiter.Active(key.ID) == 0 && h.dataPlane.accounts.Inflight(h.accountID) == 0
			}, "rejected queued turn leaked key/account admission")
		})
	}
}

func TestGatewayContinuationBusyReconnectRevalidatesParent(t *testing.T) {
	b := newGatewayContinuationBackend()
	started := make(chan struct{})
	release := make(chan struct{})
	b.hook = func(_ *websocket.Conn, turn gatewayTurn) bool {
		if turn.id == "gateway-response-2" {
			close(started)
			<-release
		}
		return true
	}
	h := newGatewayContinuationHarness(t, b, nil)
	// Always release the fake upstream before harness cleanup, including failure.
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	first := dialGatewayContinuation(t, h, h.key)
	id := gatewayResponseID(t, gatewayExchange(t, first, gatewayCreate([]any{gatewayUser("one")}, "", "shared")))
	original := gatewayObservedTurn(t, b)
	{
		err := first.WriteJSON(gatewayCreate([]any{gatewayUser("two")}, id, "shared"))
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("second turn did not start")
	}
	second := gatewayObservedTurn(t, b)
	reportTestFailure(t, second.connection != original.connection, func() {
		t.Fatal("second turn changed socket")
	})
	reconnected := dialGatewayContinuation(t, h, h.key)
	{
		err := reconnected.WriteJSON(gatewayCreate([]any{gatewayUser("competing")}, id, "different"))
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	gatewayAwait(t, func() bool { return h.dataPlane.accounts.Inflight(h.accountID) == 2 }, "reconnect did not wait for original busy socket")
	reportTestFailure(t, b.connections.Load() != 1 || b.generations.Load() != 2, func() {
		t.Fatal("busy reconnect dialed a replacement or generated twice")
	})
	close(release)
	gatewayResponseID(t, gatewayTerminal(t, first))
	{
		code := gatewayErrorCode(t, gatewayTerminal(t, reconnected))
		reportTestFailure(t, code != "previous_response_not_found", func() {
			t.Fatalf("stale queued parent error = %s", code)
		})
	}
	reportTestFailure(t, b.generations.Load() != 2 || b.httpCalls.Load() != 0, func() {
		t.Fatal("queued stale parent was replayed")
	})
	key, err := h.store.GetKeyByValue(context.Background(), h.key)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	gatewayAwait(t, func() bool {
		return h.dataPlane.accounts.Inflight(h.accountID) == 0 && h.dataPlane.limiter.Active(key.ID) == 0
	}, "busy reconnect leaked reservations")
}

// runGatewayContinuationDisconnectAndShutdownDoNotCommitCreatedCase exercises one isolated test phase with the original shared state.
func runGatewayContinuationDisconnectAndShutdownDoNotCommitCreatedCase(t *testing.T, capturedAction *string) {
	t.Helper()

	b := newGatewayContinuationBackend()
	upstreamClosed := make(chan struct{})
	b.hook = func(conn *websocket.Conn, turn gatewayTurn) bool {
		defer close(upstreamClosed)
		_ = conn.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": turn.id}})
		_, _, _ = conn.ReadMessage()
		return false
	}
	memory := session.NewMemory(session.Options{})
	defer memory.Close()
	h := newGatewayContinuationHarness(t, b, memory)
	if (*capturedAction) == "max-age" {
		// Compile/install rules before the short connection timer starts.
		// Under -race this cold setup can exceed the entire test lifetime;
		// this case must expire an active created response, not its setup.
		{
			_, _, err := h.dataPlane.ruleService.Load(t.Context())
			reportTestFailure(t, err != nil, func() {
				t.Fatalf("initialize rules before connection expiry: %v", err)
			})
		}
		h.dataPlane.wsMaxAge = time.Second
	}
	conn := dialGatewayContinuation(t, h, h.key)
	{
		err := conn.WriteJSON(gatewayCreate([]any{gatewayUser("one")}, "", ""))
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var created map[string]any
	{
		err := conn.ReadJSON(&created)
		reportTestFailure(t, err != nil || created["type"] != "response.created", func() {
			t.Fatalf("created: %v %v", created, err)
		})
	}
	turn := gatewayObservedTurn(t, b)
	if (*capturedAction) == "max-age" {
		var expired map[string]any
		{
			err := conn.ReadJSON(&expired)
			reportTestFailure(t, err != nil || gatewayErrorCode(t, expired) != "websocket_connection_limit_reached", func() {
				t.Fatalf("maximum-age notice: %v %v", expired, err)
			})
		}
	}
	if (*capturedAction) == "disconnect" {
		_ = conn.Close()
	}
	if (*capturedAction) == "shutdown" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		{
			err := h.dataPlane.Shutdown(ctx)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
	}
	select {
	case <-upstreamClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("downstream termination did not cancel upstream")
	}
	gatewayAwait(t, func() bool {
		h.dataPlane.lifecycle.mu.Lock()
		active := len(h.dataPlane.lifecycle.active)
		h.dataPlane.lifecycle.mu.Unlock()
		return active == 0 && h.dataPlane.accounts.Inflight(h.accountID) == 0
	}, "terminated downstream leaked a handler or account reservation")
	key, err := h.store.GetKeyByValue(context.Background(), h.key)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	{
		got := h.dataPlane.limiter.Active(key.ID)
		reportTestFailure(t, got != 0, func() {
			t.Fatalf("terminated downstream leaked %d key reservations", got)
		})
	}
	{
		_, err := memory.Resolve(context.Background(), key.ID, turn.id)
		reportTestFailure(t, !errors.Is(err, session.ErrMiss), func() {
			t.Fatalf("created-only response committed: %v", err)
		})
	}
	{
		_, err := h.dataPlane.upstream.ResolveContinuation(context.Background(), key.ID, turn.id)
		reportTestFailure(t, !errors.Is(err, upstream.ErrContinuationUnavailable), func() {
			t.Fatalf("created-only local state is resumable: %v", err)
		})
	}
	reportTestFailure(t, b.generations.Load() != 1 || b.httpCalls.Load() != 0, func() {
		t.Fatal("interrupted generation was replayed")
	})
}
