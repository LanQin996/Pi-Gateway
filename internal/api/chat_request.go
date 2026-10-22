package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// chatOptions describes the downstream Chat Completions representation.
// The Responses pipeline still owns subscription constraints and upstream streaming.
type chatOptions struct {
	Stream       bool
	IncludeUsage bool
}

// chatRequestToResponses translates only fields with a lossless Responses mapping.
// Unsupported Chat-specific options fail before account selection or upstream work.
func chatRequestToResponses(raw []byte) ([]byte, *chatOptions, *apiError) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil || body == nil {
		return nil, nil, chatRequestError("request body must be a JSON object")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, nil, chatRequestError("request body must contain exactly one JSON object")
	}
	options := &chatOptions{}
	result := map[string]any{}
	for key, value := range body {
		switch key {
		case "messages", "tools", "tool_choice", "response_format", "reasoning_effort", "verbosity", "stream_options":
			// Converted after validating the top-level fields.
		case "stream":
			stream, ok := value.(bool)
			if !ok {
				return nil, nil, chatRequestError("stream must be a boolean")
			}
			options.Stream = stream
		case "n":
			number, ok := value.(json.Number)
			if !ok || number.String() != "1" {
				return nil, nil, chatRequestError("only n=1 is supported")
			}
		case "max_tokens", "max_completion_tokens":
			if _, conflict := result["max_output_tokens"]; conflict {
				return nil, nil, chatRequestError("specify only one of max_tokens and max_completion_tokens")
			}
			if value != nil && !chatInteger(value, 1) {
				return nil, nil, chatRequestError(key + " must be a positive integer or null")
			}
			result["max_output_tokens"] = value
		case "model", "store", "metadata", "user", "temperature", "top_p", "top_logprobs", "service_tier", "parallel_tool_calls", "prompt_cache_key", "prompt_cache_retention", "safety_identifier":
			// Preview-rejected fields are deliberately left to the shared pipeline.
			if err := chatValidateSharedField(key, value); err != nil {
				return nil, nil, err
			}
			result[key] = value
		case "modalities":
			modalities, ok := value.([]any)
			if !ok || len(modalities) != 1 || modalities[0] != "text" {
				return nil, nil, chatRequestError("only text modality is supported")
			}
		default:
			return nil, nil, chatRequestError("unsupported Chat Completions parameter: " + key)
		}
	}
	if value, exists := body["stream_options"]; exists && value != nil {
		streamOptions, ok := value.(map[string]any)
		if !ok || !options.Stream {
			return nil, nil, chatRequestError("stream_options requires stream=true and an object")
		}
		for key, value := range streamOptions {
			include, ok := value.(bool)
			if key != "include_usage" || !ok {
				return nil, nil, chatRequestError("stream_options only supports boolean include_usage")
			}
			options.IncludeUsage = include
		}
	}
	input, err := chatMessagesToInput(body["messages"])
	if err != nil {
		return nil, nil, err
	}
	result["input"] = input
	result["stream"] = options.Stream
	if value, exists := body["tools"]; exists {
		tools, err := chatToolsToResponses(value)
		if err != nil {
			return nil, nil, err
		}
		result["tools"] = tools
	}
	if value, exists := body["tool_choice"]; exists {
		choice, err := chatToolChoiceToResponses(value)
		if err != nil {
			return nil, nil, err
		}
		result["tool_choice"] = choice
	}
	text := map[string]any{}
	if value, exists := body["response_format"]; exists {
		format, err := chatFormatToResponses(value)
		if err != nil {
			return nil, nil, err
		}
		text["format"] = format
	}
	if value, exists := body["verbosity"]; exists {
		verbosity, ok := value.(string)
		if !ok || (verbosity != "low" && verbosity != "medium" && verbosity != "high") {
			return nil, nil, chatRequestError("verbosity must be low, medium, or high")
		}
		text["verbosity"] = verbosity
	}
	if len(text) > 0 {
		result["text"] = text
	}
	if value, exists := body["reasoning_effort"]; exists {
		effort, ok := value.(string)
		if !ok || strings.TrimSpace(effort) == "" {
			return nil, nil, chatRequestError("reasoning_effort must be a non-empty string")
		}
		result["reasoning"] = map[string]any{"effort": effort}
	}
	converted, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		return nil, nil, chatRequestError("could not encode converted request")
	}
	return converted, options, nil
}

func chatMessagesToInput(value any) ([]any, *apiError) {
	messages, ok := value.([]any)
	if !ok || len(messages) == 0 {
		return nil, chatRequestError("messages must be a non-empty array")
	}
	input := make([]any, 0, len(messages))
	for index, value := range messages {
		message, ok := value.(map[string]any)
		if !ok {
			return nil, chatRequestError(fmt.Sprintf("messages[%d] must be an object", index))
		}
		if err := chatCheckKeys(message, "role", "content", "tool_calls", "tool_call_id", "refusal", "annotations", "audio", "function_call"); err != nil {
			return nil, err
		}
		role, _ := message["role"].(string)
		if role != "system" && role != "developer" && role != "user" && role != "assistant" && role != "tool" {
			return nil, chatRequestError("unsupported message role: " + role)
		}
		refusal, err := chatAssistantMetadata(message, role)
		if err != nil {
			return nil, err
		}
		if _, exists := message["tool_call_id"]; exists && role != "tool" {
			return nil, chatRequestError("tool_call_id is only valid for tool messages")
		}
		if _, exists := message["tool_calls"]; exists && role != "assistant" {
			return nil, chatRequestError("tool_calls is only valid for assistant messages")
		}
		if role == "tool" {
			id, ok := chatNonemptyString(message["tool_call_id"])
			if !ok {
				return nil, chatRequestError("tool messages require tool_call_id")
			}
			content, err := chatContentToResponses(message["content"], "tool")
			if err != nil {
				return nil, err
			}
			var output strings.Builder
			for _, part := range content {
				output.WriteString(part.(map[string]any)["text"].(string))
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": id, "output": output.String()})
			continue
		}
		calls, hasCalls := message["tool_calls"]
		hasCalls = hasCalls && calls != nil
		var content []any
		if message["content"] != nil {
			var err *apiError
			content, err = chatContentToResponses(message["content"], role)
			if err != nil {
				return nil, err
			}
		} else if role != "assistant" || (!hasCalls && refusal == "") {
			return nil, chatRequestError("message content is required unless assistant tool_calls or refusal are present")
		}
		if refusal != "" {
			content = append(content, map[string]any{"type": "refusal", "refusal": refusal})
		}
		if len(content) > 0 {
			input = append(input, map[string]any{"type": "message", "role": role, "content": content})
		}
		if hasCalls {
			converted, err := chatCallsToResponses(calls)
			if err != nil {
				return nil, err
			}
			input = append(input, converted...)
		}
	}
	return input, nil
}

// SDK model_dump output contains nullable assistant metadata. Empty metadata
// carries no history; populated unsupported fields must never disappear silently.
func chatAssistantMetadata(message map[string]any, role string) (string, *apiError) {
	for _, key := range []string{"refusal", "annotations", "audio", "function_call"} {
		value, exists := message[key]
		if !exists {
			continue
		}
		if role != "assistant" {
			return "", chatRequestError(key + " is only valid for assistant messages")
		}
		if value == nil {
			continue
		}
		switch key {
		case "refusal":
			if _, ok := chatNonemptyString(value); !ok {
				return "", chatRequestError("assistant refusal must be a non-empty string or null")
			}
		case "annotations":
			annotations, ok := value.([]any)
			if !ok || len(annotations) != 0 {
				return "", chatRequestError("only empty assistant annotations are supported")
			}
		default:
			return "", chatRequestError("unsupported assistant field: " + key)
		}
	}
	refusal, _ := message["refusal"].(string)
	return refusal, nil
}

func chatContentToResponses(value any, role string) ([]any, *apiError) {
	textType := "input_text"
	if role == "assistant" {
		textType = "output_text"
	}
	if text, ok := value.(string); ok {
		return []any{map[string]any{"type": textType, "text": text}}, nil
	}
	parts, ok := value.([]any)
	if !ok || len(parts) == 0 {
		return nil, chatRequestError("message content must be a string or non-empty content array")
	}
	result := make([]any, 0, len(parts))
	for _, value := range parts {
		part, ok := value.(map[string]any)
		if !ok {
			return nil, chatRequestError("message content parts must be objects")
		}
		switch part["type"] {
		case "refusal":
			if role != "assistant" {
				return nil, chatRequestError("refusal content is only valid for assistant messages")
			}
			if err := chatCheckKeys(part, "type", "refusal"); err != nil {
				return nil, err
			}
			refusal, ok := chatNonemptyString(part["refusal"])
			if !ok {
				return nil, chatRequestError("refusal content requires a non-empty refusal string")
			}
			result = append(result, map[string]any{"type": "refusal", "refusal": refusal})
		case "text":
			if err := chatCheckKeys(part, "type", "text"); err != nil {
				return nil, err
			}
			text, ok := part["text"].(string)
			if !ok {
				return nil, chatRequestError("text content requires a text string")
			}
			result = append(result, map[string]any{"type": textType, "text": text})
		case "image_url":
			if role != "user" {
				return nil, chatRequestError("image_url content is only supported in user messages")
			}
			if err := chatCheckKeys(part, "type", "image_url"); err != nil {
				return nil, err
			}
			image, ok := part["image_url"].(map[string]any)
			if !ok {
				return nil, chatRequestError("image_url must be an object")
			}
			if err := chatCheckKeys(image, "url", "detail"); err != nil {
				return nil, err
			}
			url, ok := chatNonemptyString(image["url"])
			if !ok {
				return nil, chatRequestError("image_url.url must be a non-empty string")
			}
			converted := map[string]any{"type": "input_image", "image_url": url}
			if detail, exists := image["detail"]; exists {
				if detail != "auto" && detail != "low" && detail != "high" {
					return nil, chatRequestError("image detail must be auto, low, or high")
				}
				converted["detail"] = detail
			}
			result = append(result, converted)
		default:
			return nil, chatRequestError("only text, user image_url, and assistant refusal content parts are supported")
		}
	}
	return result, nil
}

func chatCallsToResponses(value any) ([]any, *apiError) {
	calls, ok := value.([]any)
	if !ok || len(calls) == 0 {
		return nil, chatRequestError("tool_calls must be a non-empty array")
	}
	result := make([]any, 0, len(calls))
	for _, value := range calls {
		call, ok := value.(map[string]any)
		if !ok || call["type"] != "function" {
			return nil, chatRequestError("only function tool calls are supported")
		}
		if err := chatCheckKeys(call, "id", "type", "function"); err != nil {
			return nil, err
		}
		function, ok := call["function"].(map[string]any)
		if !ok {
			return nil, chatRequestError("tool call function must be an object")
		}
		if err := chatCheckKeys(function, "name", "arguments"); err != nil {
			return nil, err
		}
		id, hasID := chatNonemptyString(call["id"])
		name, hasName := chatNonemptyString(function["name"])
		arguments, hasArguments := function["arguments"].(string)
		if !hasID || !hasName || !hasArguments {
			return nil, chatRequestError("function tool calls require id, name, and string arguments")
		}
		result = append(result, map[string]any{"type": "function_call", "call_id": id, "name": name, "arguments": arguments})
	}
	return result, nil
}

func chatToolsToResponses(value any) ([]any, *apiError) {
	tools, ok := value.([]any)
	if !ok {
		return nil, chatRequestError("tools must be an array")
	}
	result := make([]any, 0, len(tools))
	for _, value := range tools {
		tool, ok := value.(map[string]any)
		if !ok || tool["type"] != "function" {
			return nil, chatRequestError("only function tools are supported")
		}
		if err := chatCheckKeys(tool, "type", "function"); err != nil {
			return nil, err
		}
		function, ok := tool["function"].(map[string]any)
		if !ok {
			return nil, chatRequestError("tool function must be an object")
		}
		if err := chatCheckKeys(function, "name", "description", "parameters", "strict"); err != nil {
			return nil, err
		}
		if _, ok := chatNonemptyString(function["name"]); !ok {
			return nil, chatRequestError("function tools require a non-empty name")
		}
		converted := map[string]any{"type": "function"}
		for key, value := range function {
			if key == "description" {
				if _, ok := value.(string); !ok {
					return nil, chatRequestError("function description must be a string")
				}
			}
			if key == "parameters" {
				if _, ok := value.(map[string]any); !ok {
					return nil, chatRequestError("function parameters must be an object")
				}
			}
			if key == "strict" && value != nil {
				if _, ok := value.(bool); !ok {
					return nil, chatRequestError("function strict must be a boolean or null")
				}
			}
			converted[key] = value
		}
		// Chat tools default to non-strict; Responses otherwise normalizes schemas.
		if converted["strict"] == nil {
			converted["strict"] = false
		}
		result = append(result, converted)
	}
	return result, nil
}

func chatToolChoiceToResponses(value any) (any, *apiError) {
	if choice, ok := value.(string); ok {
		if choice == "auto" || choice == "none" || choice == "required" {
			return choice, nil
		}
		return nil, chatRequestError("unsupported tool_choice")
	}
	choice, ok := value.(map[string]any)
	if !ok || choice["type"] != "function" {
		return nil, chatRequestError("tool_choice must select a function, auto, none, or required")
	}
	if err := chatCheckKeys(choice, "type", "function"); err != nil {
		return nil, err
	}
	function, ok := choice["function"].(map[string]any)
	if !ok {
		return nil, chatRequestError("tool_choice.function must be an object")
	}
	if err := chatCheckKeys(function, "name"); err != nil {
		return nil, err
	}
	name, ok := chatNonemptyString(function["name"])
	if !ok {
		return nil, chatRequestError("tool_choice.function requires a name")
	}
	return map[string]any{"type": "function", "name": name}, nil
}

func chatFormatToResponses(value any) (map[string]any, *apiError) {
	format, ok := value.(map[string]any)
	if !ok {
		return nil, chatRequestError("response_format must be an object")
	}
	if format["type"] == "text" || format["type"] == "json_object" {
		if err := chatCheckKeys(format, "type"); err != nil {
			return nil, err
		}
		return map[string]any{"type": format["type"]}, nil
	}
	if format["type"] != "json_schema" {
		return nil, chatRequestError("unsupported response_format type")
	}
	if err := chatCheckKeys(format, "type", "json_schema"); err != nil {
		return nil, err
	}
	schema, ok := format["json_schema"].(map[string]any)
	if !ok {
		return nil, chatRequestError("response_format.json_schema must be an object")
	}
	if err := chatCheckKeys(schema, "name", "description", "schema", "strict"); err != nil {
		return nil, err
	}
	if _, ok := chatNonemptyString(schema["name"]); !ok {
		return nil, chatRequestError("json_schema requires a non-empty name")
	}
	if _, ok := schema["schema"].(map[string]any); !ok {
		return nil, chatRequestError("json_schema.schema must be an object")
	}
	if description, exists := schema["description"]; exists {
		if _, ok := description.(string); !ok {
			return nil, chatRequestError("json_schema.description must be a string")
		}
	}
	if strict, exists := schema["strict"]; exists && strict != nil {
		if _, ok := strict.(bool); !ok {
			return nil, chatRequestError("json_schema.strict must be a boolean or null")
		}
	}
	converted := map[string]any{"type": "json_schema"}
	for key, value := range schema {
		converted[key] = value
	}
	return converted, nil
}

func chatCheckKeys(object map[string]any, allowed ...string) *apiError {
	for key := range object {
		found := false
		for _, candidate := range allowed {
			if key == candidate {
				found = true
				break
			}
		}
		if !found {
			return chatRequestError("unsupported Chat Completions field: " + key)
		}
	}
	return nil
}

func chatNonemptyString(value any) (string, bool) {
	text, ok := value.(string)
	return text, ok && strings.TrimSpace(text) != ""
}

func chatRequestError(message string) *apiError {
	return &apiError{Status: http.StatusBadRequest, Type: "invalid_request_error", Code: "unsupported_chat_request", Message: message}
}

// chatInputError is shared with the endpoint's framing and body validation.
func chatInputError(message string) *apiError { return chatRequestError(message) }

func chatInteger(value any, minimum int64) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	integer, err := number.Int64()
	return err == nil && integer >= minimum
}

func chatValidateSharedField(key string, value any) *apiError {
	if value == nil && key != "model" {
		return nil
	}
	valid := false
	switch key {
	case "store", "parallel_tool_calls":
		_, valid = value.(bool)
	case "metadata":
		_, valid = value.(map[string]any)
	case "temperature", "top_p":
		_, valid = value.(json.Number)
	case "top_logprobs":
		valid = chatInteger(value, 0)
	default:
		_, valid = value.(string)
	}
	if !valid {
		return chatRequestError("invalid value for " + key)
	}
	return nil
}
