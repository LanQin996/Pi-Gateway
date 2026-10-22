// Package quota reads an account's ChatGPT Codex rate-limit state: the 5-hour and
// 7-day windows, remaining credits, and the remaining "rate limit reset" credits
// that can be consumed manually.
//
// Quota endpoints live on the ChatGPT/Codex backend, separate from the model API
// host. When the base path contains "backend-api" the resource prefix is "wham",
// otherwise "api/codex" — matching the official clients.
package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"pi-gateway/internal/egress"
	"pi-gateway/internal/piwire"
	"pi-gateway/internal/store"
)

// Window kinds, derived from the window duration the backend reports.
const (
	KindShortTerm = "5h"
	KindWeekly    = "7d"
	KindMonthly   = "30d"
	KindOther     = "other"

	// CodexBaseURL is the existing Pi-compatible ChatGPT backend used for quota
	// requests. It must never be replaced with the model API base URL.
	CodexBaseURL = "https://chatgpt.com/backend-api"
)

// Window roles mirror the protocol slot names.
const (
	RolePrimary   = "primary"
	RoleSecondary = "secondary"
)

// Consume result codes returned by the upstream.
const (
	CodeReset           = "reset"
	CodeAlreadyRedeemed = "already_redeemed"
	CodeNoCredit        = "no_credit"
	CodeNothingToReset  = "nothing_to_reset"
)

// maxBodyBytes bounds a quota response body.
const maxBodyBytes = 1 << 20

// Window is one rate-limit window.
type Window struct {
	// LimitID identifies the bucket: "codex" for the top-level rate limit,
	// "code_review", or an additional per-model limit.
	LimitID string `json:"limit_id"`
	// LimitName is the human-readable name for additional limits.
	LimitName string `json:"limit_name,omitempty"`
	Role      string `json:"role"`
	Kind      string `json:"kind"`
	// UsedPercent is clamped to 0..100.
	UsedPercent float64 `json:"used_percent"`
	// ResetAt is a unix-millisecond timestamp (the backend reports seconds).
	ResetAt       int64 `json:"reset_at"`
	WindowSeconds int64 `json:"window_seconds"`
	LimitReached  bool  `json:"limit_reached"`
}

// Credits is the plan's credit balance.
type Credits struct {
	HasCredits          bool    `json:"has_credits"`
	Unlimited           bool    `json:"unlimited"`
	Balance             *string `json:"balance"`
	OverageLimitReached bool    `json:"overage_limit_reached"`
}

// SpendControl reports a provider-side spend cap.
type SpendControl struct {
	Reached bool `json:"reached"`
}

// Report is the parsed usage response plus fetch metadata.
//
// Raw keeps the upstream JSON verbatim so the UI can show what the backend
// actually returned and so new fields need no parser change.
type Report struct {
	PlanType     string          `json:"plan_type"`
	Allowed      bool            `json:"allowed"`
	LimitReached bool            `json:"limit_reached"`
	Windows      []Window        `json:"windows"`
	Credits      *Credits        `json:"credits,omitempty"`
	SpendControl *SpendControl   `json:"spend_control,omitempty"`
	Raw          json.RawMessage `json:"raw,omitempty"`
	FetchedAt    int64           `json:"fetched_at"`
	// Error records why a fetch failed without discarding the previous snapshot.
	Error string `json:"error,omitempty"`
}

// ResetCredit is one rate-limit reset card.
type ResetCredit struct {
	ID        string `json:"id"`
	Status    string `json:"status,omitempty"`
	Title     string `json:"title,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	ResetType string `json:"reset_type,omitempty"`
}

// ResetCredits is the reset-credit inventory.
type ResetCredits struct {
	AvailableCount int           `json:"available_count"`
	Credits        []ResetCredit `json:"credits"`
	FetchedAt      int64         `json:"fetched_at"`
	Error          string        `json:"error,omitempty"`
}

// ConsumeResult is the outcome of consuming one reset credit.
type ConsumeResult struct {
	Code   string       `json:"code"`
	Credit *ResetCredit `json:"credit,omitempty"`
}

// Snapshot is the persisted per-account quota state.
type Snapshot struct {
	Report       *Report       `json:"report,omitempty"`
	ResetCredits *ResetCredits `json:"reset_credits,omitempty"`
}

// Config configures the quota client.
type Config struct {
	// BaseURL is the configured upstream base (for example
	// https://chatgpt.com/backend-api).
	BaseURL string
	// OfficialBaseURL is retried for endpoint-selection failures (401/403/404)
	// and for server errors (5xx) on read-only GET requests.
	OfficialBaseURL string
	Originator      string
	UserAgent       string
	// UserAgentFunc, when set, supplies the current User-Agent so a runtime
	// settings change applies without rebuilding the client.
	UserAgentFunc func() string
	Timeout       time.Duration
}

// Client fetches quota data for accounts.
type Client struct {
	cfg     Config
	factory *egress.Factory
	logger  *slog.Logger
}

// New builds a quota client.
func New(cfg Config, factory *egress.Factory, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 20 * time.Second
	}
	if strings.TrimSpace(cfg.BaseURL) == "" || isModelAPIBase(cfg.BaseURL) {
		cfg.BaseURL = CodexBaseURL
	}
	if strings.TrimSpace(cfg.OfficialBaseURL) == "" || isModelAPIBase(cfg.OfficialBaseURL) {
		cfg.OfficialBaseURL = CodexBaseURL
	}
	return &Client{cfg: cfg, factory: factory, logger: logger}
}

// AccountEndpointURL builds a quota URL, not a model Responses URL. A legacy
// Codex /codex[/responses] suffix belongs to model traffic and must not be
// retained before /wham. Query parameters and escaped proxy prefixes survive.
func AccountEndpointURL(baseURL, resource string) string {
	base := strings.TrimSpace(baseURL)
	parsed, err := url.Parse(base)
	if err != nil {
		return base // The request constructor will report the invalid URL.
	}
	path := strings.TrimRight(parsed.EscapedPath(), "/")
	prefix := "api/codex"
	if hasBackendAPIPath(base) {
		prefix = "wham"
		path = strings.TrimSuffix(path, "/codex/responses")
		path = strings.TrimSuffix(path, "/codex")
	}
	resourcePath, resourceQuery, _ := strings.Cut(resource, "?")
	if resourceQuery != "" {
		query := parsed.Query()
		values, err := url.ParseQuery(resourceQuery)
		if err != nil {
			return base
		}
		for key, vals := range values {
			query[key] = vals
		}
		parsed.RawQuery = query.Encode()
	}
	parsed.RawPath = path + "/" + prefix + "/" + strings.TrimLeft(resourcePath, "/")
	parsed.Path, _ = url.PathUnescape(parsed.RawPath)
	return parsed.String()
}

// hasBackendAPIPath reports whether any path segment equals "backend-api".
func hasBackendAPIPath(baseURL string) bool {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment == "backend-api" {
			return true
		}
	}
	return false
}

func isModelAPIBase(baseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	// Codex quota is never served by the model API host, regardless of whether
	// an operator supplied /v1, a full Responses URL, or just the root slash.
	return strings.EqualFold(parsed.Hostname(), "api.openai.com")
}

// FetchUsage retrieves and parses the usage report.
func (c *Client) FetchUsage(ctx context.Context, account *store.Account, accessToken string) (*Report, error) {
	body, status, err := c.getAccountResource(ctx, account, accessToken, "usage")
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("upstream usage request failed (%d): %s", status, truncate(body))
	}

	report, err := ParseUsage(body)
	if err != nil {
		return nil, err
	}
	report.Raw = json.RawMessage(body)
	report.FetchedAt = store.NowMS()
	return report, nil
}

// FetchResetCredits lists the available rate-limit reset credits.
func (c *Client) FetchResetCredits(ctx context.Context, account *store.Account, accessToken string) (*ResetCredits, error) {
	body, status, err := c.getAccountResource(ctx, account, accessToken, "rate-limit-reset-credits")
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("upstream reset-credits request failed (%d): %s", status, truncate(body))
	}

	var wire struct {
		AvailableCount int `json:"available_count"`
		Credits        []struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			Title     string `json:"title"`
			ExpiresAt string `json:"expires_at"`
			ResetType string `json:"reset_type"`
		} `json:"credits"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("invalid reset-credit response: %w", err)
	}

	out := &ResetCredits{AvailableCount: wire.AvailableCount, Credits: []ResetCredit{}, FetchedAt: store.NowMS()}
	for _, credit := range wire.Credits {
		out.Credits = append(out.Credits, ResetCredit{
			ID:        strings.TrimSpace(credit.ID),
			Status:    strings.TrimSpace(credit.Status),
			Title:     strings.TrimSpace(credit.Title),
			ExpiresAt: strings.TrimSpace(credit.ExpiresAt),
			ResetType: strings.TrimSpace(credit.ResetType),
		})
	}
	return out, nil
}

// ConsumeResetCredit spends one reset credit.
//
// creditID may be empty, in which case the backend picks the best available card.
// redeemRequestID is an idempotency key.
func (c *Client) ConsumeResetCredit(ctx context.Context, account *store.Account, accessToken, creditID string) (*ConsumeResult, error) {
	payload := map[string]any{"redeem_request_id": uuid.NewString()}
	if strings.TrimSpace(creditID) != "" {
		payload["credit_id"] = strings.TrimSpace(creditID)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	body, status, err := c.accountRequest(ctx, account, accessToken, http.MethodPost, "rate-limit-reset-credits/consume", raw)
	if err != nil {
		return nil, err
	}

	// The result code is meaningful even on 409/429 responses, so parse the body
	// before treating the status as an error.
	var wire struct {
		Code   string `json:"code"`
		Credit *struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			Title     string `json:"title"`
			ExpiresAt string `json:"expires_at"`
			ResetType string `json:"reset_type"`
		} `json:"credit"`
	}
	if err := json.Unmarshal(body, &wire); err == nil && wire.Code != "" {
		result := &ConsumeResult{Code: wire.Code}
		if wire.Credit != nil {
			result.Credit = &ResetCredit{
				ID:        wire.Credit.ID,
				Status:    wire.Credit.Status,
				Title:     wire.Credit.Title,
				ExpiresAt: wire.Credit.ExpiresAt,
				ResetType: wire.Credit.ResetType,
			}
		}
		if status >= 200 && status < 300 {
			return result, nil
		}
		// A business code on a non-2xx response is a decision, not a transport
		// failure; report it so the caller can explain it.
		return result, &ConsumeError{StatusCode: status, Code: result.Code, Body: truncate(body)}
	}

	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("reset credit failed (%d): %s", status, truncate(body))
	}
	return nil, fmt.Errorf("ambiguous reset-credit response: %s", truncate(body))
}

// ConsumeError reports a business-level refusal from the upstream.
type ConsumeError struct {
	StatusCode int
	Code       string
	Body       string
}

func (e *ConsumeError) Error() string {
	switch e.Code {
	case CodeNoCredit:
		return "no rate-limit reset credit is available"
	case CodeNothingToReset:
		return "no rate-limit window needs resetting"
	case CodeAlreadyRedeemed:
		return "this reset request was already redeemed"
	}
	if e.Code != "" {
		return "upstream refused the reset: " + e.Code
	}
	return fmt.Sprintf("upstream refused the reset (%d): %s", e.StatusCode, e.Body)
}

// getAccountResource performs a GET for one account resource.
func (c *Client) getAccountResource(ctx context.Context, account *store.Account, accessToken, resource string) ([]byte, int, error) {
	return c.accountRequest(ctx, account, accessToken, http.MethodGet, resource, nil)
}

// accountRequest issues an authenticated account-scoped request, falling back to
// the official base URL on endpoint-selection refusals (401/403/404). Only GET
// requests also fall back on transport/read errors and server-side failures.
func (c *Client) accountRequest(ctx context.Context, account *store.Account, accessToken, method, resource string, body []byte) ([]byte, int, error) {
	targets := []string{c.cfg.BaseURL}
	if official := strings.TrimRight(c.cfg.OfficialBaseURL, "/"); official != "" && !strings.EqualFold(official, strings.TrimRight(c.cfg.BaseURL, "/")) {
		targets = append(targets, official)
	}

	var (
		lastBody   []byte
		lastStatus int
		lastErr    error
	)
	for i, base := range targets {
		resourceURL := AccountEndpointURL(base, resource)
		respBody, status, err := c.do(ctx, account, accessToken, method, resourceURL, body)
		if err != nil {
			// A transport/read error leaves a non-idempotent request's outcome
			// unknown: the upstream may have processed it before the response was
			// lost. Never replay POSTs across endpoints, because idempotency keys
			// are not known to be shared across endpoint domains. Read-only GETs
			// retain their existing fallback behavior.
			if method != http.MethodGet {
				return nil, 0, err
			}
			lastErr = err
			continue
		}
		// A configured proxy/backend can reject the legacy path with 401/403/404
		// or fail with a transient 5xx. Retry the known official Codex backend for
		// those endpoint-selection/server failures, but never send the token to a
		// model API base when the client is constructed by the gateway.
		if shouldFallback(method, status) && i < len(targets)-1 {
			lastBody, lastStatus = respBody, status
			continue
		}
		return respBody, status, nil
	}
	if lastErr != nil {
		return nil, 0, lastErr
	}
	return lastBody, lastStatus, nil
}

// isFallbackStatus reports whether a response status is worth trying the next
// target for: the endpoint-selection failures (401/403/404) plus transient 5xx.
func isFallbackStatus(status int) bool {
	switch status {
	case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden:
		return true
	}
	return status >= http.StatusInternalServerError && status <= 599
}

func shouldFallback(method string, status int) bool {
	if !isFallbackStatus(status) {
		return false
	}
	// A 5xx retry is safe for quota reads, but not for the reset-credit POST:
	// the backend may have consumed the credit before returning its error.
	return status < http.StatusInternalServerError || method == http.MethodGet
}

func (c *Client) do(ctx context.Context, account *store.Account, accessToken, method, resourceURL string, body []byte) ([]byte, int, error) {
	client, err := c.factory.HTTPClient(account.ProxyURL)
	if err != nil {
		return nil, 0, err
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, resourceURL, reader)
	if err != nil {
		return nil, 0, err
	}

	// Account endpoints take the identity headers but not the model-traffic ones
	// (no accept/content-type/OpenAI-Beta), matching the official clients.
	req.Header.Set("Authorization", "Bearer "+accessToken)
	// Account endpoints identify the ChatGPT account. When a Codex credential is
	// attached the quota is billed against that account, so its id takes
	// precedence over the model-traffic account id.
	accountID := strings.TrimSpace(account.CodexAccountID)
	if accountID == "" {
		return nil, 0, errors.New("quota: Codex account id is required")
	}
	req.Header.Set("chatgpt-account-id", accountID)
	req.Header.Set("originator", originatorOr(c.cfg.Originator))
	req.Header.Set("User-Agent", c.currentUserAgent())
	req.Header.Set("accept", "application/json")
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("quota request failed: %w", err)
	}
	defer resp.Body.Close()

	decoded, err := egress.DecodeBody(resp)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	defer decoded.Close()

	raw, err := io.ReadAll(io.LimitReader(decoded, maxBodyBytes+1))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read quota response: %w", err)
	}
	if int64(len(raw)) > maxBodyBytes {
		return nil, resp.StatusCode, fmt.Errorf("quota response exceeded %d bytes", maxBodyBytes)
	}
	return raw, resp.StatusCode, nil
}

func originatorOr(v string) string {
	if strings.TrimSpace(v) == "" {
		return piwire.DefaultOriginator
	}
	return v
}

// currentUserAgent resolves the User-Agent for account requests.
func (c *Client) currentUserAgent() string {
	if c.cfg.UserAgentFunc != nil {
		if ua := strings.TrimSpace(c.cfg.UserAgentFunc()); ua != "" {
			return ua
		}
	}
	if ua := strings.TrimSpace(c.cfg.UserAgent); ua != "" {
		return ua
	}
	return piwire.DefaultUserAgent()
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 400 {
		return s[:400] + "…"
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}

// ParseUsage converts a usage response body into a Report.
func ParseUsage(body []byte) (*Report, error) {
	var wire struct {
		PlanType  string `json:"plan_type"`
		RateLimit *struct {
			Allowed      bool            `json:"allowed"`
			LimitReached bool            `json:"limit_reached"`
			Primary      json.RawMessage `json:"primary_window"`
			Secondary    json.RawMessage `json:"secondary_window"`
		} `json:"rate_limit"`
		CodeReviewRateLimit *struct {
			Allowed   bool            `json:"allowed"`
			Primary   json.RawMessage `json:"primary_window"`
			Secondary json.RawMessage `json:"secondary_window"`
		} `json:"code_review_rate_limit"`
		AdditionalRateLimits []struct {
			LimitID        string          `json:"limit_id"`
			LimitName      string          `json:"limit_name"`
			MeteredFeature string          `json:"metered_feature"`
			RateLimit      json.RawMessage `json:"rate_limit"`
		} `json:"additional_rate_limits"`
		Credits *struct {
			HasCredits          bool            `json:"has_credits"`
			Unlimited           bool            `json:"unlimited"`
			Balance             json.RawMessage `json:"balance"`
			OverageLimitReached bool            `json:"overage_limit_reached"`
		} `json:"credits"`
		SpendControl *struct {
			Reached bool `json:"reached"`
		} `json:"spend_control"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("invalid usage response: %w", err)
	}

	report := &Report{
		PlanType:  strings.TrimSpace(wire.PlanType),
		Allowed:   true,
		Windows:   []Window{},
		FetchedAt: store.NowMS(),
	}

	if wire.RateLimit != nil {
		report.Allowed = wire.RateLimit.Allowed
		report.LimitReached = wire.RateLimit.LimitReached
		report.Windows = append(report.Windows, parseWindows("codex", "", wire.RateLimit.Primary, wire.RateLimit.Secondary)...)
	}
	if wire.CodeReviewRateLimit != nil {
		report.Windows = append(report.Windows,
			parseWindows("code_review", "Code review", wire.CodeReviewRateLimit.Primary, wire.CodeReviewRateLimit.Secondary)...)
	}
	for i, extra := range wire.AdditionalRateLimits {
		limitID := firstNonEmpty(extra.LimitID, extra.MeteredFeature, extra.LimitName, fmt.Sprintf("additional_%d", i))
		label := firstNonEmpty(extra.LimitName, extra.LimitID, extra.MeteredFeature, limitID)
		window, err := decodeWindowSet(extra.RateLimit)
		if err != nil {
			continue
		}
		report.Windows = append(report.Windows, parseWindows(normalizeLimitID(limitID), label, window.Primary, window.Secondary)...)
	}

	if wire.Credits != nil {
		credits := &Credits{
			HasCredits:          wire.Credits.HasCredits,
			Unlimited:           wire.Credits.Unlimited,
			OverageLimitReached: wire.Credits.OverageLimitReached,
		}
		credits.Balance = parseBalance(wire.Credits.Balance)
		if credits.HasCredits || credits.Unlimited || credits.Balance != nil || credits.OverageLimitReached {
			report.Credits = credits
		}
	}
	if wire.SpendControl != nil {
		report.SpendControl = &SpendControl{Reached: wire.SpendControl.Reached}
	}

	sortWindows(report.Windows)
	return report, nil
}

type windowSet struct {
	Primary   json.RawMessage
	Secondary json.RawMessage
}

func decodeWindowSet(raw json.RawMessage) (windowSet, error) {
	if len(raw) == 0 {
		return windowSet{}, fmt.Errorf("no rate limit")
	}
	var set struct {
		Primary   json.RawMessage `json:"primary_window"`
		Secondary json.RawMessage `json:"secondary_window"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return windowSet{}, err
	}
	return windowSet{Primary: set.Primary, Secondary: set.Secondary}, nil
}

type wireWindow struct {
	UsedPercent       *float64 `json:"used_percent"`
	ResetAt           *int64   `json:"reset_at"`
	LimitWindowSecond *int64   `json:"limit_window_seconds"`
	WindowSeconds     *int64   `json:"window_seconds"`
	WindowMinutes     *int64   `json:"window_minutes"`
	LimitReached      *bool    `json:"limit_reached"`
}

// parseWindows converts the primary/secondary slots into windows.
func parseWindows(limitID, limitName string, primary, secondary json.RawMessage) []Window {
	out := []Window{}
	for _, slot := range []struct {
		role string
		raw  json.RawMessage
	}{
		{RolePrimary, primary},
		{RoleSecondary, secondary},
	} {
		if len(slot.raw) == 0 || string(slot.raw) == "null" {
			continue
		}
		var wire wireWindow
		if err := json.Unmarshal(slot.raw, &wire); err != nil {
			continue
		}
		if wire.UsedPercent == nil {
			continue
		}
		out = append(out, decodeQuotaWindow(limitID, limitName, slot.role, wire))
	}
	return out
}

// ClassifyWindow maps a duration to a window kind using the same bands the
// official clients use, so a 18000s window is always the 5-hour session.
func ClassifyWindow(seconds int64) string {
	switch {
	case seconds >= 17100 && seconds <= 18900:
		return KindShortTerm
	case seconds >= 574560 && seconds <= 635040:
		return KindWeekly
	case seconds >= 2462400 && seconds <= 2721600:
		return KindMonthly
	default:
		return KindOther
	}
}

func normalizeLimitID(id string) string {
	normalized := strings.ToLower(strings.TrimSpace(id))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	if len(normalized) > 128 {
		normalized = normalized[:128]
	}
	if normalized == "" {
		return "additional"
	}
	return normalized
}

func parseBalance(raw json.RawMessage) *string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		trimmed := strings.TrimSpace(asString)
		if trimmed == "" {
			return nil
		}
		return &trimmed
	}
	// Numbers are kept verbatim so large decimal balances lose no precision.
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func sortWindows(windows []Window) {
	rank := map[string]int{KindShortTerm: 0, KindWeekly: 1, KindMonthly: 2, KindOther: 3}
	sort.SliceStable(windows, func(i, j int) bool {
		if windows[i].LimitID != windows[j].LimitID {
			// The top-level "codex" bucket leads.
			if windows[i].LimitID == "codex" {
				return true
			}
			if windows[j].LimitID == "codex" {
				return false
			}
			return windows[i].LimitID < windows[j].LimitID
		}
		return rank[windows[i].Kind] < rank[windows[j].Kind]
	})
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// FindWindow returns the window for a limit + kind, if present.
func FindWindow(windows []Window, limitID, kind string) *Window {
	for i := range windows {
		if windows[i].LimitID == limitID && windows[i].Kind == kind {
			return &windows[i]
		}
	}
	return nil
}

func decodeQuotaWindow(limitID, limitName, role string, wire wireWindow) Window {
	percent := *wire.UsedPercent
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}

	seconds := int64(0)
	switch {
	case wire.LimitWindowSecond != nil && *wire.LimitWindowSecond > 0:
		seconds = *wire.LimitWindowSecond
	case wire.WindowSeconds != nil && *wire.WindowSeconds > 0:
		seconds = *wire.WindowSeconds
	case wire.WindowMinutes != nil && *wire.WindowMinutes > 0:
		seconds = *wire.WindowMinutes * 60
	}

	limitReached := false
	if wire.LimitReached != nil {
		limitReached = *wire.LimitReached
	} else if percent >= 100 {
		limitReached = true
	}

	resetAt := int64(0)
	if wire.ResetAt != nil && *wire.ResetAt > 0 {
		resetAt = *wire.ResetAt * 1000
	}

	return Window{
		LimitID:       limitID,
		LimitName:     limitName,
		Role:          role,
		Kind:          ClassifyWindow(seconds),
		UsedPercent:   percent,
		ResetAt:       resetAt,
		WindowSeconds: seconds,
		LimitReached:  limitReached,
	}
}
