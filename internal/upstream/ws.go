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
		if !c.cfg.Pool || !c.validRecord(r, req.ClientKeyID, req.PreviousResponseID) || r.AccountID != req.AccountID || r.IdentityHash != c.identityHash(req) || r.CodexBridge != req.CodexBridge || !c.pool.matches(r) {
			return nil, continuationMismatch()
		}
		key = r.SessionID // trusted opaque original pool scope, not this downstream's ID
	}
	conn, acquireErr := c.pool.acquire(ctx, key)
	if acquireErr != nil {
		return nil, acquireErr
	}
	result := &StreamResult{Transport: "websocket"}
	emitted := false

	// A waiter may acquire the lane after another request advanced its parent.
	// Never dial a replacement for an explicit connection-local continuation.
	if explicit && (conn == nil || conn.id != req.Continuation.ConnectionID || !c.pool.matches(req.Continuation)) {
		if conn != nil {
			c.pool.put(key, conn)
		} else {
			c.pool.release(key)
		}
		return result, continuationMismatch()
	}
	reused := conn != nil
	if conn == nil {
		fresh, resp, err := c.dialWS(ctx, req)
		if resp != nil {
			result.Status = resp.StatusCode
		}
		if err != nil {
			c.pool.release(key)
			return result, &transportError{err: err, transport: "websocket", requestNotSent: true}
		}
		conn = &wsConn{socket: fresh, id: newConnectionID(), createdAt: time.Now(), busy: true}
		if !c.pool.register(key, conn) {
			c.pool.discard(key, conn)
			return nil, fmt.Errorf("upstream: pool closed")
		}
	}

	// On a reused socket with cached conversation state, send only the new input
	// items (previous_response_id + delta input), exactly as Pi does for
	// websocket-cached. When the cached state cannot be continued, Pi clears it and
	// sends the full body over the same socket, so we do the same.
	conn.socket.SetReadLimit(maxWSFrameBytes)

	if usePool && !explicit && reused && conn.continuation != nil {
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
		if ctx.Err() != nil {
			keep = false
		}
		if keep && key != "" && !conn.isDead() {
			c.pool.put(key, conn)
			return
		}
		c.pool.discard(key, conn)
	}()

	// Record the WebSocket handshake request side once per exchange.
	if req.Sink != nil {
		req.Sink.OnRequestHeaders(c.cfg.WSURL, req.WSHeaders)
		req.Sink.OnRequestBody(frame, false)
		req.Sink.OnFrame("out", "ws_frame", "response.create", frame, nil)
	}

	if err := conn.socket.SetWriteDeadline(time.Now().Add(StreamWriteTimeout)); err != nil {
		conn.markDead()
		return result, &transportError{err: fmt.Errorf("upstream: ws write deadline: %w", err), transport: "websocket", requestNotSent: true}
	}
	// A failed write can still have sent some or all of response.create. From this
	// point on, requestNotSent must remain false, regardless of observed events.
	if err := conn.socket.WriteMessage(websocket.TextMessage, frame); err != nil {
		conn.markDead()
		return result, &transportError{err: fmt.Errorf("upstream: ws send: %w", err), transport: "websocket"}
	}

	idle := c.cfg.IdleTimeout
	for {
		if err := ctx.Err(); err != nil {
			keep = false
			return result, err
		}
		_ = conn.socket.SetReadDeadline(time.Now().Add(idle))

		messageType, payload, err := conn.socket.ReadMessage()
		if control != nil {
			control.flush(req.Sink)
		}
		if err != nil {
			conn.markDead()
			if errors.Is(err, websocket.ErrReadLimit) {
				return result, &transportError{
					err:           &WSFrameTooLargeError{Limit: maxWSFrameBytes},
					eventsEmitted: emitted,
					transport:     "websocket",
				}
			}
			if ctx.Err() != nil {
				keep = false
				return result, ctx.Err()
			}
			if time.Since(conn.createdAt) >= c.pool.maxAge {
				return result, &transportError{err: ErrWSMaxAge, eventsEmitted: emitted, transport: "websocket"}
			}
			var networkError net.Error
			if errors.As(err, &networkError) && networkError.Timeout() {
				return result, &transportError{
					err:           &IdleTimeoutError{Timeout: idle},
					eventsEmitted: emitted,
					transport:     "websocket",
				}
			}
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				if req.Sink != nil {
					req.Sink.OnFrame("in", "ws_close", fmt.Sprintf("%d", closeErr.Code), []byte(closeErr.Text), nil)
				}
				return result, &transportError{
					err:           fmt.Errorf("upstream: websocket stream truncated before a terminal event: %w", closeErr),
					eventsEmitted: emitted,
					transport:     "websocket",
				}
			}
			return result, &transportError{
				err:           fmt.Errorf("upstream: websocket stream truncated before a terminal event: %w", err),
				eventsEmitted: emitted,
				transport:     "websocket",
			}
		}

		var decoded map[string]any
		if err := json.Unmarshal(payload, &decoded); err != nil {
			conn.markDead()
			return result, &transportError{
				err:           fmt.Errorf("upstream: invalid websocket JSON: %w", err),
				eventsEmitted: emitted,
				transport:     "websocket",
			}
		}
		eventType, _ := decoded["type"].(string)
		event := &Event{Type: eventType, Raw: payload, Data: decoded, At: time.Now()}
		failure := FailureFromEvent(event)
		deliveryEvent := failureEventForDelivery(event, failure)

		responseID := ExtractResponseID(decoded)
		if control != nil {
			if IsTerminal(eventType) || failure != nil {
				control.clear()
				control.flush(req.Sink)
			} else if responseID != "" {
				control.bind(conn, responseID)
			}
		}
		if req.Sink != nil {
			kind := "ws_frame"
			if messageType == websocket.BinaryMessage {
				kind = "ws_binary"
			}
			req.Sink.OnFrame("in", kind, eventType, payload, decoded)
			if responseID != "" {
				req.Sink.OnResponseID(responseID)
			}
			if IsTerminal(eventType) {
				req.Sink.OnUsage(ExtractUsage(decoded))
			}
		}
		ready := failure == nil && (successfulTerminal(event) || interruptedTerminal(event)) && responseID != "" && ctx.Err() == nil && !conn.isDead()
		var record *session.Record
		if ready {
			record = c.completedRecord(req, conn, key, responseID)
			// Bound the shared write before terminal delivery. A late shared record
			// alone can never pass the live socket's post-callback ready check.
			c.commitShared(ctx, record)
		}
		if err := ctx.Err(); err != nil {
			keep = false
			return result, err
		}
		if conn.isDead() {
			keep = false
			return result, &transportError{err: ErrWSMaxAge, eventsEmitted: emitted, transport: "websocket"}
		}
		if err := onEvent(deliveryEvent); err != nil {
			// A callback error may follow a partial downstream write, so it always
			// prevents transparent retry.
			emitted = true
			keep = false
			return result, &transportError{err: err, eventsEmitted: emitted, transport: "websocket"}
		}
		if failure != nil {
			// This failure reached the callback, so retry is no longer safe.
			emitted = true
			// A logical failure poisons the conversation state on this socket.
			conn.continuation = nil
			conn.markDead()
			keep = false
			return result, &transportError{err: failure, eventsEmitted: emitted, transport: "websocket"}
		}
		emitted = true
		if IsTerminal(eventType) {
			if ready && ctx.Err() == nil && !conn.isDead() {
				// Only a successfully delivered terminal may advance local readiness.
				conn.lastResponseID = responseID
				var baseline *wsContinuation
				var size int64
				// Explicit input is already a delta, not a full-history baseline.
				if usePool && !explicit && int64(len(req.Body)) <= c.cfg.MaxBaselineBytes {
					if exceptInput, input, ok := bodyExceptInput(req.Body); ok {
						baseline = &wsContinuation{bodyExceptInput: exceptInput, lastInput: input,
							lastResponseItems: responseItemsFromTerminal(decoded), lastResponseID: responseID, codexBridge: req.CodexBridge}
						size = baseline.size()
					}
				}
				c.pool.setBaseline(key, conn, baseline, size, record)
			} else {
				// incomplete/done-with-failed-status must never retain a ready parent.
				keep = false
				conn.markDead()
			}
			return result, nil
		}
	}
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

	header := http.Header{}
	for name, values := range req.WSHeaders {
		if strings.EqualFold(name, "Proxy-Authorization") {
			continue // Proxy credentials belong only to CONNECT, never the target.
		}
		for _, v := range values {
			header.Add(name, v)
		}
	}

	conn, resp, err := dialer.DialContext(ctx, c.cfg.WSURL, header)
	if stop != nil && !stop() {
		<-closed // A canceled handshake must not race with pool registration.
	}
	if ctx.Err() != nil {
		if conn != nil {
			_ = conn.Close()
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, nil, fmt.Errorf("upstream: websocket handshake failed: %w", ctx.Err())
	}
	if resp != nil && req.Sink != nil {
		// Preserve observation order: headers first, then a rejected handshake
		// body, just as for the HTTP/SSE path.
		req.Sink.OnResponseHeaders(resp.StatusCode, resp.Header)
	}
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			payload, truncated := readErrorBody(resp)
			if resp.ContentLength < 0 && len(payload) == 1024 && (resp.Header.Get("Content-Encoding") == "" || strings.EqualFold(resp.Header.Get("Content-Encoding"), "identity")) {
				// Gorilla retains at most 1024 handshake bytes. With chunked
				// responses at that boundary completeness cannot be established.
				truncated = true
			}
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
			observeErrorBody(req.Sink, payload, truncated)
			var data map[string]any
			_ = jsonUnmarshalBytes(payload, &data)
			code, _ := extractErrorFields(data)
			if code == "" {
				code = fmt.Sprintf("http_%d", resp.StatusCode)
			}
			return nil, resp, &FailureError{
				Status:    resp.StatusCode,
				Code:      code,
				RequestID: resp.Header.Get("x-request-id"),
				Message:   formatUpstreamError(resp.StatusCode, payload),
				Payload:   payload,
			}
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
