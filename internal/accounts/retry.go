package accounts

import (
	"context"
	"time"

	"pi-gateway/internal/store"
)

// retryTicket stays at the queue head through post-reservation validation, so
// another replay cannot overtake it while the database is being rechecked.
type retryTicket struct {
	wake chan struct{}
}

// AcquireRetry reacquires only the original account after a rate limit. The
// caller must release its previous reservation first and supply the entire
// retry waiting budget via ctx; this method never starts a new manager timeout.
// Cooling and request spacing are waitable, unlike authorization/health denial.
func (m *Manager) AcquireRetry(ctx context.Context, key *store.APIKey, accountID int64, model string) (*store.Account, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if key == nil || key.ID <= 0 || accountID <= 0 {
		return nil, nil, ErrNoAccounts
	}
	ticket := m.enqueueRetry(accountID)
	defer m.removeRetry(accountID, ticket)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		account, err := m.retryAccount(ctx, key, accountID, model)
		if err != nil {
			return nil, nil, err
		}
		if m.tryReserveRetry(ctx, account, ticket) {
			release := m.pinnedRelease(accountID)
			fresh, err := m.retryAccount(ctx, key, accountID, model)
			if err != nil {
				release()
				return nil, nil, err
			}
			if err := ctx.Err(); err != nil {
				release()
				return nil, nil, err
			}
			if m.retryReady(fresh) {
				if err := ctx.Err(); err != nil {
					release()
					return nil, nil, err
				}
				return fresh, release, nil
			}
			// A newly committed cooldown/interval is temporary. Keep the same
			// FIFO position, but release capacity before waiting again.
			release()
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-ticket.wake:
		case <-ticker.C:
		}
	}
}

// retryAccount uses precisely the ordinary admission policy except for its
// temporary scheduling filters, and narrows model evaluation to accountID.
func (m *Manager) retryAccount(ctx context.Context, key *store.APIKey, accountID int64, model string) (*store.Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current, err := m.currentKey(ctx, key)
	if err == nil {
		var candidates []*store.Account
		candidates, err = m.eligibleFor(ctx, current, model, accountID, true)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err == nil {
			if len(candidates) == 0 {
				return nil, ErrNoAccounts
			}
			return candidates[0], nil
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	return nil, err
}

func accountReadyAt(a *store.Account, intervalMS int64) int64 {
	ready := a.CooldownUntil
	if intervalMS > 0 && a.LastStartedAt > 0 {
		ready = max(ready, a.LastStartedAt+intervalMS)
	}
	return ready
}

func (m *Manager) retryReady(a *store.Account) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return accountReadyAt(a, m.requestInterval.Milliseconds()) <= m.nowTime().UnixMilli()
}

func (m *Manager) enqueueRetry(accountID int64) *retryTicket {
	ticket := &retryTicket{wake: make(chan struct{}, 1)}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.retryQueues == nil {
		m.retryQueues = make(map[int64][]*retryTicket)
	}
	m.retryQueues[accountID] = append(m.retryQueues[accountID], ticket)
	return ticket
}

func (m *Manager) removeRetry(accountID int64, ticket *retryTicket) {
	m.mu.Lock()
	defer m.mu.Unlock()
	queue := m.retryQueues[accountID]
	for i, candidate := range queue {
		if candidate != ticket {
			continue
		}
		copy(queue[i:], queue[i+1:])
		queue[len(queue)-1] = nil
		queue = queue[:len(queue)-1]
		if len(queue) == 0 {
			delete(m.retryQueues, accountID)
		} else {
			m.retryQueues[accountID] = queue
			m.wakeRetryLocked(accountID)
		}
		return
	}
}

// wakeRetryLocked only signals the head, which will wake the next ticket upon
// admission or removal. No queue helper performs database IO under m.mu.
func (m *Manager) wakeRetryLocked(accountID int64) {
	if queue := m.retryQueues[accountID]; len(queue) > 0 {
		select {
		case queue[0].wake <- struct{}{}:
		default:
		}
	}
}

func (m *Manager) tryReserveRetry(ctx context.Context, a *store.Account, ticket *retryTicket) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	queue := m.retryQueues[a.ID]
	if ctx.Err() != nil || len(queue) == 0 || queue[0] != ticket || accountReadyAt(a, m.requestInterval.Milliseconds()) > m.nowTime().UnixMilli() {
		return false
	}
	return m.reserveLocked(a)
}
