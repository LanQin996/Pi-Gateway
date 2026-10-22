package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pi-gateway/internal/codexbridge"
	"pi-gateway/internal/session"

	"github.com/gorilla/websocket"
)

type observedRequest struct {
	connection int64
	body       map[string]any
}
type continuationFixture struct {
	server    *httptest.Server
	dials     atomic.Int64
	requests  atomic.Int64
	httpCalls atomic.Int64
	frames    chan observedRequest
	hook      func(*websocket.Conn, int64, int64) bool
}

func newContinuationFixture(t *testing.T, hook func(*websocket.Conn, int64, int64) bool) *continuationFixture {
	t.Helper()
	f := &continuationFixture{frames: make(chan observedRequest, 64), hook: hook}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveNewContinuationFixtureRequest(w, r, &f) }))
	t.Cleanup(f.server.Close)
	return f
}
func continuationRequest(key int64, scope string) *Request {
	return &Request{ClientKeyID: key, AccountID: 7, PoolSessionID: scope, SessionID: "wire-only", Transport: "websocket-cached", WSHeaders: http.Header{"Authorization": []string{"Bearer token-one"}}, Body: []byte(`{"model":"m","input":["one"]}`)}
}
func explicitRequest(t *testing.T, req *Request, id string, input []any) *Request {
	t.Helper()
	r := *req
	r.PoolSessionID = "new-downstream"
	r.PreviousResponseID = id
	r.Body = mustJSON(t, map[string]any{"model": "m", "input": input, "previous_response_id": id})
	return &r
}
func runContinuation(t *testing.T, c *Client, req *Request) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	{
		_, err := c.Stream(ctx, req, func(*Event) error { return nil })
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
}

type continuationStore struct {
	session.Store
	mu            sync.Mutex
	records       map[string]session.Record
	readErr       error
	writeErr      error
	writes        atomic.Int64
	reads         atomic.Int64
	hits          atomic.Int64
	blockFirst    <-chan struct{}
	firstReturned chan struct{}
}

func (s *continuationStore) Resolve(_ context.Context, key int64, id string) (*session.Record, error) {
	s.reads.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return nil, s.readErr
	}
	r, ok := s.records[id]
	if !ok {
		return nil, session.ErrMiss
	}
	if r.KeyID != key {
		return nil, session.ErrWrongOwner
	}
	s.hits.Add(1)
	return &r, nil
}
func (s *continuationStore) RecordCompleted(_ context.Context, r session.Record) error {
	n := s.writes.Add(1)
	if n == 1 && s.blockFirst != nil {
		<-s.blockFirst
		defer close(s.firstReturned)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	if s.records == nil {
		s.records = make(map[string]session.Record)
	}
	s.records[r.ResponseID] = r
	return nil
}

func TestContinuationCodexBridgeProfileIsolation(t *testing.T) {
	for _, initial := range []codexbridge.Profile{{}, {Version: 1, Tools: codexbridge.ToolSendMessage}} {
		t.Run(fmt.Sprintf("initial=%v", initial), func(t *testing.T) {
			f := newContinuationFixture(t, nil)
			c := testClient(t, f.server.URL)
			store := session.NewMemory(session.Options{})
			defer store.Close()
			c.cfg.Sessions = store
			req := continuationRequest(1, "profile-scope")
			req.CodexBridge = initial
			runContinuation(t, c, req)
			<-f.frames
			record, err := c.ResolveContinuation(context.Background(), 1, "r1")
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			shared, err := store.Resolve(context.Background(), 1, "r1")
			reportTestFailure(t, err != nil || shared.CodexBridge != initial || record.CodexBridge != initial, func() {
				t.Fatalf("profile not retained: local=%#v shared=%#v err=%v", record, shared, err)
			})
			next := explicitRequest(t, req, "r1", []any{"delta"})
			next.Continuation = record
			next.CodexBridge = codexbridge.Profile{Version: 1, Tools: codexbridge.ToolSpawnAgent}
			{
				_, err := c.Stream(context.Background(), next, func(*Event) error { return nil })
				reportTestFailure(t, !errors.Is(err, ErrContinuationUnavailable), func() {
					t.Fatalf("cross-profile explicit continuation = %v", err)
				})
			}
			forged := *record
			forged.CodexBridge = next.CodexBridge
			reportTestFailure(t, c.pool.matches(&forged), func() {
				t.Fatal("forged pool profile matched local record")
			})
			next.Continuation = &forged
			{
				_, err := c.Stream(context.Background(), next, func(*Event) error { return nil })
				reportTestFailure(t, !errors.Is(err, ErrContinuationUnavailable), func() {
					t.Fatalf("forged-profile continuation = %v", err)
				})
			}
			reportTestFailure(t, f.requests.Load() != 1, func() {
				t.Fatal("mismatched explicit delta reached upstream")
			})
			// An implicit profile change falls back to full input on the same socket.
			req.CodexBridge = next.CodexBridge
			req.Body = []byte(`{"model":"m","input":["one","answer","new"]}`)
			runContinuation(t, c, req)
			frame := <-f.frames
			{
				_, ok := frame.body["previous_response_id"]
				reportTestFailure(t, ok, func() {
					t.Fatal("profile change used cached delta")
				})
			}
			reportTestFailure(t, !reflect.DeepEqual(frame.body["input"], []any{"one", "answer", "new"}) || f.dials.Load() != 1, func() {
				t.Fatalf("profile fallback did not preserve full input/socket: %+v", frame)
			})
			// Matching profile with no tools is valid for an explicit delta, and
			// must not strip a prefix even if it resembles cached full history.
			delta := []any{"one", "answer", "new", "explicit"}
			same := explicitRequest(t, req, "r2", delta)
			runContinuation(t, c, same)
			explicitFrame := <-f.frames
			reportTestFailure(t, explicitFrame.body["previous_response_id"] != "r2" || !reflect.DeepEqual(explicitFrame.body["input"], delta), func() {
				t.Fatalf("same-profile explicit delta was changed: %+v", explicitFrame)
			})
			{
				_, ok := explicitFrame.body["tools"]
				reportTestFailure(t, ok, func() {
					t.Fatal("explicit delta unexpectedly acquired tool definitions")
				})
			}
		})
	}
}

func TestContinuationRejectsInvalidCodexBridgeProfile(t *testing.T) {
	f := newContinuationFixture(t, nil)
	c := testClient(t, f.server.URL)
	req := continuationRequest(1, "invalid-profile")
	req.CodexBridge = codexbridge.Profile{Version: 99}
	{
		_, err := c.Stream(context.Background(), req, func(*Event) error { return nil })
		reportTestFailure(t, !errors.Is(err, session.ErrInvalid), func() {
			t.Fatalf("invalid request profile = %v", err)
		})
	}
	reportTestFailure(t, f.dials.Load() != 0 || f.requests.Load() != 0, func() {
		t.Fatal("invalid profile reached upstream")
	})
	req.CodexBridge = codexbridge.Profile{}
	runContinuation(t, c, req)
	<-f.frames
	record, err := c.ResolveContinuation(context.Background(), 1, "r1")
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	record.CodexBridge = codexbridge.Profile{Version: 99}
	reportTestFailure(t, c.validRecord(record, 1, "r1"), func() {
		t.Fatal("invalid persisted profile accepted")
	})
	c.pool.mu.Lock()
	c.pool.entries[record.SessionID].record = record
	c.pool.mu.Unlock()
	{
		_, err := c.ResolveContinuation(context.Background(), 1, "r1")
		reportTestFailure(t, !errors.Is(err, session.ErrInvalid), func() {
			t.Fatalf("invalid local profile = %v", err)
		})
	}
}

func TestContinuationCodexBridgeClientReplayKeepsRawBaseline(t *testing.T) {
	profile := codexbridge.Profile{Version: 1, Tools: codexbridge.ToolSendMessage}
	rawCall := map[string]any{"type": "function_call", "id": "fc1", "call_id": "call-1", "name": codexbridge.Alias + ".send_message", "arguments": `{"id":"child","message":"complete task"}`}
	frames := make(chan map[string]any, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveContinuationCodexBridgeClientReplayKeepsRawBaselineRequest(w, r, &rawCall, &frames)
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	req := continuationRequest(1, "client-replay")
	req.CodexBridge = profile
	var clientCall map[string]any
	_, err := c.Stream(context.Background(), req, func(event *Event) error {
		restored, _, err := codexbridge.RestoreEvent(event.Data, profile)
		if err != nil {
			return err
		}
		// API delivery replaces Data; ws.go still reads its original decoded map.
		event.Data = restored
		clientCall = restored["response"].(map[string]any)["output"].([]any)[0].(map[string]any)
		return nil
	})
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	<-frames
	reportTestFailure(t, clientCall["name"] != "send_message" || clientCall["namespace"] != "collaboration", func() {
		t.Fatalf("client identity was not restored: %#v", clientCall)
	})
	{
		marker, ok := clientCall["encrypted_function_args"].([]any)
		reportTestFailure(t, !ok || len(marker) != 0, func() {
			t.Fatal("client did not receive empty plaintext marker")
		})
	}
	record, err := c.ResolveContinuation(context.Background(), 1, "r1")
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	c.pool.mu.Lock()
	cont := c.pool.entries[record.SessionID].continuation
	encodedOutput := append([]byte(nil), cont.encodedResponseItems...)
	c.pool.mu.Unlock()
	var baseline []any
	{
		err := json.Unmarshal(encodedOutput, &baseline)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, !jsonEqual(baseline, []any{rawCall}), func() {
		t.Fatalf("callback restoration polluted raw baseline: %s", encodedOutput)
	})
	// The API forwards the client's restored replay before upstream comparison.
	replayed := map[string]any{"model": "m", "input": []any{"one", clientCall, map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "delivered"}}}
	forwarded, _, err := codexbridge.Forward(replayed, profile)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	req.Body = mustJSON(t, forwarded)
	runContinuation(t, c, req)
	frame := <-frames
	reportTestFailure(t, frame["previous_response_id"] != "r1", func() {
		t.Fatalf("actual client replay did not use cached delta: %#v", frame)
	})
	items, _ := frame["input"].([]any)
	reportTestFailure(t, len(items) != 1 || items[0].(map[string]any)["type"] != "function_call_output", func() {
		t.Fatalf("replay delta = %#v", items)
	})
}

func TestContinuationKeyIsolationAndReconnectExplicitDelta(t *testing.T) {
	f := newContinuationFixture(t, nil)
	c := testClient(t, f.server.URL)
	req := continuationRequest(1, "same")
	runContinuation(t, c, req)
	first := <-f.frames
	record, err := c.ResolveContinuation(context.Background(), 1, "r1")
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	other := continuationRequest(2, "same")
	runContinuation(t, c, other)
	second := <-f.frames
	reportTestFailure(t, first.connection == second.connection, func() {
		t.Fatal("different owners shared a socket")
	})
	{
		_, err := c.ResolveContinuation(context.Background(), 2, "r1")
		reportTestFailure(t, !errors.Is(err, ErrContinuationUnavailable), func() {
			t.Fatalf("cross-key resolve=%v", err)
		})
	}
	bad := explicitRequest(t, other, "r1", []any{"delta"})
	bad.Continuation = record
	{
		_, err := c.Stream(context.Background(), bad, func(*Event) error { return nil })
		reportTestFailure(t, !errors.Is(err, ErrContinuationUnavailable), func() {
			t.Fatalf("cross-key continuation=%v", err)
		})
	}
	// Matching prefix in an explicit delta must NOT be stripped again.
	delta := []any{"one", "answer", "new"}
	next := explicitRequest(t, req, "r1", delta)
	next.Transport = "sse"
	runContinuation(t, c, next)
	third := <-f.frames
	reportTestFailure(t, third.connection != first.connection || !reflect.DeepEqual(third.body["input"], delta), func() {
		t.Fatalf("explicit reconnect=%+v", third)
	})
	reportTestFailure(t, third.body["previous_response_id"] != "r1" || f.httpCalls.Load() != 0 || f.dials.Load() != 2, func() {
		t.Fatal("explicit parent changed transport or socket")
	})
	{
		_, ok := third.body["pool_session_id"]
		reportTestFailure(t, ok, func() {
			t.Fatal("internal scope leaked")
		})
	}
}

func TestContinuationImplicitPrefixAndParameterChanges(t *testing.T) {
	for _, test := range []struct {
		name       string
		body       string
		wantParent bool
		want       []any
	}{
		{"extension", `{"model":"m","input":["one","answer","two"]}`, true, []any{"two"}},
		{"prefix", `{"model":"m","input":["changed","answer","two"]}`, false, []any{"changed", "answer", "two"}},
		{"model", `{"model":"other","input":["one","answer","two"]}`, false, []any{"one", "answer", "two"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newContinuationFixture(t, nil)
			c := testClient(t, f.server.URL)
			req := continuationRequest(1, "scope")
			runContinuation(t, c, req)
			<-f.frames
			req.Body = []byte(test.body)
			runContinuation(t, c, req)
			frame := <-f.frames
			_, parent := frame.body["previous_response_id"]
			reportTestFailure(t, parent != test.wantParent || !reflect.DeepEqual(frame.body["input"], test.want), func() {
				t.Fatalf("bad delta: %+v", frame.body)
			})
			reportTestFailure(t, f.dials.Load() != 1, func() {
				t.Fatal("history comparison unexpectedly redialed")
			})
		})
	}
}

func TestContinuationRejectsIdentityChangesAndOldOrDeadSocket(t *testing.T) {
	for _, kind := range []string{"token", "proxy", "url", "account", "owner", "instance", "old-parent", "dead", "expired", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			f := newContinuationFixture(t, nil)
			c := testClient(t, f.server.URL)
			req := continuationRequest(1, "scope")
			runContinuation(t, c, req)
			<-f.frames
			record, err := c.ResolveContinuation(context.Background(), 1, "r1")
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			next := explicitRequest(t, req, "r1", []any{"delta"})
			next.Continuation = record
			next.Transport = "auto"
			switch kind {
			case "token":
				next.WSHeaders = http.Header{"Authorization": []string{"Bearer token-two"}}
			case "proxy":
				next.ProxyURL = "http://127.0.0.1:1"
			case "url":
				c.cfg.WSURL += "/other"
			case "account":
				next.AccountID++
			case "owner":
				next.ClientKeyID++
			case "instance":
				next.Continuation.InstanceID = "another-instance"
			case "old-parent":
				runContinuation(t, c, req)
				<-f.frames
			case "dead":
				c.pool.mu.Lock()
				conn := c.pool.entries[record.SessionID]
				c.pool.removeLocked(record.SessionID, conn)
				c.pool.mu.Unlock()
			case "expired":
				next.Continuation.ExpiresAt = time.Now().Add(-time.Second)
			case "unknown":
				next = explicitRequest(t, req, "unknown", []any{"delta"})
			}
			count := f.requests.Load()
			{
				_, err := c.Stream(context.Background(), next, func(*Event) error { return nil })
				reportTestFailure(t, !errors.Is(err, ErrContinuationUnavailable), func() {
					t.Fatalf("error=%v", err)
				})
			}
			reportTestFailure(t, f.requests.Load() != count || f.httpCalls.Load() != 0 || f.dials.Load() != 1, func() {
				t.Fatal("rejected parent was sent/retried")
			})
		})
	}
}

func TestContinuationBusyWaiterRevalidatesLatestParent(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{})
	f := newContinuationFixture(t, func(_ *websocket.Conn, _, n int64) bool {
		if n == 2 {
			close(started)
			<-gate
		}
		return true
	})
	c := testClient(t, f.server.URL)
	req := continuationRequest(1, "scope")
	runContinuation(t, c, req)
	<-f.frames
	first := make(chan error, 1)
	go func() {
		_, err := c.Stream(context.Background(), explicitRequest(t, req, "r1", []any{"first"}), func(*Event) error { return nil })
		first <- err
	}()
	<-started
	stale := explicitRequest(t, req, "r1", []any{"stale"})
	record, err := c.ResolveContinuation(context.Background(), 1, "r1")
	if err != nil {
		close(gate)
		t.Fatal(err)
	}
	stale.Continuation = record
	waiter := make(chan error, 1)
	go func() {
		_, err := c.Stream(context.Background(), stale, func(*Event) error { return nil })
		waiter <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		c.pool.mu.Lock()
		slot := c.pool.slots[record.SessionID]
		queued := slot != nil && len(slot.waiters) == 1
		c.pool.mu.Unlock()
		if queued {
			break
		}
		if time.Now().After(deadline) {
			close(gate)
			t.Fatal("waiter not queued")
		}
		time.Sleep(time.Millisecond)
	}
	close(gate)
	{
		err := <-first
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	{
		err := <-waiter
		reportTestFailure(t, !errors.Is(err, ErrContinuationUnavailable), func() {
			t.Fatalf("stale waiter=%v", err)
		})
	}
	reportTestFailure(t, f.requests.Load() != 2 || f.dials.Load() != 1, func() {
		t.Fatal("busy waiter created/replayed a request")
	})
}

func TestContinuationSharedFailureAndLateCommitDoNotRevokeSuccess(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprint(late), func(t *testing.T) {
			release := make(chan struct{})
			store := &continuationStore{readErr: session.ErrUnavailable, writeErr: session.ErrUnavailable}
			if late {
				store.writeErr = nil
				store.blockFirst = release
				store.firstReturned = make(chan struct{})
			}
			f := newContinuationFixture(t, nil)
			c := testClient(t, f.server.URL)
			c.cfg.Sessions = store
			c.cfg.SessionCommitTimeout = 25 * time.Millisecond
			req := continuationRequest(1, "scope")
			started := time.Now()
			runContinuation(t, c, req)
			<-f.frames
			reportTestFailure(t, time.Since(started) > time.Second, func() {
				t.Fatal("shared commit exceeded request bound")
			})
			record, err := c.ResolveContinuation(context.Background(), 1, "r1")
			reportTestFailure(t, err != nil, func() {
				t.Fatalf("trusted local fallback=%v", err)
			})
			next := explicitRequest(t, req, "r1", []any{"two"})
			next.Continuation = record
			runContinuation(t, c, next)
			<-f.frames
			if late {
				close(release)
				<-store.firstReturned
				store.mu.Lock()
				store.readErr = nil // Redis recovered, and the late record is readable.
				store.mu.Unlock()
				{
					r, err := store.Resolve(context.Background(), 1, "r1")
					reportTestFailure(t, err != nil || r.ResponseID != "r1", func() {
						t.Fatalf("late shared record was not readable: record=%v err=%v", r, err)
					})
				}
			}
			hits := store.hits.Load()
			{
				_, err := c.ResolveContinuation(context.Background(), 1, "r2")
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			{
				_, err := c.Stream(context.Background(), explicitRequest(t, req, "r1", []any{"stale"}), func(*Event) error { return nil })
				reportTestFailure(t, !errors.Is(err, ErrContinuationUnavailable), func() {
					t.Fatalf("late old write revived parent: %v", err)
				})
			}
			reportTestFailure(t, late && store.hits.Load() != hits+1, func() {
				t.Fatal("old parent rejection did not read the recovered shared record")
			})
			reportTestFailure(t, f.requests.Load() != 2 || f.dials.Load() != 1 || f.httpCalls.Load() != 0, func() {
				t.Fatal("shared failure repeated generation")
			})
		})
	}
}

func TestContinuationCommitOrderingAndCallbackFailure(t *testing.T) {
	f := newContinuationFixture(t, nil)
	c := testClient(t, f.server.URL)
	store := &continuationStore{}
	c.cfg.Sessions = store
	expected := errors.New("downstream closed")
	_, err := c.Stream(context.Background(), continuationRequest(1, "scope"), func(e *Event) error {
		if IsTerminal(e.Type) {
			reportTestFailure(t, store.writes.Load() != 1, func() {
				t.Error("terminal delivered before bounded commit")
			})
			{
				_, err := c.ResolveContinuation(context.Background(), 1, "r1")
				reportTestFailure(t, !errors.Is(err, ErrContinuationUnavailable), func() {
					t.Error("local state advanced before delivery")
				})
			}
			return expected
		}
		return nil
	})
	reportTestFailure(t, !errors.Is(err, expected), func() {
		t.Fatalf("callback=%v", err)
	})
	{
		_, err := c.ResolveContinuation(context.Background(), 1, "r1")
		reportTestFailure(t, !errors.Is(err, ErrContinuationUnavailable), func() {
			t.Fatal("undelivered result became resumable")
		})
	}
	reportTestFailure(t, c.PoolSize() != 0, func() {
		t.Fatal("failed callback retained socket")
	})
}

func TestContinuationNonSuccessfulTerminalNeverReady(t *testing.T) {
	for _, typ := range []string{"response.created", "response.incomplete", "response.failed", "response.done"} {
		t.Run(typ, func(t *testing.T) {
			f := newContinuationFixture(t, func(ws *websocket.Conn, _, _ int64) bool {
				_ = ws.WriteJSON(map[string]any{"type": typ, "response": map[string]any{"id": "not-ready", "status": "incomplete"}})
				return false
			})
			c := testClient(t, f.server.URL)
			store := &continuationStore{}
			c.cfg.Sessions = store
			_, _ = c.Stream(context.Background(), continuationRequest(1, "scope"), func(*Event) error { return nil })
			reportTestFailure(t, store.writes.Load() != 0, func() {
				t.Fatal("non-completed record committed")
			})
			{
				_, err := c.ResolveContinuation(context.Background(), 1, "not-ready")
				reportTestFailure(t, !errors.Is(err, ErrContinuationUnavailable), func() {
					t.Fatalf("non-completed resolve=%v", err)
				})
			}
			reportTestFailure(t, c.PoolSize() != 0, func() {
				t.Fatal("non-success socket retained")
			})
		})
	}
}

func TestContinuationBaselineLimitsDisableOptimizationWithoutTruncation(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(fmt.Sprint(global), func(t *testing.T) {
			f := newContinuationFixture(t, nil)
			c := testClient(t, f.server.URL)
			req := continuationRequest(1, "one")
			if global {
				c.pool.maxTotalBaselineBytes = 40
				c.cfg.MaxTotalBaselineBytes = 40
			} else {
				c.pool.maxBaselineBytes = 20
				c.cfg.MaxBaselineBytes = 20
			}
			runContinuation(t, c, req)
			<-f.frames
			if global {
				req = continuationRequest(1, "two")
				runContinuation(t, c, req)
				<-f.frames
			}
			req.Body = []byte(`{"model":"m","input":["one","answer","two"]}`)
			runContinuation(t, c, req)
			frame := <-f.frames
			{
				_, ok := frame.body["previous_response_id"]
				reportTestFailure(t, ok, func() {
					t.Fatal("oversize baseline was truncated and reused")
				})
			}
			c.pool.mu.Lock()
			total := c.pool.totalBaselineBytes
			limit := c.pool.maxTotalBaselineBytes
			c.pool.mu.Unlock()
			reportTestFailure(t, total > limit, func() {
				t.Fatalf("total baseline=%d > %d", total, limit)
			})
			{
				_, err := c.ResolveContinuation(context.Background(), 1, fmt.Sprintf("r%d", f.requests.Load()))
				reportTestFailure(t, err != nil, func() {
					t.Fatal("baseline limit disabled legal explicit metadata")
				})
			}
		})
	}
}

func TestWSActiveAgeLimitAndBusyCapacity(t *testing.T) {
	for _, age := range []bool{false, true} {
		t.Run(fmt.Sprint(age), func(t *testing.T) { runWSActiveAgeLimitAndBusyCapacityCase(t, &age) })
	}
}

func TestPoolIdentityDoesNotExposeSecretsAndZeroKeyDoesNotReuse(t *testing.T) {
	c := testClient(t, "http://127.0.0.1")
	req := continuationRequest(1, "scope")
	req.ProxyURL = "http://user:private-password@localhost"
	key := c.requestPoolKey(req)
	reportTestFailure(t, strings.Contains(key, "private") || strings.Contains(key, "token") || strings.Contains(key, "scope"), func() {
		t.Fatal("pool key leaks raw identity")
	})
	req.ClientKeyID = 0
	firstAnonymousKey := c.requestPoolKey(req)
	secondAnonymousKey := c.requestPoolKey(req)
	reportTestFailure(t, firstAnonymousKey == secondAnonymousKey, func() {
		t.Fatal("anonymous key shared pool lane")
	})
}

// serveNewContinuationFixtureRequest exercises one isolated test phase with the original shared state.
func serveNewContinuationFixtureRequest(w http.ResponseWriter, r *http.Request, capturedF **continuationFixture) {
	if !websocket.IsWebSocketUpgrade(r) {
		(*capturedF).httpCalls.Add(1)
		http.Error(w, "no SSE expected", 500)
		return
	}
	ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	connection := (*capturedF).dials.Add(1)
	for {
		_, body, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var frame map[string]any
		if json.Unmarshal(body, &frame) != nil {
			return
		}
		n := (*capturedF).requests.Add(1)
		(*capturedF).frames <- observedRequest{connection, frame}
		if (*capturedF).hook != nil && !(*capturedF).hook(ws, connection, n) {
			return
		}
		if err := ws.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("r%d", n), "status": "completed", "output": []any{"answer"}}}); err != nil {
			return
		}
	}
}

// serveContinuationCodexBridgeClientReplayKeepsRawBaselineRequest exercises one isolated test phase with the original shared state.
func serveContinuationCodexBridgeClientReplayKeepsRawBaselineRequest(w http.ResponseWriter, r *http.Request, capturedRawCall *map[string]any, capturedFrames *chan map[string]any) {
	ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	for n := 1; ; n++ {
		_, body, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var frame map[string]any
		if json.Unmarshal(body, &frame) != nil {
			return
		}
		(*capturedFrames) <- frame
		if err := ws.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("r%d", n), "status": "completed", "output": []any{(*capturedRawCall)}}}); err != nil {
			return
		}
	}
}

// runWSActiveAgeLimitAndBusyCapacityCase exercises one isolated test phase with the original shared state.
func runWSActiveAgeLimitAndBusyCapacityCase(t *testing.T, capturedAge *bool) {
	t.Helper()

	entered := make(chan struct{})
	release := make(chan struct{})
	f := newContinuationFixture(t, func(ws *websocket.Conn, ignoredParameter1_0, n int64) bool {
		return verifyWSActiveAgeLimitAndBusyCapacity(ws, ignoredParameter1_0, n, &capturedAge, &entered, &release)
	})
	c := testClient(t, f.server.URL)
	c.pool.maxConnections = 1
	if *capturedAge {
		c.pool.maxAge = 60 * time.Millisecond
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := c.Stream(ctx, continuationRequest(1, "busy"), func(*Event) error { return nil })
		result <- err
	}()
	<-entered
	if !(*capturedAge) {
		_, err := c.Stream(context.Background(), continuationRequest(2, "other"), func(*Event) error { return nil })
		if !errors.Is(err, ErrPoolBusy) {
			close(release)
			t.Fatalf("capacity=%v", err)
		}
		if f.dials.Load() != 1 {
			close(release)
			t.Fatal("capacity dialed second socket")
		}
		cancel()
	}
	select {
	case err := <-result:
		reportTestFailure(t, (*capturedAge) && !errors.Is(err, ErrWSMaxAge), func() {
			t.Fatalf("active age=%v", err)
		})
		reportTestFailure(t, !(*capturedAge) && !errors.Is(err, context.Canceled), func() {
			t.Fatalf("cancel=%v", err)
		})
	case <-time.After(time.Second):
		close(release)
		t.Fatal("active request not interrupted")
	}
	close(release)
	c.pool.mu.Lock()
	entries, slots, reservations := len(c.pool.entries), len(c.pool.slots), c.pool.reservations
	c.pool.mu.Unlock()
	reportTestFailure(t, entries != 0 || slots != 0 || reservations != 0, func() {
		t.Fatalf("resources leaked: %d %d %d", entries, slots, reservations)
	})
}

// verifyWSActiveAgeLimitAndBusyCapacity exercises one isolated test phase with the original shared state.
func verifyWSActiveAgeLimitAndBusyCapacity(ws *websocket.Conn, ignoredParameter1_0, n int64, capturedCapturedAge **bool, capturedEntered *chan struct{}, capturedRelease *chan struct{}) bool {
	if n == 1 {
		close((*capturedEntered))
		if *(*capturedCapturedAge) {
			for {
				if err := ws.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "x"}); err != nil {
					return false
				}
				select {
				case <-(*capturedRelease):
					return false
				case <-time.After(5 * time.Millisecond):
				}
			}
		}
		<-(*capturedRelease)
	}
	return true
}
