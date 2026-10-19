package upstream

import (
	"encoding/json"
	"fmt"
	"testing"

	"pi-gateway/internal/codexbridge"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// TestBuildDeltaBodySendsOnlyNewItems verifies the Pi continuation: on a reused
// socket the frame carries previous_response_id plus only the new input items.
func TestBuildDeltaBodySendsOnlyNewItems(t *testing.T) {
	turn1 := mustJSON(t, map[string]any{
		"model": "gpt-5.1-codex",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "hi"},
		},
		"stream": true,
		"store":  false,
	})

	// The previous response produced one assistant message item.
	assistantItem := map[string]any{"type": "message", "role": "assistant", "content": "hello"}
	exceptInput, input, ok := bodyExceptInput(turn1)
	if !ok {
		t.Fatal("bodyExceptInput failed")
	}
	cont := &wsContinuation{
		bodyExceptInput:   exceptInput,
		lastInput:         input,
		lastResponseItems: []any{assistantItem},
		lastResponseID:    "resp_1",
	}

	// Turn 2 = turn 1 input + assistant item + a new user item.
	turn2 := mustJSON(t, map[string]any{
		"model": "gpt-5.1-codex",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "hi"},
			assistantItem,
			map[string]any{"type": "message", "role": "user", "content": "again"},
		},
		"stream": true,
		"store":  false,
	})

	delta, ok := buildDeltaBody(turn2, cont)
	if !ok {
		t.Fatal("buildDeltaBody did not recognise the continuation")
	}
	var decoded map[string]any
	if err := json.Unmarshal(delta, &decoded); err != nil {
		t.Fatalf("delta is not JSON: %v", err)
	}
	if decoded["previous_response_id"] != "resp_1" {
		t.Errorf("previous_response_id = %v, want resp_1", decoded["previous_response_id"])
	}
	items, _ := decoded["input"].([]any)
	if len(items) != 1 {
		t.Fatalf("delta input has %d items, want 1", len(items))
	}
	item, _ := items[0].(map[string]any)
	if item["content"] != "again" {
		t.Errorf("delta item = %v, want the new user message", item)
	}
	// previous_response_id must be present but not leak into the compared body.
	if _, exists := mustDecode(t, exceptInput)["previous_response_id"]; exists {
		t.Error("bodyExceptInput must drop previous_response_id")
	}
	if _, exists := mustDecode(t, exceptInput)["input"]; exists {
		t.Error("bodyExceptInput must drop input")
	}
}

// TestBuildDeltaBodyRejectsDivergentBody verifies a changed non-input field disables
// the continuation (the full body must be sent instead).
func TestBuildDeltaBodyRejectsDivergentBody(t *testing.T) {
	turn1 := mustJSON(t, map[string]any{"model": "a", "input": []any{}, "stream": true, "store": false})
	exceptInput, input, _ := bodyExceptInput(turn1)
	cont := &wsContinuation{
		bodyExceptInput:   exceptInput,
		lastInput:         input,
		lastResponseItems: nil,
		lastResponseID:    "resp_1",
	}

	divergent := mustJSON(t, map[string]any{"model": "b", "input": []any{map[string]any{"x": 1}}, "stream": true, "store": false})
	if _, ok := buildDeltaBody(divergent, cont); ok {
		t.Error("a changed model must disable the continuation")
	}
}

// TestBuildDeltaBodyRequiresPrefixMatch verifies a rewritten history is not treated
// as a continuation.
func TestBuildDeltaBodyRequiresPrefixMatch(t *testing.T) {
	turn1 := mustJSON(t, map[string]any{
		"model":  "m",
		"input":  []any{map[string]any{"content": "one"}},
		"stream": true,
		"store":  false,
	})
	exceptInput, input, _ := bodyExceptInput(turn1)
	cont := &wsContinuation{bodyExceptInput: exceptInput, lastInput: input, lastResponseID: "resp_1"}

	rewritten := mustJSON(t, map[string]any{
		"model":  "m",
		"input":  []any{map[string]any{"content": "DIFFERENT"}, map[string]any{"content": "two"}},
		"stream": true,
		"store":  false,
	})
	if _, ok := buildDeltaBody(rewritten, cont); ok {
		t.Error("a rewritten prefix must disable the continuation")
	}
}

// TestBuildDeltaBodyWithoutCachedState verifies there is nothing to continue from.
func TestBuildDeltaBodyWithoutCachedState(t *testing.T) {
	body := mustJSON(t, map[string]any{"model": "m", "input": []any{}, "stream": true, "store": false})
	if _, ok := buildDeltaBody(body, nil); ok {
		t.Error("nil continuation must not produce a delta")
	}
	if _, ok := buildDeltaBody(body, &wsContinuation{}); ok {
		t.Error("a continuation without a response id must not produce a delta")
	}
}

// TestResponseItemsFromTerminal verifies tool-output items are excluded.
func TestResponseItemsFromTerminal(t *testing.T) {
	event := map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"output": []any{
				map[string]any{"type": "message", "role": "assistant"},
				map[string]any{"type": "function_call_output"},
				map[string]any{"type": "custom_tool_call_output"},
				map[string]any{"type": "function_call"},
			},
		},
	}
	items := responseItemsFromTerminal(event)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["type"] == "function_call_output" || item["type"] == "custom_tool_call_output" {
			t.Errorf("tool output item was not excluded: %v", item)
		}
	}
}

func TestBuildDeltaBodyCodexBridgeProjection(t *testing.T) {
	profile := codexbridge.Profile{Version: 1, Tools: codexbridge.ToolSendMessage}
	for _, rawDotted := range []bool{false, true} {
		for _, replayDotted := range []bool{false, true} {
			for _, marker := range []string{"absent", "null", "empty"} {
				t.Run(fmt.Sprintf("raw_dotted=%v/replay_dotted=%v/marker=%s", rawDotted, replayDotted, marker), func(t *testing.T) {
					first := mustJSON(t, map[string]any{"model": "m", "input": []any{"first"}})
					exceptInput, input, _ := bodyExceptInput(first)
					raw := map[string]any{"type": "function_call", "id": "fc1", "call_id": "c1", "namespace": codexbridge.Alias, "name": "send_message", "arguments": `{"id":"child","message":"完整任务"}`}
					if rawDotted {
						delete(raw, "namespace")
						raw["name"] = codexbridge.Alias + ".send_message"
					}
					if marker == "null" {
						raw["encrypted_function_args"] = nil
					}
					if marker == "empty" {
						raw["encrypted_function_args"] = []any{}
					}
					// Replay exactly the native identity/marker delivered to a Codex client.
					replay := mustDecode(t, mustJSON(t, raw))
					replay["name"], replay["namespace"] = "send_message", "collaboration"
					if replayDotted {
						delete(replay, "namespace")
						replay["name"] = "collaboration.send_message"
					}
					replay["encrypted_function_args"] = []any{}
					cont := &wsContinuation{bodyExceptInput: exceptInput, lastInput: input, lastResponseItems: []any{raw}, lastResponseID: "r1", codexBridge: profile}
					before := string(mustJSON(t, raw))
					body := mustJSON(t, map[string]any{"model": "m", "input": []any{"first", replay, "new"}})
					delta, ok := buildDeltaBody(body, cont, profile)
					if !ok {
						t.Fatal("client replay did not match raw upstream baseline")
					}
					if got := mustDecode(t, delta); got["previous_response_id"] != "r1" || !jsonEqual(got["input"], []any{"new"}) {
						t.Fatalf("bad delta: %s", delta)
					}
					if string(mustJSON(t, raw)) != before {
						t.Fatal("comparison mutated raw decoded baseline")
					}
					// Retained serialized baselines must use the identical projection.
					cont.size()
					if _, ok := buildDeltaBody(body, cont, profile); !ok {
						t.Fatal("serialized baseline did not match")
					}
					if _, ok := buildDeltaBody(body, cont); ok {
						t.Fatal("profile omission accepted a bridged baseline")
					}
					for _, field := range []string{"call_id", "arguments", "encrypted_function_args"} {
						changed := mustDecode(t, mustJSON(t, replay))
						changed[field] = "different"
						bad := mustJSON(t, map[string]any{"model": "m", "input": []any{"first", changed, "new"}})
						if _, ok := buildDeltaBody(bad, cont, profile); ok {
							t.Fatalf("changed %s matched", field)
						}
					}
					plain := *cont
					plain.codexBridge = codexbridge.Profile{}
					if _, ok := buildDeltaBody(body, &plain); ok {
						t.Fatal("ordinary request used bridge equivalence")
					}
				})
			}
		}
	}
}

func TestBuildDeltaBodyRejectsCodexBridgeProfileChanges(t *testing.T) {
	profile := codexbridge.Profile{Version: 1, Tools: codexbridge.ToolSendMessage}
	body := []byte(`{"model":"m","input":["first","new"]}`)
	exceptInput, _, _ := bodyExceptInput(body)
	cont := &wsContinuation{bodyExceptInput: exceptInput, lastInput: []any{"first"}, lastResponseID: "r1", codexBridge: profile}
	for _, changed := range []codexbridge.Profile{{}, {Version: 1, Tools: codexbridge.ToolSpawnAgent}, {Version: 99}, {Tools: 1}} {
		if _, ok := buildDeltaBody(body, cont, changed); ok {
			t.Fatalf("accepted changed profile %#v", changed)
		}
	}
}

func mustDecode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}
