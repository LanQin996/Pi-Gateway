package admin

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"pi-gateway/internal/config"
	"pi-gateway/internal/store"
)

// keyView resolves the names an operator needs: the groups the key belongs to
// (no groups means every account) and the accounts that actually reaches.
// The key value itself is exposed: the operator needs it to configure clients,
// and this endpoint is already behind admin authentication.
type keyView struct {
	*store.APIKey
	GroupNames   []string `json:"group_names"`
	AccountNames []string `json:"account_names"`
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListKeys(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	groups, err := s.store.ListAccountGroups(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	groupByID := map[int64]store.AccountGroup{}
	for _, g := range groups {
		groupByID[g.ID] = g
	}

	out := make([]keyView, 0, len(keys))
	for _, k := range keys {
		view := keyView{APIKey: k, GroupNames: []string{}, AccountNames: []string{}}
		for _, gid := range k.GroupIDs {
			if g, ok := groupByID[gid]; ok {
				view.GroupNames = append(view.GroupNames, g.Name)
			} else {
				view.GroupNames = append(view.GroupNames, "已删除的分组")
			}
		}
		// Use the same persisted group-scope query as request routing, including
		// public accounts and excluding membership solely through disabled groups.
		reachable, err := s.store.EligibleAccountsForKey(r.Context(), k)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, a := range reachable {
			view.AccountNames = append(view.AccountNames, a.Name)
		}
		sort.Strings(view.AccountNames)
		sort.Strings(view.GroupNames)
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

// keyMutation is the shared create/update payload. Zero-valued limits mean
// "unlimited", which is also what the reference implementation uses.
type keyMutation struct {
	Name              *string  `json:"name"`
	Enabled           *bool    `json:"enabled"`
	Strategy          *string  `json:"strategy"`
	Transport         *string  `json:"transport"`
	Label             *string  `json:"label"`
	MaxConcurrency    *int     `json:"max_concurrency"`
	RequestsPerMinute *int     `json:"requests_per_minute"`
	DailyLimitUSD     *float64 `json:"daily_limit_usd"`
	WeeklyLimitUSD    *float64 `json:"weekly_limit_usd"`
	GroupIDs          *[]int64 `json:"group_ids"`
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var body keyMutation
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Name == nil || strings.TrimSpace(*body.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}

	k := &store.APIKey{
		Name:      strings.TrimSpace(*body.Name),
		Enabled:   true,
		Strategy:  stringOrEmpty(body.Strategy),
		Transport: stringOrEmpty(body.Transport),
		GroupIDs:  []int64{},
	}
	if err := s.applyKeyMutation(r, k, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Enabled != nil {
		k.Enabled = *body.Enabled
	}

	if err := s.store.CreateKey(r.Context(), k); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.RecordAudit(r.Context(), "key.created", k.Name)
	writeJSON(w, http.StatusOK, map[string]any{"key": keyView{APIKey: k}})
}

func (s *Server) handleUpdateKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	k, err := s.store.GetKey(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if k == nil {
		writeErr(w, http.StatusNotFound, keyNotFoundMessage)
		return
	}

	var body keyMutation
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Name != nil {
		k.Name = strings.TrimSpace(*body.Name)
		if k.Name == "" {
			writeErr(w, http.StatusBadRequest, "name cannot be empty")
			return
		}
	}
	if body.Enabled != nil {
		k.Enabled = *body.Enabled
	}
	if err := s.applyKeyMutation(r, k, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.store.UpdateKey(r.Context(), k); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.RecordAudit(r.Context(), "key.updated", k.Name)
	writeJSON(w, http.StatusOK, map[string]any{"key": keyView{APIKey: k}})
}

// applyKeyMutation validates and applies the optional fields shared by create
// and update. Unset fields are left untouched.
func (s *Server) applyKeyMutation(r *http.Request, k *store.APIKey, body *keyMutation) error {
	if body.Strategy != nil {
		k.Strategy = *body.Strategy
	}
	if body.Transport != nil {
		k.Transport = *body.Transport
	}
	if body.Label != nil {
		k.Label = strings.TrimSpace(*body.Label)
	}
	if err := validateKeyOptions(k.Strategy, k.Transport); err != nil {
		return err
	}
	if err := applyKeyLimits(k, body); err != nil {
		return err
	}
	if err := s.applyKeyGroups(r, k, body); err != nil {
		return err
	}

	if k.GroupIDs == nil {
		k.GroupIDs = []int64{}
	}
	return nil
}

func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	k, _ := s.store.GetKey(r.Context(), id)
	if err := s.store.DeleteKey(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	name := "#" + itoa(id)
	if k != nil {
		name = k.Name
	}
	_ = s.store.RecordAudit(r.Context(), "key.deleted", name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleRevealKey returns the stored plaintext key. The reference implementation
// only exposes an existing key (there is no rotate endpoint), and neither does
// this gateway.
func (s *Server) handleRevealKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	k, err := s.store.GetKey(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if k == nil {
		writeErr(w, http.StatusNotFound, keyNotFoundMessage)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": k.Key})
}

// handleResetKeyBudget clears the used amount for the current daily, weekly or
// both budget windows without touching already-charged events.
func (s *Server) handleResetKeyBudget(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	k, err := s.store.GetKey(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if k == nil {
		writeErr(w, http.StatusNotFound, keyNotFoundMessage)
		return
	}
	var body struct {
		Period string `json:"period"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	period := strings.ToLower(strings.TrimSpace(body.Period))
	periods := []string{period}
	if period == "all" || period == "" {
		periods = []string{"daily", "weekly"}
	} else if period != "daily" && period != "weekly" {
		writeErr(w, http.StatusBadRequest, "period must be daily, weekly or all")
		return
	}
	now := time.Now().UnixMilli()
	for _, p := range periods {
		if err := s.store.ResetKeyBudget(r.Context(), id, p, now); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	_ = s.store.RecordAudit(r.Context(), "key.budget_reset", k.Name+" "+strings.Join(periods, ","))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func validateKeyOptions(strategy, transport string) error {
	if strategy != "" && !contains(config.ValidStrategies, strategy) {
		return errInvalid("strategy must be one of: " + strings.Join(config.ValidStrategies, ", "))
	}
	if transport != "" && !contains(config.ValidTransports, transport) {
		return errInvalid("transport must be one of: " + strings.Join(config.ValidTransports, ", "))
	}
	return nil
}

func stringOrEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(*v)
}

type invalidError string

func (e invalidError) Error() string { return string(e) }

func errInvalid(msg string) error { return invalidError(msg) }

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func applyKeyLimits(k *store.APIKey, body *keyMutation) error {
	if body.MaxConcurrency != nil {
		if *body.MaxConcurrency < 0 {
			return errInvalid("max_concurrency cannot be negative")
		}
		k.MaxConcurrency = *body.MaxConcurrency
	}
	if body.RequestsPerMinute != nil {
		if *body.RequestsPerMinute < 0 {
			return errInvalid("requests_per_minute cannot be negative")
		}
		k.RequestsPerMinute = *body.RequestsPerMinute
	}
	if body.DailyLimitUSD != nil {
		if *body.DailyLimitUSD < 0 {
			return errInvalid("daily_limit_usd cannot be negative")
		}
		k.DailyLimitUSD = *body.DailyLimitUSD
	}
	if body.WeeklyLimitUSD != nil {
		if *body.WeeklyLimitUSD < 0 {
			return errInvalid("weekly_limit_usd cannot be negative")
		}
		k.WeeklyLimitUSD = *body.WeeklyLimitUSD
	}

	return nil
}

func (s *Server) applyKeyGroups(r *http.Request, k *store.APIKey, body *keyMutation) error {
	if body.GroupIDs != nil {
		groups, err := s.store.ListAccountGroups(r.Context())
		if err != nil {
			return err
		}
		known := map[int64]bool{}
		for _, g := range groups {
			known[g.ID] = true
		}
		seen := map[int64]bool{}
		clean := make([]int64, 0, len(*body.GroupIDs))
		for _, gid := range *body.GroupIDs {
			if !known[gid] {
				return errInvalid("unknown account group id")
			}
			if seen[gid] {
				continue
			}
			seen[gid] = true
			clean = append(clean, gid)
		}
		sort.Slice(clean, func(i, j int) bool { return clean[i] < clean[j] })
		k.GroupIDs = clean
	}
	return nil
}
