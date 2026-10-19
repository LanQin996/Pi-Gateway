package upstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"pi-gateway/internal/session"

	"github.com/gorilla/websocket"
)

const SessionWebSocketMaxAge = 55 * time.Minute
const maxPoolWaitersPerKey = 64

var ErrPoolBusy = errors.New("upstream: websocket pool capacity or wait queue is full")
var ErrWSMaxAge = errors.New("upstream: websocket maximum age reached")

type wsConn struct {
	socket         *websocket.Conn
	id             string
	createdAt      time.Time
	busy           bool // pool.mu
	dead           bool // stateMu
	stateMu        sync.Mutex
	idleTimer      *time.Timer     // pool.mu
	ageTimer       *time.Timer     // pool.mu
	record         *session.Record // pool.mu; only the latest successfully delivered response
	baselineBytes  int64           // pool.mu
	lastResponseID string          // exclusive pool lease
	continuation   *wsContinuation // exclusive pool lease
}

func (c *wsConn) markDead()    { c.stateMu.Lock(); c.dead = true; c.stateMu.Unlock() }
func (c *wsConn) isDead() bool { c.stateMu.Lock(); defer c.stateMu.Unlock(); return c.dead }

type poolSlot struct {
	busy     bool
	refs     int
	reserved bool
	waiters  []chan struct{}
}
type wsPool struct {
	mu                    sync.Mutex
	entries               map[string]*wsConn
	slots                 map[string]*poolSlot
	done                  chan struct{}
	idleTimeout           time.Duration
	maxAge                time.Duration
	maxConnections        int
	reservations          int
	maxBaselineBytes      int64
	maxTotalBaselineBytes int64
	totalBaselineBytes    int64
	closed                bool
	logger                *slog.Logger
}

func newWSPool(idleTimeout time.Duration, logger *slog.Logger) *wsPool {
	if idleTimeout <= 0 {
		idleTimeout = 5 * time.Minute
	}
	return &wsPool{entries: map[string]*wsConn{}, slots: map[string]*poolSlot{}, done: make(chan struct{}), idleTimeout: idleTimeout, maxAge: SessionWebSocketMaxAge, maxConnections: 256, maxBaselineBytes: 2 << 20, maxTotalBaselineBytes: 64 << 20, logger: logger}
}
func poolKey(sessionID string, accountID int64) string {
	return fmt.Sprintf("%s|%d", sessionID, accountID)
}

// acquire reserves a lane and pool capacity before dialing. Full global capacity
// only evicts idle entries; it must never close another request's busy socket.
func (p *wsPool) acquire(ctx context.Context, key string) (*wsConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key == "" {
		return nil, nil
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, fmt.Errorf("upstream: pool closed")
	}
	s := p.slots[key]
	if s == nil {
		s = &poolSlot{}
		p.slots[key] = s
	}
	s.refs++
	if !s.busy {
		s.busy = true
		p.mu.Unlock()
		return p.acquired(ctx, key)
	}
	if len(s.waiters) >= maxPoolWaitersPerKey {
		s.refs--
		p.mu.Unlock()
		return nil, ErrPoolBusy
	}
	wait := make(chan struct{})
	s.waiters = append(s.waiters, wait)
	p.mu.Unlock()
	timer := time.NewTimer(p.idleTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		if !p.removeWaiter(key, s, wait) {
			p.release(key)
		}
		return nil, ctx.Err()
	case <-p.done:
		if !p.removeWaiter(key, s, wait) {
			p.release(key)
		}
		return nil, fmt.Errorf("upstream: pool closed")
	case <-timer.C:
		if !p.removeWaiter(key, s, wait) {
			p.release(key)
		}
		return nil, ErrPoolBusy
	case <-wait:
		return p.acquired(ctx, key)
	}
}
func (p *wsPool) acquired(ctx context.Context, key string) (*wsConn, error) {
	p.mu.Lock()
	if p.closed || ctx.Err() != nil {
		p.mu.Unlock()
		p.release(key)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("upstream: pool closed")
	}
	entry := p.entries[key]
	if entry != nil {
		if entry.idleTimer != nil {
			entry.idleTimer.Stop()
			entry.idleTimer = nil
		}
		if entry.isDead() || time.Since(entry.createdAt) >= p.maxAge {
			p.removeLocked(key, entry)
			entry = nil
		} else {
			entry.busy = true
		}
	}
	if entry == nil {
		if len(p.entries)+p.reservations >= p.maxConnections {
			for candidate, conn := range p.entries {
				slot := p.slots[candidate]
				if !conn.busy && (slot == nil || !slot.busy) {
					p.removeLocked(candidate, conn)
					break
				}
			}
		}
		if len(p.entries)+p.reservations >= p.maxConnections {
			p.mu.Unlock()
			p.release(key)
			return nil, ErrPoolBusy
		}
		p.slots[key].reserved = true
		p.reservations++
	}
	p.mu.Unlock()
	return entry, nil
}
func (p *wsPool) removeWaiter(key string, s *poolSlot, wait chan struct{}) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, candidate := range s.waiters {
		if candidate == wait {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			s.refs--
			if s.refs == 0 {
				delete(p.slots, key)
			}
			return true
		}
	}
	return false
}
func (p *wsPool) release(key string) {
	if key == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.slots[key]
	if s == nil {
		return
	}
	if s.reserved {
		s.reserved = false
		p.reservations--
	}
	if len(s.waiters) != 0 {
		wait := s.waiters[0]
		s.waiters = s.waiters[1:]
		s.refs--
		close(wait)
		return
	}
	s.busy = false
	s.refs--
	if s.refs == 0 {
		delete(p.slots, key)
	}
}
func (p *wsPool) register(key string, c *wsConn) bool {
	if key == "" {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	if s := p.slots[key]; s != nil && s.reserved {
		s.reserved = false
		p.reservations--
	}
	p.entries[key] = c
	// Lifetime applies while idle AND while generating, independent of event gaps.
	c.ageTimer = time.AfterFunc(time.Until(c.createdAt.Add(p.maxAge)), func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.entries[key] == c {
			p.removeLocked(key, c)
		}
	})
	return true
}
func (p *wsPool) put(key string, c *wsConn) {
	defer p.release(key)
	p.mu.Lock()
	defer p.mu.Unlock()
	if key == "" || p.closed || c.isDead() || time.Since(c.createdAt) >= p.maxAge {
		p.removeLocked(key, c)
		return
	}
	if old := p.entries[key]; old != nil && old != c {
		c.markDead()
		_ = c.socket.Close()
		return
	}
	c.busy = false
	p.entries[key] = c
	var timer *time.Timer
	timer = time.AfterFunc(p.idleTimeout, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.entries[key] == c && !c.busy && c.idleTimer == timer {
			p.removeLocked(key, c)
		}
	})
	c.idleTimer = timer
}

// Caller holds pool.mu. Do not mutate lease-owned continuation while a request
// is active: closing the socket interrupts it, and its defer releases the lease.
func (p *wsPool) removeLocked(key string, c *wsConn) {
	if c == nil {
		return
	}
	c.markDead()
	if c.idleTimer != nil {
		c.idleTimer.Stop()
	}
	if c.ageTimer != nil {
		c.ageTimer.Stop()
	}
	if c.socket != nil {
		_ = c.socket.Close()
	}
	if p.entries[key] == c {
		delete(p.entries, key)
		p.totalBaselineBytes -= c.baselineBytes
		c.baselineBytes = 0
		c.record = nil
	}
}
func (p *wsPool) discard(key string, c *wsConn) {
	p.mu.Lock()
	p.removeLocked(key, c)
	p.mu.Unlock()
	p.release(key)
}
func (p *wsPool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	close(p.done)
	for key, c := range p.entries {
		p.removeLocked(key, c)
	}
}
func (p *wsPool) Size() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.entries) }

func sameRecord(a, b *session.Record) bool {
	return a != nil && b != nil && a.ResponseID == b.ResponseID && a.KeyID == b.KeyID && a.AccountID == b.AccountID && a.ConnectionID == b.ConnectionID && a.InstanceID == b.InstanceID && a.IdentityHash == b.IdentityHash && a.SessionID == b.SessionID && a.Scope == b.Scope && a.Version == b.Version && a.CodexBridge.Valid() && b.CodexBridge.Valid() && a.CodexBridge == b.CodexBridge
}
func (p *wsPool) matches(r *session.Record) bool {
	if r == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.entries[r.SessionID]
	return c != nil && !c.isDead() && time.Since(c.createdAt) < p.maxAge && sameRecord(c.record, r)
}
func (p *wsPool) resolve(keyID int64, responseID string) *session.Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.entries {
		r := c.record
		if r != nil && r.KeyID == keyID && r.ResponseID == responseID && r.ExpiresAt.After(time.Now()) && !c.isDead() && time.Since(c.createdAt) < p.maxAge {
			v := *r
			return &v
		}
	}
	return nil
}
func (p *wsPool) setBaseline(key string, c *wsConn, baseline *wsContinuation, size int64, record *session.Record) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries[key] != c || c.isDead() {
		return
	}
	p.totalBaselineBytes -= c.baselineBytes
	c.baselineBytes = 0
	c.continuation = nil
	if baseline != nil && size <= p.maxBaselineBytes && size <= p.maxTotalBaselineBytes-p.totalBaselineBytes {
		c.continuation = baseline
		c.baselineBytes = size
		p.totalBaselineBytes += size
	}
	c.record = record
}
