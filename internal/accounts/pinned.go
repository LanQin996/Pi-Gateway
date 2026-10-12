package accounts

import (
	"context"
	"sync"
	"time"

	"pi-gateway/internal/store"
)

// AcquirePinned reserves the exact account identified by a continuation. Unlike
// the administrator's AcquireSpecific, it rechecks the client's current key and
// group/model scope on every wait iteration and never falls back to another account.
func (m *Manager) AcquirePinned(ctx context.Context, key *store.APIKey, accountID int64, model string) (*store.Account, func(), error) {
	if key == nil || key.ID <= 0 || accountID <= 0 {
		return nil, nil, ErrNoAccounts
	}
	m.mu.Lock()
	wait := m.waitTimeout
	m.mu.Unlock()
	if wait <= 0 {
		wait = 30 * time.Second
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		current, err := m.currentKey(ctx, key)
		if err != nil {
			return nil, nil, err
		}
		candidates, err := m.eligible(ctx, current, model)
		if err != nil {
			return nil, nil, err
		}
		var pinned *store.Account
		for _, account := range candidates {
			if account.ID == accountID {
				pinned = account
				break
			}
		}
		if pinned == nil {
			return nil, nil, ErrNoAccounts
		}
		if m.tryReserve(pinned) {
			release := m.pinnedRelease(pinned.ID)
			if err := ctx.Err(); err != nil {
				release()
				return nil, nil, err
			}
			fresh, err := m.revalidateReservation(ctx, key, pinned.ID, model)
			if err != nil {
				release()
				return nil, nil, err
			}
			return fresh, release, nil
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-deadline.C:
			return nil, nil, ErrAccountConcurrency
		case <-ticker.C:
		}
	}
}

func (m *Manager) pinnedRelease(accountID int64) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			m.inflight[accountID]--
			m.wakeRetryLocked(accountID)
			m.mu.Unlock()
		})
	}
}
