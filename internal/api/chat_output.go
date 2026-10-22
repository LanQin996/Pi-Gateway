package api

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"pi-gateway/internal/upstream"
)

// chatOutput translates the public Responses output surface. Reasoning items
// deliberately have no Chat Completions representation, including their opaque
// encrypted content. The original events continue to serve internal accounting.
type chatOutput struct {
	id, model string
	created   int64
	started   bool
	finished  bool
	terminal  map[string]any
	usage     map[string]any
	parts     map[chatPartKey]*chatPart
	tools     []*chatTool
	toolIDs   map[string]*chatTool
	toolSlots map[int]*chatTool
}

type chatPartKey struct {
	output, content int
	refusal         bool
}

type chatPart struct {
	text string
}

type chatTool struct {
	index, output   int
	id, name, args  string
	metadataEmitted bool
}

func newChatOutput(model string) *chatOutput {
	return &chatOutput{
		id: "chatcmpl-" + uuid.NewString(), model: model, created: time.Now().Unix(),
		parts: make(map[chatPartKey]*chatPart), toolIDs: make(map[string]*chatTool),
		toolSlots: make(map[int]*chatTool),
	}
}

func (c *chatOutput) metadata(body map[string]any) {
	if c.started {
		return
	}
	if id, _ := body["id"].(string); id != "" {
		if strings.HasPrefix(id, "chatcmpl-") {
			c.id = id
		} else {
			c.id = "chatcmpl-" + strings.TrimPrefix(id, "resp_")
		}
	}
	if created := chatInt(body["created_at"]); created > 0 {
		// Responses timestamps are seconds. Be tolerant of millisecond fixtures.
		if created > 1e12 {
			created /= 1000
		}
		c.created = created
	}
	if c.model == "" {
		c.model, _ = body["model"].(string)
	}
}

func (c *chatOutput) chunk(delta map[string]any, finish any) map[string]any {
	return map[string]any{
		"id": c.id, "object": "chat.completion.chunk", "created": c.created, "model": c.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
}

func (c *chatOutput) start(chunks *[]map[string]any) {
	if !c.started {
		c.started = true
		*chunks = append(*chunks, c.chunk(map[string]any{"role": "assistant"}, nil))
	}
}

func (c *chatOutput) event(event *upstream.Event) ([]map[string]any, error) {
	if event == nil || c.finished {
		return nil, nil
	}
	data := event.Data
	if data == nil && len(event.Raw) > 0 {
		if err := json.Unmarshal(event.Raw, &data); err != nil {
			return nil, fmt.Errorf("chat output: invalid upstream event: %w", err)
		}
	}
	typ := event.Type
	if typ == "" {
		typ, _ = data["type"].(string)
	}
	if typ == upstream.EventError || typ == upstream.EventResponseFailed || typ == "[DONE]" {
		return nil, nil // The HTTP adapter owns upstream errors and the SSE sentinel.
	}
	response, _ := data["response"].(map[string]any)
	c.metadata(response)
	var chunks []map[string]any
	switch typ {
	case "response.created", "response.in_progress":
		c.start(&chunks)
	case "response.output_text.delta", "response.refusal.delta":
		delta, _ := data["delta"].(string)
		c.text(data, delta, typ == "response.refusal.delta", false, &chunks)
	case "response.output_text.done", "response.refusal.done":
		refusal := typ == "response.refusal.done"
		field := "text"
		if refusal {
			field = "refusal"
		}
		full, _ := data[field].(string)
		c.text(data, full, refusal, true, &chunks)
	case "response.content_part.done":
		part, _ := data["part"].(map[string]any)
		if err := c.content(data, part, &chunks); err != nil {
			return nil, err
		}
	case "response.output_item.added", "response.output_item.done":
		item, _ := data["item"].(map[string]any)
		if err := c.item(item, int(chatInt(data["output_index"])), typ == "response.output_item.done", &chunks); err != nil {
			return nil, err
		}
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		tool := c.findTool(data)
		if tool == nil {
			return nil, fmt.Errorf("chat output: arguments event has no function_call item")
		}
		args, _ := data["delta"].(string)
		if typ == "response.function_call_arguments.done" {
			full, _ := data["arguments"].(string)
			args = chatUnsent(tool.args, full)
		}
		c.arguments(tool, args, &chunks)
	case upstream.EventResponseCompleted, upstream.EventResponseDone, upstream.EventResponseIncomplete:
		if response == nil {
			response = data
		}
		if typ == upstream.EventResponseIncomplete && response["incomplete_details"] == nil {
			return nil, fmt.Errorf("chat output: incomplete response has no reason")
		}
		c.metadata(response)
		// Validate the complete output before emitting a successful terminal chunk.
		_, _, calls, err := chatMessage(response)
		if err != nil {
			return nil, err
		}
		if output, present := response["output"]; present {
			items, err := chatObjects(output)
			if err != nil {
				return nil, err
			}
			for index, item := range items {
				if err := c.item(item, index, true, &chunks); err != nil {
					return nil, err
				}
			}
		}
		hasTools := calls > 0
		if _, present := response["output"]; !present {
			hasTools = len(c.tools) > 0
		}
		finish, err := chatFinish(response, hasTools)
		if err != nil {
			return nil, err
		}
		c.terminal = response
		c.usage = chatUsage(response)
		c.start(&chunks)
		chunks = append(chunks, c.chunk(map[string]any{}, finish))
		c.finished = true
	}
	return chunks, nil
}

// chatUnsent is a fallback for full text delivered by .done events. An earlier
// delta is never replayed, even if an upstream final snapshot differs from it.
func chatUnsent(sent, full string) string {
	if strings.HasPrefix(full, sent) {
		return strings.TrimPrefix(full, sent)
	}
	return ""
}

func (c *chatOutput) text(data map[string]any, value string, refusal, full bool, chunks *[]map[string]any) {
	key := chatPartKey{int(chatInt(data["output_index"])), int(chatInt(data["content_index"])), refusal}
	part := c.parts[key]
	if part == nil {
		part = &chatPart{}
		c.parts[key] = part
	}
	if full {
		value = chatUnsent(part.text, value)
	}
	if value == "" {
		return
	}
	part.text += value
	c.start(chunks)
	field := "content"
	if refusal {
		field = "refusal"
	}
	*chunks = append(*chunks, c.chunk(map[string]any{field: value}, nil))
}

func (c *chatOutput) content(data, part map[string]any, chunks *[]map[string]any) error {
	switch part["type"] {
	case "output_text":
		text, _ := part["text"].(string)
		c.text(data, text, false, true, chunks)
	case "refusal":
		text, _ := part["refusal"].(string)
		c.text(data, text, true, true, chunks)
	default:
		return fmt.Errorf("chat output: unsupported message content type %q", part["type"])
	}
	return nil
}

func (c *chatOutput) item(item map[string]any, output int, done bool, chunks *[]map[string]any) error {
	switch item["type"] {
	case "reasoning":
		return nil
	case "message":
		if !done {
			return nil
		}
		parts, err := chatObjects(item["content"])
		if err != nil {
			return err
		}
		for index, part := range parts {
			data := map[string]any{"output_index": output, "content_index": index}
			if err := c.content(data, part, chunks); err != nil {
				return err
			}
		}
	case "function_call":
		itemID, _ := item["id"].(string)
		tool := c.toolIDs[itemID]
		if tool == nil {
			tool = c.toolSlots[output]
		}
		if tool == nil {
			tool = &chatTool{index: len(c.tools), output: output}
			c.tools = append(c.tools, tool)
		}
		c.toolSlots[output] = tool
		if itemID != "" {
			c.toolIDs[itemID] = tool
		}
		if id, _ := item["call_id"].(string); id != "" {
			tool.id = id
		}
		if name, _ := item["name"].(string); name != "" {
			tool.name = name
		}
		if !tool.metadataEmitted {
			if tool.id == "" || tool.name == "" {
				return fmt.Errorf("chat output: function_call requires call_id and name")
			}
			tool.metadataEmitted = true
			c.start(chunks)
			*chunks = append(*chunks, c.chunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": tool.index, "id": tool.id, "type": "function",
				"function": map[string]any{"name": tool.name, "arguments": ""},
			}}}, nil))
		}
		// Some upstreams include complete arguments in .added and omit deltas.
		if args, _ := item["arguments"].(string); args != "" {
			c.arguments(tool, chatUnsent(tool.args, args), chunks)
		}
	default:
		return fmt.Errorf("chat output: unsupported output item type %q", item["type"])
	}
	return nil
}

func (c *chatOutput) findTool(data map[string]any) *chatTool {
	if id, _ := data["item_id"].(string); id != "" {
		if tool := c.toolIDs[id]; tool != nil {
			return tool
		}
	}
	if _, present := data["output_index"]; !present {
		return nil
	}
	return c.toolSlots[int(chatInt(data["output_index"]))]
}

func (c *chatOutput) arguments(tool *chatTool, args string, chunks *[]map[string]any) {
	if args == "" {
		return
	}
	tool.args += args
	c.start(chunks)
	*chunks = append(*chunks, c.chunk(map[string]any{"tool_calls": []any{map[string]any{
		"index": tool.index, "function": map[string]any{"arguments": args},
	}}}, nil))
}

func (c *chatOutput) completion(body map[string]any) (map[string]any, error) {
	if nested, ok := body["response"].(map[string]any); ok {
		body = nested
	}
	if body == nil {
		body = c.terminal
	}
	c.metadata(body)
	message, hasOutput, calls, err := chatMessage(body)
	if err != nil {
		return nil, err
	}
	if !hasOutput {
		message = c.accumulatedMessage()
		calls = len(c.tools)
	}
	finish, err := chatFinish(body, calls > 0)
	if err != nil {
		return nil, err
	}
	result := map[string]any{
		"id": c.id, "object": "chat.completion", "created": c.created, "model": c.model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
	}
	usage := chatUsage(body)
	if usage == nil {
		usage = c.usage
	}
	if usage != nil {
		result["usage"] = usage
	}
	return result, nil
}

func (c *chatOutput) accumulatedMessage() map[string]any {
	keys := make([]chatPartKey, 0, len(c.parts))
	for key := range c.parts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].output != keys[j].output {
			return keys[i].output < keys[j].output
		}
		return keys[i].content < keys[j].content
	})
	var text, refusal strings.Builder
	for _, key := range keys {
		if key.refusal {
			refusal.WriteString(c.parts[key].text)
		} else {
			text.WriteString(c.parts[key].text)
		}
	}
	message := chatAssistantMessage(text.String(), refusal.String())
	if len(c.tools) > 0 {
		calls := make([]any, 0, len(c.tools))
		for _, tool := range c.tools {
			calls = append(calls, chatFunction(tool.id, tool.name, tool.args))
		}
		message["tool_calls"] = calls
	}
	return message
}

func chatMessage(body map[string]any) (map[string]any, bool, int, error) {
	output, present := body["output"]
	if !present {
		return nil, false, 0, nil
	}
	items, err := chatObjects(output)
	if err != nil {
		return nil, true, 0, err
	}
	var text, refusal strings.Builder
	var calls []any
	for _, item := range items {
		switch item["type"] {
		case "reasoning":
			continue
		case "message":
			parts, err := chatObjects(item["content"])
			if err != nil {
				return nil, true, 0, err
			}
			for _, part := range parts {
				switch part["type"] {
				case "output_text":
					value, _ := part["text"].(string)
					text.WriteString(value)
				case "refusal":
					value, _ := part["refusal"].(string)
					refusal.WriteString(value)
				default:
					return nil, true, 0, fmt.Errorf("chat output: unsupported message content type %q", part["type"])
				}
			}
		case "function_call":
			id, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			args, _ := item["arguments"].(string)
			if id == "" || name == "" {
				return nil, true, 0, fmt.Errorf("chat output: function_call requires call_id and name")
			}
			calls = append(calls, chatFunction(id, name, args))
		default:
			return nil, true, 0, fmt.Errorf("chat output: unsupported output item type %q", item["type"])
		}
	}
	message := chatAssistantMessage(text.String(), refusal.String())
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	return message, true, len(calls), nil
}

func chatAssistantMessage(text, refusal string) map[string]any {
	message := map[string]any{"role": "assistant", "content": nil}
	if text != "" {
		message["content"] = text
	}
	if refusal != "" {
		message["refusal"] = refusal
	}
	return message
}

func chatFunction(id, name, args string) map[string]any {
	return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}
}

func chatFinish(body map[string]any, hasTools bool) (string, error) {
	status, _ := body["status"].(string)
	if status != "" && status != "completed" && status != "incomplete" {
		return "", fmt.Errorf("chat output: upstream response status %s", status)
	}
	if details, ok := body["incomplete_details"].(map[string]any); ok {
		switch details["reason"] {
		case "max_output_tokens", "max_tokens":
			return "length", nil
		case "content_filter":
			return "content_filter", nil
		default:
			return "", fmt.Errorf("chat output: unsupported incomplete reason %q", details["reason"])
		}
	}
	if status == "incomplete" {
		return "", fmt.Errorf("chat output: incomplete response has no reason")
	}
	if hasTools {
		return "tool_calls", nil
	}
	return "stop", nil
}

func chatUsage(body map[string]any) map[string]any {
	usage := upstream.ExtractUsage(body)
	if !usage.HasUsage {
		return nil
	}
	total := usage.TotalTokens
	if original, _ := body["usage"].(map[string]any); original["total_tokens"] == nil {
		total = usage.InputTokens + usage.OutputTokens
	}
	return map[string]any{
		"prompt_tokens": usage.InputTokens, "completion_tokens": usage.OutputTokens, "total_tokens": total,
		"prompt_tokens_details":     map[string]any{"cached_tokens": usage.CachedTokens},
		"completion_tokens_details": map[string]any{"reasoning_tokens": usage.ReasoningTokens},
	}
}

func (c *chatOutput) usageChunk() map[string]any {
	if !c.finished || c.usage == nil {
		return nil
	}
	return map[string]any{
		"id": c.id, "object": "chat.completion.chunk", "created": c.created, "model": c.model,
		"choices": []any{}, "usage": c.usage,
	}
}

func chatObjects(value any) ([]map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	if objects, ok := value.([]map[string]any); ok {
		return objects, nil
	}
	values, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("chat output: expected an output/content array")
	}
	objects := make([]map[string]any, 0, len(values))
	for _, value := range values {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("chat output: expected an output/content object")
		}
		objects = append(objects, object)
	}
	return objects, nil
}

func chatInt(value any) int64 {
	switch value := value.(type) {
	case int:
		return int64(value)
	case int64:
		return value
	case float64:
		return int64(value)
	case json.Number:
		n, _ := value.Int64()
		return n
	}
	return 0
}
