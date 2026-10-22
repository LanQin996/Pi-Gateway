package accounts

import (
	"context"
	"errors"
	"time"

	"pi-gateway/internal/store"
)

// RefreshOfficialUsage backfills 84 UTC days initially, then corrects the last
// seven days hourly, following codex2api. Manual refreshes are debounced for 30s.
func (m *Manager) RefreshOfficialUsage(ctx context.Context, account *store.Account) error {
	m.mu.Lock()
	if m.officialUsageMu == nil {
		m.officialUsageMu = newCredentialRefreshLock()
	}
	lock := m.officialUsageMu
	m.mu.Unlock()
	if err := lock.LockContext(ctx); err != nil {
		return err
	}
	defer lock.Unlock()
	if account == nil || !account.CodexLinked() {
		return ErrCodexNotLinked
	}
	client := m.QuotaClient()
	if client == nil {
		return errors.New("official usage fetching is not configured")
	}
	state, err := m.store.OfficialUsageSyncFor(ctx, store.OfficialUsageKey(account))
	if err != nil {
		return err
	}
	if state.AttemptedAt > time.Now().Add(-30*time.Second).UnixMilli() {
		return nil
	}
	// Read the current credential before refreshing, so parallel callers cannot
	// reuse a rotated refresh token from an old account list.
	current, err := m.store.GetAccount(ctx, account.ID)
	if err != nil {
		return err
	}
	if current == nil || !current.CodexLinked() || current.CodexAccountID != account.CodexAccountID {
		return store.ErrCodexCredentialChanged
	}
	token, _, _, ok, err := m.ensureFreshCodexToken(ctx, current)
	if err == nil && !ok {
		err = ErrCodexNotLinked
	}
	start, end := officialUsageDateRange(state.SyncedAt == 0)
	var records []store.OfficialUsageDay
	if err == nil {
		records, err = client.FetchDailyUsage(ctx, current, token, start, end)
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	if saveErr := m.store.SaveOfficialUsage(ctx, current, start, end, records, errText); saveErr != nil {
		return saveErr
	}
	return err
}

func (m *Manager) StartOfficialUsageRefresher(ctx context.Context) {
	go func() {
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			m.refreshDueOfficialUsage(ctx)
			timer.Reset(time.Hour)
		}
	}()
}

func (m *Manager) refreshDueOfficialUsage(ctx context.Context) {
	accounts, err := m.store.ListEnabledAccounts(ctx)
	if err != nil {
		return
	}

	seen := map[string]bool{}
	for _, a := range accounts {
		if ctx.Err() != nil {
			return
		}
		key := store.OfficialUsageKey(a)
		if !a.CodexLinked() || seen[key] {
			continue
		}
		seen[key] = true
		state, err := m.store.OfficialUsageSyncFor(ctx, key)
		if err != nil || state.AttemptedAt > time.Now().Add(-time.Hour).UnixMilli() {
			continue
		}
		fetchCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		if err := m.RefreshOfficialUsage(fetchCtx, a); err != nil {
			m.logger.Debug("official usage sync failed", "account", a.Name, "error", err)
		}
		cancel()
	}
}

func officialUsageDateRange(initial bool) (start, end string) {
	days := 7
	if initial {
		days = 84
	}
	now := time.Now().UTC()
	end = now.Format(time.DateOnly)
	start = now.AddDate(0, 0, -(days - 1)).Format(time.DateOnly)
	return start, end
}
