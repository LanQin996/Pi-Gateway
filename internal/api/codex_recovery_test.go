package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"pi-gateway/internal/codexbridge"
	"pi-gateway/internal/config"
	"pi-gateway/internal/piwire"
	"pi-gateway/internal/rules"
	"pi-gateway/internal/store"
	"pi-gateway/internal/upstream"
)

func bridgeRecoveryCipher() string {
	b := make([]byte, 73)
	b[0] = 128
	return base64.URLEncoding.EncodeToString(b)
}
func bridgeRecoveryRequest() map[string]any {
	request := bridgeTestRequest()
	input := request["input"].([]any)
	message := input[1].(map[string]any)
	message["content"] = append(message["content"].([]any), map[string]any{"type": "encrypted_content", "encrypted_content": "private-agent-cipher"})
	call := bridgeTestCall(codexbridge.Namespace)
	call["arguments"] = bridgeJSONForRecovery(bridgeRecoveryCipher())
	request["input"] = append(input, call, map[string]any{"type": "function_call_output", "call_id": "call_bridge", "output": "already executed; never replay"})
	return request
}
func bridgeJSONForRecovery(message string) string {
	raw, _ := json.Marshal(map[string]any{"task_name": "worker", "message": message})
	return string(raw)
}

func TestCodexRecoveryProtocolMatrix(t *testing.T) {
	for _, transport := range []string{"sse", "websocket", "websocket-cached", "auto-fallback-sse"} {
		for _, downstream := range []string{"sse", "json", "ws"} {
			if transport == "auto-fallback-sse" && downstream == "ws" {
				continue
			}
			t.Run(downstream+"/"+transport, func(t *testing.T) {
				h, observed, calls := unifiedHarness(t, transport, bridgeTestEvents(t))
				request := bridgeRecoveryRequest()
				request["stream"] = downstream != "json"
				if downstream == "ws" {
					events, _ := unifiedWSResponse(t, h, bridgeJSON(t, request))
					last := unifiedObject(t, events[len(events)-1])
					if last["type"] != "response.completed" {
						t.Fatalf("terminal=%v", last["type"])
					}
				} else {
					status, raw := gatewayHTTP(t, h, request)
					if status != 200 {
						t.Fatalf("status=%d body=%s", status, raw)
					}
				}
				_, raw := observed.get()
				if transport == "websocket" || transport == "websocket-cached" {
					raw = observed.getWS()
				}
				wire := unifiedObject(t, raw)
				input := wire["input"].([]any)
				if len(input) != 4 || input[2].(map[string]any)["call_id"] != "call_bridge" || input[3].(map[string]any)["output"] != "already executed; never replay" {
					t.Fatal("tool pairing/results changed")
				}
				if strings.Contains(string(raw), "private-agent-cipher") || strings.Contains(string(raw), bridgeRecoveryCipher()) || !strings.Contains(string(raw), codexbridge.UnavailableHistory) {
					t.Fatal("cipher leaked or unavailable notice missing")
				}
				if calls.Load() < 1 {
					t.Fatal("request not sent")
				}
				if !strings.Contains(bridgeJSON(t, request), "private-agent-cipher") {
					t.Fatal("client original mutated")
				}
			})
		}
	}
}

func TestCodexRecoveryResponseCacheAndTenantIsolation(t *testing.T) {
	s := &Server{cfg: config.Default(), codexRecovery: codexbridge.NewRecovery()}
	profile := codexbridge.Profile{Version: 1, Tools: codexbridge.ToolSpawnAgent}
	makePrepared := func(key int64, session string) *prepared {
		body := map[string]any{"model": "test", "input": []any{bridgeTestCall(codexbridge.Namespace)}}
		item := body["input"].([]any)[0].(map[string]any)
		item["arguments"] = bridgeJSONForRecovery(bridgeRecoveryCipher())
		ordered := piwire.NewOrderedMap()
		for k, v := range body {
			ordered.Set(k, v)
		}
		return &prepared{Key: &store.APIKey{ID: key}, ClientBody: map[string]any{"prompt_cache_key": session}, CodexBridge: profile, Built: &piwire.BuiltRequest{Body: ordered}}
	}
	p := makePrepared(1, "session-a")
	p.WantsStream = true
	p.CodexRecovery = s.codexRecovery
	p.CodexRecoveryScope = codexRecoveryScope(p)
	// Cache the final transformed response, including the original argument bytes.
	event := unifiedObject(t, []byte(bridgeTestEvents(t)[3]))
	if _, err := p.transformResponseEvent(context.Background(), &upstream.Event{Type: "response.output_item.done", Data: event}); err != nil {
		t.Fatal(err)
	}
	if err := s.prepareCodexBridge(p); err != nil {
		t.Fatal(err)
	}
	input, _ := p.Built.Body.Get("input")
	item := input.([]any)[0].(map[string]any)
	var got, want map[string]any
	_ = json.Unmarshal([]byte(item["arguments"].(string)), &got)
	_ = json.Unmarshal([]byte(bridgeTestCall(codexbridge.Alias)["arguments"].(string)), &want)
	if got["message"] != want["message"] {
		t.Fatal("cached plaintext was not recovered")
	}
	for _, other := range []*prepared{makePrepared(2, "session-a"), makePrepared(1, "session-b"), makePrepared(1, "")} {
		if err := s.prepareCodexBridge(other); err == nil || err.Code != "codex_collaboration_encrypted" {
			t.Fatal("cross-scope/unresolved recovery permitted")
		}
	}
	newPolicy := makePrepared(1, "session-a")
	newPolicy.RuleVersion = 1
	if err := s.prepareCodexBridge(newPolicy); err == nil || err.Code != "codex_collaboration_encrypted" {
		t.Fatal("plaintext from an earlier policy revision was reused")
	}
	// A new encrypted output must fail closed, even with recovery enabled.
	bad := bridgeTestCall(codexbridge.Alias)
	bad["arguments"] = bridgeJSONForRecovery(bridgeRecoveryCipher())
	if _, err := p.transformResponseBody(context.Background(), map[string]any{"output": []any{bad}}); err == nil {
		t.Fatal("new ciphertext was allowed")
	}
}

func TestCodexRecoveryAggregatedResponsePolicy(t *testing.T) {
	h, observed, _ := unifiedHarness(t, "sse", bridgeTestEvents(t))
	publishUnifiedRules(t, h, unifiedRule("redact-call", rules.PhaseResponseBody, 10,
		unifiedSet("redact-arguments", "/output/0/arguments", bridgeJSONForRecovery("policy-redacted"))))
	request := bridgeTestRequest()
	request["stream"] = false
	request["prompt_cache_key"] = "policy-isolated-session"
	status, raw := gatewayHTTP(t, h, request)
	if status != http.StatusOK || !strings.Contains(string(raw), "policy-redacted") {
		t.Fatalf("response redaction failed: status=%d body=%s", status, raw)
	}
	// The final body changed plaintext that appeared in upstream events.
	// A replay must recover only the final, policy-approved message.
	item := bridgeTestCall(codexbridge.Namespace)
	item["arguments"] = bridgeJSONForRecovery(bridgeRecoveryCipher())
	request["input"] = []any{item}
	status, raw = gatewayHTTP(t, h, request)
	if status != http.StatusOK {
		t.Fatalf("body policy bypassed via event cache: status=%d", status)
	}
	_, wire := observed.get()
	if !strings.Contains(string(wire), "policy-redacted") || strings.Contains(string(wire), "精确保留参数") {
		t.Fatal("event cache undid final body redaction")
	}
}

func TestCodexRecoveryCompaction(t *testing.T) {
	for _, mode := range []string{"off", "on", "auto"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				calls.Add(1)
				if strings.Contains(string(raw), "private-agent-cipher") || strings.Contains(string(raw), bridgeRecoveryCipher()) {
					t.Error("compaction forwarded ciphertext history")
				}
				if strings.HasSuffix(r.URL.Path, "/compact") {
					if mode == "auto" {
						http.Error(w, "unsupported", 404)
						return
					}
					writeJSON(w, 200, map[string]any{"object": "response.compaction", "output": []any{map[string]any{"type": "compaction", "encrypted_content": "native-compact-preserved"}}})
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp_recovery_summary","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"visible progress; encrypted history unavailable"}]}]}}`+"\n\n")
			}), "sse")
			setCompactionMode(t, h, mode)
			response, raw := postCompactTest(t, h, "/v1/responses/compact", bridgeJSON(t, bridgeRecoveryRequest()))
			if response.StatusCode != 200 || calls.Load() == 0 {
				t.Fatalf("status=%d body=%s", response.StatusCode, raw)
			}
			if mode == "off" && !strings.Contains(string(raw), "native-compact-preserved") {
				t.Fatal("native compaction damaged")
			}
		})
	}
}
