package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"time"

	"pi-gateway/internal/store"
)

// Daily usage follows codex2api/proxy/usage_wham_daily.go. Credits are
// authoritative; USD is only the same 25 credits/USD reference conversion.
const CreditsPerUSD = 25.0

type DailyUsageCounts struct {
	Credits             *float64 `json:"credits"`
	Users               int64    `json:"users"`
	Threads             int64    `json:"threads"`
	Turns               int64    `json:"turns"`
	UncachedInputTokens *int64   `json:"uncached_text_input_tokens"`
	CachedInputTokens   *int64   `json:"cached_text_input_tokens"`
	OutputTokens        *int64   `json:"text_output_tokens"`
	TotalTokens         *int64   `json:"text_total_tokens"`
}

type DailyUsageDay struct {
	Date    string           `json:"date"`
	Totals  DailyUsageCounts `json:"totals"`
	Clients json.RawMessage  `json:"clients"`
	Models  json.RawMessage  `json:"models"`
}

// FetchDailyUsage uses the same account identity, egress and GET fallback as
// quota reads. Date bounds are inclusive UTC calendar days.
func (c *Client) FetchDailyUsage(ctx context.Context, account *store.Account, token, start, end string) ([]store.OfficialUsageDay, error) {
	for _, date := range []string{start, end} {
		if _, err := time.Parse(time.DateOnly, date); err != nil {
			return nil, fmt.Errorf("official usage: invalid date")
		}
	}
	if start > end {
		return nil, fmt.Errorf("official usage: invalid date range")
	}
	q := url.Values{"start_date": {start}, "end_date": {end}, "group_by": {"day"}, "workspace_user": {"true"}}
	raw, status, err := c.getAccountResource(ctx, account, token, "analytics/daily-workspace-usage-counts?"+q.Encode())
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("official usage returned HTTP %d", status)
	}
	var response struct {
		Data    []DailyUsageDay `json:"data"`
		GroupBy string          `json:"group_by"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("official usage: invalid JSON response")
	}
	if response.Data == nil || (response.GroupBy != "" && response.GroupBy != "day") {
		return nil, fmt.Errorf("official usage: missing daily data")
	}
	out := make([]store.OfficialUsageDay, 0, len(response.Data))
	today := time.Now().UTC().Format(time.DateOnly)
	for _, d := range response.Data {
		if err := validateDailyUsage(d, start, end); err != nil {
			return nil, err
		}
		dayRaw, _ := json.Marshal(d)
		out = append(out, store.OfficialUsageDay{Day: d.Date, Credits: d.Totals.Credits, UncachedInputTokens: d.Totals.UncachedInputTokens,
			CachedInputTokens: d.Totals.CachedInputTokens, OutputTokens: d.Totals.OutputTokens, TotalTokens: d.Totals.TotalTokens,
			Users: d.Totals.Users, Threads: d.Totals.Threads, Turns: d.Totals.Turns, Settled: d.Date < today && d.Totals.TotalTokens != nil, Raw: string(dayRaw)})
	}
	return out, nil
}

func validateDailyUsage(d DailyUsageDay, start, end string) error {
	if _, err := time.Parse(time.DateOnly, d.Date); err != nil || d.Date < start || d.Date > end {
		return fmt.Errorf("official usage: invalid daily date")
	}
	if d.Totals.Credits != nil && (*d.Totals.Credits < 0 || math.IsNaN(*d.Totals.Credits) || math.IsInf(*d.Totals.Credits, 0)) {
		return fmt.Errorf("official usage: invalid credits")
	}
	for _, n := range []*int64{d.Totals.UncachedInputTokens, d.Totals.CachedInputTokens, d.Totals.OutputTokens, d.Totals.TotalTokens} {
		if n != nil && *n < 0 {
			return fmt.Errorf("official usage: negative token count")
		}
	}
	return nil
}
