package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"pi-gateway/internal/capture"
	"pi-gateway/internal/rulescapture"
	"pi-gateway/internal/store"
	"pi-gateway/internal/upstream"
)

// maxClientWSFrameBytes bounds a single inbound client frame so a client cannot
// make the gateway buffer an arbitrarily large message.
const maxClientWSFrameBytes = 64 << 20

// wsUpgrader accepts client WebSocket connections.
//
// Browsers are not the target audience (these are CLI/agent clients), so origin
// checks are disabled; authentication still happens on the first frame via the
// API key, or during the handshake through the Authorization/x-api-key headers.
var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  64 * 1024,
	WriteBufferSize: 64 * 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
	// Do not negotiate permessage-deflate: the upstream (undici) does not offer
	// it, and keeping both sides identical avoids any fingerprint difference.
	EnableCompression: false,
}

// handleResponsesWS serves the WebSocket variant of the Responses API.
//
// Each text frame the client sends is treated as one Responses request, exactly
// like Pi's `{"type":"response.create", ...body}` envelope. Every upstream event
// is written back as its own JSON text frame, so a WebSocket client sees the same
// frame sequence it would see against the real backend. The connection stays open
// for further turns, which is what lets clients reuse conversation state.
func (s *Server) handleResponsesWS(w http.ResponseWriter, r *http.Request) {
	// Authenticate before upgrading when a key is supplied in the handshake.
	if extractClientKey(r) == "" {
		writeError(w, &apiError{Status: http.StatusUnauthorized, Message: "invalid or missing API key", Type: "invalid_request_error"})
		return
	}
	if _, err := s.authenticate(r.Context(), r); err != nil {
		writeError(w, &apiError{Status: http.StatusUnauthorized, Message: "invalid or missing API key", Type: "invalid_request_error"})
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote a response.
		s.logger.Debug("websocket upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	conn.SetReadLimit(maxClientWSFrameBytes)

	ctx, cancel := context.WithTimeout(r.Context(), s.wsMaxAge)
	wsID := uuid.NewString()
	ctx = context.WithValue(ctx, downstreamWSContextKey{}, wsID)
	control := &upstream.ResponseControl{}
	ctx = upstream.WithResponseControl(ctx, control)
	r = r.WithContext(ctx)
	if !s.lifecycle.track(wsID, cancel) {
		cancel()
		_ = conn.Close()
		return
	}
	defer s.lifecycle.untrack(wsID)
	defer cancel()

	// Keep one reader goroutine active for the entire connection. It notices a
	// close while an exchange is blocked upstream, but the handler still consumes
	// and processes requests strictly one at a time.
	// The buffered slot also preserves the first frame before the handler is
	// scheduled to consume it. All replies share one connection-wide writer.
	messages := make(chan wsReadEvent, 1)
	writer := newWSFrameWriter(conn)
	go func() {
		defer cancel()
		_ = readWSMessages(ctx, conn, writer, messages, control)
	}()
	go func() {
		<-ctx.Done()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// A bounded best-effort notice must not wait behind a stalled writer.
			noticeCtx, stop := context.WithTimeout(context.Background(), time.Second)
			_ = writer.writeJSON(noticeCtx, wsConnectionLimitPayload())
			stop()
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "connection_limit"),
				time.Now().Add(100*time.Millisecond))
		}
		// This also interrupts blocked reads/writes during an active exchange.
		_ = conn.Close()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-messages:
			if !ok {
				return
			}
			if ctx.Err() != nil || event.messageType != websocket.TextMessage {
				continue
			}

			body, err := requestBodyFromWSFrame(event.payload)
			if err != nil {
				_ = writer.writeJSON(ctx, errorFramePayload(&apiError{Status: http.StatusBadRequest, Message: err.Error(), Type: "invalid_request_error"}))
				continue
			}

			if err := s.serveWSExchange(ctx, writer, r, body, event.payload); err != nil {
				// serveWSExchange already reported the failure to the client.
				s.logger.Debug("websocket exchange failed", "error", err)
			}
		}
	}
}

type wsReadEvent struct {
	messageType int
	payload     []byte
}

func readWSMessages(ctx context.Context, conn *websocket.Conn, writer *wsFrameWriter, messages chan<- wsReadEvent, controls ...*upstream.ResponseControl) error {
	defer close(messages)
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if messageType != websocket.TextMessage {
			continue
		}
		handled, err := handleWSInterrupt(ctx, writer, payload, controls)
		if err != nil {
			return err
		}
		if handled {
			continue
		}
		// A second request may wait, but further requests must receive an explicit
		// rejection rather than disappearing while the exchange is in progress.
		if err := enqueueWSMessage(ctx, writer, messages, wsReadEvent{messageType: messageType, payload: payload}); err != nil {
			return err
		}
	}
}

// wsConnectionLimitPayload is the error frame sent when a Responses WebSocket
// connection reaches its 60-minute cap (codex-proxy-rs shape).
func wsConnectionLimitPayload() map[string]any {
	return map[string]any{
		"type":   "error",
		"status": http.StatusBadRequest,
		"error": map[string]any{
			"type":    "invalid_request_error",
			"code":    "websocket_connection_limit_reached",
			"message": "Responses websocket connection limit reached (60 minutes). Create a new websocket connection to continue.",
		},
	}
}

// requestBodyFromWSFrame unwraps the client's response.create envelope.
//
// Any "type" field is removed so the remaining object is a plain Responses API
// body; frames that are already a plain body pass through unchanged.
func requestBodyFromWSFrame(payload []byte) (map[string]any, error) {
	return rulescapture.UnwrapWS(payload)
}

// serveWSExchange runs one request/response exchange over the client socket.
func (s *Server) serveWSExchange(ctx context.Context, writer *wsFrameWriter, r *http.Request, body map[string]any, rawIn []byte) error {
	raw, err := json.Marshal(body)
	if err != nil {
		_ = writer.writeJSON(ctx, errorFramePayload(&apiError{Status: http.StatusBadRequest, Message: "could not encode request", Type: "invalid_request_error"}))
		return err
	}

	// A per-exchange context so a client disconnect cannot leave the upstream call
	// running, but a slow upstream still streams to the client.
	exchangeCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	p, apiErr := s.prepare(exchangeCtx, r, raw, "ws")
	if p != nil && p.Recorder != nil && len(rawIn) > 0 {
		// Retain the original response.create envelope even when policy blocks it.
		p.Recorder.OnFrame(capture.DirClientIn, "ws_frame", "response.create", rawIn, nil)
	}
	if apiErr != nil {
		s.rejectPreparedWS(ctx, exchangeCtx, writer, p, apiErr)
		return apiErr
	}

	outcome := store.OutcomeOK
	var runErr error
	defer func() { s.finish(context.WithoutCancel(exchangeCtx), p, outcome, runErr) }()

	req, err := s.newUpstreamRequest(exchangeCtx, p)
	if err != nil {
		outcome, runErr = store.OutcomeError, err
		frame, _ := json.Marshal(streamErrorPayload(err, p.ResponseHeaders))
		if werr := writer.write(ctx, frame); werr == nil && p.Recorder != nil {
			p.Recorder.OnFrame(capture.DirClientOut, "ws_frame", "error", frame, nil)
		}
		return err
	}

	delivery := &wsDelivery{writer: writer, prepared: p, first: true}
	_, err = s.streamWith429Retry(exchangeCtx, p, req, func(event *upstream.Event) error { return delivery.onEvent(exchangeCtx, event) })
	if err != nil {
		outcome, runErr = outcomeFor(err, exchangeCtx), err
		if exchangeCtx.Err() != nil || delivery.errorForwarded {
			return err
		}
		frame, _ := json.Marshal(streamErrorPayload(err, p.ResponseHeaders))
		if werr := writer.write(exchangeCtx, frame); werr == nil && p.Recorder != nil {
			p.Recorder.OnFrame(capture.DirClientOut, "ws_frame", "error", frame, nil)
		}
		return err
	}
	return nil
}

// wsFrameWriter serializes all writes on a client connection, including queue
// rejection frames sent by the reader goroutine. Its context-aware lock avoids
// waiting behind a stalled write after the exchange has been cancelled.
type wsFrameWriter struct {
	conn *websocket.Conn
	mu   chan struct{}
}

func newWSFrameWriter(conn *websocket.Conn) *wsFrameWriter {
	mu := make(chan struct{}, 1)
	mu <- struct{}{}
	return &wsFrameWriter{conn: conn, mu: mu}
}

func (w *wsFrameWriter) write(ctx context.Context, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.mu:
	}
	defer func() { w.mu <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Use the same five-minute downstream budget as normal stream writes; this
	// keeps control/error frames from outliving the account slot indefinitely.
	deadline := time.Now().Add(upstream.DownstreamWriteTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = w.conn.SetWriteDeadline(deadline)
	return w.conn.WriteMessage(websocket.TextMessage, payload)
}

func (w *wsFrameWriter) writeJSON(ctx context.Context, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return w.write(ctx, raw)
}

func isNormalWSClose(err error) bool {
	if errors.Is(err, websocket.ErrCloseSent) {
		return true
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		switch closeErr.Code {
		case websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived:
			return true
		}
		return true
	}
	return strings.Contains(err.Error(), "use of closed network connection")
}

func handleWSInterrupt(ctx context.Context, writer *wsFrameWriter, payload []byte, controls []*upstream.ResponseControl) (bool, error) {
	var frame struct {
		Type       string          `json:"type"`
		ResponseID json.RawMessage `json:"response_id"`
		Mode       json.RawMessage `json:"mode"`
		EventID    json.RawMessage `json:"event_id"`
	}
	if json.Unmarshal(payload, &frame) == nil && frame.Type == "response.interrupt" && len(controls) > 0 {
		var responseID, mode, eventID string
		_ = json.Unmarshal(frame.ResponseID, &responseID)
		_ = json.Unmarshal(frame.Mode, &mode)
		_ = json.Unmarshal(frame.EventID, &eventID)
		if err := controls[0].Interrupt(ctx, responseID, mode); err != nil {
			rejection := errorFramePayload(&apiError{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: err.Error()})
			if eventID != "" {
				rejection["event_id"] = eventID
			}
			if err := writer.writeJSON(ctx, rejection); err != nil {
				return true, err
			}
		}
		return true, nil
	}

	return false, nil
}

func rejectWSQueueFull(ctx context.Context, writer *wsFrameWriter, payload []byte) error {
	rejection := errorFramePayload(&apiError{
		Status: http.StatusTooManyRequests, Type: "rate_limit_error",
		Code: "too_many_requests", Message: "WebSocket request queue is full; retry after the current response completes.",
	})
	// Echo the optional event ID so a pipelining client can identify which
	// request was rejected even while earlier response events are flowing.
	var request struct {
		EventID string `json:"event_id"`
	}
	if json.Unmarshal(payload, &request) == nil && request.EventID != "" {
		rejection["event_id"] = request.EventID
	}
	if err := writer.writeJSON(ctx, rejection); err != nil {
		return err
	}
	return nil
}

type wsDelivery struct {
	writer                *wsFrameWriter
	prepared              *prepared
	first, errorForwarded bool
}

func (delivery *wsDelivery) onEvent(ctx context.Context, event *upstream.Event) error {
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
	// Preserve ordinary events verbatim. Error events carry the safe retry
	// metadata since this socket has already completed its HTTP upgrade.
	rawOut, dataOut := delivery.eventPayload(event)
	if werr := delivery.writer.write(ctx, rawOut); werr != nil {
		return werr
	}
	if delivery.prepared.Recorder != nil {
		delivery.prepared.Recorder.OnFrame(capture.DirClientOut, "ws_frame", event.Type, rawOut, dataOut)
	}
	return nil
}

func (s *Server) rejectPreparedWS(ctx, exchangeCtx context.Context, writer *wsFrameWriter, p *prepared, apiErr *apiError) {
	if p != nil {
		frame := errorFramePayload(apiErr)
		if werr := writer.writeJSON(ctx, frame); werr == nil && p.Recorder != nil {
			encoded, _ := json.Marshal(frame)
			p.Recorder.OnFrame(capture.DirClientOut, "ws_frame", "error", encoded, frame)
		}
		s.finish(context.WithoutCancel(exchangeCtx), p, store.OutcomeError, apiErr)
	} else {
		_ = writer.writeJSON(ctx, errorFramePayload(apiErr))
	}
}

func (delivery *wsDelivery) eventPayload(event *upstream.Event) ([]byte, map[string]any) {
	rawOut, dataOut := event.Raw, event.Data
	if upstreamEventContainsError(event) {
		delivery.errorForwarded = true
		if len(delivery.prepared.ResponseHeaders) > 0 {
			dataOut = make(map[string]any, len(event.Data)+1)
			for key, value := range event.Data {
				dataOut[key] = value
			}
			addRetryHeadersToPayload(dataOut, delivery.prepared.ResponseHeaders)
			if encoded, encodeErr := json.Marshal(dataOut); encodeErr == nil {
				rawOut = encoded
			}
		}
	}
	return rawOut, dataOut
}

func enqueueWSMessage(ctx context.Context, writer *wsFrameWriter, messages chan<- wsReadEvent, message wsReadEvent) error {
	select {
	case messages <- message:
	default:
		if err := rejectWSQueueFull(ctx, writer, message.payload); err != nil {
			return err
		}
	}
	return nil
}
