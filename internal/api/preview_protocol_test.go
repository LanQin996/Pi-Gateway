package api

import (
	"net/http"
	"reflect"
	"testing"

	"pi-gateway/internal/rules"
)

// Inspect the actual upstream payload after rules and transport framing. This
// catches fields lost by either normalization, final enforcement or bridging.
func TestPreviewOptionsAcrossTransportsAndRules(t *testing.T) {
	for _, transport := range []string{"sse", "websocket", "websocket-cached", "auto-fallback-sse"} {
		for _, downstream := range []string{"http", "ws"} {
			// auto selects websocket-cached for downstream WS, without SSE fallback.
			if transport == "auto-fallback-sse" && downstream == "ws" {
				continue
			}
			t.Run(downstream+"/"+transport, func(t *testing.T) { runPreviewOptionsAcrossTransportsAndRulesCase(t, &transport, &downstream) })
		}
	}
}

// runPreviewOptionsAcrossTransportsAndRulesCase exercises one isolated test phase with the original shared state.
func runPreviewOptionsAcrossTransportsAndRulesCase(t *testing.T, capturedTransport *string, capturedDownstream *string) {
	t.Helper()

	h, observed, _ := unifiedHarness(t, (*capturedTransport), unifiedRulesEvents)
	publishUnifiedRules(t, h, unifiedRule("options", rules.PhaseRequest, 10,
		unifiedSet("edit-instructions", "/instructions", "from rule"),
		unifiedSet("edit-verbosity", "/text/verbosity", "low"),
		unifiedSet("unsupported", "/metadata", map[string]any{"must": "be dropped"})))
	request := `{"model":"gpt-5.5","input":"hello","stream":false,"store":true,"instructions":"from client","text":{"verbosity":"high","format":{"type":"text"}},"parallel_tool_calls":false,"future_option":{"flag":false,"nullable":null},"temperature":0.5}`
	if (*capturedDownstream) == "ws" {
		events, _ := unifiedWSResponse(t, h, request)
		reportTestFailure(t, unifiedObject(t, events[len(events)-1])["type"] != "response.completed", func() {
			t.Fatalf("request failed: %s", events[len(events)-1])
		})
	} else {
		response, body, _ := capturedHTTPResponse(t, h, request)
		reportTestFailure(t, response.StatusCode != http.StatusOK, func() {
			t.Fatalf("status=%d body=%s", response.StatusCode, body)
		})
	}
	_, raw := observed.get()
	wsUpstream := (*capturedTransport) == "websocket" || (*capturedTransport) == "websocket-cached"
	if wsUpstream {
		observed.mu.Lock()
		raw = append([]byte(nil), observed.wsFrame...)
		observed.mu.Unlock()
	}
	payload := unifiedObject(t, raw)
	reportTestFailure(t, payload["instructions"] != "from rule" || payload["parallel_tool_calls"] != false || payload["stream"] != true || payload["store"] != false, func() {
		t.Fatalf("client options or route constraints changed: %s", raw)
	})
	reportTestFailure(t, !reflect.DeepEqual(payload["text"], map[string]any{"verbosity": "low", "format": map[string]any{"type": "text"}}) || !reflect.DeepEqual(payload["future_option"], map[string]any{"flag": false, "nullable": nil}), func() {
		t.Fatalf("nested options lost: %s", raw)
	})
	for _, field := range []string{"metadata", "temperature", "previous_response_id"} {
		{
			_, exists := payload[field]
			reportTestFailure(t, exists, func() {
				t.Errorf("unsupported HTTP field or rule-reintroduced field %s reached upstream: %s", field, raw)
			})
		}
	}
	reportTestFailure(t, wsUpstream && payload["type"] != "response.create", func() {
		t.Fatalf("missing WS envelope: %s", raw)
	})
	reportTestFailure(t, !wsUpstream && payload["type"] != nil, func() {
		t.Fatalf("WS envelope leaked into HTTP: %s", raw)
	})
}
