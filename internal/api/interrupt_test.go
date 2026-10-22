package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"pi-gateway/internal/upstream"
)

func TestGatewaySSEInterruptedContinuation(t *testing.T) {
	backend := newGatewayContinuationBackend()
	backend.hook = func(socket *websocket.Conn, turn gatewayTurn) bool {
		_ = socket.SetReadDeadline(time.Now().Add(5 * time.Second))
		if err := socket.WriteJSON(map[string]any{"type": "response.incomplete", "response": map[string]any{"id": turn.id, "status": "incomplete", "incomplete_details": map[string]any{"reason": "interrupted"}, "output": []any{}}}); err != nil {
			return false
		}
		var next map[string]any
		if err := socket.ReadJSON(&next); err != nil {
			t.Errorf("SSE continuation: %v", err)
			return false
		}
		if next["type"] != "response.create" || next["previous_response_id"] != turn.id {
			t.Errorf("SSE continuation changed socket: %+v", next)
			return false
		}
		backend.generations.Add(1)
		_ = socket.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "sse-continued", "status": "completed", "output": []any{}}})
		return false
	}
	h := newGatewayContinuationHarness(t, backend, nil)
	status, payload := gatewayHTTP(t, h, map[string]any{"model": "test-model", "input": []any{gatewayUser("first")}, "stream": true})
	reportTestFailure(t, status != 200, func() {
		t.Fatalf("SSE first turn: %d %s", status, payload)
	})
	event, err := upstream.ParseSSE(strings.NewReader(string(payload))).Next()
	reportTestFailure(t, err != nil || event.Type != "response.incomplete", func() {
		t.Fatalf("SSE terminal: %+v %v", event, err)
	})
	id := event.Data["response"].(map[string]any)["id"].(string)
	status, payload = gatewayHTTP(t, h, map[string]any{"model": "test-model", "input": []any{gatewayUser("next")}, "previous_response_id": id, "stream": true})
	reportTestFailure(t, status != 200 || strings.Contains(string(payload), `"type":"error"`) || !strings.Contains(string(payload), `"type":"response.completed"`), func() {
		t.Fatalf("SSE continuation rejected: %d %s", status, payload)
	})
	reportTestFailure(t, backend.connections.Load() != 1 || backend.generations.Load() != 2 || backend.httpCalls.Load() != 0, func() {
		t.Fatal("SSE continuation replayed or switched transport")
	})
}

func TestGatewayWSInterruptRejectsHTTPUpstreamWithoutStoppingResponse(t *testing.T) {
	finish := make(chan struct{})
	defer func() {
		select {
		case <-finish:
		default:
			close(finish)
		}
	}()
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"http-active\"}}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-finish:
		case <-r.Context().Done():
			return
		}
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"http-active\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}), "sse")
	client := dialGatewayContinuation(t, h, h.key)
	{
		err := client.WriteJSON(gatewayCreate([]any{gatewayUser("first")}, "", ""))
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	var created map[string]any
	{
		err := client.ReadJSON(&created)
		reportTestFailure(t, err != nil || created["type"] != "response.created", func() {
			t.Fatalf("created: %+v %v", created, err)
		})
	}
	{
		err := client.WriteJSON(map[string]any{"type": "response.interrupt", "response_id": "http-active", "mode": "discard_partial_items"})
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	var rejection map[string]any
	{
		err := client.ReadJSON(&rejection)
		reportTestFailure(t, err != nil || rejection["type"] != "error" || rejection["status"] != float64(400), func() {
			t.Fatalf("HTTP control rejection: %+v %v", rejection, err)
		})
	}
	close(finish)
	{
		event := gatewayTerminal(t, client)
		reportTestFailure(t, event["type"] != "response.completed", func() {
			t.Fatalf("control stopped HTTP stream: %+v", event)
		})
	}
}

func TestGatewaySSEInterruptedTerminal(t *testing.T) {
	for _, transport := range []string{"sse", "websocket-cached"} {
		t.Run(transport, func(t *testing.T) { runGatewaySSEInterruptedTerminalCase(t, &transport) })
	}
}

func TestGatewayWSInterruptAndContinuation(t *testing.T) {
	for _, terminal := range []string{"response.incomplete", "response.completed"} {
		t.Run(terminal, func(t *testing.T) { runGatewayWSInterruptAndContinuationCase(t, &terminal) })
	}
}

// runGatewaySSEInterruptedTerminalCase exercises one isolated test phase with the original shared state.
func runGatewaySSEInterruptedTerminalCase(t *testing.T, capturedTransport *string) {
	t.Helper()

	terminal := map[string]any{"type": "response.incomplete", "response": map[string]any{"id": "sse-interrupted", "status": "incomplete", "incomplete_details": map[string]any{"reason": "interrupted"}, "output": []any{}}}
	raw, _ := json.Marshal(terminal)
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			socket, err := wsUpgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer socket.Close()
			var create map[string]any
			if socket.ReadJSON(&create) != nil {
				return
			}
			_ = socket.WriteMessage(websocket.TextMessage, raw)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
	}), (*capturedTransport))
	status, payload := gatewayHTTP(t, h, map[string]any{"model": "test-model", "input": []any{}, "stream": true})
	reportTestFailure(t, status != 200 || strings.Contains(string(payload), `"type":"error"`), func() {
		t.Fatalf("interrupted terminal became a stream error: %d %s", status, payload)
	})
	event, err := upstream.ParseSSE(strings.NewReader(string(payload))).Next()
	reportTestFailure(t, err != nil || event.Type != "response.incomplete" || !strings.Contains(string(event.Raw), `"reason":"interrupted"`), func() {
		t.Fatalf("lost SSE terminal: %+v %v", event, err)
	})
}

// runGatewayWSInterruptAndContinuationCase exercises one isolated test phase with the original shared state.
func runGatewayWSInterruptAndContinuationCase(t *testing.T, capturedTerminal *string) {
	t.Helper()

	backend := newGatewayContinuationBackend()
	releaseTerminal := make(chan struct{})
	defer func() {
		select {
		case <-releaseTerminal:
		default:
			close(releaseTerminal)
		}
	}()
	backend.hook = func(socket *websocket.Conn, turn gatewayTurn) bool {
		return verifyGatewayWSInterruptAndContinuation(socket, turn, &t, &capturedTerminal, &backend, &releaseTerminal)
	}
	h := newGatewayContinuationHarness(t, backend, nil)
	client := dialGatewayContinuation(t, h, h.key)
	read := func() map[string]any {
		_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
		var event map[string]any
		{
			err := client.ReadJSON(&event)
			reportTestFailure(t, err != nil, func() {
				t.Fatalf("read: %v", err)
			})
		}
		return event
	}
	write := func(frame map[string]any) {
		{
			err := client.WriteJSON(frame)
			reportTestFailure(t, err != nil, func() {
				t.Fatalf("write: %v", err)
			})
		}
	}
	// No active response: reject immediately, without creating a request.
	write(map[string]any{"type": "response.interrupt", "response_id": "missing", "mode": "discard_partial_items", "event_id": "idle"})
	{
		event := read()
		reportTestFailure(t, event["type"] != "error" || event["event_id"] != "idle", func() {
			t.Fatalf("idle interrupt: %+v", event)
		})
	}
	write(gatewayCreate([]any{gatewayUser("first")}, "", ""))
	created := read()
	reportTestFailure(t, created["type"] != "response.created", func() {
		t.Fatalf("created: %+v", created)
	})
	id := created["response"].(map[string]any)["id"].(string)
	// Invalid mode and wrong ID must not stop the active response.
	for _, frame := range []map[string]any{
		{"type": "response.interrupt", "response_id": id, "mode": "invalid"},
		{"type": "response.interrupt", "response_id": "wrong", "mode": "discard_partial_items"},
		{"type": "response.interrupt", "response_id": 123, "mode": "discard_partial_items"},
		{"type": "response.interrupt", "response_id": id, "mode": false},
	} {
		write(frame)
		{
			event := read()
			reportTestFailure(t, event["type"] != "error" || event["status"] != float64(http.StatusBadRequest), func() {
				t.Fatalf("invalid interrupt: %+v", event)
			})
		}
	}
	// A queued create cannot block the control frame or cause a new attempt.
	write(gatewayCreate([]any{gatewayUser("next")}, id, ""))
	for i := 0; i < 2; i++ {
		write(map[string]any{"type": "response.interrupt", "response_id": id, "mode": "discard_partial_items"})
	}
	// The error is a reader-side barrier proving both interrupts were consumed.
	write(map[string]any{"type": "response.interrupt", "response_id": id, "mode": "invalid", "event_id": "barrier"})
	{
		event := read()
		reportTestFailure(t, event["type"] != "error" || event["event_id"] != "barrier", func() {
			t.Fatalf("duplicate interrupt: %+v", event)
		})
	}
	close(releaseTerminal)
	{
		event := read()
		reportTestFailure(t, event["type"] != (*capturedTerminal), func() {
			t.Fatalf("interrupt terminal: %+v", event)
		})
	}
	result := gatewayTerminal(t, client)
	reportTestFailure(t, result["type"] != "response.completed", func() {
		t.Fatalf("continuation result: %+v", result)
	})
	reportTestFailure(t, backend.connections.Load() != 1 || backend.generations.Load() != 2 || backend.httpCalls.Load() != 0, func() {
		t.Fatalf("control caused extra attempts: sockets=%d generations=%d http=%d", backend.connections.Load(), backend.generations.Load(), backend.httpCalls.Load())
	})
}

// verifyGatewayWSInterruptAndContinuation exercises one isolated test phase with the original shared state.
func verifyGatewayWSInterruptAndContinuation(socket *websocket.Conn, turn gatewayTurn, capturedT **testing.T, capturedCapturedTerminal **string, capturedBackend **gatewayContinuationBackend, capturedReleaseTerminal *chan struct{}) bool {
	(*capturedT).Helper()

	_ = socket.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := socket.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": turn.id}}); err != nil {
		return false
	}
	var interrupt map[string]any
	if err := socket.ReadJSON(&interrupt); err != nil {
		(*capturedT).Errorf("upstream interrupt: %v", err)
		return false
	}
	if interrupt["type"] != "response.interrupt" || interrupt["response_id"] != turn.id || interrupt["mode"] != "discard_partial_items" {
		(*capturedT).Errorf("wrong control frame: %+v", interrupt)
		return false
	}
	select {
	case <-(*capturedReleaseTerminal):
	case <-time.After(5 * time.Second):
		(*capturedT).Error("interrupt processing stalled")
		return false
	}
	response := map[string]any{"id": turn.id, "status": "completed", "output": []any{}}
	if (*(*capturedCapturedTerminal)) == "response.incomplete" {
		response["status"] = "incomplete"
		response["incomplete_details"] = map[string]any{"reason": "interrupted"}
	}
	if err := socket.WriteJSON(map[string]any{"type": (*(*capturedCapturedTerminal)), "response": response}); err != nil {
		return false
	}
	var next map[string]any
	if err := socket.ReadJSON(&next); err != nil {
		(*capturedT).Errorf("continuation: %v", err)
		return false
	}
	if next["type"] != "response.create" || next["previous_response_id"] != turn.id {
		(*capturedT).Errorf("interrupt was duplicated or continuation changed socket: %+v", next)
		return false
	}
	(*capturedBackend).generations.Add(1)
	_ = socket.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "after-interrupt", "status": "completed", "output": []any{}}})
	return false
}
