package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"pi-gateway/internal/session"

	"github.com/gorilla/websocket"
)

// responseCreatePrefix mirrors Pi's `JSON.stringify({ type: "response.create", ...requestBody })`.
// The envelope is prepended byte-wise so the body's own key order is preserved exactly.
const responseCreatePrefix = `{"type":"response.create",`

const maxWSFrameBytes = 4 << 20

// ErrWSFrameTooLarge identifies an upstream WebSocket frame over the configured limit.
var ErrWSFrameTooLarge = errors.New("upstream: websocket frame exceeds size limit")

// WSFrameTooLargeError reports the configured WebSocket frame limit.
type WSFrameTooLargeError struct{ Limit int }

func (e *WSFrameTooLargeError) Error() string {
	return fmt.Sprintf("upstream: websocket frame exceeds size limit (%d bytes)", e.Limit)
}
func (e *WSFrameTooLargeError) Unwrap() error { return ErrWSFrameTooLarge }

// buildWSFrame wraps the Pi-shaped request body in the response.create envelope.
// Pi's custom openai-codex-responses transport sends stream:true and store:false
// unchanged (buildRequestBody + processWebSocketStream); the SDK ResponsesWS
// comment about implicit streaming does not describe that custom wire payload.
func buildWSFrame(body []byte) ([]byte, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed == "{}" {
		return []byte(`{"type":"response.create"}`), nil
	}
	if !strings.HasPrefix(trimmed, "{") {
		return nil, fmt.Errorf("upstream: request body is not a JSON object")
	}
	// Replace the leading '{' with the envelope, which already contains it.
	rest := trimmed[1:]
	out := make([]byte, 0, len(responseCreatePrefix)+len(rest))
	out = append(out, responseCreatePrefix...)
	out = append(out, rest...)
	return out, nil
}

// streamWS makes one attempt. After any response.create write, even a first-frame
// rejection cannot trigger a transparent retry: execution may already have begun.
func (c *Client) streamWS(ctx context.Context, req *Request, onEvent func(*Event) error, usePool bool) (*StreamResult, error) {
	control, _ := ctx.Value(responseControlKey{}).(*ResponseControl)
	if control != nil {
		defer func() { control.clear(); control.flush(req.Sink) }()
	}
	frame, err := buildWSFrame(req.Body)
	if err != nil {
		return nil, err
	}

	key := ""
	if usePool && c.cfg.Pool {
		key = c.requestPoolKey(req)
	}

	explicit := req.PreviousResponseID != ""
	if explicit {
		r := req.Continuation
		if !c.validWSContinuation(req, r) {
			return nil, continuationMismatch()
		}
		key = r.SessionID // trusted opaque original pool scope, not this downstream's ID
	}
	conn, reused, result, err := c.acquireWSConnection(ctx, req, key, explicit)
	if err != nil {
		return result, err
	}

	// On a reused socket with cached conversation state, send only the new input
	// items (previous_response_id + delta input), exactly as Pi does for
	// websocket-cached. When the cached state cannot be continued, Pi clears it and
	// sends the full body over the same socket, so we do the same.
	conn.socket.SetReadLimit(maxWSFrameBytes)

	if usePool && !explicit && reused && conn.continuation != nil {
		frame = c.continuedWSFrame(req, conn, key, frame)
	}

	keep := true
	watchDone := make(chan struct{})
	watchExited := make(chan struct{})
	ageTimer := time.NewTimer(time.Until(conn.createdAt.Add(c.pool.maxAge)))
	defer ageTimer.Stop()
	go func() {
		defer close(watchExited)
		select {
		case <-ctx.Done():
			conn.markDead()
			_ = conn.socket.Close()
		case <-ageTimer.C:
			conn.markDead()
			_ = conn.socket.Close()
		case <-watchDone:
		}
	}()
	defer func() {
		if control != nil {
			control.clear()
		}
		close(watchDone)
		<-watchExited // Never let a previous request's watcher close a reused socket.
		c.releaseWSConnection(ctx, key, conn, keep)
	}()

	// Record the WebSocket handshake request side once per exchange.
	c.observeWSRequest(req, frame)

	if err := writeWSRequest(conn, frame); err != nil {
		return result, err
	}

	exchange := wsExchange{client: c, request: req, onEvent: onEvent, connection: conn, key: key, usePool: usePool, explicit: explicit, result: result, control: control, keep: true}
	result, err = exchange.readLoop(ctx)
	keep = exchange.keep
	return result, err
}

// dialWS opens a WebSocket connection with the Pi handshake headers, honouring the
// account's egress proxy.
func (c *Client) dialWS(ctx context.Context, req *Request) (*websocket.Conn, *http.Response, error) {
	dialCfg, err := c.factory.WSDialConfig(req.ProxyURL)
	if err != nil {
		return nil, nil, err
	}

	// Own the whole handshake context: Gorilla's deadline does not interrupt
	// explicit cancellation while reading the HTTP upgrade response.
	ctx, cancel := context.WithTimeout(ctx, c.cfg.ConnectTimeout)
	defer cancel()
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 0
	// undici (Node's WebSocket, used by Pi) does not offer permessage-deflate, so
	// neither do we; sending the extension would be an immediate fingerprint
	// difference.
	dialer.EnableCompression = false
	dialer.ReadBufferSize = 64 * 1024
	dialer.WriteBufferSize = 64 * 1024
	dialer.Proxy = dialCfg.Proxy
	dialer.TLSClientConfig = dialCfg.TLSClientConfig
	var stop func() bool
	closed := make(chan struct{})
	dialer.NetDialContext = func(dialCtx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dialCfg.NetDialContext(dialCtx, network, addr)
		if err != nil {
			return nil, err
		}
		stop = context.AfterFunc(ctx, func() {
			_ = conn.Close()
			close(closed)
		})
		return conn, nil
	}

	header := wsHandshakeHeaders(req.WSHeaders)

	conn, resp, err := dialer.DialContext(ctx, c.cfg.WSURL, header)
	if stop != nil && !stop() {
		<-closed // A canceled handshake must not race with pool registration.
	}
	if ctx.Err() != nil {
		closeWSHandshake(conn, resp)
		return nil, nil, fmt.Errorf("upstream: websocket handshake failed: %w", ctx.Err())
	}
	if resp != nil && req.Sink != nil {
		// Preserve observation order: headers first, then a rejected handshake
		// body, just as for the HTTP/SSE path.
		req.Sink.OnResponseHeaders(resp.StatusCode, resp.Header)
	}
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			return nil, resp, rejectedWSHandshake(resp, req.Sink)
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, nil, fmt.Errorf("upstream: websocket handshake failed: %w", err)
	}
	return conn, resp, nil
}

// PoolSize exposes the pooled connection count for the admin UI.
func (c *Client) PoolSize() int { return c.pool.Size() }

// wsExchange owns the mutable state for exactly one response.create exchange.
type wsExchange struct {
	client                           *Client
	request                          *Request
	onEvent                          func(*Event) error
	connection                       *wsConn
	key                              string
	usePool, explicit, keep, emitted bool
	result                           *StreamResult
	control                          *ResponseControl
}

func (x *wsExchange) readLoop(ctx context.Context) (*StreamResult, error) {
	idle := x.client.cfg.IdleTimeout
	for {
		if err := ctx.Err(); err != nil {
			x.keep = false
			return x.result, err
		}
		_ = x.connection.socket.SetReadDeadline(time.Now().Add(idle))

		messageType, payload, err := x.connection.socket.ReadMessage()
		if x.control != nil {
			x.control.flush(x.request.Sink)
		}
		if err != nil {
			return x.result, x.readError(ctx, err, idle)
		}

		var decoded map[string]any
		if err := json.Unmarshal(payload, &decoded); err != nil {
			x.connection.markDead()
			return x.result, &transportError{
				err:           fmt.Errorf("upstream: invalid websocket JSON: %w", err),
				eventsEmitted: x.emitted,
				transport:     "websocket",
			}
		}
		eventType, _ := decoded["type"].(string)
		event := &Event{Type: eventType, Raw: payload, Data: decoded, At: time.Now()}
		failure := FailureFromEvent(event)
		deliveryEvent := failureEventForDelivery(event, failure)

		responseID := ExtractResponseID(decoded)
		x.observeEvent(event, failure, responseID, messageType)
		ready := x.readyToCommit(ctx, event, failure, responseID)
		var record *session.Record
		if ready {
			record = x.client.completedRecord(x.request, x.connection, x.key, responseID)
			// Bound the shared write before terminal delivery. A late shared record
			// alone can never pass the live socket's post-callback ready check.
			x.client.commitShared(ctx, record)
		}
		if err := x.deliverEvent(ctx, deliveryEvent, failure); err != nil {
			return x.result, err
		}
		x.emitted = true
		if IsTerminal(eventType) {
			x.finishTerminal(ctx, ready, responseID, decoded, record)
			return x.result, nil
		}
	}
}

func (x *wsExchange) readError(ctx context.Context, err error, idle time.Duration) error {
	x.connection.markDead()
	if errors.Is(err, websocket.ErrReadLimit) {
		return &transportError{
			err:           &WSFrameTooLargeError{Limit: maxWSFrameBytes},
			eventsEmitted: x.emitted,
			transport:     "websocket",
		}
	}
	if ctx.Err() != nil {
		x.keep = false
		return ctx.Err()
	}
	if time.Since(x.connection.createdAt) >= x.client.pool.maxAge {
		return &transportError{err: ErrWSMaxAge, eventsEmitted: x.emitted, transport: "websocket"}
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return &transportError{
			err:           &IdleTimeoutError{Timeout: idle},
			eventsEmitted: x.emitted,
			transport:     "websocket",
		}
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		if x.request.Sink != nil {
			x.request.Sink.OnFrame("in", "ws_close", fmt.Sprintf("%d", closeErr.Code), []byte(closeErr.Text), nil)
		}
		return &transportError{
			err:           fmt.Errorf("upstream: websocket stream truncated before a terminal event: %w", closeErr),
			eventsEmitted: x.emitted,
			transport:     "websocket",
		}
	}
	return &transportError{
		err:           fmt.Errorf("upstream: websocket stream truncated before a terminal event: %w", err),
		eventsEmitted: x.emitted,
		transport:     "websocket",
	}
}

func (x *wsExchange) observeEvent(event *Event, failure error, responseID string, messageType int) {
	if x.control != nil {
		if IsTerminal(event.Type) || failure != nil {
			x.control.clear()
			x.control.flush(x.request.Sink)
		} else if responseID != "" {
			x.control.bind(x.connection, responseID)
		}
	}
	if x.request.Sink != nil {
		kind := "ws_frame"
		if messageType == websocket.BinaryMessage {
			kind = "ws_binary"
		}
		x.request.Sink.OnFrame("in", kind, event.Type, event.Raw, event.Data)
		if responseID != "" {
			x.request.Sink.OnResponseID(responseID)
		}
		if IsTerminal(event.Type) {
			x.request.Sink.OnUsage(ExtractUsage(event.Data))
		}
	}
}

func (x *wsExchange) finishTerminal(ctx context.Context, ready bool, responseID string, decoded map[string]any, record *session.Record) {
	if ready && ctx.Err() == nil && !x.connection.isDead() {
		// Only a successfully delivered terminal may advance local readiness.
		x.connection.lastResponseID = responseID
		var baseline *wsContinuation
		var size int64
		// Explicit input is already a delta, not a full-history baseline.
		if x.usePool && !x.explicit && int64(len(x.request.Body)) <= x.client.cfg.MaxBaselineBytes {
			if exceptInput, input, ok := bodyExceptInput(x.request.Body); ok {
				baseline = &wsContinuation{bodyExceptInput: exceptInput, lastInput: input,
					lastResponseItems: responseItemsFromTerminal(decoded), lastResponseID: responseID, codexBridge: x.request.CodexBridge}
				size = baseline.size()
			}
		}
		x.client.pool.setBaseline(x.key, x.connection, baseline, size, record)
	} else {
		// incomplete/done-with-failed-status must never retain a ready parent.
		x.keep = false
		x.connection.markDead()
	}
}

func (c *Client) acquireWSConnection(ctx context.Context, req *Request, key string, explicit bool) (*wsConn, bool, *StreamResult, error) {
	conn, acquireErr := c.pool.acquire(ctx, key)
	if acquireErr != nil {
		return nil, false, nil, acquireErr
	}
	result := &StreamResult{Transport: "websocket"}

	// A waiter may acquire the lane after another request advanced its parent.
	// Never dial a replacement for an explicit connection-local continuation.
	if explicit && (conn == nil || conn.id != req.Continuation.ConnectionID || !c.pool.matches(req.Continuation)) {
		if conn != nil {
			c.pool.put(key, conn)
		} else {
			c.pool.release(key)
		}
		return nil, false, result, continuationMismatch()
	}
	reused := conn != nil
	if conn == nil {
		fresh, resp, err := c.dialWS(ctx, req)
		if resp != nil {
			result.Status = resp.StatusCode
		}
		if err != nil {
			c.pool.release(key)
			return nil, false, result, &transportError{err: err, transport: "websocket", requestNotSent: true}
		}
		conn = &wsConn{socket: fresh, id: newConnectionID(), createdAt: time.Now(), busy: true}
		if !c.pool.register(key, conn) {
			c.pool.discard(key, conn)
			return nil, false, nil, fmt.Errorf("upstream: pool closed")
		}
	}

	return conn, reused, result, nil
}

func (c *Client) continuedWSFrame(req *Request, conn *wsConn, key string, frame []byte) []byte {
	continued := false
	if deltaBody, ok := buildDeltaBody(req.Body, conn.continuation, req.CodexBridge); ok {
		if deltaFrame, err := buildWSFrame(deltaBody); err == nil {
			frame = deltaFrame
			continued = true
		}
	}
	if !continued {
		c.pool.setBaseline(key, conn, nil, 0, nil)
	}

	return frame
}

func (x *wsExchange) deliverEvent(ctx context.Context, deliveryEvent *Event, failure error) error {
	if err := ctx.Err(); err != nil {
		x.keep = false
		return err
	}
	if x.connection.isDead() {
		x.keep = false
		return &transportError{err: ErrWSMaxAge, eventsEmitted: x.emitted, transport: "websocket"}
	}
	if err := x.onEvent(deliveryEvent); err != nil {
		// A callback error may follow a partial downstream write, so it always
		// prevents transparent retry.
		x.emitted = true
		x.keep = false
		return &transportError{err: err, eventsEmitted: x.emitted, transport: "websocket"}
	}
	if failure != nil {
		// This failure reached the callback, so retry is no longer safe.
		x.emitted = true
		// A logical failure poisons the conversation state on this socket.
		x.connection.continuation = nil
		x.connection.markDead()
		x.keep = false
		return &transportError{err: failure, eventsEmitted: x.emitted, transport: "websocket"}
	}
	return nil
}

func rejectedWSHandshake(resp *http.Response, sink Sink) error {
	payload, truncated := readErrorBody(resp)
	if resp.ContentLength < 0 && len(payload) == 1024 && (resp.Header.Get("Content-Encoding") == "" || strings.EqualFold(resp.Header.Get("Content-Encoding"), "identity")) {
		// Gorilla retains at most 1024 handshake bytes. With chunked
		// responses at that boundary completeness cannot be established.
		truncated = true
	}
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	observeErrorBody(sink, payload, truncated)
	var data map[string]any
	_ = jsonUnmarshalBytes(payload, &data)
	code, _ := extractErrorFields(data)
	if code == "" {
		code = fmt.Sprintf("http_%d", resp.StatusCode)
	}
	return &FailureError{
		Status:    resp.StatusCode,
		Code:      code,
		RequestID: resp.Header.Get("x-request-id"),
		Message:   formatUpstreamError(resp.StatusCode, payload),
		Payload:   payload,
	}
}

func (c *Client) validWSContinuation(req *Request, r *session.Record) bool {
	return c.cfg.Pool && c.validRecord(r, req.ClientKeyID, req.PreviousResponseID) && r.AccountID == req.AccountID && r.IdentityHash == c.identityHash(req) && r.CodexBridge == req.CodexBridge && c.pool.matches(r)
}

func (x *wsExchange) readyToCommit(ctx context.Context, event *Event, failure error, responseID string) bool {
	return failure == nil && (successfulTerminal(event) || interruptedTerminal(event)) && responseID != "" && ctx.Err() == nil && !x.connection.isDead()
}

func wsHandshakeHeaders(source http.Header) http.Header {
	header := http.Header{}
	for name, values := range source {
		if strings.EqualFold(name, "Proxy-Authorization") {
			continue // Proxy credentials belong only to CONNECT, never the target.
		}
		for _, v := range values {
			header.Add(name, v)
		}
	}

	return header
}

func closeWSHandshake(conn *websocket.Conn, resp *http.Response) {
	if conn != nil {
		_ = conn.Close()
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
}

func (c *Client) releaseWSConnection(ctx context.Context, key string, conn *wsConn, keep bool) {
	if ctx.Err() != nil {
		keep = false
	}
	if keep && key != "" && !conn.isDead() {
		c.pool.put(key, conn)
		return
	}
	c.pool.discard(key, conn)
}

func writeWSRequest(conn *wsConn, frame []byte) error {
	if err := conn.socket.SetWriteDeadline(time.Now().Add(StreamWriteTimeout)); err != nil {
		conn.markDead()
		return &transportError{err: fmt.Errorf("upstream: ws write deadline: %w", err), transport: "websocket", requestNotSent: true}
	}
	// A failed write can still have sent some or all of response.create. From this
	// point on, requestNotSent must remain false, regardless of observed events.
	if err := conn.socket.WriteMessage(websocket.TextMessage, frame); err != nil {
		conn.markDead()
		return &transportError{err: fmt.Errorf("upstream: ws send: %w", err), transport: "websocket"}
	}

	return nil
}

func (c *Client) observeWSRequest(req *Request, frame []byte) {
	if req.Sink != nil {
		req.Sink.OnRequestHeaders(c.cfg.WSURL, req.WSHeaders)
		req.Sink.OnRequestBody(frame, false)
		req.Sink.OnFrame("out", "ws_frame", "response.create", frame, nil)
	}
}
