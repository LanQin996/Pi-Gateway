package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"pi-gateway/internal/capture"
	"pi-gateway/internal/piwire"
	"pi-gateway/internal/rulescapture"
	"pi-gateway/internal/store"
	"pi-gateway/internal/upstream"
)

// handleResponsesHTTP serves POST /v1/responses.
//
// With stream:true (the default for these clients) the upstream event stream is
// relayed as SSE frames built from "data:" lines only, which is what the ChatGPT
// backend emits and what Pi's parser consumes. With stream:false the terminal
// event's response object is returned as a single JSON body.
func (s *Server) handleResponsesHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rawBody, err := readBody(r)
	if err != nil {
		writeError(w, &apiError{Status: http.StatusBadRequest, Message: err.Error(), Type: "invalid_request_error"})
		return
	}

	p, apiErr := s.prepare(ctx, r, rawBody, "sse")
	if apiErr != nil {
		if p != nil {
			writeError(captureJSONResponse(w, p.Recorder, "error"), apiErr)
			s.finish(context.WithoutCancel(ctx), p, outcomeFor(apiErr, ctx), apiErr)
		} else {
			writeError(w, apiErr)
		}
		return
	}

	outcome := store.OutcomeOK
	var runErr error
	defer func() { s.finish(context.WithoutCancel(ctx), p, outcome, runErr) }()

	req, err := s.newUpstreamRequest(ctx, p)
	if err != nil {
		outcome, runErr = store.OutcomeError, err
		writeError(captureJSONResponse(w, p.Recorder, "error"), errorFromUpstream(err))
		return
	}

	if p.WantsStream {
		outcome, runErr = s.serveSSE(w, ctx, p, req)
		return
	}
	outcome, runErr = s.serveAggregated(w, ctx, p, req)
}

// newUpstreamRequest refreshes the account token and assembles the Pi-shaped
// upstream request.
func (s *Server) newUpstreamRequest(ctx context.Context, p *prepared) (*upstream.Request, error) {
	accessToken, accountID, err := s.accounts.EnsureFreshToken(ctx, p.Account)
	if err != nil {
		return nil, err
	}

	rt := s.settings.Get()
	headerOpts := piwire.HeaderOptions{
		ClientHeaders: p.ClientHeaders,
		AccessToken:   accessToken,
		AccountID:     accountID,
		SessionID:     p.WireSessionID,
		Originator:    s.cfg.Upstream.Originator,
		UserAgent:     firstNonEmpty(rt.UserAgent, s.cfg.Upstream.UserAgent),
		Platform: piwire.Platform{
			OS:             s.cfg.Upstream.StainlessOS,
			Arch:           s.cfg.Upstream.StainlessArch,
			Runtime:        s.cfg.Upstream.StainlessRuntime,
			RuntimeVersion: s.cfg.Upstream.StainlessRuntimeVersion,
		},
	}
	if s.cfg.Upstream.RequestTimeoutSeconds > 0 {
		headerOpts.TimeoutSeconds = s.cfg.Upstream.RequestTimeoutSeconds
	}

	sseHeaders, wsHeaders := piwire.BuildSSEHeaders(headerOpts), piwire.BuildWSHeaders(headerOpts)
	if p.RuleEngine != nil {
		p.RuleContext["settings"] = s.ruleSettings(rt)
		sseHeaders, err = p.buildRuleHeaders(ctx, "sse", accountID)
		if err != nil {
			return nil, pipelineAPIError(err)
		}
		wsHeaders, err = p.buildRuleHeaders(ctx, "websocket", accountID)
		if err != nil {
			return nil, pipelineAPIError(err)
		}
		sseHeaders.Set("Authorization", "Bearer "+accessToken)
		wsHeaders.Set("Authorization", "Bearer "+accessToken)
	}
	return &upstream.Request{
		CompactionMode:     p.CompactionMode,
		CompactionModel:    p.CompactionModel,
		CompactionPrompt:   p.CompactionPrompt,
		BeforeSummary:      func(ctx context.Context, model string) error { return s.prepareSummaryModel(ctx, p, model) },
		Compact:            p.Compact,
		CompactDirect:      p.CompactDirect,
		ProxyURL:           p.Account.ProxyURL,
		SSEHeaders:         sseHeaders,
		WSHeaders:          wsHeaders,
		Body:               p.Built.JSON,
		SessionID:          p.SessionID,
		PoolSessionID:      p.PoolSessionID,
		ClientKeyID:        p.Key.ID,
		Continuation:       p.Continuation,
		CodexBridge:        p.CodexBridge,
		AccountID:          p.Account.ID,
		Transport:          p.Transport,
		Sink:               p.sink(),
		PreviousResponseID: p.PreviousResponseID,
	}, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// serveSSE relays the upstream stream to the client as Server-Sent Events.
func (s *Server) serveSSE(w http.ResponseWriter, ctx context.Context, p *prepared, req *upstream.Request) (string, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		err := &apiError{Status: http.StatusInternalServerError, Message: "streaming unsupported", Type: "server_error"}
		writeError(captureJSONResponse(w, p.Recorder, "error"), err)
		return store.OutcomeError, err
	}

	delivery := &sseDelivery{writer: w, prepared: p, flusher: flusher, first: true}
	_, err := s.streamWith429Retry(ctx, p, req, func(event *upstream.Event) error { return delivery.onEvent(ctx, event) })
	if err != nil {
		outcome := outcomeFor(err, ctx)
		if !delivery.wroteHeaders {
			writeError(captureJSONResponse(w, p.Recorder, "error"), errorFromUpstream(err, p.ResponseHeaders))
			return outcome, err
		}
		// HTTP headers are already committed; include retry metadata in the
		// structured error frame as well, without duplicating upstream errors.
		if delivery.errorForwarded {
			return outcome, err
		}
		frame, _ := json.Marshal(streamErrorPayload(err, p.ResponseHeaders))
		setResponseWriteDeadline(w)
		formatted := formatSSEFrame(frame)
		if n, werr := w.Write(formatted); werr == nil && n == len(formatted) && p.Recorder != nil {
			p.Recorder.OnFrame(capture.DirClientOut, "sse_event", "error", frame, nil)
		}
		setResponseWriteDeadline(w)
		flusher.Flush()
		return outcome, err
	}

	delivery.commit()
	setResponseWriteDeadline(w)
	flusher.Flush()
	return store.OutcomeOK, nil
}

func setResponseWriteDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(upstream.DownstreamWriteTimeout))
}

// formatSSEFrame emits a data: line for every payload line. This preserves
// multiline JSON as one SSE data field instead of turning later lines into
// unrelated protocol text.
func formatSSEFrame(raw []byte) []byte {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return []byte(b.String())
}

func upstreamEventContainsError(event *upstream.Event) bool {
	if event == nil {
		return false
	}
	if event.Type == upstream.EventError {
		return true
	}
	if event.Type == upstream.EventResponseFailed {
		return true
	}
	return false
}

// serveAggregated handles stream:false by returning the terminal response object.
func (s *Server) serveAggregated(w http.ResponseWriter, ctx context.Context, p *prepared, req *upstream.Request) (string, error) {
	var terminal map[string]any
	var lastEvent *upstream.Event

	_, err := s.streamWith429Retry(ctx, p, req, func(event *upstream.Event) error {
		transformed, transformErr := p.transformResponseEvent(ctx, event)
		if transformErr != nil {
			return transformErr
		}
		if transformed == nil {
			return nil
		}
		lastEvent = transformed
		if upstream.IsTerminal(transformed.Type) {
			terminal = transformed.Data
		}
		return nil
	})
	if err != nil {
		writeError(captureJSONResponse(w, p.Recorder, "error"), errorFromUpstream(err, p.ResponseHeaders))
		return outcomeFor(err, ctx), err
	}

	copyResponseHeaders(w, p.ResponseHeaders)
	if terminal == nil {
		terminal = lastEventPayload(lastEvent)
	}
	if terminal == nil {
		apiErr := &apiError{Status: http.StatusBadGateway, Message: "upstream returned no events", Type: "upstream_error"}
		writeError(captureJSONResponse(w, p.Recorder, "error"), apiErr)
		return store.OutcomeError, apiErr
	}

	// The terminal event wraps the response object; the OpenAI shape is the inner one.
	payload := rulescapture.AggregatePayload(terminal)
	var transformErr error
	payload, transformErr = p.transformResponseBody(ctx, payload)
	if transformErr != nil {
		writeError(captureJSONResponse(w, p.Recorder, "error"), errorFromUpstream(transformErr, p.ResponseHeaders))
		return store.OutcomeError, transformErr
	}
	writeJSON(captureJSONResponse(w, p.Recorder, "response"), http.StatusOK, payload)
	return store.OutcomeOK, nil
}

// captureJSONResponse observes only non-streaming writes, keeping writeError and
// writeJSON as the single source of truth for the client-visible payload.
func captureJSONResponse(w http.ResponseWriter, recorder *capture.Recorder, eventType string) http.ResponseWriter {
	if recorder == nil {
		return w
	}
	return &clientJSONCaptureWriter{ResponseWriter: w, recorder: recorder, eventType: eventType}
}

type clientJSONCaptureWriter struct {
	http.ResponseWriter
	recorder     *capture.Recorder
	eventType    string
	wroteHeaders bool
}

func (w *clientJSONCaptureWriter) WriteHeader(status int) {
	w.ResponseWriter.WriteHeader(status)
	if !w.wroteHeaders {
		w.wroteHeaders = true
		w.recorder.OnClientResponseHeaders(status, w.Header())
	}
}

func (w *clientJSONCaptureWriter) Write(raw []byte) (int, error) {
	if !w.wroteHeaders {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(raw)
	if n > 0 {
		// json.Encoder reuses its buffer after Write returns. Retain only the
		// accepted bytes, independently of that buffer and of the upstream body.
		w.recorder.OnFrame(capture.DirClientOut, store.KindHTTPResponse, w.eventType, bytes.Clone(raw[:n]), nil)
	}
	return n, err
}

func lastEventPayload(event *upstream.Event) map[string]any {
	if event == nil {
		return nil
	}
	if resp, ok := event.Data["response"].(map[string]any); ok {
		return resp
	}
	return event.Data
}

// outcomeFor classifies an error into a capture outcome.
func outcomeFor(err error, ctx context.Context) string {
	if err == nil {
		return store.OutcomeOK
	}
	if ctx.Err() != nil {
		return store.OutcomeAborted
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "aborted") || strings.Contains(msg, "client closed") ||
		strings.Contains(msg, "broken pipe") || strings.Contains(msg, "reset by peer") {
		return store.OutcomeAborted
	}
	return store.OutcomeError
}

// safeRetryHeaders copies only SDK retry controls and the upstream request id.
// Do not forward credentials, cookies, framing headers or unrelated metadata.
func safeRetryHeaders(sources ...http.Header) http.Header {
	out := make(http.Header)
	for _, headers := range sources {
		for name, values := range headers {
			switch strings.ToLower(name) {
			case "x-should-retry", "retry-after", "retry-after-ms", "x-request-id":
				out[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
			}
		}
	}
	return out
}

// copyResponseHeaders forwards only the safe retry allowlist, replacing old values.
func copyResponseHeaders(w http.ResponseWriter, headers http.Header) {
	for name, values := range safeRetryHeaders(headers) {
		w.Header()[name] = values
	}
}

func previousResponseID(body map[string]any) string {
	if v, ok := body["previous_response_id"].(string); ok {
		return v
	}
	return ""
}

type sseDelivery struct {
	writer                              http.ResponseWriter
	prepared                            *prepared
	flusher                             http.Flusher
	wroteHeaders, first, errorForwarded bool
}

func (delivery *sseDelivery) commit() {
	if delivery.wroteHeaders {
		return
	}
	delivery.wroteHeaders = true
	copyResponseHeaders(delivery.writer, delivery.prepared.ResponseHeaders)
	delivery.writer.Header().Set("Content-Type", "text/event-stream")
	delivery.writer.Header().Set("Cache-Control", "no-cache, no-transform")
	delivery.writer.Header().Set("Connection", "keep-alive")
	// Ask intermediary proxies not to buffer, so tokens arrive immediately.
	delivery.writer.Header().Set("X-Accel-Buffering", "no")
	setResponseWriteDeadline(delivery.writer)
	delivery.writer.WriteHeader(http.StatusOK)
	if delivery.prepared.Recorder != nil {
		delivery.prepared.Recorder.OnClientResponseHeaders(http.StatusOK, delivery.writer.Header())
	}
}
func (delivery *sseDelivery) onEvent(ctx context.Context, event *upstream.Event) error {
	if delivery.first {
		delivery.first = false
		if delivery.prepared.Recorder != nil {
			delivery.prepared.Recorder.NoteTTFB()
		}
	}
	event, transformErr := delivery.prepared.transformResponseEvent(ctx, event)
	if transformErr != nil {
		return transformErr
	}
	if event == nil {
		return nil
	}
	delivery.commit()
	setResponseWriteDeadline(delivery.writer)
	if _, werr := delivery.writer.Write(formatSSEFrame(event.Raw)); werr != nil {
		return werr
	}
	if upstreamEventContainsError(event) {
		delivery.errorForwarded = true
	}
	if delivery.prepared.Recorder != nil {
		// The client hop: exactly the bytes handed to the client.
		delivery.prepared.Recorder.OnFrame(capture.DirClientOut, "sse_event", event.Type, event.Raw, event.Data)
	}
	delivery.flusher.Flush()
	return nil
}
