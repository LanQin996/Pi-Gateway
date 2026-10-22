package admin

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"time"

	"pi-gateway/internal/quota"
	"pi-gateway/internal/store"
)

// Official days have UTC granularity, independent of local request statistics.
func officialDateRange(r *http.Request) (string, string, error) {
	q := r.URL.Query()
	start, end := q.Get("start_date"), q.Get("end_date")
	if start == "" && end == "" {
		days := 30
		if q.Get("days") != "" {
			n, err := strconv.Atoi(q.Get("days"))
			if err != nil || n < 1 || n > 366 {
				return "", "", errInvalid("days must be 1..366")
			}
			days = n
		}
		now := time.Now().UTC()
		start, end = now.AddDate(0, 0, -days+1).Format(time.DateOnly), now.Format(time.DateOnly)
	}
	s, err := time.Parse(time.DateOnly, start)
	if err != nil {
		return "", "", errInvalid("start_date must be YYYY-MM-DD")
	}
	e, err := time.Parse(time.DateOnly, end)
	if err != nil || s.After(e) || e.Sub(s) > 365*24*time.Hour || end > time.Now().UTC().Format(time.DateOnly) {
		return "", "", errInvalid("invalid date range (maximum 366 days)")
	}
	return start, end, nil
}

func (s *Server) handleOfficialUsage(w http.ResponseWriter, r *http.Request) {
	start, end, err := officialDateRange(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	id, err := queryInt64(r.URL.Query().Get("account_id"))
	if err != nil || id < 0 {
		writeErr(w, 400, "invalid account_id")
		return
	}
	accounts, err := s.store.ListAccounts(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	items, states, err := s.loadOfficialUsage(r.Context(), accounts, id, start, end)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Day == items[j].Day {
			return items[i].WorkspaceID < items[j].WorkspaceID
		}
		return items[i].Day > items[j].Day
	})
	var credits float64
	known := false
	missingCredits, pending := 0, 0
	for _, d := range items {
		if d.Credits != nil {
			credits += *d.Credits
			known = true
		} else {
			missingCredits++
		}
		if !d.Settled {
			pending++
		}
	}
	var creditTotal, usd *float64
	if known {
		creditTotal = &credits
		value := credits / quota.CreditsPerUSD
		usd = &value
	}
	writeJSON(w, 200, map[string]any{"start_date": start, "end_date": end, "timezone": "UTC", "items": items, "sync": states,
		"total_credits": creditTotal, "equivalent_usd": usd, "credits_per_usd": quota.CreditsPerUSD, "missing_credit_days": missingCredits, "pending_days": pending, "workspace_count": len(states)})
}

func (s *Server) handleRefreshOfficialUsage(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a, err := s.store.GetAccount(r.Context(), id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if a == nil {
		writeErr(w, 404, "account not found")
		return
	}
	if !a.CodexLinked() {
		writeErr(w, 409, "link a Codex credential to read official usage")
		return
	}
	if s.accounts == nil {
		writeErr(w, 503, "official usage fetching is not configured")
		return
	}
	err = s.accounts.RefreshOfficialUsage(r.Context(), a)
	state, stateErr := s.store.OfficialUsageSyncFor(r.Context(), store.OfficialUsageKey(a))
	if stateErr != nil {
		writeErr(w, 500, stateErr.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"sync": state, "error": errorString(err)})
}

func (s *Server) loadOfficialUsage(ctx context.Context, accounts []*store.Account, id int64, start, end string) ([]store.OfficialUsageDay, []store.OfficialUsageSync, error) {
	items := []store.OfficialUsageDay{}
	states := []store.OfficialUsageSync{}
	seen := map[string]bool{}
	for _, a := range accounts {
		key := store.OfficialUsageKey(a)
		if !a.CodexLinked() || a.CodexAccountID == "" || (id != 0 && a.ID != id) || seen[key] {
			continue
		}
		seen[key] = true
		rows, err := s.store.ListOfficialUsage(ctx, key, start, end)
		if err != nil {
			return nil, nil, err
		}
		for i := range rows {
			rows[i].AccountID = a.ID
			rows[i].WorkspaceID = a.CodexAccountID
		}
		state, err := s.store.OfficialUsageSyncFor(ctx, key)
		if err != nil {
			return nil, nil, err
		}
		items = append(items, rows...)
		states = append(states, state)
	}

	return items, states, nil
}
