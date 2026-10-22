package api

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"testing"
)

func TestChatRequestConvertsConversationAndTools(t *testing.T) {
	raw := `{"model":"test-model","messages":[
		{"role":"system","content":"system prompt"},
		{"role":"developer","content":[{"type":"text","text":"developer prompt"}]},
		{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA","detail":"high"}}]},
		{"role":"assistant","content":"checking","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}},{"id":"call-2","type":"function","function":{"name":"other","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call-1","content":"result"},
		{"role":"tool","tool_call_id":"call-2","content":[{"type":"text","text":"one"},{"type":"text","text":"two"}]},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call-3","type":"function","function":{"name":"lookup","arguments":"{}"}}]}
	],"tools":[{"type":"function","function":{"name":"lookup","description":"find","parameters":{"type":"object","properties":{}},"strict":false}},{"type":"function","function":{"name":"other"}}],"tool_choice":{"type":"function","function":{"name":"lookup"}},"parallel_tool_calls":false,"metadata":null,"n":1}`
	body, options := chatConvertedForTest(t, raw)
	if options.Stream || options.IncludeUsage || body["stream"] != false {
		t.Fatalf("incorrect default options: %+v body=%v", options, body)
	}
	input := body["input"].([]any)
	if len(input) != 9 {
		t.Fatalf("input lost messages or tool calls: %v", input)
	}
	want := []any{
		map[string]any{"type": "message", "role": "system", "content": []any{map[string]any{"type": "input_text", "text": "system prompt"}}},
		map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "developer prompt"}}},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "describe"}, map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA", "detail": "high"}}},
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "checking"}}},
		map[string]any{"type": "function_call", "call_id": "call-1", "name": "lookup", "arguments": `{"q":1}`},
		map[string]any{"type": "function_call", "call_id": "call-2", "name": "other", "arguments": `{}`},
		map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "result"},
		map[string]any{"type": "function_call_output", "call_id": "call-2", "output": "onetwo"},
		map[string]any{"type": "function_call", "call_id": "call-3", "name": "lookup", "arguments": `{}`},
	}
	if !reflect.DeepEqual(input, want) {
		t.Fatalf("conversation conversion mismatch\ngot: %#v\nwant: %#v", input, want)
	}
	tools := body["tools"].([]any)
	for _, value := range tools {
		tool := value.(map[string]any)
		if tool["type"] != "function" || tool["strict"] != false || tool["function"] != nil {
			t.Fatalf("tool schema or Chat non-strict default changed: %v", tool)
		}
	}
	if !reflect.DeepEqual(body["tool_choice"], map[string]any{"type": "function", "name": "lookup"}) || body["parallel_tool_calls"] != false {
		t.Fatalf("tool choice or explicit false lost: %v", body)
	}
	if value, exists := body["metadata"]; !exists || value != nil {
		t.Fatalf("explicit null lost: %v", body)
	}
	for _, key := range []string{"messages", "n", "stream_options"} {
		if _, exists := body[key]; exists {
			t.Errorf("Chat-only field leaked: %s", key)
		}
	}
}

func TestChatRequestConvertsFormattingAndStreaming(t *testing.T) {
	body, options := chatConvertedForTest(t, `{"messages":[{"role":"user","content":""}],"stream":true,"stream_options":{"include_usage":true},"response_format":{"type":"json_schema","json_schema":{"name":"answer","description":"result","schema":{"type":"object","properties":{"id":{"const":9007199254740993}}},"strict":false}},"reasoning_effort":"high","verbosity":"low","max_completion_tokens":123,"temperature":0.5,"store":true,"modalities":["text"]}`)
	if !options.Stream || !options.IncludeUsage || body["stream"] != true {
		t.Fatalf("stream options lost: %+v %v", options, body)
	}
	text := body["text"].(map[string]any)
	format := text["format"].(map[string]any)
	if text["verbosity"] != "low" || format["name"] != "answer" || format["description"] != "result" || format["strict"] != false || format["type"] != "json_schema" {
		t.Fatalf("format conversion lost fields: %v", text)
	}
	constant := format["schema"].(map[string]any)["properties"].(map[string]any)["id"].(map[string]any)["const"]
	if constant != json.Number("9007199254740993") {
		t.Fatalf("schema number lost precision: %#v", constant)
	}
	if !reflect.DeepEqual(body["reasoning"], map[string]any{"effort": "high"}) || body["max_output_tokens"] != json.Number("123") || body["temperature"] != json.Number("0.5") || body["store"] != true {
		t.Fatalf("shared-pipeline options changed prematurely: %v", body)
	}
	for _, key := range []string{"response_format", "reasoning_effort", "verbosity", "max_completion_tokens", "modalities"} {
		if _, exists := body[key]; exists {
			t.Errorf("Chat-only field leaked: %s", key)
		}
	}
}

func TestChatRequestSupportedVariants(t *testing.T) {
	for _, choice := range []string{"auto", "none", "required"} {
		t.Run(choice, func(t *testing.T) {
			body, _ := chatConvertedForTest(t, `{"messages":[{"role":"user","content":"hello"}],"tools":[],"tool_choice":"`+choice+`","response_format":{"type":"json_object"},"max_tokens":12}`)
			if body["tool_choice"] != choice || body["max_output_tokens"] != json.Number("12") || !reflect.DeepEqual(body["text"], map[string]any{"format": map[string]any{"type": "json_object"}}) {
				t.Fatalf("variant mismatch: %v", body)
			}
		})
	}
	body, _ := chatConvertedForTest(t, `{"messages":[{"role":"user","content":"hello"}],"response_format":{"type":"text"},"tools":[{"type":"function","function":{"name":"f","strict":null}}]}`)
	if !reflect.DeepEqual(body["text"], map[string]any{"format": map[string]any{"type": "text"}}) || body["tools"].([]any)[0].(map[string]any)["strict"] != false {
		t.Fatalf("text format or nullable strict mismatch: %v", body)
	}
}

func TestChatRequestAcceptsAssistantSDKMetadataAndRefusal(t *testing.T) {
	for _, streaming := range []string{"false", "true"} {
		t.Run(streaming, func(t *testing.T) {
			body, options := chatConvertedForTest(t, `{"stream":`+streaming+`,"stream_options":null,"messages":[
				{"role":"assistant","content":"answer","refusal":null,"tool_calls":null,"annotations":[],"audio":null,"function_call":null},
				{"role":"assistant","content":null,"refusal":"cannot comply","tool_calls":null,"annotations":null},
				{"role":"assistant","content":[{"type":"text","text":"context"},{"type":"refusal","refusal":"declined"}],"refusal":null},
				{"role":"user","content":"follow up"}
			]}`)
			if options.Stream != (streaming == "true") || options.IncludeUsage {
				t.Fatalf("null stream_options changed options: %+v", options)
			}
			input := body["input"].([]any)
			want := []any{
				map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "answer"}}},
				map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "refusal", "refusal": "cannot comply"}}},
				map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "context"}, map[string]any{"type": "refusal", "refusal": "declined"}}},
				map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "follow up"}}},
			}
			if !reflect.DeepEqual(input, want) {
				t.Fatalf("SDK metadata or refusal conversion mismatch\ngot: %#v\nwant: %#v", input, want)
			}
		})
	}
}

func TestChatRequestRejectsUnsupportedAndMalformedInput(t *testing.T) {
	invalid := []string{
		``, `null`, `[]`, `{}`, `{"messages":[]}`, `{"messages":"text"}`,
		`{"messages":[null]}`, `{"messages":[{"role":"function","content":"x"}]}`,
		`{"messages":[{"role":"user"}]}`, `{"messages":[{"role":"assistant","content":null}]}`,
		`{"messages":[{"role":"user","content":[]}]}`, `{"messages":[{"role":"user","content":1}]}`,
		`{"messages":[{"role":"user","content":[null]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":1}]}]}`,
		`{"messages":[{"role":"user","content":"x","name":"named"}]}`,
		`{"messages":[{"role":"user","content":"x","tool_call_id":"c"}]}`,
		`{"messages":[{"role":"user","content":"x","tool_calls":[]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"x","format":"wav"}}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"x"}}]}]}`,
		`{"messages":[{"role":"assistant","content":"x","audio":{}}]}`,
		`{"messages":[{"role":"assistant","content":null,"refusal":false}]}`,
		`{"messages":[{"role":"assistant","content":null,"refusal":""}]}`,
		`{"messages":[{"role":"user","content":"x","refusal":"no"}]}`,
		`{"messages":[{"role":"assistant","content":"x","function_call":{"name":"f","arguments":"{}"}}]}`,
		`{"messages":[{"role":"assistant","content":"x","annotations":[{"type":"url_citation"}]}]}`,
		`{"messages":[{"role":"assistant","content":"x","annotations":{}}]}`,
		`{"messages":[{"role":"user","content":"x","audio":null}]}`,
		`{"messages":[{"role":"assistant","content":null,"tool_calls":null,"refusal":null}]}`,
		`{"messages":[{"role":"user","content":[{"type":"refusal","refusal":"no"}]}]}`,
		`{"messages":[{"role":"assistant","content":[{"type":"refusal","refusal":null}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://example.test/x"}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":""}}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x","detail":"bad"}}]}]}`,
		`{"messages":[{"role":"assistant","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`,
		`{"messages":[{"role":"tool","content":"x"}]}`,
		`{"messages":[{"role":"tool","tool_call_id":"x","content":null}]}`,
		`{"messages":[{"role":"assistant","content":null,"tool_calls":[]}]}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"id":"x","type":"function","function":{"name":"f","arguments":{}}}]}]}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"f","arguments":"{}"}}]}]}`,
	}
	base := `{"messages":[{"role":"user","content":"x"}]`
	for _, field := range []string{
		`"n":2`, `"n":"1"`, `"n":null`, `"stream":1`, `"stream":null`,
		`"functions":[]`, `"function_call":"auto"`, `"audio":{}`, `"modalities":["audio"]`,
		`"frequency_penalty":0`, `"presence_penalty":0`, `"logprobs":true`, `"logit_bias":{}`, `"seed":1`, `"stop":["END"]`, `"prediction":{}`, `"unknown":true`,
		`"stream_options":{"include_usage":true}`, `"stream":true,"stream_options":{"include_usage":"yes"}`, `"stream":true,"stream_options":{"unknown":true}`,
		`"tools":{}`, `"tools":[{"type":"web_search"}]`, `"tools":[{"type":"function","function":{"name":""}}]`,
		`"tools":[{"type":"function","function":{"name":"f","strict":"yes"}}]`, `"tools":[{"type":"function","function":{"name":"f","parameters":[]}}]`,
		`"tool_choice":"bad"`, `"tool_choice":{"type":"function","function":{"name":""}}`,
		`"response_format":{"type":"unknown"}`, `"response_format":{"type":"json_schema","json_schema":{"name":"x"}}`,
		`"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{},"strict":"yes"}}`,
		`"reasoning_effort":false`, `"verbosity":"unknown"`, `"max_tokens":1,"max_completion_tokens":2`, `"max_tokens":-1`, `"max_tokens":1.5`,
		`"model":{}`, `"store":"yes"`, `"parallel_tool_calls":1`, `"temperature":"hot"`, `"metadata":[]`, `"top_logprobs":-1`,
	} {
		invalid = append(invalid, base+`,`+field+`}`)
	}
	invalid = append(invalid, base+`} {}`, base+`} garbage`)
	for index, raw := range invalid {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			converted, options, err := chatRequestToResponses([]byte(raw))
			if err == nil || err.Status != 400 || err.Type != "invalid_request_error" || err.Message == "" || converted != nil || options != nil {
				t.Fatalf("malformed/unsupported request accepted: %s; body=%s options=%+v error=%+v", raw, converted, options, err)
			}
		})
	}
}

func chatConvertedForTest(t *testing.T, raw string) (map[string]any, *chatOptions) {
	t.Helper()
	converted, options, err := chatRequestToResponses([]byte(raw))
	if err != nil {
		t.Fatalf("convert request: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(converted))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body, options
}
