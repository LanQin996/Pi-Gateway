package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pi-gateway/internal/egress"

	"github.com/gorilla/websocket"
)

func testClient(t *testing.T, url string) *Client {
	t.Helper()
	c := New(Config{SSEURL: url, WSURL: "ws" + strings.TrimPrefix(url, "http"), Pool: true, IdleTimeout: time.Second}, egress.NewFactory(egress.Options{}))
	t.Cleanup(c.Close)
	return c
}
func TestSSEIdleTimeoutAfterFirstEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\"}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	c := New(Config{SSEURL: server.URL, IdleTimeout: 60 * time.Millisecond}, egress.NewFactory(egress.Options{}))
	defer c.Close()
	started := time.Now()
	events := 0
	_, err := c.Stream(context.Background(), &Request{Transport: "sse", Body: []byte(`{}`)}, func(*Event) error {
		events++
		return nil
	})
	reportTestFailure(t, !IsIdleTimeoutError(err) || !errors.Is(err, ErrIdleTimeout), func() {
		t.Fatalf("expected identifiable idle timeout, got %v", err)
	})
	reportTestFailure(t, events != 1, func() {
		t.Fatalf("events=%d want 1", events)
	})
	{
		elapsed := time.Since(started)
		reportTestFailure(t, elapsed > time.Second, func() {
			t.Fatalf("idle timeout took too long: %s", elapsed)
		})
	}
}

func TestSSEResponseHeaderTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	const timeout = 50 * time.Millisecond
	c := New(Config{SSEURL: server.URL, IdleTimeout: time.Second}, egress.NewFactory(egress.Options{
		ConnectTimeout:        time.Second,
		ResponseHeaderTimeout: timeout,
	}))
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	_, err := c.Stream(ctx, &Request{Transport: "sse", Body: []byte(`{}`)}, func(*Event) error { return nil })
	reportTestFailure(t, ctx.Err() != nil || err == nil || !strings.Contains(err.Error(), "timeout awaiting response headers"), func() {
		t.Fatalf("expected response header timeout, got %v", err)
	})
	var netErr net.Error
	reportTestFailure(t, !errors.As(err, &netErr) || !netErr.Timeout(), func() {
		t.Fatalf("expected net timeout error, got %T: %v", err, err)
	})
	{
		elapsed := time.Since(started)
		reportTestFailure(t, elapsed > time.Second, func() {
			t.Fatalf("response header timeout took too long: %s", elapsed)
		})
	}
}

func TestWSIdleTimeoutDoesNotFallbackToSSE(t *testing.T) {
	var creates, posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"response.completed\"}\n\n")
			return
		}
		socket, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer socket.Close()
		if _, _, err := socket.ReadMessage(); err != nil {
			return
		}
		creates.Add(1)
		_ = socket.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, _ = socket.ReadMessage() // Wait for the client's timeout to close the socket.
	}))
	defer server.Close()
	c := New(Config{SSEURL: server.URL, WSURL: "ws" + strings.TrimPrefix(server.URL, "http"), IdleTimeout: 50 * time.Millisecond}, egress.NewFactory(egress.Options{}))
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	events := 0
	_, err := c.Stream(ctx, &Request{Transport: "auto", Body: []byte(`{}`)}, func(*Event) error { events++; return nil })
	reportTestFailure(t, !IsIdleTimeoutError(err) || !errors.Is(err, ErrIdleTimeout) || events != 0 || time.Since(started) > time.Second, func() {
		t.Fatalf("expected prompt idle timeout before any event, got %v events=%d", err, events)
	})
	var te *transportError
	reportTestFailure(t, !errors.As(err, &te) || te.requestNotSent, func() {
		t.Fatalf("sent request was not recorded: %v", err)
	})
	reportTestFailure(t, creates.Load() != 1 || posts.Load() != 0, func() {
		t.Fatalf("auto retried or fell back: websocket=%d sse=%d", creates.Load(), posts.Load())
	})
}

func TestWSHandshakeFailurePreservesFailureErrorAndFallbackPolicy(t *testing.T) {
	for _, transport := range []string{"websocket", "auto"} {
		for _, status := range []int{200, 302, 400, 401, 403, 404, 426, 429, 500, 502, 503, 599} {
			t.Run(fmt.Sprintf("%s/%d", transport, status), func(t *testing.T) {
				runWSHandshakeFailurePreservesFailureErrorAndFallbackPolicyCase(t, &transport, &status)
			})
		}
	}
}

func TestWSSentBeforeFirstEventDoesNotFallback(t *testing.T) {
	for _, mode := range []string{"close", "invalid-json"} {
		t.Run(mode, func(t *testing.T) { runWSSentBeforeFirstEventDoesNotFallbackCase(t, &mode) })
	}
}

func TestWSConnectionFailureFallbackPreservesFinalError(t *testing.T) {
	for _, failSSE := range []bool{false, true} {
		t.Run(fmt.Sprint(failSSE), func(t *testing.T) { runWSConnectionFailureFallbackPreservesFinalErrorCase(t, &failSSE) })
	}
}

type failedWSWriteConn struct {
	net.Conn
	partial bool
	err     error
}

func (c *failedWSWriteConn) Write(p []byte) (int, error) {
	if len(p) > 0 && p[0] == 0x81 { // First (text) response.create frame, not the HTTP handshake.
		if c.partial {
			n, err := c.Conn.Write(p[:1])
			if err != nil {
				return n, err
			}
			return n, c.err
		}
		return 0, c.err
	}
	return c.Conn.Write(p)
}

func TestWSWriteFailureDoesNotFallback(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			var handshakes, posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					fmt.Fprint(w, "data: {\"type\":\"response.completed\"}\n\n")
					return
				}
				handshakes.Add(1)
				socket, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer socket.Close()
				_ = socket.SetReadDeadline(time.Now().Add(2 * time.Second))
				_, _, _ = socket.ReadMessage()
			}))
			defer server.Close()
			injected := &net.OpError{Op: "write", Net: "tcp", Err: errors.New("injected write failure")}
			factory := egress.NewFactory(egress.Options{})
			factory.BaseDialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				return &failedWSWriteConn{Conn: conn, partial: partial, err: injected}, nil
			}
			c := New(Config{SSEURL: server.URL, WSURL: "ws" + strings.TrimPrefix(server.URL, "http")}, factory)
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err := c.Stream(ctx, &Request{Transport: "auto", Body: []byte(`{}`)}, func(*Event) error { return nil })
			var te *transportError
			reportTestFailure(t, !errors.Is(err, injected) || !errors.As(err, &te) || te.requestNotSent || handshakes.Load() != 1 || posts.Load() != 0, func() {
				t.Fatalf("write failure replayed: err=%v handshakes=%d posts=%d", err, handshakes.Load(), posts.Load())
			})
		})
	}
}

func TestWSFallbackRequiresProofRequestNotSent(t *testing.T) {
	connectionError := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection failed")}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"unannotated", connectionError, false},
		{"unknown-send-state", &transportError{err: connectionError, transport: "websocket"}, false},
		{"definitely-not-sent", &transportError{err: connectionError, transport: "websocket", requestNotSent: true}, true},
		{"already-emitted", &transportError{err: connectionError, transport: "websocket", requestNotSent: true, eventsEmitted: true}, false},
		{"protocol-error", &transportError{err: websocket.ErrBadHandshake, transport: "websocket", requestNotSent: true}, false},
		{"idle-error", &transportError{err: &IdleTimeoutError{Timeout: time.Second}, transport: "websocket", requestNotSent: true}, false},
		{"cancelled", &transportError{err: context.Canceled, transport: "websocket", requestNotSent: true}, false},
		{"sse-error", &transportError{err: connectionError, transport: "sse", requestNotSent: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			{
				got := canFallbackToSSE(fmt.Errorf("wrapped: %w", tc.err))
				reportTestFailure(t, got != tc.want, func() {
					t.Fatalf("canFallbackToSSE=%v want=%v", got, tc.want)
				})
			}
		})
	}
}

func TestWSHandshakePayloadIsBounded(t *testing.T) {
	payload := strings.Repeat("x", 2<<20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-request-id", "bounded-body")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, payload)
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := c.Stream(ctx, &Request{Transport: "websocket", Body: []byte(`{}`)}, func(*Event) error { return nil })
	var failure *FailureError
	reportTestFailure(t, !errors.As(err, &failure) || failure.Status != 403 || failure.Code != "http_403" || failure.RequestID != "bounded-body", func() {
		t.Fatalf("lost handshake metadata: %v", err)
	})
	reportTestFailure(t, len(failure.Payload) == 0 || len(failure.Payload) > 1<<20 || !strings.HasPrefix(payload, string(failure.Payload)), func() {
		t.Fatalf("unexpected handshake payload: length=%d", len(failure.Payload))
	})
}

func TestSSEFailureEventIsForwardedBeforeFailureReturn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"error\",\"code\":\"bad\",\"message\":\"failed\"}\n\n")
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	order := []string{}
	_, err := c.Stream(context.Background(), &Request{Transport: "sse", Body: []byte(`{}`)}, func(*Event) error {
		order = append(order, "event")
		return nil
	})
	order = append(order, "return")
	reportTestFailure(t, !IsFailureError(err) || strings.Join(order, ",") != "event,return", func() {
		t.Fatalf("failure order=%v err=%v", order, err)
	})
}

func TestSSENamedErrorWithoutTypeFailsAndIsDeliveredOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-request-id", "req-header")
		fmt.Fprint(w, "event: error\ndata: {\"code\":\"bad\",\"message\":\"failed\",\"status\":503}\n\ndata: {\"type\":\"response.completed\"}\n\n")
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	var events []*Event
	_, err := c.Stream(context.Background(), &Request{Transport: "sse", Body: []byte(`{}`)}, func(event *Event) error {
		events = append(events, event)
		return nil
	})
	reportTestFailure(t, !IsFailureError(err) || len(events) != 1, func() {
		t.Fatalf("named error delivery count=%d err=%v", len(events), err)
	})
	var failure *FailureError
	reportTestFailure(t, !errors.As(err, &failure) || failure.Status != 503 || failure.Code != "bad" || failure.RequestID != "req-header", func() {
		t.Fatalf("named error lost status/code/header request id: %+v", failure)
	})
	reportTestFailure(t, events[0].Type != EventError || events[0].SSEEvent != EventError || string(events[0].Raw) == "", func() {
		t.Fatalf("named error was not normalized for one delivery: %+v", events[0])
	})
	var decoded map[string]any
	{
		err := json.Unmarshal(events[0].Raw, &decoded)
		reportTestFailure(t, err != nil || decoded["type"] != EventError, func() {
			t.Fatalf("delivered error frame is not structured: %s (%v)", events[0].Raw, err)
		})
	}
}

func TestSSETopLevelErrorWithoutTypeFailsAndIsDeliveredOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"error\":{\"code\":\"bad\",\"status\":429,\"request_id\":\"req-1\"}}\n\n")
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	count := 0
	var delivered *Event
	_, err := c.Stream(context.Background(), &Request{Transport: "sse", Body: []byte(`{}`)}, func(event *Event) error {
		count++
		delivered = event
		return nil
	})
	reportTestFailure(t, !IsFailureError(err) || count != 1 || delivered == nil || delivered.Type != EventError, func() {
		t.Fatalf("top-level error count=%d event=%+v err=%v", count, delivered, err)
	})
	var failure *FailureError
	reportTestFailure(t, !errors.As(err, &failure) || failure.Status != 429 || failure.Code != "bad" || failure.RequestID != "req-1", func() {
		t.Fatalf("top-level error details lost: %+v", failure)
	})
}

func TestSSEOrdinaryNamedEventIsNotAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"delta\":\"hello\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	var events []*Event
	_, err := c.Stream(context.Background(), &Request{Transport: "sse", Body: []byte(`{}`)}, func(event *Event) error {
		events = append(events, event)
		return nil
	})
	reportTestFailure(t, err != nil || len(events) != 2, func() {
		t.Fatalf("ordinary named events err=%v count=%d", err, len(events))
	})
	reportTestFailure(t, events[0].SSEEvent != "response.output_text.delta" || events[0].Type != "" || string(events[0].Raw) != `{"delta":"hello"}`, func() {
		t.Fatalf("ordinary event changed: %+v", events[0])
	})
}

func TestSSEFailureOverridesSuccessfulTypeAndPreventsDuplicateFrame(t *testing.T) {
	for _, wire := range []string{
		"event: error\ndata: {\"type\":\"response.completed\",\"message\":\"failed\"}\n\n",
		"data: {\"type\":\"response.completed\",\"error\":{}}\n\n",
	} {
		t.Run(wire, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, wire+wire)
			}))
			defer server.Close()
			c := testClient(t, server.URL)
			var frames [][]byte
			forwardedError := false
			_, err := c.Stream(context.Background(), &Request{Transport: "sse", Body: []byte(`{}`)}, func(event *Event) error {
				frames = append(frames, FormatSSEFrame(event.Raw))
				// Match the existing API callback's failure/duplicate guard.
				forwardedError = event.Type == EventError || event.Type == EventResponseFailed
				return nil
			})
			if err != nil && !forwardedError {
				frames = append(frames, FormatSSEFrame([]byte(`{"type":"error"}`)))
			}
			reportTestFailure(t, !IsFailureError(err) || !forwardedError || len(frames) != 1, func() {
				t.Fatalf("expected one recognized error frame: count=%d forwarded=%v err=%v", len(frames), forwardedError, err)
			})
		})
	}
}

func TestWSTopLevelErrorIsDeliveredOnceWithoutRetry(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"error":{"code":"bad","status":429,"request_id":"req-ws"}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`))
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	count := 0
	_, err := c.Stream(context.Background(), &Request{Transport: "auto", Body: []byte(`{}`)}, func(event *Event) error {
		count++
		reportTestFailure(t, event.Type != EventError || event.Data["type"] != EventError, func() {
			t.Errorf("top-level WS error not normalized: %+v", event)
		})
		return nil
	})
	var failure *FailureError
	reportTestFailure(t, !errors.As(err, &failure) || failure.Code != "bad" || failure.Status != 429 || failure.RequestID != "req-ws" || count != 1 || requests.Load() != 1, func() {
		t.Fatalf("failure=%+v count=%d requests=%d error=%v", failure, count, requests.Load(), err)
	})
}

func TestSSERequiresTerminalAndForwardsFailure(t *testing.T) {
	for _, tc := range []struct {
		name, wire         string
		events             int
		failure, truncated bool
	}{
		{"empty", "", 0, false, true},
		{"delta", `data: {"type":"response.output_text.delta"}` + "\n\n", 1, false, true},
		{"done sentinel", "data: [DONE]\n\n", 0, false, true},
		{"completed", `data: {"type":"response.completed"}` + "\n\n", 1, false, false},
		{"incomplete", `data: {"type":"response.incomplete"}` + "\n\n", 1, false, false},
		{"delta then done", `data: {"type":"response.output_text.delta"}` + "\n\ndata: [DONE]\n\n", 1, false, true},
		{"named done sentinel", "event: error\ndata: [DONE]\n\n", 0, false, true},
		{"done", `data: {"type":"response.done"}` + "\n\n", 1, false, false},
		{"failed", `data: {"type":"response.failed","response":{"error":{"code":"bad","message":"failed"}}}` + "\n\n", 1, true, false},
		{"error", `data: {"type":"error","code":"bad","message":"failed"}` + "\n\n", 1, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.wire)
			}))
			defer server.Close()
			c := testClient(t, server.URL)
			n := 0
			_, err := c.Stream(context.Background(), &Request{Transport: "sse", Body: []byte(`{}`)}, func(e *Event) error {
				n++
				reportTestFailure(t, !strings.Contains(tc.wire, string(e.Raw)), func() {
					t.Errorf("raw event changed: %s", e.Raw)
				})
				return nil
			})
			reportTestFailure(t, n != tc.events, func() {
				t.Fatalf("events=%d want=%d", n, tc.events)
			})
			reportTestFailure(t, tc.failure != IsFailureError(err), func() {
				t.Fatalf("failure=%v error=%v", tc.failure, err)
			})
			if tc.truncated {
				reportTestFailure(t, err == nil || !strings.Contains(err.Error(), "truncated"), func() {
					t.Fatalf("expected truncation, got %v", err)
				})
			} else {
				reportTestFailure(t, !tc.failure && err != nil, func() {
					t.Fatal(err)
				})
			}
		})
	}
}
func TestWSBinaryJSONFrameIsDecodedLikeText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer socket.Close()
		if _, _, err := socket.ReadMessage(); err != nil {
			return
		}
		_ = socket.WriteMessage(websocket.BinaryMessage, []byte(`{"type":"response.completed","response":{"id":"binary-id"}}`))
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	events := 0
	_, err := c.Stream(context.Background(), &Request{Transport: "websocket", Body: []byte(`{}`)}, func(event *Event) error {
		events++
		reportTestFailure(t, event.Type != EventResponseCompleted || event.Data["type"] != EventResponseCompleted, func() {
			t.Fatalf("unexpected binary event: %+v", event)
		})
		return nil
	})
	reportTestFailure(t, err != nil || events != 1, func() {
		t.Fatalf("binary websocket stream err=%v events=%d", err, events)
	})
}

func TestWSBinaryNonJSONFrameReturnsProtocolError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer socket.Close()
		if _, _, err := socket.ReadMessage(); err != nil {
			return
		}
		_ = socket.WriteMessage(websocket.BinaryMessage, []byte("not json"))
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	_, err := c.Stream(context.Background(), &Request{Transport: "websocket", Body: []byte(`{}`)}, func(*Event) error { return nil })
	reportTestFailure(t, err == nil || !strings.Contains(err.Error(), "invalid websocket JSON"), func() {
		t.Fatalf("expected binary JSON protocol error, got %v", err)
	})
}

func TestWSRejectsOversizedFrame(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer socket.Close()
		if _, _, err := socket.ReadMessage(); err != nil {
			return
		}
		_ = socket.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("x", maxWSFrameBytes+1)))
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	_, err := c.Stream(context.Background(), &Request{Transport: "websocket", Body: []byte(`{}`)}, func(*Event) error { return nil })
	reportTestFailure(t, !errors.Is(err, ErrWSFrameTooLarge), func() {
		t.Fatalf("expected oversized websocket error, got %v", err)
	})
}

func TestWSFirstFailureAfterSendDoesNotRetry(t *testing.T) {
	for _, transport := range []string{"websocket", "websocket-cached", "auto"} {
		for _, code := range []string{"previous_response_not_found", "websocket_connection_limit_reached"} {
			t.Run(transport+"/"+code, func(t *testing.T) { runWSFirstFailureAfterSendDoesNotRetryCase(t, &transport, &code) })
		}
	}
}

func TestWSRequiresTerminalAndNoRetryAfterDelivery(t *testing.T) {
	for _, code := range []string{"", "previous_response_not_found", "websocket_connection_limit_reached", "bad"} {
		t.Run(code, func(t *testing.T) { runWSRequiresTerminalAndNoRetryAfterDeliveryCase(t, &code) })
	}
}
func TestWSCancellationInterruptsBlockedRead(t *testing.T) {
	ready := make(chan struct{})
	closed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer socket.Close()
		_, _, _ = socket.ReadMessage()
		close(ready)
		_, _, _ = socket.ReadMessage()
		close(closed)
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Stream(ctx, &Request{Transport: "websocket-cached", Body: []byte(`{}`)}, func(*Event) error { return nil })
		done <- err
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("request not received")
	}
	cancel()
	select {
	case err := <-done:
		reportTestFailure(t, !errors.Is(err, context.Canceled), func() {
			t.Fatalf("error=%v", err)
		})
	case <-time.After(500 * time.Millisecond):
		t.Fatal("read was not canceled promptly")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("socket remains open")
	}
	reportTestFailure(t, c.PoolSize() != 0, func() {
		t.Fatal("canceled socket returned to pool")
	})
}
func TestWSConcurrentSameKeyReusesOneSocket(t *testing.T) {
	var dials atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveWSConcurrentSameKeyReusesOneSocketRequest(w, r, &dials)
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			events := 0
			_, err := c.Stream(ctx, &Request{SessionID: "same", PoolSessionID: "same-internal", ClientKeyID: 1, AccountID: 1, Transport: "websocket-cached", Body: []byte(`{"input":[]}`)}, func(*Event) error { events++; return nil })
			if err == nil && events != 1 {
				err = fmt.Errorf("events=%d", events)
			}
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		reportTestFailure(t, err != nil, func() {
			t.Error(err)
		})
	}
	reportTestFailure(t, dials.Load() != 1, func() {
		t.Errorf("same key created %d sockets, want 1", dials.Load())
	})
}
func TestWSPoolCloseUnblocksWaiters(t *testing.T) {
	p := newWSPool(time.Minute, nil)
	_, err := p.acquire(context.Background(), "key")
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	done := make(chan error, 1)
	go func() { _, err := p.acquire(context.Background(), "key"); done <- err }()
	p.closeAll()
	select {
	case err := <-done:
		reportTestFailure(t, err == nil, func() {
			t.Fatal("closed pool admitted waiter")
		})
	case <-time.After(time.Second):
		t.Fatal("waiter stuck")
	}
	p.release("key")
	p.mu.Lock()
	defer p.mu.Unlock()
	reportTestFailure(t, len(p.slots) != 0, func() {
		t.Fatalf("leaked %d slots", len(p.slots))
	})
}

// runWSHandshakeFailurePreservesFailureErrorAndFallbackPolicyCase exercises one isolated test phase with the original shared state.
func runWSHandshakeFailurePreservesFailureErrorAndFallbackPolicyCase(t *testing.T, capturedTransport *string, capturedStatus *int) {
	t.Helper()

	const payload = `{"error":{"code":"upstream_rejected","message":"rejected request"}}`
	var handshakes, posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			fmt.Fprint(w, "data: {\"type\":\"response.completed\"}\n\n")
			return
		}
		handshakes.Add(1)
		w.Header().Set("x-request-id", "req-handshake")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader((*capturedStatus))
		fmt.Fprint(w, payload)
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	events := 0
	res, err := c.Stream(ctx, &Request{Transport: (*capturedTransport), Body: []byte(`{}`)}, func(*Event) error { events++; return nil })
	reportTestFailure(t, handshakes.Load() != 1, func() {
		t.Fatalf("handshakes=%d want 1", handshakes.Load())
	})
	if (*capturedTransport) == "auto" && (*capturedStatus) >= 500 {
		reportTestFailure(t, err != nil || posts.Load() != 1 || events != 1 || res == nil || res.FallbackFrom != "websocket" || res.Transport != "sse", func() {
			t.Fatalf("expected one safe fallback: res=%+v err=%v posts=%d events=%d", res, err, posts.Load(), events)
		})
		return
	}
	var failure *FailureError
	reportTestFailure(t, !errors.As(err, &failure) || failure.Status != (*capturedStatus) || failure.Code != "upstream_rejected" || failure.RequestID != "req-handshake" || string(failure.Payload) != payload || failure.Message == "", func() {
		t.Fatalf("lost structured handshake failure: %+v error=%v", failure, err)
	})
	reportTestFailure(t, res == nil || res.Status != (*capturedStatus) || posts.Load() != 0 || events != 0 || !IsFailureError(err), func() {
		t.Fatalf("unexpected fallback/result: res=%+v posts=%d events=%d error=%v", res, posts.Load(), events, err)
	})
	var te *transportError
	reportTestFailure(t, !errors.As(err, &te) || !te.requestNotSent, func() {
		t.Fatalf("pre-send failure not recorded: %v", err)
	})
}

// runWSSentBeforeFirstEventDoesNotFallbackCase exercises one isolated test phase with the original shared state.
func runWSSentBeforeFirstEventDoesNotFallbackCase(t *testing.T, capturedMode *string) {
	t.Helper()

	var creates, posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			fmt.Fprint(w, "data: {\"type\":\"response.completed\"}\n\n")
			return
		}
		socket, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer socket.Close()
		if _, _, err := socket.ReadMessage(); err != nil {
			return
		}
		creates.Add(1)
		if (*capturedMode) == "invalid-json" {
			_ = socket.WriteMessage(websocket.TextMessage, []byte("invalid json"))
		}
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	events := 0
	_, err := c.Stream(ctx, &Request{Transport: "auto", Body: []byte(`{}`)}, func(*Event) error { events++; return nil })
	var te *transportError
	reportTestFailure(t, !errors.As(err, &te) || te.requestNotSent || te.eventsEmitted || events != 0 || creates.Load() != 1 || posts.Load() != 0, func() {
		t.Fatalf("unsafe replay: error=%v events=%d creates=%d posts=%d", err, events, creates.Load(), posts.Load())
	})
}

// runWSConnectionFailureFallbackPreservesFinalErrorCase exercises one isolated test phase with the original shared state.
func runWSConnectionFailureFallbackPreservesFinalErrorCase(t *testing.T, capturedFailSSE *bool) {
	t.Helper()

	var posts, dials atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reportTestFailure(t, r.Method != http.MethodPost, func() {
			t.Errorf("unexpected method: %s", r.Method)
		})
		posts.Add(1)
		if *capturedFailSSE {
			w.Header().Set("x-request-id", "final-sse")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"code":"final_failure","message":"rate limit"}}`)
			return
		}
		fmt.Fprint(w, "data: {\"type\":\"response.completed\"}\n\n")
	}))
	defer server.Close()
	factory := egress.NewFactory(egress.Options{})
	factory.BaseDialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if dials.Add(1) == 1 {
			return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("injected connection failure")}
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	c := New(Config{SSEURL: server.URL, WSURL: "ws" + strings.TrimPrefix(server.URL, "http")}, factory)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := c.Stream(ctx, &Request{Transport: "auto", Body: []byte(`{}`)}, func(*Event) error { return nil })
	reportTestFailure(t, posts.Load() != 1 || dials.Load() != 2, func() {
		t.Fatalf("posts=%d dials=%d", posts.Load(), dials.Load())
	})
	if *capturedFailSSE {
		var failure *FailureError
		reportTestFailure(t, !errors.As(err, &failure) || failure.Code != "final_failure" || failure.Status != 429 || failure.RequestID != "final-sse", func() {
			t.Fatalf("lost final SSE failure: %v", err)
		})
	} else {
		reportTestFailure(t, err != nil || res == nil || res.FallbackFrom != "websocket", func() {
			t.Fatalf("expected safe fallback, got res=%+v error=%v", res, err)
		})
	}
}

// runWSFirstFailureAfterSendDoesNotRetryCase exercises one isolated test phase with the original shared state.
func runWSFirstFailureAfterSendDoesNotRetryCase(t *testing.T, capturedTransport *string, capturedCode *string) {
	t.Helper()

	var creates, posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			fmt.Fprint(w, "data: {\"type\":\"response.completed\"}\n\n")
			return
		}
		socket, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer socket.Close()
		if _, _, err := socket.ReadMessage(); err != nil {
			return
		}
		creates.Add(1)
		_ = socket.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"error","code":%q}`, (*capturedCode))))
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var events []string
	_, err := c.Stream(ctx, &Request{Transport: (*capturedTransport), Body: []byte(`{}`)}, func(event *Event) error {
		events = append(events, event.Type)
		return nil
	})
	var failure *FailureError
	reportTestFailure(t, !errors.As(err, &failure) || failure.Code != (*capturedCode) || creates.Load() != 1 || posts.Load() != 0 || len(events) != 1 || events[0] != EventError, func() {
		t.Fatalf("error=%v creates=%d posts=%d events=%v", err, creates.Load(), posts.Load(), events)
	})
	var te *transportError
	reportTestFailure(t, !errors.As(err, &te) || te.requestNotSent, func() {
		t.Fatalf("sent request was not recorded: %v", err)
	})
}

// runWSRequiresTerminalAndNoRetryAfterDeliveryCase exercises one isolated test phase with the original shared state.
func runWSRequiresTerminalAndNoRetryAfterDeliveryCase(t *testing.T, capturedCode *string) {
	t.Helper()

	var dials atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer socket.Close()
		dials.Add(1)
		if _, _, err = socket.ReadMessage(); err != nil {
			return
		}
		_ = socket.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"hello"}`))
		if (*capturedCode) != "" {
			_ = socket.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"error","code":%q,"message":"failed"}`, (*capturedCode))))
		}
		_ = socket.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"), time.Now().Add(time.Second))
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	events := 0
	_, err := c.Stream(context.Background(), &Request{Transport: "auto", Body: []byte(`{}`)}, func(e *Event) error { events++; return nil })
	reportTestFailure(t, err == nil, func() {
		t.Fatal("expected stream failure")
	})
	reportTestFailure(t, dials.Load() != 1, func() {
		t.Fatalf("dialed %d times", dials.Load())
	})
	var te *transportError
	reportTestFailure(t, !errors.As(err, &te) || !te.eventsEmitted, func() {
		t.Fatalf("missing emitted flag: %v", err)
	})
	want := 2
	if (*capturedCode) == "" {
		want = 1
		var closeErr *websocket.CloseError
		reportTestFailure(t, !errors.As(err, &closeErr) || closeErr.Code != 1000, func() {
			t.Fatalf("lost close code: %v", err)
		})
	}
	reportTestFailure(t, events != want, func() {
		t.Fatalf("events=%d want=%d", events, want)
	})
}

// serveWSConcurrentSameKeyReusesOneSocketRequest exercises one isolated test phase with the original shared state.
func serveWSConcurrentSameKeyReusesOneSocketRequest(w http.ResponseWriter, r *http.Request, capturedDials *atomic.Int32) {
	socket, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer socket.Close()
	(*capturedDials).Add(1)
	for {
		if _, _, err := socket.ReadMessage(); err != nil {
			return
		}
		if err := socket.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"r","output":[]}}`)); err != nil {
			return
		}
	}
}
