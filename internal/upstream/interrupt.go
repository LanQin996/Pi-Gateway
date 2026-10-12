package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ResponseControl belongs to one downstream connection. Only the active
// response's original upstream socket may receive an interrupt.
type ResponseControl struct {
	mu      sync.Mutex
	id      string
	socket  *wsConn
	sent    bool
	pending []byte
}

type responseControlKey struct{}

func WithResponseControl(ctx context.Context, control *ResponseControl) context.Context {
	return context.WithValue(ctx, responseControlKey{}, control)
}

func (c *ResponseControl) Interrupt(ctx context.Context, id, mode string) error {
	if mode != "discard_partial_items" || id == "" {
		return fmt.Errorf("response.interrupt requires response_id and mode discard_partial_items")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.socket == nil || c.id != id {
		return fmt.Errorf("response.interrupt does not match an active upstream WebSocket response")
	}
	if c.sent {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"type": "response.interrupt", "response_id": id, "mode": mode})
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := c.socket.socket.SetWriteDeadline(deadline); err != nil {
		c.socket.markDead()
		_ = c.socket.socket.Close()
		return err
	}
	if err := c.socket.socket.WriteMessage(websocket.TextMessage, payload); err != nil {
		c.socket.markDead()
		_ = c.socket.socket.Close() // A partial control write cannot return to the pool.
		return err
	}
	c.sent = true
	c.pending = payload
	return nil
}

func (c *ResponseControl) bind(socket *wsConn, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.socket != socket || c.id != id {
		c.sent = false
	}
	c.socket, c.id = socket, id
}

// Wire observations run on the stream goroutine, like all other sink calls.
func (c *ResponseControl) flush(sink Sink) {
	c.mu.Lock()
	payload := c.pending
	c.pending = nil
	c.mu.Unlock()
	if sink != nil && payload != nil {
		sink.OnFrame("out", "ws_frame", "response.interrupt", payload, nil)
	}
}

func (c *ResponseControl) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.socket, c.id, c.sent = nil, "", false
}

func interruptedTerminal(event *Event) bool {
	if event.Type != EventResponseIncomplete {
		return false
	}
	response, _ := event.Data["response"].(map[string]any)
	details, _ := response["incomplete_details"].(map[string]any)
	return response["status"] == "incomplete" && details["reason"] == "interrupted"
}
