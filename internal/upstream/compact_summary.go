package upstream

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"pi-gateway/internal/compactprompt"
)

const summaryPrefix = "pi_compact_v1:"

type compactionCodec struct{ cipher cipher.AEAD }

func newCompactionCodec(key []byte) *compactionCodec {
	if len(key) != 32 {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			panic(err)
		}
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return &compactionCodec{aead}
}

func summaryAAD(owner int64) []byte { return []byte(fmt.Sprintf("pi-compaction-v1/key/%d", owner)) }

func (c *compactionCodec) seal(summary string, owner int64) (string, error) {
	nonce := make([]byte, c.cipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := c.cipher.Seal(nonce, nonce, []byte(summary), summaryAAD(owner))
	return summaryPrefix + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (c *compactionCodec) open(value string, owner int64) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, summaryPrefix))
	if err != nil || len(raw) < c.cipher.NonceSize()+c.cipher.Overhead() {
		return "", fmt.Errorf("invalid gateway compaction item")
	}
	summary, err := c.cipher.Open(nil, raw[:c.cipher.NonceSize()], raw[c.cipher.NonceSize():], summaryAAD(owner))
	if err != nil {
		return "", fmt.Errorf("gateway compaction item cannot be decrypted for this API key")
	}
	return string(summary), nil
}

// ExpandCompactionBody translates only authenticated gateway items. Native
// encrypted items remain byte-for-byte intact. No conversation is stored here.
func (c *Client) ExpandCompactionBody(body []byte, owner int64) ([]byte, error) {
	if !strings.Contains(string(body), summaryPrefix) {
		return body, nil
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	items, _ := request["input"].([]any)
	changed := false
	for i, raw := range items {
		item, _ := raw.(map[string]any)
		value, _ := item["encrypted_content"].(string)
		if item["type"] != "compaction" || !strings.HasPrefix(value, summaryPrefix) {
			continue
		}
		summary, err := c.compactCodec.open(value, owner)
		if err != nil {
			return nil, &FailureError{Code: "invalid_compaction_item", Status: 400, Message: err.Error()}
		}
		items[i] = map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{
			"type": "input_text", "text": "Conversation summary for continuation (historical context):\n" + summary,
		}}}
		changed = true
	}
	if !changed {
		return body, nil
	}
	return json.Marshal(request)
}

func compactFallbackAllowed(f *FailureError) bool {
	// Never turn expired credentials, quota errors, or arbitrary server failures
	// into a second model execution. Only a known operation denial or missing
	// endpoint justifies a summary attempt.
	if f.Status == 404 || f.Status == 405 || f.Status == 501 {
		return true
	}
	return f.Status == 403 && strings.Contains(strings.ToLower(f.Message), "chatpass credential is not authorized for the requested operation")
}

func (c *Client) streamSummaryCompact(ctx context.Context, req *Request, onEvent func(*Event) error) (*StreamResult, error) {
	var original map[string]any
	if err := json.Unmarshal(req.Body, &original); err != nil {
		return nil, err
	}
	model := strings.TrimSpace(req.CompactionModel)
	if model == "" {
		model, _ = original["model"].(string)
	}
	if req.BeforeSummary != nil {
		if err := req.BeforeSummary(ctx, model); err != nil {
			return nil, err
		}
	}
	prompt := req.CompactionPrompt
	if strings.TrimSpace(prompt) == "" {
		prompt = compactprompt.Default
	}
	history, ok := original["input"].([]any)
	if !ok {
		return nil, fmt.Errorf("upstream: compaction input must be an array")
	}
	input := append(append([]any(nil), history...), map[string]any{"role": "user", "content": []any{map[string]any{
		"type": "input_text", "text": prompt,
	}}})
	// Codex local compaction retains the task's base instructions and appends
	// the compaction prompt as a user message to the ordinary Responses input.
	summaryBody := map[string]any{
		"model": model, "stream": true, "store": false,
		"input": input,
	}
	if instructions, exists := original["instructions"]; exists {
		summaryBody["instructions"] = instructions
	}
	body, err := json.Marshal(summaryBody)
	if err != nil {
		return nil, err
	}
	summaryReq := *req
	summaryReq.Compact, summaryReq.CompactDirect = false, false
	summaryReq.Body = body
	summaryReq.PreviousResponseID, summaryReq.Continuation = "", nil
	observeSummaryRequest(req, model)
	collector := &summaryCollector{streamedText: make(map[int]string)}
	result, err := c.streamSSE(ctx, &summaryReq, collector.onEvent)
	terminal, streamedText := collector.terminal, collector.streamedText
	if err != nil {
		return result, err
	}
	if terminal == nil || terminal["status"] != "completed" {
		return result, fmt.Errorf("upstream: summary compaction did not complete")
	}
	value := completedSummaryText(terminal, streamedText)
	if value == "" {
		return result, fmt.Errorf("upstream: summary compaction returned no text")
	}
	if len(value) > 1<<20 {
		return result, fmt.Errorf("upstream: summary compaction exceeds byte limit")
	}
	encrypted, err := c.compactCodec.seal(value, req.ClientKeyID)
	if err != nil {
		return result, err
	}
	payload := map[string]any{"id": "cmp_" + newConnectionID(), "object": "response.compaction", "created_at": time.Now().Unix(),
		"output": []any{map[string]any{"id": "cmp_" + newConnectionID(), "type": "compaction", "encrypted_content": encrypted}}, "usage": terminal["usage"]}
	emitReq := *req
	emitReq.Body = body
	return emitCompaction(&emitReq, result, payload, onEvent)
}

func summaryMessageText(item map[string]any) string {
	if item["type"] != "message" || item["role"] != "assistant" {
		return ""
	}
	var text strings.Builder
	content, _ := item["content"].([]any)
	for _, raw := range content {
		part, _ := raw.(map[string]any)
		if part["type"] == "output_text" {
			value, _ := part["text"].(string)
			text.WriteString(value)
			text.WriteByte('\n')
		}
	}
	return text.String()
}

type summaryCollector struct {
	terminal      map[string]any
	streamedText  map[int]string
	streamedBytes int
}

func (collector *summaryCollector) onEvent(event *Event) error {
	if event.Type == "response.output_item.done" {
		item, _ := event.Data["item"].(map[string]any)
		index := intValue(event.Data["output_index"])
		text := summaryMessageText(item)
		collector.streamedBytes += len(text) - len(collector.streamedText[index])
		if collector.streamedBytes > 1<<20 {
			return fmt.Errorf("upstream: summary compaction exceeds byte limit")
		}
		collector.streamedText[index] = text
	}
	if IsTerminal(event.Type) {
		collector.terminal, _ = event.Data["response"].(map[string]any)
	}
	return nil // Model summary tokens must never become the client's task answer.
}

func completedSummaryText(terminal map[string]any, streamedText map[int]string) string {
	var summary strings.Builder
	output, _ := terminal["output"].([]any)
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		summary.WriteString(summaryMessageText(item))
	}
	value := strings.TrimSpace(summary.String())
	if value == "" {
		// Some upstreams omit output from the completed response even though
		// completed message items were delivered earlier in the SSE stream.
		indices := make([]int, 0, len(streamedText))
		for index := range streamedText {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		for _, index := range indices {
			summary.WriteString(streamedText[index])
		}
		value = strings.TrimSpace(summary.String())
	}
	return value
}

func observeSummaryRequest(req *Request, model string) {
	if req.Sink != nil {
		eventType := "compaction_model_summary"
		if req.CompactionMode != "on" {
			eventType = "compaction_summary_fallback"
		}
		meta := map[string]any{"mode": "model_summary", "model": model}
		raw, _ := json.Marshal(meta)
		req.Sink.OnFrame("out", "transport", eventType, raw, meta)
	}
}
