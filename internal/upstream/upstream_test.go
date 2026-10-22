package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuildDeltaBodyRejectsMismatchedExplicitPreviousResponseID(t *testing.T) {
	body := mustJSON(t, map[string]any{"model": "m", "input": []any{}, "previous_response_id": "client-id"})
	except, input, ok := bodyExceptInput(body)
	reportTestFailure(t, !ok, func() {
		t.Fatal("bodyExceptInput failed")
	})
	cont := &wsContinuation{bodyExceptInput: except, lastInput: input, lastResponseID: "cached-id"}
	{
		_, ok := buildDeltaBody(body, cont)
		reportTestFailure(t, ok, func() {
			t.Fatal("mismatched explicit previous_response_id must disable continuation")
		})
	}
}

func TestBuildDeltaBodyDoesNotReconvertExplicitPreviousResponseID(t *testing.T) {
	first := mustJSON(t, map[string]any{"model": "m", "input": []any{map[string]any{"content": "one"}}})
	except, input, _ := bodyExceptInput(first)
	cont := &wsContinuation{bodyExceptInput: except, lastInput: input, lastResponseID: "cached-id"}
	body := mustJSON(t, map[string]any{"model": "m", "input": []any{map[string]any{"content": "one"}, map[string]any{"content": "two"}}, "previous_response_id": "cached-id"})
	{
		_, ok := buildDeltaBody(body, cont)
		reportTestFailure(t, ok, func() {
			t.Fatal("explicit previous_response_id must remain an upstream delta")
		})
	}
}

func TestWSPoolSerializesSameKeyAndHonorsCancellation(t *testing.T) {
	p := newWSPool(time.Minute, nil)
	key := poolKey("session", 1)
	ctx := context.Background()
	{
		conn, err := p.acquire(ctx, key)
		reportTestFailure(t, err != nil || conn != nil, func() {
			t.Fatalf("first acquire = %v, %v", conn, err)
		})
	}
	acquired := make(chan error, 1)
	go func() { _, err := p.acquire(ctx, key); acquired <- err }()
	select {
	case err := <-acquired:
		t.Fatalf("concurrent acquire completed early: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	p.release(key)
	select {
	case err := <-acquired:
		reportTestFailure(t, err != nil, func() {
			t.Fatalf("concurrent acquire: %v", err)
		})
	case <-time.After(time.Second):
		t.Fatal("concurrent acquire remained blocked")
	}
	blockedCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	{
		_, err := p.acquire(blockedCtx, key)
		reportTestFailure(t, err == nil, func() {
			t.Fatal("canceled same-key acquire must return an error")
		})
	}
	p.release(key)
}

func TestWSPoolWaitQueueLimitAndCancellation(t *testing.T) {
	p := newWSPool(time.Minute, nil)
	key := poolKey("busy", 1)
	{
		_, err := p.acquire(context.Background(), key)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(maxPoolWaitersPerKey)
	for i := 0; i < maxPoolWaitersPerKey; i++ {
		go func() {
			defer wg.Done()
			_, _ = p.acquire(ctx, key)
		}()
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		p.mu.Lock()
		queued := len(p.slots[key].waiters)
		p.mu.Unlock()
		if queued == maxPoolWaitersPerKey {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("waiters=%d want=%d", queued, maxPoolWaitersPerKey)
		default:
			time.Sleep(time.Millisecond)
		}
	}

	busyDone := make(chan error, 1)
	go func() {
		_, err := p.acquire(context.Background(), key)
		busyDone <- err
	}()
	select {
	case err := <-busyDone:
		reportTestFailure(t, !errors.Is(err, ErrPoolBusy), func() {
			t.Fatalf("busy error=%v", err)
		})
	case <-time.After(100 * time.Millisecond):
		t.Fatal("pool busy acquire blocked")
	}
	cancel()
	p.release(key)
	wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	reportTestFailure(t, len(p.slots) != 0, func() {
		t.Fatalf("leaked %d pool slots", len(p.slots))
	})
}

func TestIsTerminalIncludesFailureEvents(t *testing.T) {
	for _, typ := range []string{EventResponseCompleted, EventResponseDone, EventResponseIncomplete, EventResponseFailed, EventError} {
		reportTestFailure(t, !IsTerminal(typ), func() {
			t.Errorf("IsTerminal(%q) = false", typ)
		})
	}
}

func TestFailureFromEventPreservesRawFailureEvent(t *testing.T) {
	raw := []byte(`{"type":"error","code":"upstream_failure","message":"missing"}`)
	event := &Event{Type: EventError, Raw: raw, Data: map[string]any{"type": "error", "code": "upstream_failure", "message": "missing"}}
	err := FailureFromEvent(event)
	var failure *FailureError
	reportTestFailure(t, !errors.As(err, &failure) || string(failure.Payload) != string(raw), func() {
		t.Fatalf("failure payload was not preserved: %#v", failure)
	})
}

func TestFailureFromEventExtractsNestedResponseFailureDetails(t *testing.T) {
	raw := []byte(`{"type":"response.failed","status_code":503,"response":{"id":"resp_123","status":"failed","error":{"code":"upstream_failure","message":"backend failed","headers":{"x-request-id":"req_123"}}}}`)
	event := &Event{Type: EventResponseFailed, Raw: raw, Data: map[string]any{
		"type":        EventResponseFailed,
		"status_code": float64(503),
		"response": map[string]any{
			"id":     "resp_123",
			"status": "failed",
			"error": map[string]any{
				"code":    "upstream_failure",
				"message": "backend failed",
				"headers": map[string]any{"x-request-id": "req_123"},
			},
		},
	}}
	var failure *FailureError
	reportTestFailure(t, !errors.As(FailureFromEvent(event), &failure), func() {
		t.Fatal("expected FailureError")
	})
	reportTestFailure(t, failure.Code != "upstream_failure" || failure.Message != "backend failed" || failure.Status != 503 || failure.RequestID != "req_123", func() {
		t.Fatalf("unexpected failure details: %+v", failure)
	})
	reportTestFailure(t, string(failure.Payload) != string(raw), func() {
		t.Fatal("failure payload was changed")
	})
}

func TestFailureFromEventCodeOnlyGetsDefaultMessageAndHeaderRequestID(t *testing.T) {
	raw := []byte(`{"type":"error","code":"previous_response_not_found","error":{"headers":{"x-request-id":"req_456"}}}`)
	event := &Event{Type: EventError, Raw: raw, Data: map[string]any{
		"type": "error", "code": "previous_response_not_found",
		"error": map[string]any{"headers": map[string]any{"x-request-id": "req_456"}},
	}}
	failure, ok := FailureFromEvent(event).(*PreviousResponseNotFoundError)
	reportTestFailure(t, !ok || failure.Failure == nil, func() {
		t.Fatalf("expected previous response error: %v", FailureFromEvent(event))
	})
	reportTestFailure(t, failure.Failure.Message == "" || failure.Failure.RequestID != "req_456", func() {
		t.Fatalf("missing default message/request id: %+v", failure.Failure)
	})
}

func TestSSEReaderPreservesEventNameAndMultilineData(t *testing.T) {
	wire := "event: ignored\r\nevent: error\r\n: keep-alive\r\ndata: {\r\ndata: \"code\":\"bad\",\r\ndata: \"message\":\"failed\"}\r\n\r\n" +
		"data: {\"type\":\"response.completed\"}\n\n"
	reader := ParseSSE(strings.NewReader(wire))
	event, err := reader.Next()
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	const raw = "{\n\"code\":\"bad\",\n\"message\":\"failed\"}"
	reportTestFailure(t, event.SSEEvent != EventError || event.Type != "" || string(event.Raw) != raw, func() {
		t.Fatalf("lost event name or multiline payload: %+v", event)
	})
	var failure *FailureError
	reportTestFailure(t, !errors.As(FailureFromEvent(event), &failure) || failure.Code != "bad" || string(failure.Payload) != raw, func() {
		t.Fatalf("named error was not recognized: %+v", failure)
	})
	terminal, err := reader.Next()
	reportTestFailure(t, err != nil || terminal.SSEEvent != "" || terminal.Type != EventResponseCompleted, func() {
		t.Fatalf("name leaked to next event: %+v, %v", terminal, err)
	})
}

func TestSSEReaderResetsEventName(t *testing.T) {
	for _, prefix := range []string{
		"event: error\n\n", // A frame without data must not leak its name.
		"event: error\nevent:\n",
		"event: error\nevent\n",
	} {
		event, err := ParseSSE(strings.NewReader(prefix + "data: {}\n\n")).Next()
		reportTestFailure(t, err != nil || event.SSEEvent != "" || FailureFromEvent(event) != nil, func() {
			t.Fatalf("prefix %q leaked error name: %+v, %v", prefix, event, err)
		})
	}
}

func TestSSEReaderNamedErrorAtEOF(t *testing.T) {
	reader := ParseSSE(strings.NewReader("event: error\ndata: {\"code\":\"bad\"}"))
	event, err := reader.Next()
	reportTestFailure(t, err != nil || event.SSEEvent != EventError || !IsFailureError(FailureFromEvent(event)), func() {
		t.Fatalf("lost trailing named error: %+v, %v", event, err)
	})
	{
		_, err := reader.Next()
		reportTestFailure(t, err != io.EOF, func() {
			t.Fatalf("next error = %v, want EOF", err)
		})
	}
}

func TestFailureFromEventUsesJSONErrorTruthiness(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"null", false}, {"false", false}, {"0", false}, {"-0.0", false}, {`""`, false},
		{"true", true}, {"1", true}, {"-1.5", true}, {`"failed"`, true},
		{"{}", true}, {"[]", true}, {`{"code":"bad"}`, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			raw := `{"error":` + tc.value + `}`
			event, err := ParseSSE(strings.NewReader("data: " + raw + "\n\n")).Next()
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			failure := FailureFromEvent(event)
			reportTestFailure(t, (failure != nil) != tc.want, func() {
				t.Fatalf("error=%s failure=%v want=%v", tc.value, failure, tc.want)
			})
		})
	}
	reportTestFailure(t, FailureFromEvent(nil) != nil, func() {
		t.Fatal("nil event is not an error")
	})
}

func TestFailureEventForDeliveryPreservesOriginal(t *testing.T) {
	const raw = `{"type":"response.output_text.delta","error":{"code":"bad","status":429,"request_id":"req-body"}}`
	event, err := ParseSSE(strings.NewReader("data: " + raw + "\n\n")).Next()
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	failure := FailureFromEvent(event)
	out := failureEventForDelivery(event, failure)
	var decoded map[string]any
	{
		err := json.Unmarshal(out.Raw, &decoded)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, out.Type != EventError || out.Data["type"] != EventError || decoded["type"] != EventError, func() {
		t.Fatalf("failure not normalized consistently: %+v", out)
	})
	reportTestFailure(t, string(event.Raw) != raw || event.Type != "response.output_text.delta" || event.Data["type"] != event.Type, func() {
		t.Fatalf("original event was mutated: %+v", event)
	})
	var details *FailureError
	reportTestFailure(t, !errors.As(failure, &details) || string(details.Payload) != raw || details.Status != 429 || details.Code != "bad" || details.RequestID != "req-body", func() {
		t.Fatalf("failure details lost: %+v", details)
	})
}

func TestSSEReaderRejectsOversizedEvent(t *testing.T) {
	_, err := ParseSSE(strings.NewReader("data: " + strings.Repeat("x", maxSSEEventBytes) + "\n\n")).Next()
	reportTestFailure(t, !errors.Is(err, ErrSSEEventTooLarge), func() {
		t.Fatalf("expected oversized SSE error, got %v", err)
	})
}

func TestSSEReaderAcceptsNormalLargeEvent(t *testing.T) {
	wire := `data: {"type":"response.output_text.delta","delta":"` + strings.Repeat("a", 128<<10) + `"}` + "\n\n"
	event, err := ParseSSE(strings.NewReader(wire)).Next()
	reportTestFailure(t, err != nil || event == nil || event.Type != "response.output_text.delta", func() {
		t.Fatalf("normal large SSE event failed: event=%v err=%v", event, err)
	})
}
