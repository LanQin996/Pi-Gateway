// Package upstream speaks the ChatGPT Codex responses protocol on behalf of
// accounts, over either SSE (HTTP) or WebSocket, and normalises both into one
// event stream.
package upstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Event is one responses stream event from the upstream.
type Event struct {
	// Type is the "type" field of the event payload.
	Type string
	// SSEEvent is the explicit SSE event name, independent of the JSON type.
	// It is empty for WebSocket events or an absent/empty SSE event field.
	SSEEvent string
	// Raw is the JSON payload. Parsers preserve the received payload; stream
	// callbacks receive type:error for otherwise untyped logical failures.
	Raw []byte
	// Data is the decoded payload.
	Data map[string]any
	// At is when the event was observed.
	At time.Time
}

// Usage summarises token accounting found on the terminal event.
//
// HasUsage distinguishes "the upstream sent no usage block" from "the upstream
// reported zero tokens"; the usage ledger stores nullable columns so a cache-hit
// rate never divides by a fabricated zero.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	HasUsage         bool  `json:"has_usage"`
}

// Terminal event types.
const (
	EventResponseCompleted  = "response.completed"
	EventResponseDone       = "response.done"
	EventResponseIncomplete = "response.incomplete"
	EventResponseFailed     = "response.failed"
	EventError              = "error"
)

// IsTerminal reports whether an event type ends the stream.
func IsTerminal(eventType string) bool {
	switch eventType {
	case EventResponseCompleted, EventResponseDone, EventResponseIncomplete, EventResponseFailed, EventError:
		return true
	}
	return false
}

// FailureError is returned when the upstream reports a logical failure.
type FailureError struct {
	OperationDenied bool // Endpoint permission failure, not account health.
	Code            string
	Message         string
	Payload         []byte
	Status          int
	RequestID       string
}

func (e *FailureError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Code != "" {
		return "upstream error: " + e.Code
	}
	return "upstream error"
}

// ConnectionLimitError signals the WebSocket connection-limit condition Pi retries on.
type ConnectionLimitError struct {
	Message string
	Failure *FailureError
}

func (e *ConnectionLimitError) Unwrap() error {
	if e.Failure == nil {
		return nil
	}
	return e.Failure
}

func (e *ConnectionLimitError) Error() string { return e.Message }

// PreviousResponseNotFoundError signals a lost WebSocket continuation; Pi retries
// with the full context.
type PreviousResponseNotFoundError struct {
	Message string
	Failure *FailureError
}

func (e *PreviousResponseNotFoundError) Unwrap() error {
	if e.Failure == nil {
		return nil
	}
	return e.Failure
}

func (e *PreviousResponseNotFoundError) Error() string { return e.Message }

// IsConnectionLimitError reports the websocket_connection_limit_reached condition.
func IsConnectionLimitError(err error) bool {
	var target *ConnectionLimitError
	return errors.As(err, &target)
}

// IsPreviousResponseNotFound reports the previous_response_not_found condition.
func IsPreviousResponseNotFound(err error) bool {
	var target *PreviousResponseNotFoundError
	return errors.As(err, &target)
}

// ---- SSE parsing ----

// ParseSSE reads Server-Sent Events from r and yields one Event per data frame.
//
// The explicit "event:" name is retained separately from the payload's "type"
// so named errors can be recognized without changing ordinary JSON payloads.
func ParseSSE(r io.Reader) *SSEReader {
	return &SSEReader{reader: bufio.NewReaderSize(r, 64*1024)}
}

// SSEReader incrementally decodes an SSE byte stream.
type SSEReader struct {
	reader *bufio.Reader
}

// Next returns the next event, or io.EOF at end of stream.
const maxSSEEventBytes = 1 << 20

// ErrSSEEventTooLarge identifies an SSE event which exceeded the per-event limit.
var ErrSSEEventTooLarge = errors.New("upstream: SSE event exceeds size limit")

// SSEEventTooLargeError reports the configured SSE event limit.
type SSEEventTooLargeError struct{ Limit int }

func (e *SSEEventTooLargeError) Error() string {
	return fmt.Sprintf("upstream: SSE event exceeds size limit (%d bytes)", e.Limit)
}
func (e *SSEEventTooLargeError) Unwrap() error { return ErrSSEEventTooLarge }

type sseFrame struct {
	dataBuf   bytes.Buffer
	sawData   bool
	eventName string
}

func (s *SSEReader) Next() (*Event, error) {
	frame := &sseFrame{}
	frameBytes := 0
	for {
		line, err := s.readFrameLine(&frameBytes)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if len(line) > 0 {
			event, lineErr := frame.processLine(s, line)
			if lineErr != nil || event != nil {
				return event, lineErr
			}
		}
		if errors.Is(err, io.EOF) {
			if frame.sawData {
				return s.buildEvent(frame.dataBuf.String(), frame.eventName)
			}
			return nil, io.EOF
		}
	}
}

func (s *SSEReader) readFrameLine(frameBytes *int) ([]byte, error) {
	var lineBuf bytes.Buffer
	for {
		chunk, err := s.reader.ReadSlice('\n')
		if len(chunk) > 0 {
			*frameBytes += len(chunk)
			if *frameBytes > maxSSEEventBytes {
				return nil, &SSEEventTooLargeError{Limit: maxSSEEventBytes}
			}
			lineBuf.Write(chunk)
			if lineBuf.Len() > maxSSEEventBytes {
				return nil, &SSEEventTooLargeError{Limit: maxSSEEventBytes}
			}
		}
		// A physical line can span several bufio chunks without bypassing limits.
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		return lineBuf.Bytes(), err
	}
}

func (f *sseFrame) processLine(s *SSEReader, line []byte) (*Event, error) {
	trimmed := strings.TrimRight(string(line), "\r\n")

	// A blank line terminates the frame.
	if trimmed == "" {
		if !f.sawData {
			f.eventName = ""
			return nil, nil
		}
		return s.buildEvent(f.dataBuf.String(), f.eventName)
	}

	if strings.HasPrefix(trimmed, ":") {
		// Comment / keep-alive.
		return nil, nil
	}

	if strings.HasPrefix(trimmed, "data:") {
		payload := strings.TrimPrefix(trimmed, "data:")
		payload = strings.TrimPrefix(payload, " ")
		if f.dataBuf.Len() > 0 {
			f.dataBuf.WriteByte('\n')
		}
		f.dataBuf.WriteString(payload)
		f.sawData = true
	}
	if strings.HasPrefix(trimmed, "event:") {
		f.eventName = strings.TrimPrefix(strings.TrimPrefix(trimmed, "event:"), " ")
	} else if trimmed == "event" {
		// A field without a colon has an empty value.
		f.eventName = ""
	}
	// Other fields (id:, retry:) carry no payload we need.
	return nil, nil
}

func (s *SSEReader) buildEvent(data, eventName string) (*Event, error) {
	data = strings.TrimSpace(data)
	if data == "" || data == "[DONE]" {
		// A sentinel with no payload: surface it as an empty terminator so callers
		// can stop cleanly.
		return &Event{Type: "[DONE]", SSEEvent: eventName, Raw: []byte(data), Data: map[string]any{}, At: time.Now()}, nil
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(data), &decoded); err != nil {
		return nil, fmt.Errorf("upstream: invalid SSE JSON: %w", err)
	}
	eventType, _ := decoded["type"].(string)
	return &Event{Type: eventType, SSEEvent: eventName, Raw: []byte(data), Data: decoded, At: time.Now()}, nil
}

// ---- SSE framing (for producing a client stream) ----

// FormatSSEFrame renders an event as an SSE frame using only a data line, which
// is what the ChatGPT backend emits and what Pi's parser expects.
func FormatSSEFrame(raw []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString("data: ")
	buf.Write(raw)
	buf.WriteString("\n\n")
	return buf.Bytes()
}

// ---- helpers ----

// ExtractUsage pulls token accounting from a terminal event payload.
func ExtractUsage(data map[string]any) Usage {
	var usage Usage
	if data == nil {
		return usage
	}
	resp, _ := data["response"].(map[string]any)
	if resp == nil {
		resp = data
	}
	u, _ := resp["usage"].(map[string]any)
	if u == nil {
		return usage
	}
	usage.HasUsage = true
	usage.InputTokens = intFrom(u, "input_tokens")
	usage.OutputTokens = intFrom(u, "output_tokens")
	usage.TotalTokens = intFrom(u, "total_tokens")
	if details, ok := u["output_tokens_details"].(map[string]any); ok {
		usage.ReasoningTokens = intFrom(details, "reasoning_tokens")
	}
	// Responses and Chat completions name the cached counters differently, and
	// the reference implementation falls back the same way.
	if details, ok := u["input_tokens_details"].(map[string]any); ok {
		usage.CachedTokens = intFrom(details, "cached_tokens")
		usage.CacheWriteTokens = intFrom(details, "cache_write_tokens")
	}
	if usage.CachedTokens == 0 {
		usage.CachedTokens = intFrom(u, "cache_read_input_tokens")
	}
	if usage.CacheWriteTokens == 0 {
		usage.CacheWriteTokens = intFrom(u, "cache_write_tokens")
	}
	return usage
}

// ExtractResponseID pulls the response id from any event that carries one.
func ExtractResponseID(data map[string]any) string {
	if data == nil {
		return ""
	}
	if id, ok := data["response_id"].(string); ok && id != "" {
		return id
	}
	if resp, ok := data["response"].(map[string]any); ok {
		if id, ok := resp["id"].(string); ok {
			return id
		}
	}
	if id, ok := data["id"].(string); ok {
		return id
	}
	return ""
}

// FailureFromEvent converts a failure event into an error.
func FailureFromEvent(event *Event) error {
	if event == nil || (event.Type != EventError && event.Type != EventResponseFailed &&
		event.SSEEvent != EventError && !jsonTruthy(event.Data["error"])) {
		return nil
	}

	data := event.Data
	code, message := extractErrorFields(data)
	requestID := extractRequestID(data)
	status := extractStatus(data)

	if response, ok := data["response"].(map[string]any); ok {
		if nestedCode, nestedMessage := extractErrorFields(response); code == "" {
			code = nestedCode
			message = nestedMessage
		} else if message == "" {
			message = nestedMessage
		}
		if requestID == "" {
			requestID = extractRequestID(response)
		}
		if status == 0 {
			status = extractStatus(response)
		}
	}

	if message == "" {
		switch {
		case code != "":
			message = "upstream error: " + code
		case event.Type == EventResponseFailed:
			message = "upstream response failed"
		default:
			message = "upstream error"
		}
	}

	failure := &FailureError{Code: code, Message: message, Payload: event.Raw, RequestID: requestID, Status: status}
	switch code {
	case "websocket_connection_limit_reached":
		return &ConnectionLimitError{Message: message, Failure: failure}
	case "previous_response_not_found":
		return &PreviousResponseNotFoundError{Message: message, Failure: failure}
	}
	return failure
}

// jsonTruthy matches JavaScript truthiness for values decoded from JSON.
// In particular, empty objects/arrays are truthy, unlike false, zero and null.
func jsonTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	default:
		return true
	}
}

// failureEventForDelivery gives newly recognized failures an explicit JSON type.
// Data-only downstream transports cannot preserve an SSE event name, and existing
// callbacks use Type to avoid appending a duplicate error. Already typed failures
// remain byte-for-byte unchanged; FailureError.Payload always keeps the original.
func failureEventForDelivery(event *Event, failure error) *Event {
	if failure == nil || event.Type == EventError || event.Type == EventResponseFailed {
		return event
	}
	data := make(map[string]any, len(event.Data)+1)
	for key, value := range event.Data {
		data[key] = value
	}
	data["type"] = EventError
	// The map contains only values decoded from JSON, plus the type string.
	raw, err := json.Marshal(data)
	if err != nil {
		return event
	}
	out := *event
	out.Type, out.Raw, out.Data = EventError, raw, data
	return &out
}

func extractErrorFields(data map[string]any) (code, message string) {
	if data == nil {
		return "", ""
	}
	if s, ok := data["message"].(string); ok {
		message = strings.TrimSpace(s)
	}
	if s, ok := data["code"].(string); ok {
		code = strings.TrimSpace(s)
	}
	if errObj, ok := data["error"].(map[string]any); ok {
		nestedCode, nestedMessage := extractErrorFields(errObj)
		if code == "" {
			code = nestedCode
		}
		if code == "" {
			if s, ok := errObj["type"].(string); ok {
				code = strings.TrimSpace(s)
			}
		}
		if message == "" {
			message = nestedMessage
		}
	}
	return code, message
}

func extractStatus(data map[string]any) int {
	if data == nil {
		return 0
	}
	for _, key := range []string{"status_code", "status"} {
		if status := intValue(data[key]); status != 0 {
			return status
		}
	}
	if errObj, ok := data["error"].(map[string]any); ok {
		if status := extractStatus(errObj); status != 0 {
			return status
		}
	}
	return 0
}

func extractRequestID(data map[string]any) string {
	if data == nil {
		return ""
	}
	for _, key := range []string{"request_id", "requestId"} {
		if id, ok := data[key].(string); ok && strings.TrimSpace(id) != "" {
			return id
		}
	}
	for _, key := range []string{"headers", "error"} {
		if nested, ok := data[key].(map[string]any); ok {
			if id := extractRequestID(nested); id != "" {
				return id
			}
		}
	}
	return requestIDHeaderValue(data)
}

func intValue(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case float32:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		return n
	default:
		return 0
	}
}

func intFrom(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

func requestIDHeaderValue(data map[string]any) string {
	for key, value := range data {
		if strings.EqualFold(key, "x-request-id") {
			if id, ok := value.(string); ok && strings.TrimSpace(id) != "" {
				return id
			}
		}
	}
	return ""
}
