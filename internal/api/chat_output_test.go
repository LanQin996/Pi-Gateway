package api

import (
	"encoding/json"
	"strings"
	"testing"

	"pi-gateway/internal/upstream"
)

func chatOutputEvent(typ string, data map[string]any) *upstream.Event {
	return &upstream.Event{Type: typ, Data: data}
}

func chatOutputChunks(t *testing.T, output *chatOutput, typ string, data map[string]any) []map[string]any {
	t.Helper()
	chunks, err := output.event(chatOutputEvent(typ, data))
	if err != nil {
		t.Fatalf("%s: %v", typ, err)
	}
	return chunks
}

func chatOutputChoice(chunk map[string]any) map[string]any {
	return chunk["choices"].([]any)[0].(map[string]any)
}

func chatOutputDelta(chunk map[string]any) map[string]any {
	return chatOutputChoice(chunk)["delta"].(map[string]any)
}

func chatOutputMessage(text string) map[string]any {
	return map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": text}}}
}

func TestChatOutputTextFallbackMetadataAndUsage(t *testing.T) {
	output := newChatOutput("requested-model")
	chunks := chatOutputChunks(t, output, "response.created", map[string]any{"response": map[string]any{
		"id": "resp_abc", "created_at": int64(1720000000), "model": "upstream-alias",
	}})
	if len(chunks) != 1 || chatOutputDelta(chunks[0])["role"] != "assistant" {
		t.Fatalf("missing initial assistant role: %#v", chunks)
	}
	chunks = append(chunks, chatOutputChunks(t, output, "response.output_text.delta", map[string]any{"delta": "hel"})...)
	chunks = append(chunks, chatOutputChunks(t, output, "response.output_text.done", map[string]any{"text": "hello"})...)
	if got := chatOutputDelta(chunks[2])["content"]; got != "lo" {
		t.Fatalf("done replayed text: %#v", got)
	}
	if repeat := chatOutputChunks(t, output, "response.output_text.done", map[string]any{"text": "hello"}); len(repeat) != 0 {
		t.Fatalf("repeated done emitted: %#v", repeat)
	}
	terminal := map[string]any{
		"id": "resp_late", "created_at": 1729999999, "model": "another-model", "status": "completed",
		"output": []any{chatOutputMessage("hello")},
		"usage": map[string]any{
			"input_tokens": 13, "output_tokens": 7, "total_tokens": 20,
			"input_tokens_details":  map[string]any{"cached_tokens": 3},
			"output_tokens_details": map[string]any{"reasoning_tokens": 4},
		},
	}
	finish := chatOutputChunks(t, output, "response.completed", map[string]any{"response": terminal})
	if len(finish) != 1 || chatOutputChoice(finish[0])["finish_reason"] != "stop" {
		t.Fatalf("invalid terminal chunks: %#v", finish)
	}
	chunks = append(chunks, finish...)
	for _, chunk := range chunks {
		if chunk["id"] != "chatcmpl-abc" || chunk["created"] != int64(1720000000) || chunk["model"] != "requested-model" {
			t.Fatalf("stream metadata changed: %#v", chunk)
		}
	}
	if repeated := chatOutputChunks(t, output, "response.done", map[string]any{"response": terminal}); len(repeated) > 0 {
		t.Fatalf("duplicate terminal chunks: %#v", repeated)
	}
	usage := output.usageChunk()
	if len(usage["choices"].([]any)) != 0 {
		t.Fatalf("usage chunk must have empty choices: %#v", usage)
	}
	u := usage["usage"].(map[string]any)
	if u["prompt_tokens"] != int64(13) || u["completion_tokens"] != int64(7) || u["total_tokens"] != int64(20) ||
		u["prompt_tokens_details"].(map[string]any)["cached_tokens"] != int64(3) ||
		u["completion_tokens_details"].(map[string]any)["reasoning_tokens"] != int64(4) {
		t.Fatalf("incorrect usage: %#v", u)
	}
}

func TestChatOutputToolsUseItemIDAndContinuousIndexes(t *testing.T) {
	output := newChatOutput("model")
	chatOutputChunks(t, output, "response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{
		"id": "reason", "type": "reasoning", "encrypted_content": "private",
	}})
	for index, item := range []map[string]any{
		{"id": "fc_a", "type": "function_call", "call_id": "call_a", "name": "first", "arguments": ""},
		{"id": "fc_b", "type": "function_call", "call_id": "call_b", "name": "second", "arguments": ""},
	} {
		chunks := chatOutputChunks(t, output, "response.output_item.added", map[string]any{"output_index": index + 2, "item": item})
		delta := chatOutputDelta(chunks[len(chunks)-1])
		tool := delta["tool_calls"].([]any)[0].(map[string]any)
		if tool["index"] != index || tool["id"] != item["call_id"] {
			t.Fatalf("wrong tool mapping: %#v", tool)
		}
	}
	// Deliberately incorrect output_index: item_id must win.
	chunks := chatOutputChunks(t, output, "response.function_call_arguments.delta", map[string]any{
		"item_id": "fc_b", "output_index": 2, "delta": "{\"b\":",
	})
	tool := chatOutputDelta(chunks[0])["tool_calls"].([]any)[0].(map[string]any)
	if tool["index"] != 1 {
		t.Fatalf("arguments attached to wrong tool: %#v", tool)
	}
	// Older events without item_id still map by the original output_index.
	chatOutputChunks(t, output, "response.function_call_arguments.delta", map[string]any{"output_index": 2, "delta": "{}"})
	chunks = chatOutputChunks(t, output, "response.function_call_arguments.done", map[string]any{
		"item_id": "fc_b", "arguments": "{\"b\":2}",
	})
	if got := chatOutputDelta(chunks[0])["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"]; got != "2}" {
		t.Fatalf("arguments done replayed existing prefix: %#v", got)
	}
	terminal := map[string]any{"status": "completed"} // No output: preserve accumulated deltas.
	finish := chatOutputChunks(t, output, "response.completed", map[string]any{"response": terminal})
	if chatOutputChoice(finish[0])["finish_reason"] != "tool_calls" {
		t.Fatalf("missing tool finish reason: %#v", finish)
	}
	completion, err := output.completion(terminal)
	if err != nil {
		t.Fatal(err)
	}
	message := chatOutputChoice(completion)["message"].(map[string]any)
	calls := message["tool_calls"].([]any)
	if len(calls) != 2 || message["content"] != nil ||
		calls[0].(map[string]any)["function"].(map[string]any)["arguments"] != "{}" ||
		calls[1].(map[string]any)["function"].(map[string]any)["arguments"] != "{\"b\":2}" {
		t.Fatalf("incorrect accumulated tools: %#v", message)
	}
}

func TestChatOutputCompletionUsesAuthoritativeSnapshot(t *testing.T) {
	output := newChatOutput("model")
	chatOutputChunks(t, output, "response.output_text.delta", map[string]any{"delta": "draft"})
	completion, err := output.completion(map[string]any{"output": []any{
		map[string]any{"type": "reasoning", "encrypted_content": "never-return-this", "summary": []any{}},
		chatOutputMessage("final"),
		map[string]any{"type": "message", "content": []any{map[string]any{"type": "refusal", "refusal": "cannot"}}},
		map[string]any{"type": "function_call", "call_id": "call_x", "name": "f", "arguments": "{}"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	choice := chatOutputChoice(completion)
	message := choice["message"].(map[string]any)
	if message["content"] != "final" || message["refusal"] != "cannot" || choice["finish_reason"] != "tool_calls" {
		t.Fatalf("incorrect authoritative result: %#v", completion)
	}
	raw, _ := json.Marshal(completion)
	if strings.Contains(string(raw), "never-return-this") || strings.Contains(string(raw), "encrypted_content") || strings.Contains(string(raw), "draft") {
		t.Fatalf("snapshot leaked intermediate/private data: %s", raw)
	}
	// A present, empty output array is authoritative as well.
	completion, err = output.completion(map[string]any{"output": []any{}})
	if err != nil || chatOutputChoice(completion)["message"].(map[string]any)["content"] != nil {
		t.Fatalf("empty output fell back to stale deltas: %#v, %v", completion, err)
	}
}

func TestChatOutputDoneOnlyAndRefusal(t *testing.T) {
	output := newChatOutput("model")
	chatOutputChunks(t, output, "response.refusal.delta", map[string]any{"delta": "No", "content_index": 1})
	chunks := chatOutputChunks(t, output, "response.refusal.done", map[string]any{"refusal": "No thanks", "content_index": 1})
	if len(chunks) != 1 || chatOutputDelta(chunks[0])["refusal"] != " thanks" {
		t.Fatalf("incorrect refusal done fallback: %#v", chunks)
	}
	chunks = chatOutputChunks(t, output, "response.output_item.done", map[string]any{
		"output_index": 1, "item": chatOutputMessage("done-only"),
	})
	if len(chunks) != 1 || chatOutputDelta(chunks[0])["content"] != "done-only" {
		t.Fatalf("done-only message lost: %#v", chunks)
	}
	completion, err := output.completion(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	message := chatOutputChoice(completion)["message"].(map[string]any)
	if message["content"] != "done-only" || message["refusal"] != "No thanks" {
		t.Fatalf("incorrect accumulated content: %#v", message)
	}
}

func TestChatOutputTerminalWithoutDeltas(t *testing.T) {
	output := newChatOutput("model")
	chunks := chatOutputChunks(t, output, "response.done", map[string]any{"response": map[string]any{
		"id": "resp_done", "output": []any{
			chatOutputMessage("terminal text"),
			map[string]any{"type": "function_call", "id": "fc", "call_id": "call", "name": "run", "arguments": "{}"},
		},
	}})
	if len(chunks) != 5 || chatOutputDelta(chunks[0])["role"] != "assistant" ||
		chatOutputDelta(chunks[1])["content"] != "terminal text" ||
		chatOutputChoice(chunks[4])["finish_reason"] != "tool_calls" {
		t.Fatalf("incorrect terminal fallback: %#v", chunks)
	}
	if output.usageChunk() != nil {
		t.Fatal("fabricated usage for response without usage")
	}
}

func TestChatOutputFinishReasons(t *testing.T) {
	for _, tc := range []struct {
		reason, want string
	}{
		{"max_output_tokens", "length"},
		{"content_filter", "content_filter"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			output := newChatOutput("model")
			body := map[string]any{"status": "incomplete", "incomplete_details": map[string]any{"reason": tc.reason}}
			chunks := chatOutputChunks(t, output, "response.incomplete", map[string]any{"response": body})
			if got := chatOutputChoice(chunks[len(chunks)-1])["finish_reason"]; got != tc.want {
				t.Fatalf("finish_reason = %v, want %s", got, tc.want)
			}
			completion, err := output.completion(body)
			if err != nil || chatOutputChoice(completion)["finish_reason"] != tc.want {
				t.Fatalf("aggregate finish_reason: %#v, %v", completion, err)
			}
		})
	}
}

func TestChatOutputRejectsUnsupportedOutput(t *testing.T) {
	for _, item := range []map[string]any{
		{"type": "computer_call"},
		{"type": "message", "content": []any{map[string]any{"type": "output_audio"}}},
		{"type": "function_call", "name": "no_call_id"},
	} {
		output := newChatOutput("model")
		body := map[string]any{"output": []any{item}}
		chunks, err := output.event(chatOutputEvent("response.completed", map[string]any{"response": body}))
		if err == nil || len(chunks) != 0 || output.finished {
			t.Fatalf("unsupported output succeeded: %#v, %v", chunks, err)
		}
		if _, err := output.completion(body); err == nil {
			t.Fatalf("unsupported output aggregated successfully: %#v", item)
		}
	}
	output := newChatOutput("model")
	if _, err := output.completion(map[string]any{"status": "incomplete"}); err == nil {
		t.Fatal("incomplete response without a reason succeeded")
	}
	if _, err := output.completion(map[string]any{"status": "in_progress"}); err == nil {
		t.Fatal("unfinished response succeeded")
	}
	if _, err := output.event(chatOutputEvent("response.incomplete", map[string]any{})); err == nil {
		t.Fatal("incomplete event without a reason succeeded")
	}
	if _, err := output.event(chatOutputEvent("response.output_item.added", map[string]any{"item": map[string]any{"type": "web_search_call"}})); err == nil {
		t.Fatal("unsupported added item succeeded")
	}
	if _, err := output.event(chatOutputEvent("response.function_call_arguments.delta", map[string]any{"item_id": "unknown", "delta": "{}"})); err == nil {
		t.Fatal("unmapped arguments event succeeded")
	}
}
