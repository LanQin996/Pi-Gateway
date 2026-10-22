package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pi-gateway/internal/capture"
	"pi-gateway/internal/rules"
	"pi-gateway/internal/store"
)

func postChat(t *testing.T, h *testHarness, body, key string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(raw)
}

func chatBackend(record *upstreamCapture, events ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if record != nil {
			record.set(r, body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "req-chat")
		for _, event := range events {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
			w.(http.Flusher).Flush()
		}
	}
}

const chatTextTerminal = `{"type":"response.completed","response":{"id":"resp_chat","created_at":1700000000,"model":"test-model","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"你好","annotations":[]}]}],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}}`

func TestChatCompletionsJSON(t *testing.T) {
	record := &upstreamCapture{}
	h := newHarness(t, chatBackend(record, chatTextTerminal), "sse")
	resp, raw := postChat(t, h, `{"model":"test-model","messages":[{"role":"system","content":"Be helpful"},{"role":"user","content":"hi"}]}`, h.key)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("status=%d headers=%v body=%s", resp.StatusCode, resp.Header, raw)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result["object"] != "chat.completion" || result["created"] != float64(1700000000) {
		t.Fatalf("unexpected completion: %s", raw)
	}
	choice := result["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["role"] != "assistant" || message["content"] != "你好" || choice["finish_reason"] != "stop" {
		t.Fatalf("unexpected message: %s", raw)
	}
	usage := result["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(11) || usage["completion_tokens"] != float64(7) || usage["total_tokens"] != float64(18) {
		t.Fatalf("unexpected usage: %s", raw)
	}
	if resp.Header.Get("X-Request-Id") != "req-chat" {
		t.Fatalf("missing request id: %v", resp.Header)
	}
	_, request := record.get()
	var upstreamBody map[string]any
	if err := json.Unmarshal(request, &upstreamBody); err != nil {
		t.Fatal(err)
	}
	if upstreamBody["stream"] != true || upstreamBody["store"] != false || upstreamBody["messages"] != nil || len(upstreamBody["input"].([]any)) != 2 {
		t.Fatalf("unexpected Responses payload: %s", request)
	}
}

func TestChatCompletionsStreamToolsAndUsage(t *testing.T) {
	h := newHarness(t, chatBackend(nil,
		`{"type":"response.created","response":{"id":"resp_tools","created_at":1700000000}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"private"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"weather","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"{\"city\":"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"\"Paris\"}"}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"time","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_2","output_index":2,"delta":"{}"}`,
		`{"type":"response.completed","response":{"id":"resp_tools","created_at":1700000000,"status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"weather","arguments":"{\"city\":\"Paris\"}"},{"type":"function_call","id":"fc_2","call_id":"call_2","name":"time","arguments":"{}"}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
	), "sse")
	resp, raw := postChat(t, h, `{"model":"test-model","messages":[{"role":"user","content":"weather and time"}],"stream":true,"stream_options":{"include_usage":true}}`, h.key)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status=%d headers=%v body=%s", resp.StatusCode, resp.Header, raw)
	}
	if !strings.HasSuffix(raw, "data: [DONE]\n\n") || strings.Contains(raw, "private") || strings.Contains(raw, "response.completed") {
		t.Fatalf("invalid chat stream: %s", raw)
	}
	arguments := map[int]string{}
	names := map[int]string{}
	var id any
	var finish any
	usageSeen := false
	for _, frame := range strings.Split(raw, "\n\n") {
		payload := strings.TrimPrefix(frame, "data: ")
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("bad chunk %s: %v", payload, err)
		}
		if chunk["object"] != "chat.completion.chunk" {
			t.Fatalf("unexpected chunk: %s", payload)
		}
		if id == nil {
			id = chunk["id"]
		}
		if chunk["id"] != id || chunk["created"] != float64(1700000000) {
			t.Fatalf("inconsistent metadata: %s", raw)
		}
		choices := chunk["choices"].([]any)
		if len(choices) == 0 {
			usageSeen = chunk["usage"].(map[string]any)["total_tokens"] == float64(7)
			continue
		}
		choice := choices[0].(map[string]any)
		if choice["finish_reason"] != nil {
			finish = choice["finish_reason"]
		}
		delta := choice["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		for _, value := range calls {
			call := value.(map[string]any)
			index := int(call["index"].(float64))
			fn := call["function"].(map[string]any)
			if name, ok := fn["name"].(string); ok {
				names[index] = name
			}
			if args, ok := fn["arguments"].(string); ok {
				arguments[index] += args
			}
		}
	}
	if names[0] != "weather" || names[1] != "time" || arguments[0] != `{"city":"Paris"}` || arguments[1] != "{}" || finish != "tool_calls" || !usageSeen {
		t.Fatalf("tools=%v args=%v finish=%v usage=%v stream=%s", names, arguments, finish, usageSeen, raw)
	}
}

func TestChatCompletionsStreamText(t *testing.T) {
	h := newHarness(t, chatBackend(nil,
		`{"type":"response.created","response":{"id":"resp_chat","created_at":1700000000}}`,
		`{"type":"response.output_text.delta","delta":"你"}`,
		`{"type":"response.output_text.delta","delta":"好"}`,
		chatTextTerminal,
	), "sse")
	_, raw := postChat(t, h, `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`, h.key)
	if strings.Count(raw, `"content":"你"`) != 1 || strings.Count(raw, `"content":"好"`) != 1 || strings.Contains(raw, `"content":"你好"`) || strings.Contains(raw, `"choices":[]`) {
		t.Fatalf("duplicated text or unsolicited usage: %s", raw)
	}
}

func TestChatCompletionsRejectBeforeUpstream(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}), "sse")
	for _, tc := range []struct {
		body string
		key  string
		code int
	}{
		{`{"model":"test","messages":[{"role":"user","content":"hi"}]}`, "", 401},
		{`{"model":"test","messages":[{"role":"user","content":"hi"}],"n":2}`, h.key, 400},
		{`{"model":"test","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{}}]}]}`, h.key, 400},
		{`{"model":"test","messages":[]}`, h.key, 400},
	} {
		resp, raw := postChat(t, h, tc.body, tc.key)
		if resp.StatusCode != tc.code || !strings.Contains(raw, `"error"`) {
			t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, tc.code, raw)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("rejected request reached upstream %d times", calls.Load())
	}
}

func TestChatCompletionsUpstreamErrors(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprint(started), func(t *testing.T) {
			var events []string
			if started {
				events = append(events, `{"type":"response.output_text.delta","delta":"hello"}`)
			}
			events = append(events, `{"type":"response.failed","response":{"id":"resp_error","status":"failed","error":{"code":"server_error","message":"backend failed"}}}`)
			h := newHarness(t, chatBackend(nil, events...), "sse")
			resp, raw := postChat(t, h, `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`, h.key)
			if !strings.Contains(raw, `"error"`) || !strings.Contains(raw, "backend failed") || strings.Contains(raw, `"finish_reason":"stop"`) {
				t.Fatalf("lost failure: status=%d body=%s", resp.StatusCode, raw)
			}
			if started && resp.StatusCode != 200 || !started && resp.StatusCode < 400 {
				t.Fatalf("wrong error commitment: status=%d body=%s", resp.StatusCode, raw)
			}
		})
	}
}

func TestChatCompletionsRulesAndCaptureAcrossTransports(t *testing.T) {
	for _, transport := range []string{"sse", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			h, observed, _ := unifiedHarness(t, transport, unifiedRulesEvents)
			requestRule := unifiedRule("chat-request", rules.PhaseRequest, 1, unifiedSet("instructions", "/instructions", "request rule"))
			bodyRule := unifiedRule("chat-body", rules.PhaseResponseBody, 1, unifiedSet("text", "/output/0/content/0/text", "after-body-rule"))
			publishUnifiedRules(t, h, requestRule, bodyRule)
			request := `{"model":"test-model","messages":[{"role":"user","content":"hello"}],"temperature":0.4,"max_tokens":16}`
			resp, body := postChat(t, h, request, h.key)
			if resp.StatusCode != 200 || !strings.Contains(body, `"content":"after-body-rule"`) {
				t.Fatalf("response body rule was not applied: %d %s", resp.StatusCode, body)
			}
			record := waitForResponseCapture(t, h)
			if transport == "sse" {
				assertCapturedRequestHops(t, record, observed, request, true)
			} else {
				clientFrames := captureFrames(record, capture.DirClientIn, store.KindRequestBody)
				if len(clientFrames) != 1 || !equalCaptureJSON(captureFrameBytes(t, clientFrames[0]), []byte(request)) || record.RequestBody != string(observed.getWS()) {
					t.Fatalf("WebSocket capture lost original or converted request: %+v", record)
				}
			}
			assertCapturedJSONBody(t, record, []byte(body), "chat.completion")
			if record.Outcome != store.OutcomeOK || record.PromptTokens != 11 || record.CompletionTokens != 7 {
				t.Fatalf("usage/capture mismatch: %+v", record)
			}
			var wire map[string]any
			if transport == "sse" {
				_, raw := observed.get()
				if err := json.Unmarshal(raw, &wire); err != nil {
					t.Fatal(err)
				}
			} else if err := json.Unmarshal(observed.getWS(), &wire); err != nil {
				t.Fatal(err)
			}
			if wire["instructions"] != "request rule" || wire["temperature"] != nil || wire["max_output_tokens"] != nil || wire["messages"] != nil {
				t.Fatalf("rule/SiWC constraints bypassed: %+v", wire)
			}
		})
	}
}

func TestChatCompletionsStreamFlushesBeforeTerminal(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, deltaSSE)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, terminalSSE)
	}), "sse")
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("chat output was buffered until terminal: %v", err)
		}
		if strings.Contains(line, `"content":"hello"`) {
			break
		}
	}
}

func TestChatCompletionsStreamCaptureContainsChatFrames(t *testing.T) {
	h := newHarness(t, chatBackend(nil, chatTextTerminal), "sse")
	resp, body := postChat(t, h, `{"model":"test-model","messages":[{"role":"user","content":"hello"}],"stream":true}`, h.key)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	record := waitForResponseCapture(t, h)
	var captured strings.Builder
	for _, frame := range captureFrames(record, capture.DirClientOut, store.KindSSEEvent) {
		captured.Write(formatSSEFrame(captureFrameBytes(t, frame)))
	}
	if captured.String() != body {
		t.Fatalf("capture did not retain client chat frames: captured=%s actual=%s", captured.String(), body)
	}
}

func TestChatCompletionsResponseCanBeReplayedAsHistory(t *testing.T) {
	for _, refusal := range []bool{false, true} {
		t.Run(fmt.Sprint(refusal), func(t *testing.T) {
			event := chatTextTerminal
			if refusal {
				event = `{"type":"response.completed","response":{"id":"resp_refuse","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"Cannot answer"}]}]}}`
			}
			record := &upstreamCapture{}
			h := newHarness(t, chatBackend(record, event), "sse")
			user := map[string]any{"role": "user", "content": "hello"}
			first, err := json.Marshal(map[string]any{"model": "test-model", "messages": []any{user}})
			if err != nil {
				t.Fatal(err)
			}
			resp, raw := postChat(t, h, string(first), h.key)
			if resp.StatusCode != 200 {
				t.Fatalf("first request: %d %s", resp.StatusCode, raw)
			}
			var completion map[string]any
			if err := json.Unmarshal([]byte(raw), &completion); err != nil {
				t.Fatal(err)
			}
			message := completion["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
			// Model dumps from typed Chat SDKs may include empty optional fields.
			message["tool_calls"], message["function_call"], message["audio"] = nil, nil, nil
			message["annotations"] = []any{}
			if !refusal {
				message["refusal"] = nil
			}
			second, err := json.Marshal(map[string]any{
				"model": "test-model", "messages": []any{user, message, user}, "stream_options": nil,
			})
			if err != nil {
				t.Fatal(err)
			}
			resp, raw = postChat(t, h, string(second), h.key)
			if resp.StatusCode != 200 {
				t.Fatalf("replaying output failed: %d %s request=%s", resp.StatusCode, raw, second)
			}
			_, wire := record.get()
			var input map[string]any
			if err := json.Unmarshal(wire, &input); err != nil {
				t.Fatal(err)
			}
			messages := input["input"].([]any)
			if len(messages) != 3 {
				t.Fatalf("history lost: %s", wire)
			}
			if refusal {
				part := messages[1].(map[string]any)["content"].([]any)[0].(map[string]any)
				if part["type"] != "refusal" || part["refusal"] != "Cannot answer" {
					t.Fatalf("refusal history lost: %s", wire)
				}
			}
		})
	}
}
