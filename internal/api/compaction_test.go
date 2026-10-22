package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"pi-gateway/internal/capture"
	"pi-gateway/internal/rules"
	"pi-gateway/internal/store"
	"pi-gateway/internal/upstream"
)

const compactFixture = `{"id":"cmp_response","object":"response.compaction","created_at":1791300000,"output":[{"id":"msg_retained","type":"message","role":"user","content":[{"type":"input_text","text":"Keep the task requirements"}]},{"id":"cmp_item","type":"compaction","encrypted_content":"opaque+/=State"}],"usage":{"input_tokens":1200,"output_tokens":100,"total_tokens":1300}}`
const compactHistory = `[{"type":"message","role":"user","content":[{"type":"input_text","text":"Keep the task requirements"}]},{"type":"compaction_trigger"}]`

func TestSummaryCompactionHTTPContinuationAndRecompact(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) { runSummaryCompactionHTTPContinuationAndRecompactCase(t, &stream) })
	}
}

func TestSummaryCompactionWebSocket(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/compact") {
			w.WriteHeader(404)
			return
		}
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"WS summary"}]}]}}`+"\n\n")
	}), "websocket-cached")
	setCompactionMode(t, h, "auto")
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.server.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer " + h.key}})
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	{
		err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test","input":`+compactHistory+`}`))
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	for {
		var frame map[string]any
		{
			err := conn.ReadJSON(&frame)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		reportTestFailure(t, frame["type"] == "response.output_text.delta", func() {
			t.Fatal("summary output leaked")
		})
		if frame["type"] == "response.completed" {
			item := frame["response"].(map[string]any)["output"].([]any)[0].(map[string]any)
			reportTestFailure(t, item["type"] != "compaction" || !strings.HasPrefix(item["encrypted_content"].(string), "pi_compact_v1:"), func() {
				t.Fatalf("bad WS compaction: %+v", item)
			})
			break
		}
	}
}

func TestCompactDenialDoesNotBanAccount(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `{"error":{"code":"compact_denied","message":"compaction unavailable"}}`)
	}), "sse")
	setCompactionMode(t, h, "auto")
	resp, _ := postCompactTest(t, h, "/v1/responses/compact", `{"model":"test","input":[]}`)
	reportTestFailure(t, resp.StatusCode != 403, func() {
		t.Fatalf("status=%d", resp.StatusCode)
	})
	a, err := h.store.GetAccount(context.Background(), h.accountID)
	reportTestFailure(t, err != nil || a.Status != store.AccountStatusReady, func() {
		t.Fatalf("operation denial banned account: %+v %v", a, err)
	})
}

func setCompactionMode(t *testing.T, h *testHarness, mode string) {
	t.Helper()
	rt := h.dataPlane.settings.Get()
	rt.CompactionMode = mode
	{
		err := h.dataPlane.settings.Set(context.Background(), rt)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
}

func postCompactTest(t *testing.T, h *testHarness, path, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.server.URL+path, strings.NewReader(body))
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	req.Header.Set("Authorization", "Bearer "+h.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Session_id", "compact-session")
	resp, err := http.DefaultClient.Do(req)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	return resp, raw
}

func assertCompactOutput(t *testing.T, response map[string]any, direct bool) []any {
	t.Helper()
	var want map[string]any
	_ = json.Unmarshal([]byte(compactFixture), &want)
	reportTestFailure(t, !reflect.DeepEqual(response["output"], want["output"]) || !reflect.DeepEqual(response["usage"], want["usage"]), func() {
		t.Fatalf("compacted window or usage changed: %+v", response)
	})
	if direct {
		reportTestFailure(t, !reflect.DeepEqual(response, want), func() {
			t.Fatalf("direct compact response changed: %+v", response)
		})
	} else {
		reportTestFailure(t, response["object"] != "response" || response["status"] != "completed", func() {
			t.Fatalf("expected Responses envelope: %+v", response)
		})
	}
	return response["output"].([]any)
}

func compactSSEOutput(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	reader := upstream.ParseSSE(bytes.NewReader(raw))
	var doneItems []any
	sequence := 0
	for {
		event, err := reader.Next()
		reportTestFailure(t, err != nil, func() {
			t.Fatalf("missing compaction terminal event: %v, %s", err, raw)
		})
		reportTestFailure(t, event.Data["sequence_number"] != float64(sequence), func() {
			t.Fatalf("bad sequence: %+v", event.Data)
		})
		sequence++
		if event.Type == "response.output_item.done" {
			reportTestFailure(t, event.Data["output_index"] != float64(len(doneItems)), func() {
				t.Fatal("bad output index")
			})
			doneItems = append(doneItems, event.Data["item"])
		}
		if event.Type == "response.completed" {
			response := event.Data["response"].(map[string]any)
			reportTestFailure(t, !reflect.DeepEqual(doneItems, response["output"]), func() {
				t.Fatal("items and terminal output disagree")
			})
			return response
		}
	}
}

func TestCompactionHTTPAndNextTurn(t *testing.T) {
	for _, tc := range []struct {
		name, path     string
		direct, stream bool
	}{
		{"trigger-json", "/v1/responses", false, false},
		{"trigger-sse", "/v1/responses", false, true},
		{"direct", "/v1/responses/compact", true, false},
		{"direct-codex-alias", "/backend-api/codex/responses/compact", true, true},
		{"direct-short-alias", "/codex/responses/compact", true, false},
		{"direct-root-alias", "/responses/compact", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) { runCompactionHTTPAndNextTurnCase(t, &tc) })
	}
}

func TestCompactionWSReturnsItems(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reportTestFailure(t, !strings.HasSuffix(r.URL.Path, "/compact") || websocket.IsWebSocketUpgrade(r), func() {
			t.Error("compaction used generation transport")
		})
		_, _ = io.WriteString(w, compactFixture)
	}), "websocket-cached")
	setCompactionMode(t, h, "auto")
	header := http.Header{"Authorization": []string{"Bearer " + h.key}}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.server.URL, "http")+"/v1/responses", header)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	{
		err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test","input":`+compactHistory+`}`))
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	var done []any
	for {
		var frame map[string]any
		{
			err := conn.ReadJSON(&frame)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		if frame["type"] == "response.output_item.done" {
			done = append(done, frame["item"])
		}
		if frame["type"] == "response.completed" {
			response := frame["response"].(map[string]any)
			assertCompactOutput(t, response, false)
			reportTestFailure(t, !reflect.DeepEqual(done, response["output"]), func() {
				t.Fatal("WS items disagree with terminal output")
			})
			break
		}
	}
}

func TestCompactionRejectsIncompleteOrMalformedRequests(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }), "sse")
	setCompactionMode(t, h, "auto")
	for _, tc := range []struct{ path, body string }{
		{"/v1/responses", `{"model":"test","input":[{"type":"compaction_trigger"},{"role":"user","content":"hi"}]}`},
		{"/v1/responses", `{"model":"test","input":` + compactHistory + `,"previous_response_id":"resp_old"}`},
		{"/v1/responses/compact", `{"model":"test","input":[],"previous_response_id":"resp_old"}`},
	} {
		resp, raw := postCompactTest(t, h, tc.path, tc.body)
		reportTestFailure(t, resp.StatusCode != 400 || !bytes.Contains(raw, []byte("invalid_compaction_request")), func() {
			t.Fatalf("status=%d %s", resp.StatusCode, raw)
		})
	}
	reportTestFailure(t, calls.Load() != 0, func() {
		t.Fatal("invalid compact request reached upstream")
	})
}

func TestCompactionCaptureAndRejection(t *testing.T) {
	for _, status := range []int{200, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Request-Id", "req-compact")
				w.WriteHeader(status)
				if status == 200 {
					_, _ = io.WriteString(w, compactFixture)
				} else {
					_, _ = io.WriteString(w, `{"error":{"code":"compact_denied","message":"compaction unavailable"}}`)
				}
			}), "sse")
			setCompactionMode(t, h, "auto")
			resp, raw, record := capturedHTTPResponse(t, h, `{"model":"test","input":`+compactHistory+`,"stream":true}`)
			reportTestFailure(t, resp.StatusCode != status || !strings.HasSuffix(record.URL, "/responses/compact"), func() {
				t.Fatalf("status=%d record=%+v body=%s", resp.StatusCode, record, raw)
			})
			reportTestFailure(t, strings.Contains(record.RequestBody, "compaction_trigger"), func() {
				t.Fatal("trigger reached upstream")
			})
			if status == 200 {
				reportTestFailure(t, record.Outcome != store.OutcomeOK || record.TotalTokens != 1300 || len(captureFrames(record, "in", store.KindHTTPResponse)) != 1, func() {
					t.Fatalf("missing raw response/usage: %+v", record)
				})
				assertCapturedSSEBody(t, record, raw, 7)
			} else {
				reportTestFailure(t, record.Outcome != store.OutcomeError || resp.Header.Get("X-Request-Id") != "req-compact" || !bytes.Contains(raw, []byte("compact_denied")) || len(captureFrames(record, capture.DirClientOut, store.KindSSEEvent)) != 0, func() {
					t.Fatalf("bad rejection: %+v %s", record, raw)
				})
			}
		})
	}
}

func TestCompactionPolicyCannotLoseOperationOrRestoreInstructions(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reportTestFailure(t, !strings.HasSuffix(r.URL.Path, "/compact"), func() {
			t.Error("dropping trigger lost compaction intent")
		})
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		{
			_, exists := body["instructions"]
			reportTestFailure(t, exists, func() {
				t.Error("removed instructions restored after policy")
			})
		}
		reportTestFailure(t, body["model"] != "mapped", func() {
			t.Errorf("model policy lost: %+v", body)
		})
		_, _ = io.WriteString(w, compactFixture)
	}), "sse")
	setCompactionMode(t, h, "auto")
	publishUnifiedRules(t, h, unifiedRule("compact-policy", rules.PhaseRequest, 1,
		unifiedAction("drop-trigger", "drop_input_items", map[string]any{"types": []any{"compaction_trigger"}}),
		unifiedAction("remove-instructions", "json_remove", map[string]any{"paths": []any{"/instructions"}}),
		unifiedSet("mapped", "/model", "mapped"),
	))
	resp, raw := postCompactTest(t, h, "/v1/responses", `{"model":"test","instructions":"removed","input":`+compactHistory+`,"stream":false}`)
	reportTestFailure(t, resp.StatusCode != 200, func() {
		t.Fatalf("status=%d %s", resp.StatusCode, raw)
	})
}

func TestV2FinalizeSelectsActualSummaryModel(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		{
			err := json.NewDecoder(r.Body).Decode(&body)
			reportTestFailure(t, err != nil, func() {
				t.Error(err)
			})
		}
		reportTestFailure(t, strings.HasSuffix(r.URL.Path, "/compact") || body["model"] != "final-summary", func() {
			t.Errorf("finalize model did not reach summary request: path=%s body=%+v", r.URL.Path, body)
		})
		_, _ = io.WriteString(w, summaryTestSSE)
	}), "sse")
	setCompactionMode(t, h, "on")
	publishUnifiedRules(t, h, unifiedRule("summary-finalize", rules.PhaseRequestFinalize, 1,
		unifiedSet("final-model", "/model", "final-summary")))
	resp, raw := postCompactTest(t, h, "/v1/responses/compact", `{"model":"test","input":[]}`)
	reportTestFailure(t, resp.StatusCode != 200, func() {
		t.Fatalf("status=%d %s", resp.StatusCode, raw)
	})
}

func TestCompaction429RetryKeepsHTTPCompaction(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reportTestFailure(t, !strings.HasSuffix(r.URL.Path, "/compact") || websocket.IsWebSocketUpgrade(r), func() {
			t.Error("retry lost HTTP compact operation")
		})
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "90")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"limited"}}`)
			return
		}
		_, _ = io.WriteString(w, compactFixture)
	}), "websocket-cached")
	setCompactionMode(t, h, "auto")
	ctx := context.Background()
	a := &store.Account{Name: "second", AccountID: "acct-second", AccessToken: fakeJWT(t, "acct-second"), ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Enabled: true, Weight: 1, Concurrency: 3, Status: store.AccountStatusReady}
	{
		err := h.store.CreateAccount(ctx, a)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	rt := h.dataPlane.settings.Get()
	rt.SwitchOn429, rt.MaxAttempts = true, 2
	{
		err := h.dataPlane.settings.Set(ctx, rt)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	h.dataPlane.accounts.SetSettings(rt)
	resp, raw := postCompactTest(t, h, "/v1/responses", `{"model":"test","input":`+compactHistory+`,"stream":false}`)
	reportTestFailure(t, resp.StatusCode != 200 || calls.Load() != 2, func() {
		t.Fatalf("status=%d calls=%d %s", resp.StatusCode, calls.Load(), raw)
	})
	var response map[string]any
	_ = json.Unmarshal(raw, &response)
	assertCompactOutput(t, response, false)
	assertCompactionUsage(t, h)
}

func assertCompactionUsage(t *testing.T, h *testHarness) {
	t.Helper()
	records, _, err := h.store.ListUsageRecords(t.Context(), store.UsageFilter{})
	reportTestFailure(t, err != nil || len(records) == 0, func() {
		t.Fatalf("missing compaction usage: %v", err)
	})
	for _, record := range records {
		reportTestFailure(t, record.RequestKind != "compaction", func() {
			t.Fatalf("compaction ledger missing request type: %+v", record)
		})
	}
}

// runSummaryCompactionHTTPContinuationAndRecompactCase exercises one isolated test phase with the original shared state.
func runSummaryCompactionHTTPContinuationAndRecompactCase(t *testing.T, capturedStream *bool) {
	t.Helper()

	var compactCalls, summaryCalls, generationCalls atomic.Int32
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		encoded, _ := json.Marshal(body)
		reportTestFailure(t, bytes.Contains(encoded, []byte("pi_compact_v1:")), func() {
			t.Error("gateway item reached upstream")
		})
		if strings.HasSuffix(r.URL.Path, "/compact") {
			reportTestFailure(t, compactCalls.Add(1) > 1 && !bytes.Contains(encoded, []byte("summary of requirements")), func() {
				t.Error("recompact lost summary")
			})
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"error":{"message":"This ChatPass credential is not authorized for the requested operation."}}`)
			return
		}
		if bytes.Contains(encoded, []byte("CONTEXT CHECKPOINT COMPACTION")) {
			summaryCalls.Add(1)
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary of requirements"}]}],"usage":{"input_tokens":30,"output_tokens":8,"total_tokens":38}}}`+"\n\n")
			return
		}
		generationCalls.Add(1)
		reportTestFailure(t, !bytes.Contains(encoded, []byte("summary of requirements")) || !bytes.Contains(encoded, []byte("continue task")), func() {
			t.Error("next turn lost history or new input")
		})
		_, _ = io.WriteString(w, terminalSSE)
	}), "sse")
	setCompactionMode(t, h, "auto")
	path := "/v1/responses/compact"
	if *capturedStream {
		path = "/v1/responses"
	}
	resp, raw := postCompactTest(t, h, path, fmt.Sprintf(`{"model":"test","input":%s,"stream":%t}`, compactHistory, (*capturedStream)))
	reportTestFailure(t, resp.StatusCode != 200, func() {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	})
	var compact map[string]any
	if *capturedStream {
		compact = compactSSEOutput(t, raw)
	} else {
		_ = json.Unmarshal(raw, &compact)
	}
	reportTestFailure(t, compact["usage"].(map[string]any)["total_tokens"] != float64(38), func() {
		t.Fatal("real summary usage missing")
	})
	assertCompactionUsage(t, h)
	output := compact["output"].([]any)
	next := append(append([]any(nil), output...), map[string]any{"role": "user", "content": "continue task"})
	body, _ := json.Marshal(map[string]any{"model": "test", "input": next, "stream": false})
	resp, raw = postCompactTest(t, h, "/v1/responses", string(body))
	reportTestFailure(t, resp.StatusCode != 200, func() {
		t.Fatalf("next status=%d body=%s", resp.StatusCode, raw)
	})
	body, _ = json.Marshal(map[string]any{"model": "test", "input": next})
	resp, raw = postCompactTest(t, h, "/v1/responses/compact", string(body))
	reportTestFailure(t, resp.StatusCode != 200, func() {
		t.Fatalf("recompact status=%d body=%s", resp.StatusCode, raw)
	})
	reportTestFailure(t, compactCalls.Load() != 2 || summaryCalls.Load() != 2 || generationCalls.Load() != 1, func() {
		t.Fatal("unexpected executions")
	})
	a, err := h.store.GetAccount(context.Background(), h.accountID)
	reportTestFailure(t, err != nil || a.Status != store.AccountStatusReady || a.ConsecutiveFailures != 0, func() {
		t.Fatalf("fallback poisoned account: %+v %v", a, err)
	})
}

// runCompactionHTTPAndNextTurnCase exercises one isolated test phase with the original shared state.
func runCompactionHTTPAndNextTurnCase(t *testing.T, capturedTc *struct {
	name   string
	path   string
	direct bool
	stream bool
}) {
	t.Helper()

	var calls atomic.Int32
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		{
			err := json.NewDecoder(r.Body).Decode(&body)
			reportTestFailure(t, err != nil, func() {
				t.Error(err)
			})
		}
		if strings.HasSuffix(r.URL.Path, "/compact") {
			reportTestFailure(t, r.URL.Path != "/backend-api/codex/responses/compact" || r.Method != "POST", func() {
				t.Errorf("bad endpoint %s", r.URL)
			})
			reportTestFailure(t, !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "), func() {
				t.Error("missing upstream credential")
			})
			for _, field := range []string{"stream", "store", "tools", "tool_choice", "reasoning", "include", "previous_response_id"} {
				{
					_, ok := body[field]
					reportTestFailure(t, ok, func() {
						t.Errorf("create field leaked to compact: %s", field)
					})
				}
			}
			items, _ := body["input"].([]any)
			reportTestFailure(t, len(items) != 1 || body["instructions"] != "Preserve requirements" || body["prompt_cache_key"] != "compact-session", func() {
				t.Errorf("bad compact input: %+v", body)
			})
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, compactFixture)
			return
		}
		items, _ := body["input"].([]any)
		reportTestFailure(t, len(items) != 3 || items[1].(map[string]any)["encrypted_content"] != "opaque+/=State" || items[0].(map[string]any)["id"] != "msg_retained", func() {
			t.Errorf("next-turn input changed: %+v", body)
		})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, terminalSSE)
	}), "websocket-cached")
	setCompactionMode(t, h, "auto")
	body := fmt.Sprintf(`{"model":"test","input":%s,"instructions":"Preserve requirements","stream":%t,"tools":[{"type":"function","name":"dangerous","parameters":{}}]}`, compactHistory, (*capturedTc).stream)
	resp, raw := postCompactTest(t, h, (*capturedTc).path, body)
	reportTestFailure(t, resp.StatusCode != 200, func() {
		t.Fatalf("status=%d %s", resp.StatusCode, raw)
	})
	var response map[string]any
	if (*capturedTc).stream && !(*capturedTc).direct {
		response = compactSSEOutput(t, raw)
	} else {
		err := json.Unmarshal(raw, &response)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	output := assertCompactOutput(t, response, (*capturedTc).direct)
	assertCompactionUsage(t, h)
	// A compacted window is full input, not a continuation of the synthetic
	// response ID. Explicit SSE keeps this test independent of socket mocks.
	rt := h.dataPlane.settings.Get()
	rt.UpstreamTransport = "sse"
	{
		err := h.dataPlane.settings.Set(context.Background(), rt)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	nextInput := append(output, map[string]any{"role": "user", "content": "Continue"})
	next, _ := json.Marshal(map[string]any{"model": "test", "input": nextInput, "stream": false})
	resp, raw = postCompactTest(t, h, "/v1/responses", string(next))
	reportTestFailure(t, resp.StatusCode != 200 || calls.Load() != 2, func() {
		t.Fatalf("next turn: status=%d calls=%d %s", resp.StatusCode, calls.Load(), raw)
	})
}
