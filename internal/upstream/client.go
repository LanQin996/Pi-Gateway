package upstream

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"pi-gateway/internal/codexbridge"
	"pi-gateway/internal/egress"
	"pi-gateway/internal/session"
)

// DownstreamWriteTimeout bounds writes to clients; exported for the API layer.
const DownstreamWriteTimeout = 5 * time.Minute

// UpstreamWriteTimeout bounds stream writes, independently of connection setup.
const UpstreamWriteTimeout = 5 * time.Minute

// StreamWriteTimeout is the upstream stream write deadline.
const StreamWriteTimeout = UpstreamWriteTimeout

// ErrIdleTimeout identifies an upstream stream which produced no event before
// the configured idle timeout.
var ErrIdleTimeout = errors.New("upstream idle timeout")

// IdleTimeoutError reports an upstream event gap exceeding the configured limit.
type IdleTimeoutError struct{ Timeout time.Duration }

func (e *IdleTimeoutError) Error() string {
	return fmt.Sprintf("upstream: idle timeout after %s", e.Timeout)
}
func (e *IdleTimeoutError) Unwrap() error { return ErrIdleTimeout }

// IsIdleTimeoutError reports whether err is an upstream idle timeout.
func IsIdleTimeoutError(err error) bool { return errors.Is(err, ErrIdleTimeout) }

// Config configures the upstream client.
type Config struct {
	// CompactionKey persists gateway summaries across restarts (32 bytes).
	CompactionKey []byte
	SSEURL        string
	WSURL         string
	Originator    string
	UserAgent     string
	// Zstd enables zstd compression of the SSE request body (Pi does this).
	Zstd bool
	// ConnectTimeout bounds the WebSocket handshake and TCP connect.
	ConnectTimeout time.Duration
	// IdleTimeout bounds the gap between upstream events.
	IdleTimeout time.Duration
	// Pool reuses WebSocket connections within an authenticated owner and identity.
	Pool                  bool
	Sessions              session.Store
	SessionCommitTimeout  time.Duration
	SessionTTL            time.Duration
	MaxBaselineBytes      int64
	MaxTotalBaselineBytes int64
	MaxPoolConnections    int
	Logger                *slog.Logger
}

// Sink receives everything observed on the wire so a capture record can be built.
//
// Implementations must be safe for use from a single goroutine.
type Sink interface {
	OnRequestHeaders(url string, headers http.Header)
	OnRequestBody(raw []byte, compressed bool)
	OnResponseHeaders(status int, headers http.Header)
	OnFrame(dir, kind, eventType string, raw []byte, parsed map[string]any)
	OnUsage(usage Usage)
	OnResponseID(id string)
}

// NopSink is a Sink that records nothing.
type NopSink struct{}

func (NopSink) OnRequestHeaders(string, http.Header)                   {}
func (NopSink) OnRequestBody([]byte, bool)                             {}
func (NopSink) OnResponseHeaders(int, http.Header)                     {}
func (NopSink) OnFrame(string, string, string, []byte, map[string]any) {}
func (NopSink) OnUsage(Usage)                                          {}
func (NopSink) OnResponseID(string)                                    {}

// Client performs upstream requests.
type Client struct {
	compactCodec *compactionCodec
	cfg          Config
	factory      *egress.Factory
	pool         *wsPool
	instanceID   string
	commits      chan struct{}
}

// New builds an upstream client.
func New(cfg Config, factory *egress.Factory) *Client {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 15 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 300 * time.Second
	}
	if cfg.SessionCommitTimeout <= 0 {
		cfg.SessionCommitTimeout = 100 * time.Millisecond
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 4 * time.Hour
	}
	if cfg.MaxBaselineBytes <= 0 {
		cfg.MaxBaselineBytes = 2 << 20
	}
	if cfg.MaxTotalBaselineBytes <= 0 {
		cfg.MaxTotalBaselineBytes = 64 << 20
	}
	if cfg.MaxPoolConnections <= 0 {
		cfg.MaxPoolConnections = 256
	}
	c := &Client{cfg: cfg, factory: factory, instanceID: newConnectionID()}
	c.compactCodec = newCompactionCodec(cfg.CompactionKey)
	c.pool = newWSPool(cfg.IdleTimeout, cfg.Logger)
	c.commits = make(chan struct{}, cfg.MaxPoolConnections)
	c.pool.maxConnections = cfg.MaxPoolConnections
	c.pool.maxBaselineBytes = cfg.MaxBaselineBytes
	c.pool.maxTotalBaselineBytes = cfg.MaxTotalBaselineBytes
	return c
}

// Close releases pooled connections.
func (c *Client) Close() { c.pool.closeAll() }

// Request describes one upstream call.
type Request struct {
	// CompactionMode: on uses model summaries directly, auto tries native first,
	// off uses native only. Empty keeps auto for internal legacy callers.
	CompactionMode   string
	CompactionModel  string
	CompactionPrompt string // Appended as the final user message for model summaries only.
	BeforeSummary    func(context.Context, string) error
	// Compact invokes the native HTTP compaction endpoint. CompactDirect keeps
	// its JSON response shape; otherwise it is adapted into Responses events.
	Compact       bool
	CompactDirect bool
	// ProxyURL is the account's egress proxy ("" = direct).
	ProxyURL string
	// Headers are the fully built Pi headers (both SSE and WS variants are derived
	// from the same options by the caller).
	SSEHeaders http.Header
	WSHeaders  http.Header
	// Body is the Pi-shaped request JSON.
	Body []byte
	// SessionID is the caller's wire session hint; it is never synthesized here.
	SessionID string
	// PoolSessionID is the internal downstream scope, separate from wire headers.
	PoolSessionID string
	ClientKeyID   int64
	Continuation  *session.Record
	// CodexBridge is bounded protocol metadata, never tool arguments or history.
	CodexBridge codexbridge.Profile
	// AccountID participates in the pool key so accounts never share a socket.
	AccountID int64
	// Transport is one of: sse, websocket, websocket-cached, auto.
	Transport string
	// Sink receives wire observations.
	Sink Sink
	// PreviousResponseID is echoed for logging/pooling decisions.
	PreviousResponseID string
}

// StreamResult reports how the request was served.
type StreamResult struct {
	Transport string
	Status    int
	// FallbackFrom is set when the WebSocket attempt was abandoned for SSE.
	FallbackFrom string
}

// Stream sends the request upstream and delivers events through onEvent until the
// stream completes. onEvent returning an error aborts the stream.
//
// Transport behaviour mirrors Pi:
//   - sse:              POST to the responses endpoint, stream SSE events
//   - websocket:        a fresh WebSocket connection per request
//   - websocket-cached: a pooled WebSocket connection reused across turns
//   - auto:             websocket-cached, falling back to SSE only for allowed
//     setup failures known to precede sending response.create
func (c *Client) Stream(ctx context.Context, req *Request, onEvent func(*Event) error) (*StreamResult, error) {
	if !req.CodexBridge.Valid() {
		return nil, &ContinuationError{Cause: session.ErrInvalid}
	}
	body, err := c.ExpandCompactionBody(req.Body, req.ClientKeyID)
	if err != nil {
		return nil, err
	}
	copyReq := *req
	copyReq.Body = body
	req = &copyReq
	if req.Compact {
		switch req.CompactionMode {
		case "on":
			return c.streamSummaryCompact(ctx, req, onEvent)
		case "", "auto", "off":
		default:
			return nil, &FailureError{Code: "invalid_compaction_mode", Status: 400, Message: "compaction_mode must be auto, on or off"}
		}
		return c.streamCompact(ctx, req, onEvent)
	}
	previous, err := requestPreviousID(req)
	if err != nil {
		return nil, err
	}
	if previous != "" {
		// An explicit parent is always connection-local. Even SSE callers must use
		// its original live socket; no HTTP fallback can restore that state.
		copyReq := *req
		copyReq.PreviousResponseID = previous
		if copyReq.Continuation == nil {
			copyReq.Continuation, err = c.ResolveContinuation(ctx, req.ClientKeyID, previous)
			if err != nil {
				return nil, err
			}
		}
		return c.streamWS(ctx, &copyReq, onEvent, true)
	}
	switch strings.ToLower(strings.TrimSpace(req.Transport)) {
	case "sse":
		return c.streamSSE(ctx, req, onEvent)
	case "websocket":
		return c.streamWS(ctx, req, onEvent, false)
	case "websocket-cached":
		return c.streamWS(ctx, req, onEvent, true)
	case "auto", "":
		res, err := c.streamWS(ctx, req, onEvent, true)
		if err == nil {
			return res, nil
		}
		if ctx.Err() != nil || !canFallbackToSSE(err) {
			return res, err
		}
		c.cfg.Logger.Warn("websocket transport failed; falling back to SSE", "error", err)
		if sink := req.Sink; sink != nil {
			sink.OnFrame("out", "transport", "fallback_sse", []byte(err.Error()), nil)
		}
		res, sseErr := c.streamSSE(ctx, req, onEvent)
		if sseErr != nil {
			return nil, sseErr
		}
		res.FallbackFrom = "websocket"
		return res, nil
	default:
		return c.streamSSE(ctx, req, onEvent)
	}
}

// transportError annotates a transport failure with how far the stream got.
type transportError struct {
	err           error
	eventsEmitted bool
	transport     string
	// requestNotSent is affirmative proof that response.create was never written.
	// The zero value means it may have been sent/executed, including partial writes.
	requestNotSent bool
}

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// canFallbackToSSE is deliberately fail-closed: no observed events does not prove
// that the upstream did not execute the request. Only pre-send connection errors
// and HTTP 5xx handshake responses are eligible; protocol and 4xx errors are not.
func canFallbackToSSE(err error) bool {
	var transport *transportError
	if !errors.As(err, &transport) || transport.transport != "websocket" || !transport.requestNotSent || transport.eventsEmitted {
		return false
	}
	if IsIdleTimeoutError(err) || IsConnectionLimitError(err) || IsPreviousResponseNotFound(err) {
		return false
	}
	var failure *FailureError
	if errors.As(err, &failure) {
		return failure.Status >= 500 && failure.Status <= 599
	}
	var networkError net.Error
	return errors.As(err, &networkError) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// IsFailureError reports whether the error contains a structured upstream failure.
// HTTP 5xx WebSocket handshake failures may allow fallback before a request is sent.
func IsFailureError(err error) bool {
	var target *FailureError
	return asErr(err, &target)
}

func asErr(err error, target **FailureError) bool {
	for err != nil {
		if e, ok := err.(*FailureError); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// ---- SSE transport ----

type sseNextResult struct {
	event *Event
	err   error
}

// nextSSEWithTimeout performs one blocking SSE read while enforcing the gap
// between events. The request context is canceled by the caller on timeout so
// the underlying HTTP response is unblocked as well.
func nextSSEWithTimeout(ctx context.Context, sse *SSEReader, idle time.Duration, cancel context.CancelFunc) (*Event, error) {
	result := make(chan sseNextResult, 1)
	go func() {
		event, err := sse.Next()
		result <- sseNextResult{event: event, err: err}
	}()

	timer := time.NewTimer(idle)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		cancel()
		return nil, &IdleTimeoutError{Timeout: idle}
	case next := <-result:
		return next.event, next.err
	}
}

func (c *Client) streamSSE(ctx context.Context, req *Request, onEvent func(*Event) error) (*StreamResult, error) {
	requestCtx, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()

	client, err := c.factory.HTTPClient(req.ProxyURL)
	if err != nil {
		return nil, err
	}

	headers := req.SSEHeaders.Clone()
	body := req.Body
	compressed := false

	if c.cfg.Zstd && len(body) > 0 {
		if enc, err := compressZstd(body); err == nil {
			body = enc
			compressed = true
			headers.Set("content-encoding", "zstd")
		} else {
			c.cfg.Logger.Debug("zstd compression unavailable; sending plain JSON", "error", err)
			headers.Del("content-encoding")
		}
	} else {
		headers.Del("content-encoding")
	}

	if sink := req.Sink; sink != nil {
		sink.OnRequestHeaders(c.cfg.SSEURL, headers)
		sink.OnRequestBody(req.Body, compressed)
	}

	httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.cfg.SSEURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("upstream: build request: %w", err)
	}
	httpReq.Header = headers

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, &transportError{err: fmt.Errorf("upstream: sse request failed: %w", err), transport: "sse"}
	}
	defer resp.Body.Close()

	if sink := req.Sink; sink != nil {
		sink.OnResponseHeaders(resp.StatusCode, resp.Header)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, truncated := readErrorBody(resp)
		observeErrorBody(req.Sink, bodyBytes, truncated)
		var data map[string]any
		_ = jsonUnmarshalBytes(bodyBytes, &data)
		code, _ := extractErrorFields(data)
		if code == "" {
			code = fmt.Sprintf("http_%d", resp.StatusCode)
		}
		return &StreamResult{Transport: "sse", Status: resp.StatusCode}, &FailureError{
			Code:      code,
			Status:    resp.StatusCode,
			RequestID: resp.Header.Get("x-request-id"),
			Message:   formatUpstreamError(resp.StatusCode, bodyBytes),
			Payload:   bodyBytes,
		}
	}

	reader, err := egress.DecodeBody(resp)
	if err != nil {
		return nil, fmt.Errorf("upstream: decode response: %w", err)
	}
	defer reader.Close()

	sse := ParseSSE(reader)
	result := &StreamResult{Transport: "sse", Status: resp.StatusCode}
	emitted := false

	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		event, err := nextSSEWithTimeout(requestCtx, sse, c.cfg.IdleTimeout, cancelRequest)
		if err != nil {
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return result, ctx.Err()
			}
			if err == io.EOF {
				return result, &transportError{
					err:           fmt.Errorf("upstream: sse stream truncated before a terminal event"),
					eventsEmitted: emitted,
					transport:     "sse",
				}
			}
			return result, &transportError{
				err:           fmt.Errorf("upstream: sse read: %w", err),
				eventsEmitted: emitted,
				transport:     "sse",
			}
		}
		if event.Type == "[DONE]" {
			return result, &transportError{err: fmt.Errorf("upstream: sse stream truncated before a response terminal event"), eventsEmitted: emitted, transport: "sse"}
		}

		emitted = true
		if sink := req.Sink; sink != nil {
			sink.OnFrame("in", "sse_event", event.Type, event.Raw, event.Data)
			if id := ExtractResponseID(event.Data); id != "" {
				sink.OnResponseID(id)
			}
			if IsTerminal(event.Type) {
				sink.OnUsage(ExtractUsage(event.Data))
			}
		}

		failure := FailureFromEvent(event)
		var details *FailureError
		if errors.As(failure, &details) && details.RequestID == "" {
			details.RequestID = resp.Header.Get("x-request-id")
		}
		if err := onEvent(failureEventForDelivery(event, failure)); err != nil {
			return result, &transportError{err: err, eventsEmitted: emitted, transport: "sse"}
		}
		if failure != nil {
			return result, &transportError{err: failure, eventsEmitted: emitted, transport: "sse"}
		}
		if IsTerminal(event.Type) {
			return result, nil
		}
	}
}

// KindHTTPError names a captured upstream error body frame.
const KindHTTPError = "http_error_body"

// readErrorBody applies the same decoding as successful responses, with a
// decompressed bound. Gorilla may already have clipped a rejected handshake;
// preserve that fact so diagnostic consumers never mistake a prefix for a body.
func readErrorBody(resp *http.Response) ([]byte, bool) {
	if resp == nil || resp.Body == nil {
		return nil, false
	}
	const limit = 1 << 20
	var reader io.ReadCloser
	var err error
	encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if encoding == "deflate" {
		// Sniff without consuming bytes: trying zlib and then raw deflate on
		// the same stream otherwise loses the header bytes on the failed try.
		buffered := bufio.NewReader(resp.Body)
		header, _ := buffered.Peek(2)
		if len(header) == 2 && header[0]&15 == 8 && (int(header[0])<<8|int(header[1]))%31 == 0 {
			reader, err = zlib.NewReader(buffered)
		} else {
			reader = flate.NewReader(buffered)
		}
	} else {
		reader, err = egress.DecodeBody(resp)
	}
	if err != nil {
		return nil, true // Do not display compressed garbage as a plaintext body.
	}
	defer reader.Close()
	payload, readErr := io.ReadAll(io.LimitReader(reader, limit+1))
	truncated := readErr != nil || len(payload) > limit
	if (encoding == "" || encoding == "identity") && resp.ContentLength > int64(len(payload)) {
		truncated = true
	}
	if len(payload) > limit {
		payload = payload[:limit]
	}
	return payload, truncated
}

func observeErrorBody(sink Sink, body []byte, truncated bool) {
	if sink == nil {
		return
	}
	eventType := "error_body"
	if truncated {
		eventType = "error_body_truncated"
	}
	sink.OnFrame("in", KindHTTPError, eventType, body, nil)
}

// formatUpstreamError renders a friendly message for a non-2xx upstream response,
// mirroring Pi's parseErrorResponse.
func formatUpstreamError(status int, body []byte) string {
	message := strings.TrimSpace(string(body))
	friendly := ""

	var parsed struct {
		Error struct {
			Code     string `json:"code"`
			Type     string `json:"type"`
			Message  string `json:"message"`
			PlanType string `json:"plan_type"`
			ResetsAt *int64 `json:"resets_at"`
		} `json:"error"`
	}
	if err := jsonUnmarshalBytes(body, &parsed); err == nil && parsed.Error.Message != "" {
		message = parsed.Error.Message
		code := parsed.Error.Code
		if code == "" {
			code = parsed.Error.Type
		}
		if isUsageLimitCode(code) || status == 429 {
			plan := ""
			if parsed.Error.PlanType != "" {
				plan = " (" + strings.ToLower(parsed.Error.PlanType) + " plan)"
			}
			friendly = "You have hit your ChatGPT usage limit" + plan + "."
			if parsed.Error.ResetsAt != nil {
				mins := (*parsed.Error.ResetsAt*1000 - time.Now().UnixMilli()) / 60000
				if mins < 0 {
					mins = 0
				}
				friendly += fmt.Sprintf(" Try again in ~%d min.", mins)
			}
		}
	}
	if friendly != "" {
		return friendly
	}
	if message == "" {
		message = http.StatusText(status)
	}
	if message == "" {
		message = fmt.Sprintf("upstream returned status %d", status)
	}
	return message
}

func isUsageLimitCode(code string) bool {
	switch strings.ToLower(code) {
	case "usage_limit_reached", "usage_not_included", "rate_limit_exceeded":
		return true
	}
	return false
}
