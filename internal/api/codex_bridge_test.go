package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pi-gateway/internal/capture"
	"pi-gateway/internal/codexbridge"
	"pi-gateway/internal/piwire"
	"pi-gateway/internal/store"
	"pi-gateway/internal/upstream"
)

func bridgeTestTools() []any {
	var members []any
	for _, name := range []string{"spawn_agent", "send_message", "followup_task"} {
		members = append(members, map[string]any{
			"type": "function", "name": name,
			"parameters": map[string]any{"type": "object", "properties": map[string]any{
				"message": map[string]any{"type": "string", "encrypted": true},
			}},
		})
	}
	members = append(members, map[string]any{"type": "function", "name": "wait_agent", "parameters": map[string]any{"type": "object"}})
	return []any{map[string]any{"type": "namespace", "name": codexbridge.Namespace, "tools": members}}
}

func bridgeTestMessage(kind, payload string) map[string]any {
	return map[string]any{"type": "agent_message", "id": "amsg_private", "author": "/root", "recipient": "/root/worker",
		"internal_chat_message_metadata_passthrough": map[string]any{"turn_id": "private-turn"},
		"content": []any{map[string]any{"type": "input_text", "text": "Message Type: " + kind + "\nTask name: /root/worker\nSender: /root\nPayload:\n" + payload}}}
}

func bridgeTestRequest() map[string]any {
	return map[string]any{"model": "gpt-5.5", "input": []any{
		map[string]any{"type": "additional_tools", "role": "developer", "tools": bridgeTestTools()},
		bridgeTestMessage("NEW_TASK", "完整独有任务：统计三行"),
	}, "stream": true}
}

func bridgeTestCall(namespace string) map[string]any {
	return map[string]any{"type": "function_call", "id": "fc_bridge", "call_id": "call_bridge", "namespace": namespace,
		"name": "spawn_agent", "arguments": "{\"task_name\":\"worker\", \"message\":\"精确保留参数\\n任务\"}"}
}

func bridgeJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	return string(raw)
}

func bridgeTestEvents(t *testing.T) []string {
	t.Helper()
	call := bridgeTestCall(codexbridge.Alias)
	added := bridgeTestCall(codexbridge.Alias)
	added["arguments"] = ""
	return []string{
		bridgeJSON(t, map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_bridge", "status": "in_progress"}}),
		bridgeJSON(t, map[string]any{"type": "response.output_item.added", "output_index": 0, "item": added}),
		bridgeJSON(t, map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc_bridge", "output_index": 0, "delta": call["arguments"]}),
		bridgeJSON(t, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": call}),
		bridgeJSON(t, map[string]any{"type": "response.completed", "response": map[string]any{
			"id": "resp_bridge", "status": "completed", "output": []any{call},
			"usage": map[string]any{"input_tokens": 11, "output_tokens": 7, "total_tokens": 18},
		}}),
	}
}

func assertBridgeCall(t *testing.T, call map[string]any) {
	t.Helper()
	reportTestFailure(t, call["namespace"] != codexbridge.Namespace || call["name"] != "spawn_agent" || call["call_id"] != "call_bridge" || call["id"] != "fc_bridge", func() {
		t.Fatalf("client tool identity changed: %+v", call)
	})
	{
		marker, ok := call["encrypted_function_args"].([]any)
		reportTestFailure(t, !ok || len(marker) != 0, func() {
			t.Fatalf("Codex requires a literal empty encryption marker: %+v", call)
		})
	}
	reportTestFailure(t, call["arguments"] != "" && call["arguments"] != bridgeTestCall(codexbridge.Alias)["arguments"], func() {
		t.Fatalf("tool arguments were reserialized: %q", call["arguments"])
	})
}

func assertBridgeWire(t *testing.T, body map[string]any) {
	t.Helper()
	input, _ := body["input"].([]any)
	reportTestFailure(t, len(input) != 2, func() {
		t.Fatalf("input changed length: %+v", body)
	})
	declarations := input[0].(map[string]any)["tools"].([]any)
	namespace := declarations[0].(map[string]any)
	reportTestFailure(t, namespace["name"] != codexbridge.Alias, func() {
		t.Fatalf("native collaboration reached upstream: %+v", namespace)
	})
	for _, value := range namespace["tools"].([]any) {
		tool := value.(map[string]any)
		if tool["name"] == "wait_agent" {
			continue
		}
		message := tool["parameters"].(map[string]any)["properties"].(map[string]any)["message"].(map[string]any)
		{
			_, exists := message["encrypted"]
			reportTestFailure(t, exists, func() {
				t.Fatalf("encrypted schema reached upstream: %+v", message)
			})
		}
	}
	message := input[1].(map[string]any)
	reportTestFailure(t, message["type"] != "message" || message["role"] != "user" || message["id"] != nil || message["author"] != nil || message["recipient"] != nil || message["internal_chat_message_metadata_passthrough"] != nil, func() {
		t.Fatalf("agent message did not become a portable message: %+v", message)
	})
	reportTestFailure(t, !strings.Contains(bridgeJSON(t, message["content"]), "完整独有任务：统计三行") || !strings.Contains(bridgeJSON(t, message["content"]), "NEW_TASK"), func() {
		t.Fatalf("delegation task was lost: %+v", message)
	})
}

func TestCodexBridgeProtocolMatrix(t *testing.T) {
	for _, transport := range []string{"sse", "websocket", "websocket-cached", "auto-fallback-sse"} {
		for _, downstream := range []string{"sse", "json", "ws"} {
			if transport == "auto-fallback-sse" && downstream == "ws" {
				continue
			}
			t.Run(downstream+"/"+transport, func(t *testing.T) { runCodexBridgeProtocolMatrixCase(t, &transport, &downstream) })
		}
	}
}

func TestCodexBridgeRejectsOldCipherBeforeUpstream(t *testing.T) {
	for _, downstream := range []string{"http", "ws"} {
		t.Run(downstream, func(t *testing.T) {
			h, _, calls := unifiedHarness(t, "sse", bridgeTestEvents(t))
			h.baseConfig.Upstream.CodexHistoryRecovery = false
			request := bridgeTestRequest()
			message := request["input"].([]any)[1].(map[string]any)
			message["content"] = []any{map[string]any{"type": "encrypted_content", "encrypted_content": "private-cipher-content"}}
			var code string
			if downstream == "ws" {
				events, _ := unifiedWSResponse(t, h, bridgeJSON(t, request))
				last := unifiedObject(t, events[len(events)-1])
				code, _ = last["error"].(map[string]any)["code"].(string)
			} else {
				status, raw := gatewayHTTP(t, h, request)
				reportTestFailure(t, status != http.StatusBadRequest, func() {
					t.Fatalf("status=%d body=%s", status, raw)
				})
				reportTestFailure(t, strings.Contains(string(raw), "private-cipher-content"), func() {
					t.Fatalf("error leaked encrypted task: %s", raw)
				})
				code, _ = unifiedObject(t, raw)["error"].(map[string]any)["code"].(string)
			}
			reportTestFailure(t, code != "codex_agent_message_encrypted" || calls.Load() != 0, func() {
				t.Fatalf("cipher request reached upstream: code=%q calls=%d", code, calls.Load())
			})
		})
	}
}

func TestCodexBridgeDisabledAndRuleless(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		h, observed, _ := unifiedHarness(t, "sse", bridgeTestEvents(t))
		h.baseConfig.Upstream.CodexMultiAgentCompat = false
		request := bridgeTestRequest()
		status, _ := gatewayHTTP(t, h, request)
		reportTestFailure(t, status != http.StatusOK, func() {
			t.Fatalf("disabled passthrough status=%d", status)
		})
		_, raw := observed.get()
		wire := unifiedObject(t, raw)
		reportTestFailure(t, wire["input"].([]any)[1].(map[string]any)["type"] != "agent_message", func() {
			t.Fatalf("disabled bridge modified request: %s", raw)
		})
	})
	t.Run("ruleless", func(t *testing.T) {
		profile, err := codexbridge.Detect(bridgeTestRequest())
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
		p := &prepared{CodexBridge: profile, Built: &piwire.BuiltRequest{}}
		original := unifiedObject(t, []byte(bridgeTestEvents(t)[3]))
		raw := []byte(bridgeJSON(t, original))
		event, err := p.transformResponseEvent(context.Background(), &upstream.Event{Type: "response.output_item.done", Data: original, Raw: raw})
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
		assertBridgeCall(t, event.Data["item"].(map[string]any))
		reportTestFailure(t, original["item"].(map[string]any)["namespace"] != codexbridge.Alias || string(raw) != bridgeJSON(t, original), func() {
			t.Fatal("bridge modified original response event")
		})
	})
}

func TestCodexBridgeRulesCannotReintroduceEncryptedArguments(t *testing.T) {
	h, _, _ := unifiedHarness(t, "sse", bridgeTestEvents(t))
	publishUnifiedRules(t, h, unifiedEventRule("bad-marker", "response.output_item.done",
		unifiedSet("cipher", "/item/encrypted_function_args", []any{"message"})))
	request := bridgeTestRequest()
	request["stream"] = false
	status, raw := gatewayHTTP(t, h, request)
	reportTestFailure(t, status != http.StatusBadGateway || !strings.Contains(string(raw), "codex_collaboration_encrypted"), func() {
		t.Fatalf("rule bypassed safe plaintext protocol: status=%d body=%s", status, raw)
	})
}

func TestCodexBridgeCachedClientHistoryAndExplicitDelta(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%v", explicit), func(t *testing.T) {
			backend := newGatewayContinuationBackend()
			backend.output = []any{bridgeTestCall(codexbridge.Alias)}
			h := newGatewayContinuationHarness(t, backend, nil)
			conn := dialGatewayContinuation(t, h, h.key)
			input := []any{gatewayUser("first")}
			firstRequest := gatewayCreate(input, "", "")
			firstRequest["tools"] = bridgeTestTools()
			first := gatewayExchange(t, conn, firstRequest)
			id := gatewayResponseID(t, first)
			firstTurn := gatewayObservedTurn(t, backend)
			clientCall := first["response"].(map[string]any)["output"].([]any)[0].(map[string]any)
			assertBridgeCall(t, clientCall)
			result := map[string]any{"type": "function_call_output", "call_id": "call_bridge", "output": "spawned"}
			secondRequest := gatewayCreate([]any{result}, id, "")
			if !explicit {
				secondRequest = gatewayCreate([]any{gatewayUser("first"), clientCall, result}, "", "")
				secondRequest["tools"] = bridgeTestTools()
			}
			second := gatewayExchange(t, conn, secondRequest)
			_ = gatewayResponseID(t, second)
			secondTurn := gatewayObservedTurn(t, backend)
			reportTestFailure(t, firstTurn.connection != secondTurn.connection || secondTurn.body["previous_response_id"] != id, func() {
				t.Fatalf("client-restored call did not continue original upstream socket: %+v", secondTurn)
			})
			delta := secondTurn.body["input"].([]any)
			reportTestFailure(t, len(delta) != 1 || !reflect.DeepEqual(delta[0], result), func() {
				t.Fatalf("replayed client history did not reduce to exact tool-result delta: %+v", secondTurn.body)
			})
			if !explicit {
				namespace := secondTurn.body["tools"].([]any)[0].(map[string]any)
				reportTestFailure(t, namespace["name"] != codexbridge.Alias, func() {
					t.Fatalf("history lost bridge tool namespace: %+v", namespace)
				})
			}
		})
	}
}

func TestCodexBridgeCompactionModesAndFallback(t *testing.T) {
	for _, mode := range []string{"off", "on", "auto"} {
		t.Run(mode, func(t *testing.T) { runCodexBridgeCompactionModesAndFallbackCase(t, &mode) })
	}
}

func TestCodexBridgeCapturePreservesRawResponse(t *testing.T) {
	h, _, _ := unifiedHarness(t, "sse", bridgeTestEvents(t))
	request := bridgeTestRequest()
	request["stream"] = false
	_, _, record := capturedHTTPResponse(t, h, bridgeJSON(t, request))
	rawFrames := captureFrames(record, "in", "sse_event")
	reportTestFailure(t, len(rawFrames) == 0, func() {
		t.Fatal("raw upstream capture missing")
	})
	var rawCall map[string]any
	for _, frame := range rawFrames {
		value := unifiedObject(t, captureFrameBytes(t, frame))
		if value["type"] == "response.output_item.done" {
			rawCall = value["item"].(map[string]any)
		}
	}
	reportTestFailure(t, rawCall == nil || rawCall["namespace"] != codexbridge.Alias || rawCall["encrypted_function_args"] != nil, func() {
		t.Fatalf("raw response was modified by client restoration: %+v", rawCall)
	})
	reportTestFailure(t, len(captureFrames(record, capture.DirClientOut, "http_response")) == 0, func() {
		t.Fatal("client response capture missing")
	})
}

func TestCodexBridge429RetriesKeepWireProfile(t *testing.T) {
	for _, switchAccount := range []bool{false, true} {
		t.Run(fmt.Sprintf("switch=%v", switchAccount), func(t *testing.T) { runCodexBridge429RetriesKeepWireProfileCase(t, &switchAccount) })
	}
}

func TestCodexBridgeNativeCompactWindowRestoresVisibleCalls(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := unifiedObject(t, raw)
		input, _ := body["input"].([]any)
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, "/compact") {
			writeJSON(w, http.StatusOK, map[string]any{"id": "cmp_visible", "object": "response.compaction", "output": []any{
				input[0], bridgeTestCall(codexbridge.Alias),
				map[string]any{"type": "compaction", "encrypted_content": "opaque-visible-window"},
			}})
			return
		}
		// Replay the actual client-visible compact window, not the raw alias
		// fixture. Declarations must survive Restore -> Detect -> Forward.
		if len(input) != 5 {
			t.Errorf("compacted window changed size: %s", raw)
			return
		}
		namespace := input[0].(map[string]any)["tools"].([]any)[0].(map[string]any)
		reportTestFailure(t, namespace["name"] != codexbridge.Alias, func() {
			t.Errorf("compacted declarations did not return to upstream domain: %s", raw)
		})
		call := input[1].(map[string]any)
		reportTestFailure(t, call["namespace"] != codexbridge.Alias || call["encrypted_function_args"] != nil, func() {
			t.Errorf("compacted tool identity was not forwarded: %+v", call)
		})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, terminalSSE)
	}), "sse")
	setCompactionMode(t, h, "off")
	response, raw := postCompactTest(t, h, "/v1/responses/compact", bridgeJSON(t, bridgeTestRequest()))
	reportTestFailure(t, response.StatusCode != http.StatusOK, func() {
		t.Fatalf("status=%d body=%s", response.StatusCode, raw)
	})
	output := unifiedObject(t, raw)["output"].([]any)
	declaration := output[0].(map[string]any)["tools"].([]any)[0].(map[string]any)
	reportTestFailure(t, declaration["name"] != codexbridge.Namespace, func() {
		t.Fatalf("compact window leaked reserved alias: %+v", declaration)
	})
	assertBridgeCall(t, output[1].(map[string]any))
	reportTestFailure(t, output[2].(map[string]any)["encrypted_content"] != "opaque-visible-window", func() {
		t.Fatalf("native compaction ciphertext was damaged: %s", raw)
	})
	continued := append(append([]any(nil), output...), map[string]any{"type": "function_call_output", "call_id": "call_bridge", "output": "spawned"}, gatewayUser("next after compact"))
	status, next := gatewayHTTP(t, h, map[string]any{"model": "gpt-5.5", "input": continued, "stream": false})
	reportTestFailure(t, status != http.StatusOK || calls.Load() != 2, func() {
		t.Fatalf("restored compact window could not be replayed: status=%d calls=%d body=%s", status, calls.Load(), next)
	})
}

// runCodexBridgeProtocolMatrixCase exercises one isolated test phase with the original shared state.
func runCodexBridgeProtocolMatrixCase(t *testing.T, capturedTransport *string, capturedDownstream *string) {
	t.Helper()

	h, observed, _ := unifiedHarness(t, (*capturedTransport), bridgeTestEvents(t))
	request := bridgeTestRequest()
	request["stream"] = (*capturedDownstream) != "json"
	var terminal map[string]any
	func() { verifyCodexBridgeProtocolMatrix(&t, &capturedDownstream, &h, &request, &terminal) }()
	reportTestFailure(t, terminal == nil, func() {
		t.Fatal("missing terminal response")
	})
	assertBridgeCall(t, terminal["output"].([]any)[0].(map[string]any))
	_, raw := observed.get()
	if (*capturedTransport) == "websocket" || (*capturedTransport) == "websocket-cached" {
		raw = observed.getWS()
	}
	assertBridgeWire(t, unifiedObject(t, raw))
}

// runCodexBridgeCompactionModesAndFallbackCase exercises one isolated test phase with the original shared state.
func runCodexBridgeCompactionModesAndFallbackCase(t *testing.T, capturedMode *string) {
	t.Helper()

	var calls atomic.Int32
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := unifiedObject(t, raw)
		input, _ := body["input"].([]any)
		reportTestFailure(t, len(input) == 0 || input[0].(map[string]any)["type"] != "message" || strings.Contains(string(raw), `"type":"agent_message"`), func() {
			t.Errorf("compaction forwarded unsupported agent item: %s", raw)
		})
		calls.Add(1)
		if (*capturedMode) == "auto" && strings.HasSuffix(r.URL.Path, "/compact") {
			http.Error(w, "unsupported", http.StatusNotFound)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/compact") {
			writeJSON(w, http.StatusOK, map[string]any{"object": "response.compaction", "id": "cmp_bridge", "output": []any{
				map[string]any{"type": "compaction", "id": "cmp_item", "encrypted_content": "native-compact-opaque"},
			}, "usage": map[string]any{"input_tokens": 2, "output_tokens": 1, "total_tokens": 3}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp_bridge_summary","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"代理任务与结果摘要"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`+"\n\n")
	}), "sse")
	setCompactionMode(t, h, (*capturedMode))
	response, raw := postCompactTest(t, h, "/v1/responses/compact", bridgeJSON(t, map[string]any{
		"model": "gpt-5.5", "tools": bridgeTestTools(), "input": []any{bridgeTestMessage("FINAL_ANSWER", "子代理结果完整保留")},
	}))
	reportTestFailure(t, response.StatusCode != http.StatusOK, func() {
		t.Fatalf("mode=%s status=%d body=%s", (*capturedMode), response.StatusCode, raw)
	})
	compacted := unifiedObject(t, raw)
	output, _ := compacted["output"].([]any)
	reportTestFailure(t, compacted["object"] != "response.compaction" || len(output) != 1, func() {
		t.Fatalf("compacted history missing: %s", raw)
	})
	item := output[0].(map[string]any)
	reportTestFailure(t, item["type"] != "compaction" || item["encrypted_content"] == nil, func() {
		t.Fatalf("opaque compaction was modified: %+v", item)
	})
	reportTestFailure(t, (*capturedMode) == "off" && item["encrypted_content"] != "native-compact-opaque", func() {
		t.Fatalf("native compact ciphertext was changed: %+v", item)
	})
	wantCalls := int32(1)
	if (*capturedMode) == "auto" {
		wantCalls = 2
	}
	reportTestFailure(t, calls.Load() != wantCalls, func() {
		t.Fatalf("mode=%s calls=%d want=%d", (*capturedMode), calls.Load(), wantCalls)
	})
}

// runCodexBridge429RetriesKeepWireProfileCase exercises one isolated test phase with the original shared state.
func runCodexBridge429RetriesKeepWireProfileCase(t *testing.T, capturedSwitchAccount *bool) {
	t.Helper()

	var calls atomic.Int32
	var mu sync.Mutex
	var tokens []string
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		assertBridgeWire(t, unifiedObject(t, raw))
		mu.Lock()
		tokens = append(tokens, r.Header.Get("Authorization"))
		mu.Unlock()
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After-Ms", "1")
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{"code": "rate_limit_exceeded", "message": "limited"}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range bridgeTestEvents(t) {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}), "sse")
	ctx := context.Background()
	second := &store.Account{Name: "bridge-second", AccountID: "bridge-second", AccessToken: fakeJWT(t, "bridge-second"),
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Enabled: true, Weight: 1, Concurrency: 3, Status: store.AccountStatusReady}
	{
		err := h.store.CreateAccount(ctx, second)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	cooldown := 0
	if *capturedSwitchAccount {
		cooldown = 30
	}
	for _, id := range []int64{h.accountID, second.ID} {
		{
			err := h.store.PatchAccountManagementFields(ctx, id, store.AccountManagementPatch{Cooldown429Seconds: &cooldown})
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
	}
	rt := h.dataPlane.settings.Get()
	rt.SwitchOn429, rt.MaxAttempts = (*capturedSwitchAccount), 2
	{
		err := h.dataPlane.settings.Set(ctx, rt)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	h.dataPlane.accounts.SetSettings(rt)
	request := bridgeTestRequest()
	request["stream"] = false
	status, raw := gatewayHTTP(t, h, request)
	reportTestFailure(t, status != http.StatusOK || calls.Load() != 2, func() {
		t.Fatalf("status=%d calls=%d body=%s", status, calls.Load(), raw)
	})
	assertBridgeCall(t, unifiedObject(t, raw)["output"].([]any)[0].(map[string]any))
	mu.Lock()
	defer mu.Unlock()
	reportTestFailure(t, len(tokens) != 2 || (tokens[0] != tokens[1]) != (*capturedSwitchAccount), func() {
		t.Fatalf("retry account policy changed: switch=%v attempts=%d", (*capturedSwitchAccount), len(tokens))
	})
}

// verifyCodexBridgeProtocolMatrix exercises one isolated test phase with the original shared state.
func verifyCodexBridgeProtocolMatrix(capturedT **testing.T, capturedCapturedDownstream **string, capturedH **testHarness, capturedRequest *map[string]any, capturedTerminal *map[string]any) {
	(*capturedT).Helper()

	if (*(*capturedCapturedDownstream)) == "ws" {
		events, _ := unifiedWSResponse((*capturedT), (*capturedH), bridgeJSON((*capturedT), (*capturedRequest)))
		for _, raw := range events {
			event := unifiedObject((*capturedT), raw)
			if item, ok := event["item"].(map[string]any); ok {
				assertBridgeCall((*capturedT), item)
			}
			if event["type"] == "response.completed" {
				(*capturedTerminal) = event["response"].(map[string]any)
			}
		}
	} else {
		response, raw, _ := capturedHTTPResponse((*capturedT), (*capturedH), bridgeJSON((*capturedT), (*capturedRequest)))
		reportTestFailure((*capturedT), response.StatusCode != http.StatusOK, func() {
			(*capturedT).Fatalf("status=%d: %s", response.StatusCode, raw)
		})
		func() {
			verifyCodexBridgeProtocolMatrix2(&capturedT, &capturedCapturedDownstream, &capturedTerminal, &raw)
		}()
	}
}

// verifyCodexBridgeProtocolMatrix2 exercises one isolated test phase with the original shared state.
func verifyCodexBridgeProtocolMatrix2(capturedCapturedT ***testing.T, capturedCapturedCapturedDownstream ***string, capturedCapturedTerminal **map[string]any, capturedRaw *[]byte) {
	if (*(*(*capturedCapturedCapturedDownstream))) == "json" {
		(*(*capturedCapturedTerminal)) = unifiedObject((*(*capturedCapturedT)), (*capturedRaw))
	} else {
		for _, eventRaw := range unifiedSSEEvents((*(*capturedCapturedT)), (*capturedRaw)) {
			event := unifiedObject((*(*capturedCapturedT)), eventRaw)
			if item, ok := event["item"].(map[string]any); ok {
				assertBridgeCall((*(*capturedCapturedT)), item)
			}
			reportTestFailure((*(*capturedCapturedT)), event["type"] == "response.function_call_arguments.delta" && event["delta"] != bridgeTestCall(codexbridge.Alias)["arguments"], func() {
				(*(*capturedCapturedT)).Fatalf("argument delta changed: %+v", event)
			})
			if event["type"] == "response.completed" {
				(*(*capturedCapturedTerminal)) = event["response"].(map[string]any)
			}
		}
	}
}
