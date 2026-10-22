// Package quotaaccounts wires quota fetching into the account manager: it fetches
// usage + reset credits for an account, persists the snapshot, and consumes reset
// credits on demand.
package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"pi-gateway/internal/quota"
	"pi-gateway/internal/store"
)

// ErrCodexNotLinked reports that a quota operation was attempted without the
// optional Codex credential that quota state actually lives behind.
var ErrCodexNotLinked = errors.New("accounts: no Codex credential is linked to this account")

// SetQuotaClient installs the quota client (called during startup).
func (m *Manager) SetQuotaClient(client *quota.Client) {
	m.mu.Lock()
	m.quotaClient = client
	m.mu.Unlock()
}

// QuotaClient returns the installed quota client, if any.
func (m *Manager) QuotaClient() *quota.Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.quotaClient
}

// QuotaSnapshot returns the persisted snapshot for an account.
func (m *Manager) QuotaSnapshot(account *store.Account) *quota.Snapshot {
	if account == nil || account.QuotaJSON == "" {
		return nil
	}
	var snapshot quota.Snapshot
	if err := json.Unmarshal([]byte(account.QuotaJSON), &snapshot); err != nil {
		return nil
	}
	return &snapshot
}

// RefreshQuota fetches the usage report and the reset-credit inventory for one
// account, persists them, and returns the resulting snapshot.
//
// A failure in either call is recorded rather than discarded so the UI can show
// the last known good state together with the reason it is stale.
func (m *Manager) RefreshQuota(ctx context.Context, account *store.Account) (*quota.Snapshot, error) {
	client := m.QuotaClient()
	if client == nil {
		return nil, errors.New("quota fetching is not configured")
	}

	// Quota state lives on the Codex backend, so it is read with the optional
	// Codex credential rather than the ChatGPT credential used for model traffic.
	accessToken, _, refreshToken, ok, err := m.ensureFreshCodexToken(ctx, account)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrCodexNotLinked
	}

	snapshot := m.QuotaSnapshot(account)
	if snapshot == nil {
		snapshot = &quota.Snapshot{}
	}

	var (
		firstErr   error
		planType   string
		reportErr  string
		creditsErr string
	)

	planType, reportErr, firstErr = refreshQuotaReport(ctx, client, account, accessToken, snapshot)

	credits, err := client.FetchResetCredits(ctx, account, accessToken)
	if err != nil {
		creditsErr = err.Error()
		if firstErr == nil {
			firstErr = err
		}
	} else {
		snapshot.ResetCredits = credits
	}
	if snapshot.ResetCredits != nil && creditsErr != "" {
		snapshot.ResetCredits.Error = creditsErr
	}

	raw, marshalErr := json.Marshal(snapshot)
	if marshalErr != nil {
		return nil, fmt.Errorf("accounts: encode quota snapshot: %w", marshalErr)
	}

	combinedErr := reportErr
	if combinedErr == "" {
		combinedErr = creditsErr
	} else if creditsErr != "" {
		combinedErr = combinedErr + "; " + creditsErr
	}
	if err := m.store.SaveAccountQuotaIfCurrent(ctx, account.ID, refreshToken, string(raw), planType, combinedErr); err != nil {
		return nil, err
	}

	account.QuotaJSON = string(raw)
	account.QuotaError = combinedErr
	account.QuotaUpdatedAt = store.NowMS()

	if firstErr != nil {
		return snapshot, firstErr
	}
	return snapshot, nil
}

// ConsumeResetCredit spends one rate-limit reset credit and refreshes the quota so
// the UI immediately reflects the new window state.
func (m *Manager) ConsumeResetCredit(ctx context.Context, account *store.Account, creditID string) (*quota.ConsumeResult, *quota.Snapshot, error) {
	client := m.QuotaClient()
	if client == nil {
		return nil, nil, errors.New("quota fetching is not configured")
	}

	accessToken, _, _, ok, err := m.ensureFreshCodexToken(ctx, account)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, ErrCodexNotLinked
	}

	result, err := client.ConsumeResetCredit(ctx, account, accessToken, creditID)
	if err != nil {
		// Report the business code when there is one so the UI can explain it.
		var consumeErr *quota.ConsumeError
		if errors.As(err, &consumeErr) {
			snapshot, refreshErr := m.RefreshQuota(ctx, account)
			if refreshErr != nil {
				m.logger.Debug("quota refresh after a refused reset failed", "error", refreshErr)
			}
			return result, snapshot, err
		}
		return nil, nil, err
	}

	m.logger.Info("rate-limit reset consumed", "account", account.Name, "code", result.Code)

	snapshot, refreshErr := m.RefreshQuota(ctx, account)
	if refreshErr != nil {
		m.logger.Debug("quota refresh after a reset failed", "error", refreshErr)
	}
	return result, snapshot, nil
}

// StartQuotaRefresher periodically refreshes quota data for enabled accounts so the
// dashboard is not entirely dependent on manual refreshes.
func (m *Manager) StartQuotaRefresher(ctx context.Context, interval, concurrency int, jitter time.Duration) {
	if interval <= 0 {
		return
	}
	period := time.Duration(interval) * time.Second
	if period < 30*time.Second {
		period = 30 * time.Second
	}
	if concurrency <= 0 {
		concurrency = 2
	}

	go func() {
		// Stagger the first sweep so startup is not blocked by network calls.
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}

			m.refreshAllQuotas(ctx, concurrency, jitter)

			timer.Reset(period)
		}
	}()
}

func (m *Manager) refreshAllQuotas(ctx context.Context, concurrency int, jitter time.Duration) {
	if m.QuotaClient() == nil {
		return
	}
	accounts, err := m.store.ListEnabledAccounts(ctx)
	if err != nil {
		m.logger.Error("listing accounts for a quota refresh failed", "error", err)
		return
	}
	if len(accounts) == 0 {
		return
	}

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, account := range accounts {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(acc *store.Account) {
			defer wg.Done()
			defer func() { <-sem }()
			// Spread the load so several accounts do not hit the backend at once.
			if jitter > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Duration(acc.ID%int64(jitter/time.Millisecond+1)) * time.Millisecond):
				}
			}
			tctx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			if _, err := m.RefreshQuota(tctx, acc); err != nil {
				m.logger.Debug("scheduled quota refresh failed", "account", acc.Name, "error", err)
			}
		}(account)
	}
	wg.Wait()
}

func refreshQuotaReport(ctx context.Context, client *quota.Client, account *store.Account, accessToken string, snapshot *quota.Snapshot) (planType, reportErr string, firstErr error) {
	report, err := client.FetchUsage(ctx, account, accessToken)
	if err != nil {
		reportErr = err.Error()
		if firstErr == nil {
			firstErr = err
		}
	} else {
		snapshot.Report = report
		planType = report.PlanType
		if report.PlanType != "" && report.PlanType != account.PlanType {
			account.PlanType = report.PlanType
		}
	}
	if snapshot.Report != nil && reportErr != "" {
		snapshot.Report.Error = reportErr
	}

	return planType, reportErr, firstErr
}
