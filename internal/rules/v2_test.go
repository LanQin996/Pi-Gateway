package rules

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"pi-gateway/internal/piwire"
)

func TestV2CompositionScopesAndFragments(t *testing.T) {
	r := rule("flow", step("save", "let", map[string]any{"name": "suffix", "value": "!"}),
		step("process", "for_each", map[string]any{"path": "/input", "bind": "message", "steps": []Action{
			branch("select", cond("type", "/text", "string"), []Action{setStep("edit", "/text", expr("concat", refExpr("current", "/text"), refExpr("vars", "/suffix")))}),
			step("local", "let", map[string]any{"name": "suffix", "value": "local"}),
		}, "keep": cond("ne", "/type", "reasoning")}), setStep("outer", "/suffix", refExpr("vars", "/suffix")),
		step("scope", "scope", map[string]any{"functions": map[string]any{"finish": []Action{setStep("finish-value", "/done", true)}}, "steps": []Action{step("invoke", "call", map[string]any{"name": "finish"})}}))
	result := applyTest(t, r, map[string]any{"input": []any{map[string]any{"type": "message", "text": "hello"}, map[string]any{"type": "reasoning", "text": "hidden"}}})
	assertJSON(t, result.Body, map[string]any{"input": []any{map[string]any{"type": "message", "text": "hello!"}}, "suffix": "!", "done": true})
	found := false
	for _, trace := range result.Traces {
		if trace.ActionID == "edit" && trace.ItemPath == "/input/0" {
			found = true
		}
	}
	if !found {
		t.Fatal("nested trace lost step or item path")
	}
}

func TestV2LexicalFragmentsAndRecursion(t *testing.T) {
	call := func(name string) Action { return step("call-"+name, "call", map[string]any{"name": name}) }
	r := rule("lexical", step("outer", "scope", map[string]any{"functions": map[string]any{
		"a": []Action{call("b")}, "b": []Action{setStep("outer-value", "/result", "outer")},
	}, "steps": []Action{step("inner", "scope", map[string]any{"functions": map[string]any{"b": []Action{setStep("inner-value", "/result", "inner")}}, "steps": []Action{call("a")}})}}))
	assertJSON(t, applyTest(t, r, map[string]any{}).Body, map[string]any{"result": "outer"})
	for _, functions := range []map[string]any{{"a": []Action{call("a")}}, {"a": []Action{call("b")}, "b": []Action{call("a")}}} {
		_, err := Compile([]Rule{rule("recursive", step("scope", "scope", map[string]any{"functions": functions, "steps": []Action{}}))})
		if err == nil || !strings.Contains(err.Error(), "recursive") {
			t.Fatalf("recursion accepted: %v", err)
		}
	}
}

func TestV2MissingShortCircuitAndRollback(t *testing.T) {
	r := rule("short", setStep("fallback", "/fallback", expr("coalesce", refExpr("current", "/missing"), false)),
		setStep("exists", "/exists", expr("exists", refExpr("current", "/null"))),
		branch("skip", testExpr(expr("and", false, expr("gt", "wrong", 1))), []Action{setStep("unreachable", "/bad", true)}))
	assertJSON(t, applyTest(t, r, map[string]any{"null": nil}).Body, map[string]any{"null": nil, "fallback": false, "exists": true})
	r.Actions = append(r.Actions, setStep("missing", "/error", refExpr("current", "/missing")))
	r.OnError = OnErrorSkipRule
	result, err := compileTest(t, r).Apply(t.Context(), PhaseRequest, &Input{Body: map[string]any{"null": nil}, Trace: true})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, result.Body, map[string]any{"null": nil})
	for _, trace := range result.Traces {
		if !trace.RolledBack {
			t.Fatal("trace not rolled back")
		}
	}
}

func TestV2EmptyIterationConsumesBudget(t *testing.T) {
	items := make([]any, MaxExecutionSteps+1)
	r := rule("budget", step("each", "for_each", map[string]any{"path": "/items", "steps": []Action{}}))
	result, err := compileTest(t, r).Apply(context.Background(), PhaseRequest, &Input{Body: map[string]any{"items": items}})
	if err == nil || !strings.Contains(err.Error(), "steps") {
		t.Fatalf("unbounded iteration: %v", err)
	}
	if result.Changed {
		t.Fatal("failed iteration committed")
	}
}

func TestV2TextRecipeAndNestedToolFiltering(t *testing.T) {
	result := applyTest(t, rule("text", act("drop_input_items", map[string]any{"pattern": "^hello\nworld$", "target": "text"})), map[string]any{"input": []any{
		map[string]any{"content": []any{map[string]any{"text": "hello"}, map[string]any{"text": "world"}}},
		map[string]any{"content": []any{map[string]any{"text": "safe", "other": "hello\nworld"}}},
	}})
	if len(pathValue(t, result.Body, "/input").([]any)) != 1 {
		t.Fatal("visible text recipe matched serialized content")
	}
	result = applyTest(t, rule("tools", act("drop_tools", map[string]any{"types": []any{"image_generation"}})), map[string]any{"tools": []any{
		map[string]any{"type": "namespace", "name": "outer", "tools": []any{map[string]any{"type": "namespace", "name": "empty", "tools": []any{map[string]any{"type": "image_generation"}}}, map[string]any{"type": "function", "name": "safe"}}},
	}, "tool_choice": map[string]any{"type": "image_generation"}})
	assertJSON(t, result.Body, map[string]any{"tools": []any{map[string]any{"type": "namespace", "name": "outer", "tools": []any{map[string]any{"type": "function", "name": "safe"}}}}})
}

func TestV2ProfileMatchesRequestBuilder(t *testing.T) {
	settings := map[string]any{"default_model": "fallback", "model_mappings": map[string]any{"alias": "target", "empty": ""}, "reasoning_effort": "medium", "reasoning_summary": "auto", "compaction_mode": "auto", "compaction_model": "summary"}
	cases := []string{
		`{"input":"hello","instructions":"keep","text":{"verbosity":"low"},"parallel_tool_calls":false,"future":null,"metadata":{},"temperature":1}`,
		`{"model":"alias","input":[],"reasoning":{"effort":"low","summary":null,"future":false},"include":[],"tools":[]}`,
		`{"model":"empty","reasoning":null,"previous_response_id":""}`,
		`{"model":42,"reasoning":{},"tools":{}}`,
		`{"model":"test","reasoning":"invalid","tool_choice":false}`,
	}
	engine := compileTest(t, DefaultProfile()...)
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			var client map[string]any
			json.Unmarshal([]byte(raw), &client)
			old, err := piwire.BuildRequest(client, piwire.BuildOptions{DefaultModel: "fallback", ModelMappings: map[string]string{"alias": "target", "empty": ""}, DefaultReasoningEffort: "medium", DefaultReasoningSummary: "auto", SessionID: "session"})
			if err != nil {
				t.Fatal(err)
			}
			facts := map[string]any{"settings": settings, "wire_session_id": "session", "compact": false}
			result, err := engine.Apply(t.Context(), PhaseRequestNormalize, &Input{Body: client, ClientBody: client, Context: facts})
			if err != nil {
				t.Fatal(err)
			}
			result, err = engine.Apply(t.Context(), PhaseRequestFinalize, &Input{Body: result.Body, ClientBody: client, Context: facts})
			if err != nil {
				t.Fatal(err)
			}
			assertJSON(t, result.Body, old.Body)
		})
	}
}

func TestV2ProtocolProfileUsesOnlyPrimitives(t *testing.T) {
	for _, definition := range DefaultProfile() {
		assertPrimitiveActions(t, definition.Actions)
	}
}
func assertPrimitiveActions(t *testing.T, actions []Action) {
	t.Helper()
	for _, action := range actions {
		assertPrimitiveAction(t, action)
	}
}
func assertPrimitiveAction(t *testing.T, action Action) {
	t.Helper()
	if isLegacyAction(action.Type) {
		t.Fatalf("protocol profile uses specialized action %s", action.Type)
	}
	for _, key := range []string{"steps", "then", "else"} {
		if value, exists := action.Params[key]; exists {
			children, err := decodeActions(value, "/"+key)
			requireNoError(t, err)
			assertPrimitiveActions(t, children)
		}
	}
	if functions, ok := action.Params["functions"].(map[string]any); ok {
		for name, value := range functions {
			children, err := decodeActions(value, "/functions/"+name)
			requireNoError(t, err)
			assertPrimitiveActions(t, children)
		}
	}
}

func TestV2HeaderBoundary(t *testing.T) {
	headers := http.Header{"Authorization": {"Bearer secret"}, "X-Api-Key": {"secret"}, "Openai-Api-Key": {"secret"}, "Cookie": {"secret"}, "User-Agent": {"pi (test)"}}
	facts := RedactedHeaders(headers)
	assertJSON(t, facts, map[string]any{"user-agent": []any{"pi (test)"}})
	for _, body := range []any{map[string]any{"authorization": []any{"x"}}, map[string]any{"x-api-key": []any{"x"}}, map[string]any{"connection": []any{"upgrade"}}, map[string]any{"X-Test": []any{"a"}, "x-test": []any{"b"}}, map[string]any{"test": []any{"bad\r\nheader"}}, map[string]any{"test": []any{"bad\x01"}}} {
		if _, err := DecodeHeaders(body); err == nil {
			t.Fatalf("invalid header accepted: %#v", body)
		}
	}
	result, err := compileTest(t, DefaultProfile()...).Apply(t.Context(), PhaseUpstreamHeaders, &Input{Body: map[string]any{}, Context: map[string]any{"transport": "sse", "wire_session_id": "session", "client_headers": facts, "settings": map[string]any{"user_agent": "configured", "timeout_seconds": 30, "stainless_os": "Linux", "stainless_arch": "x64", "stainless_runtime": "node", "stainless_runtime_version": "v22"}}})
	if err != nil {
		t.Fatal(err)
	}
	built, err := DecodeHeaders(result.Body)
	if err != nil {
		t.Fatal(err)
	}
	if built.Get("User-Agent") != "pi (test)" || built.Get("X-Stainless-Timeout") != "30" || built.Get("Session_id") != "session" {
		t.Fatalf("metadata rule failed: %#v", built)
	}
}

func TestV2VariablesDoNotLeakAcrossRules(t *testing.T) {
	first := rule("first", step("save", "let", map[string]any{"name": "private", "value": true}))
	second := rule("second", setStep("leak", "/leak", true))
	second.Priority = first.Priority + 1
	second.When = testExpr(expr("exists", refExpr("vars", "/private")))
	result, err := compileTest(t, first, second).Apply(t.Context(), PhaseRequest, &Input{Body: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, result.Body, map[string]any{})
}

func TestV2WalkOriginalPrecedesDescendantEdits(t *testing.T) {
	r := rule("walk-original", step("walk", "walk", map[string]any{
		"path": "", "predicate": cond("type", "", "object"),
		"steps": []Action{
			setStep("snapshot", "/snapshot", refExpr("original", "")),
			setStep("edited", "/edited", true),
		},
	}))
	before := map[string]any{"child": map[string]any{"value": "before"}}
	result := applyTest(t, r, before)
	assertJSON(t, pathValue(t, result.Body, "/snapshot"), before)
	assertJSON(t, pathValue(t, result.Body, "/child/snapshot"), before["child"])
	if pathValue(t, result.Body, "/child/edited") != true {
		t.Fatal("child edit did not commit")
	}
}

func TestV2EnvironmentMetadataAndEmptyText(t *testing.T) {
	body := map[string]any{"input": []any{
		map[string]any{"content": ""},
		map[string]any{"content": []any{map[string]any{"text": ""}, map[string]any{"text": "keep"}, map[string]any{"text": "<environment_context>secret</environment_context>"}}, "internal_chat_message_metadata_passthrough": map[string]any{"content_item_kinds": []any{"normal", "normal", "normal"}}},
	}}
	result := applyTest(t, rule("environment", act("drop_environment_context", map[string]any{})), body)
	assertJSON(t, result.Body, map[string]any{"input": []any{map[string]any{"content": ""}, map[string]any{"content": []any{map[string]any{"text": ""}, map[string]any{"text": "keep"}}}}})
}
