package admin

import (
	"context"
	"errors"
	"math"
	"net/http"
	"time"

	"pi-gateway/internal/quota"
	"pi-gateway/internal/store"
)

// quotaView is the account quota state as the UI consumes it.
type quotaView struct {
	// Cost is all retained account usage; window costs align with the quota report.
	Cost        *store.UsageCost  `json:"cost"`
	WindowCosts []quotaWindowCost `json:"window_costs"`
	// Report is the parsed usage: rate-limit windows, credits, spend control.
	Report *quota.Report `json:"report"`
	// ResetCredits is the inventory of consumable rate-limit resets.
	ResetCredits *quota.ResetCredits `json:"reset_credits"`
	// Windows is a flattened convenience list ordered for display.
	Windows []quota.Window `json:"windows"`
	// Session is the 5-hour window of the primary ("codex") bucket, if reported.
	Session *quota.Window `json:"session"`
	// Weekly is the 7-day window of the primary bucket, if reported.
	Weekly *quota.Window `json:"weekly"`
	// UpdatedAt is when the snapshot was fetched (unix ms).
	UpdatedAt int64 `json:"updated_at"`
	// Error explains why the snapshot may be stale.
	Error string `json:"error,omitempty"`
	// Available reports whether a quota client is configured at all.
	Available bool `json:"available"`
}

type quotaWindowCost struct {
	LimitID               string           `json:"limit_id"`
	Role                  string           `json:"role"`
	StartAt               int64            `json:"start_at"`
	AsOf                  int64            `json:"as_of"`
	Usage                 *store.UsageCost `json:"usage"`
	EstimatedTotalUSD     *float64         `json:"estimated_total_usd"`
	EstimatedRemainingUSD *float64         `json:"estimated_remaining_usd"`
	UnavailableReason     string           `json:"unavailable_reason,omitempty"`
}

func (s *Server) buildQuotaView(ctx context.Context, account *store.Account) quotaView {
	view := quotaView{
		Windows:     []quota.Window{},
		WindowCosts: []quotaWindowCost{},
		Available:   s.accounts.QuotaClient() != nil,
	}
	if account != nil {
		view.Cost, _ = s.store.AccountUsageCost(ctx, account.ID, store.UsageRange{}, 0)
	}
	// Quota state is produced by the Codex credential; without one there is no
	// authoritative snapshot to show, so no account-facing response may expose a
	// stale cached value.
	if account == nil || !account.CodexLinked() {
		return view
	}
	view.UpdatedAt = account.QuotaUpdatedAt
	view.Error = account.QuotaError
	snapshot := s.accounts.QuotaSnapshot(account)
	if snapshot == nil {
		return view
	}
	view.Report = snapshot.Report
	view.ResetCredits = snapshot.ResetCredits
	if snapshot.Report != nil {
		view.Windows = snapshot.Report.Windows
		view.Session = quota.FindWindow(snapshot.Report.Windows, "codex", quota.KindShortTerm)
		view.Weekly = quota.FindWindow(snapshot.Report.Windows, "codex", quota.KindWeekly)
		asOf := snapshot.Report.FetchedAt
		// Legacy reports may lack fetched_at. A failed refresh updates the account
		// timestamp without updating the report, so it cannot be used in that case.
		if asOf == 0 && account.QuotaError == "" && snapshot.Report.Error == "" {
			asOf = account.QuotaUpdatedAt
		}
		for _, window := range view.Windows {
			// Additional buckets do not identify which model requests consumed them.
			if window.LimitID == "codex" {
				view.WindowCosts = append(view.WindowCosts, s.buildQuotaWindowCost(ctx, account.ID, window, asOf, store.NowMS()))
			}
		}
	}
	return view
}

func (s *Server) buildQuotaWindowCost(ctx context.Context, accountID int64, window quota.Window, asOf, now int64) quotaWindowCost {
	cost := quotaWindowCost{LimitID: window.LimitID, Role: window.Role, AsOf: asOf}
	if window.ResetAt <= 0 || window.WindowSeconds <= 0 || window.WindowSeconds > window.ResetAt/1000 {
		cost.UnavailableReason = "missing_window"
		return cost
	}
	cost.StartAt = window.ResetAt - window.WindowSeconds*1000
	if asOf <= 0 || asOf > now || asOf < cost.StartAt || asOf >= window.ResetAt {
		cost.UnavailableReason = "missing_snapshot"
		return cost
	}
	var err error
	cost.Usage, err = s.store.AccountUsageCost(ctx, accountID, store.UsageRange{Start: cost.StartAt, End: asOf}, asOf)
	switch {
	case err != nil:
		cost.Usage = nil
		cost.UnavailableReason = "query_failed"
	case window.ResetAt <= now:
		cost.UnavailableReason = "expired"
	case window.UsedPercent <= 0 || window.UsedPercent > 100 || math.IsNaN(window.UsedPercent) || math.IsInf(window.UsedPercent, 0):
		cost.UnavailableReason = "no_consumption"
	default:
		// Missing prices/tokens do not block projection. Unknown requests add no
		// recorded cost; their count remains available for the detail tooltip.
		total := float64(cost.Usage.CostMicros) / 1e6 * 100 / window.UsedPercent
		remaining := total * (100 - window.UsedPercent) / 100
		if math.IsInf(total, 0) || math.IsNaN(total) {
			cost.UnavailableReason = "no_consumption"
			break
		}
		cost.EstimatedTotalUSD, cost.EstimatedRemainingUSD = &total, &remaining
	}
	return cost
}

// handleGetQuota returns the stored snapshot without contacting the upstream.
func (s *Server) handleGetQuota(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	account, err := s.store.GetAccount(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if account == nil {
		writeErr(w, http.StatusNotFound, accountNotFoundMessage)
		return
	}
	// The snapshot is produced by the Codex credential; without one there is
	// nothing authoritative to show, so surface the same conflict as refresh.
	if !account.CodexLinked() {
		writeErr(w, http.StatusConflict, codexCredentialRequiredMessage)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"quota": s.buildQuotaView(r.Context(), account)})
}

// handleRefreshQuota fetches fresh quota data from the upstream and returns it.
func (s *Server) handleRefreshQuota(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	account, err := s.store.GetAccount(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if account == nil {
		writeErr(w, http.StatusNotFound, accountNotFoundMessage)
		return
	}
	if s.accounts.QuotaClient() == nil {
		writeErr(w, http.StatusServiceUnavailable, "quota fetching is not configured")
		return
	}
	// Quota state lives on the Codex backend, so it needs the account's optional
	// Codex credential rather than the ChatGPT model credential.
	if !account.CodexLinked() {
		writeErr(w, http.StatusConflict, codexCredentialRequiredMessage)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	snapshot, refreshErr := s.accounts.RefreshQuota(ctx, account)
	updated, err := s.store.GetAccount(r.Context(), id)
	if err != nil || updated == nil {
		updated = account
	}

	view := s.buildQuotaView(r.Context(), updated)
	writeJSON(w, http.StatusOK, map[string]any{
		"quota":   view,
		"fetched": snapshot != nil,
		"error":   errorString(refreshErr),
	})
}

// handleConsumeResetCredit spends one rate-limit reset credit for an account.
//
// This is the manual "use a reset" action: it immediately restores the exhausted
// window, so the response carries the refreshed quota.
func (s *Server) handleConsumeResetCredit(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	account, err := s.store.GetAccount(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if account == nil {
		writeErr(w, http.StatusNotFound, accountNotFoundMessage)
		return
	}
	if s.accounts.QuotaClient() == nil {
		writeErr(w, http.StatusServiceUnavailable, "quota fetching is not configured")
		return
	}
	// Quota state lives on the Codex backend, so it needs the account's optional
	// Codex credential rather than the ChatGPT model credential.
	if !account.CodexLinked() {
		writeErr(w, http.StatusConflict, codexCredentialRequiredMessage)
		return
	}

	var body struct {
		CreditID string `json:"credit_id"`
	}
	_ = decodeJSON(r, &body)

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	result, snapshot, err := s.accounts.ConsumeResetCredit(ctx, account, body.CreditID)

	updated, getErr := s.store.GetAccount(r.Context(), id)
	if getErr != nil || updated == nil {
		updated = account
	}
	view := s.buildQuotaView(r.Context(), updated)

	status := http.StatusOK
	if err != nil {
		status = http.StatusBadGateway
		var consumeErr *quota.ConsumeError
		switch {
		case errors.As(err, &consumeErr):
			// A business refusal is a client-visible decision, not a gateway fault.
			status = http.StatusConflict
			if consumeErr.Code == quota.CodeNoCredit || consumeErr.Code == quota.CodeNothingToReset {
				status = http.StatusConflict
			}
		}
	}

	payload := map[string]any{
		"quota":   view,
		"fetched": snapshot != nil,
		"error":   errorString(err),
	}
	if result != nil {
		payload["result"] = result
	}
	if err == nil && result != nil {
		_ = s.store.RecordAudit(r.Context(), "account.reset_credit_consumed", account.Name+" ("+result.Code+")")
	}
	writeJSON(w, status, payload)
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
