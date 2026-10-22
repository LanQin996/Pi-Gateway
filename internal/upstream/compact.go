package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"pi-gateway/internal/egress"
)

const maxCompactionResponseBytes = 64 << 20

func compactionURL(responsesURL string) (string, error) {
	u, err := url.Parse(responsesURL)
	if err != nil {
		return "", err
	}
	u.RawPath = strings.TrimRight(u.EscapedPath(), "/") + "/compact"
	u.Path, err = url.PathUnescape(u.RawPath)
	return u.String(), err
}

// Native compaction is an HTTP JSON operation even for downstream WebSockets.
// It must never run through pooled previous_response_id + input-delta logic.
func (c *Client) streamCompact(ctx context.Context, req *Request, onEvent func(*Event) error) (*StreamResult, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.IdleTimeout)
	defer cancel()
	endpoint, err := compactionURL(c.cfg.SSEURL)
	if err != nil {
		return nil, fmt.Errorf("upstream: build compaction URL: %w", err)
	}
	client, err := c.factory.HTTPClient(req.ProxyURL)
	if err != nil {
		return nil, err
	}
	headers := req.SSEHeaders.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("accept", "application/json")
	headers.Set("content-type", "application/json")
	headers.Del("content-encoding")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	httpReq.Header = headers
	if req.Sink != nil {
		req.Sink.OnRequestHeaders(endpoint, headers)
		req.Sink.OnRequestBody(req.Body, false)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("upstream: compaction request failed: %w", err)
	}
	defer resp.Body.Close()
	result := &StreamResult{Transport: "sse", Status: resp.StatusCode}
	if req.Sink != nil {
		req.Sink.OnResponseHeaders(resp.StatusCode, resp.Header)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.compactHTTPFailure(ctx, req, onEvent, resp, result)
	}
	reader, err := egress.DecodeBody(resp)
	if err != nil {
		return result, err
	}
	defer reader.Close()
	raw, payload, err := readCompactionPayload(reader)
	if err != nil {
		return result, err
	}
	if req.Sink != nil {
		req.Sink.OnFrame("in", "http_response", compactionResponseType, raw, payload)
		req.Sink.OnResponseID(ExtractResponseID(payload))
		req.Sink.OnUsage(ExtractUsage(payload))
	}
	if failure := FailureFromEvent(&Event{Type: compactionResponseType, Data: payload, Raw: raw}); failure != nil {
		return result, failure
	}
	if err := validateCompactedOutput(payload); err != nil {
		return result, err
	}
	return emitCompaction(req, result, payload, onEvent)
}

func emitCompaction(req *Request, result *StreamResult, payload map[string]any, onEvent func(*Event) error) (*StreamResult, error) {
	items := payload["output"].([]any)
	emit := func(kind string, data map[string]any) error {
		data["type"] = kind
		encoded, err := json.Marshal(data)
		if err != nil {
			return err
		}
		return onEvent(&Event{Type: kind, Data: data, Raw: encoded, At: time.Now()})
	}
	if req.CompactDirect {
		return result, emit(EventResponseCompleted, map[string]any{"response": payload})
	}
	// A trigger arrived on /responses: return its normal event envelope, while
	// preserving the entire compacted window and every encrypted item verbatim.
	response := map[string]any{"id": "resp_" + newConnectionID(), "object": "response",
		"created_at": payload["created_at"], "status": "completed", "model": compactModel(req.Body),
		"output": items, "usage": payload["usage"], "error": nil, "incomplete_details": nil, "store": false}
	inProgress := make(map[string]any, len(response))
	for key, value := range response {
		inProgress[key] = value
	}
	inProgress["status"], inProgress["output"], inProgress["usage"] = "in_progress", []any{}, nil
	sequence := 0
	frame := func(kind string, data map[string]any) error {
		data["sequence_number"] = sequence
		sequence++
		return emit(kind, data)
	}
	for _, kind := range []string{"response.created", "response.in_progress"} {
		if err := frame(kind, map[string]any{"response": inProgress}); err != nil {
			return result, err
		}
	}
	for index, item := range items {
		for _, kind := range []string{"response.output_item.added", "response.output_item.done"} {
			if err := frame(kind, map[string]any{"output_index": index, "item": item}); err != nil {
				return result, err
			}
		}
	}
	return result, frame(EventResponseCompleted, map[string]any{"response": response})
}

func compactModel(body []byte) string {
	var request struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &request)
	return request.Model
}

func validateCompactedOutput(payload map[string]any) error {
	items, ok := payload["output"].([]any)
	if payload["object"] != compactionResponseType || !ok || len(items) == 0 {
		return fmt.Errorf("upstream: expected response.compaction with a compacted output window")
	}
	found := false
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			return fmt.Errorf("upstream: invalid compaction output item")
		}
		if item["type"] == "compaction" {
			encrypted, _ := item["encrypted_content"].(string)
			if encrypted == "" {
				return fmt.Errorf("upstream: compaction item has no encrypted_content")
			}
			found = true
		}
	}
	if !found {
		return fmt.Errorf("upstream: compacted output has no compaction item")
	}
	return nil
}

func readCompactionPayload(reader io.Reader) ([]byte, map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, maxCompactionResponseBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("upstream: read compaction response: %w", err)
	}
	if len(raw) > maxCompactionResponseBytes {
		return nil, nil, fmt.Errorf("upstream: compaction response exceeds byte limit")
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		return nil, nil, fmt.Errorf("upstream: invalid compaction JSON response")
	}
	return raw, payload, nil
}

func (c *Client) compactHTTPFailure(ctx context.Context, req *Request, onEvent func(*Event) error, resp *http.Response, result *StreamResult) (*StreamResult, error) {
	failure := httpResponseFailure(resp, req.Sink)
	code := failure.Code
	if req.CompactionMode != "off" && compactFallbackAllowed(failure) {
		c.cfg.Logger.Warn("native compaction unavailable; using model summary", "status", resp.StatusCode, "code", code)
		return c.streamSummaryCompact(ctx, req, onEvent)
	}
	// A compact-specific rejection is not evidence the account is banned.
	failure.OperationDenied = resp.StatusCode == http.StatusForbidden && (code == "compact_denied" || compactFallbackAllowed(failure))
	return result, failure
}
