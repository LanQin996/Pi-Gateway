package quota

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"pi-gateway/internal/egress"
	"pi-gateway/internal/store"
)

// usageSample mirrors the real /wham/usage response shape.
const usageSample = `{
  "plan_type": "pro",
  "active_limit": "premium",
  "rate_limit": {
    "allowed": true,
    "limit_reached": false,
    "primary_window":   { "used_percent": 12.5, "reset_at": 1800000100, "limit_window_seconds": 18000 },
    "secondary_window": { "used_percent": 30.0, "reset_at": 1800604800, "limit_window_seconds": 604800 }
  },
  "code_review_rate_limit": {
    "allowed": true,
    "primary_window": { "used_percent": 80, "reset_at": 1900604800, "limit_window_seconds": 604800 }
  },
  "additional_rate_limits": [
    {
      "limit_name": "GPT-5.3-Codex-Spark",
      "metered_feature": "codex_bengalfox",
      "rate_limit": { "primary_window": { "used_percent": 0, "reset_at": 1900000000, "limit_window_seconds": 604800 } }
    }
  ],
  "credits": { "has_credits": true, "unlimited": false, "balance": "9007199254740993.1234567890", "overage_limit_reached": false },
  "spend_control": { "reached": false, "individual_limit": { "used_percent": 12, "reset_at": 1902592000 } }
}`

// TestParseUsageWindows verifies window extraction and 5h/7d classification.
func TestParseUsageWindows(t *testing.T) {
	report, err := ParseUsage([]byte(usageSample))
	if err != nil {
		t.Fatalf("ParseUsage: %v", err)
	}

	if report.PlanType != "pro" {
		t.Errorf("plan type = %q, want pro", report.PlanType)
	}
	if !report.Allowed {
		t.Error("allowed should be true")
	}

	// The primary bucket must expose both a 5h session and a 7d weekly window.
	session := FindWindow(report.Windows, "codex", KindShortTerm)
	if session == nil {
		t.Fatalf("no 5h window found; windows = %+v", report.Windows)
	}
	if session.UsedPercent != 12.5 {
		t.Errorf("session used percent = %v, want 12.5", session.UsedPercent)
	}
	// reset_at is reported in seconds and must be stored in milliseconds.
	if session.ResetAt != 1800000100*1000 {
		t.Errorf("session reset at = %d, want %d", session.ResetAt, int64(1800000100*1000))
	}
	if session.WindowSeconds != 18000 {
		t.Errorf("session window seconds = %d, want 18000", session.WindowSeconds)
	}
	if session.Role != RolePrimary {
		t.Errorf("session role = %q, want primary", session.Role)
	}

	weekly := FindWindow(report.Windows, "codex", KindWeekly)
	if weekly == nil {
		t.Fatalf("no 7d window found; windows = %+v", report.Windows)
	}
	if weekly.UsedPercent != 30 {
		t.Errorf("weekly used percent = %v, want 30", weekly.UsedPercent)
	}
	if weekly.Role != RoleSecondary {
		t.Errorf("weekly role = %q, want secondary", weekly.Role)
	}

	// Additional per-model limits must be surfaced under their own bucket.
	spark := FindWindow(report.Windows, "codex_bengalfox", KindWeekly)
	if spark == nil {
		t.Fatalf("additional limit bucket missing; windows = %+v", report.Windows)
	}
	if spark.LimitName != "GPT-5.3-Codex-Spark" {
		t.Errorf("additional limit name = %q", spark.LimitName)
	}

	// Code review has its own bucket.
	if FindWindow(report.Windows, "code_review", KindWeekly) == nil {
		t.Error("code review bucket missing")
	}
}

// TestParseUsageCredits verifies the credit balance keeps large decimals exact.
func TestParseUsageCredits(t *testing.T) {
	report, err := ParseUsage([]byte(usageSample))
	if err != nil {
		t.Fatalf("ParseUsage: %v", err)
	}
	if report.Credits == nil {
		t.Fatal("credits missing")
	}
	if !report.Credits.HasCredits {
		t.Error("has_credits should be true")
	}
	if report.Credits.Unlimited {
		t.Error("unlimited should be false")
	}
	if report.Credits.Balance == nil {
		t.Fatal("balance missing")
	}
	// The value must not be routed through a float, which would lose precision.
	if *report.Credits.Balance != "9007199254740993.1234567890" {
		t.Errorf("balance = %q, want the exact string", *report.Credits.Balance)
	}
	if report.SpendControl == nil || report.SpendControl.Reached {
		t.Error("spend control should be present and not reached")
	}
}

// TestParseUsageHandlesNulls verifies optional sections may be null.
func TestParseUsageHandlesNulls(t *testing.T) {
	body := `{
		"plan_type": "plus",
		"rate_limit": {
			"primary_window": { "used_percent": 100, "reset_at": 1800000100, "limit_window_seconds": 18000 },
			"secondary_window": null
		},
		"additional_rate_limits": null,
		"credits": null
	}`
	report, err := ParseUsage([]byte(body))
	if err != nil {
		t.Fatalf("ParseUsage: %v", err)
	}
	session := FindWindow(report.Windows, "codex", KindShortTerm)
	if session == nil {
		t.Fatal("session window missing")
	}
	// A fully used window is a reached limit even without the explicit flag.
	if !session.LimitReached {
		t.Error("a 100% window should count as limit reached")
	}
	if FindWindow(report.Windows, "codex", KindWeekly) != nil {
		t.Error("a null secondary window must not produce a window")
	}
	if report.Credits != nil {
		t.Error("null credits must stay nil")
	}
}

// TestClassifyWindow verifies the duration bands.
func TestClassifyWindow(t *testing.T) {
	cases := []struct {
		seconds int64
		want    string
	}{
		{18000, KindShortTerm}, // 5 hours
		{17100, KindShortTerm}, // lower bound
		{18900, KindShortTerm}, // upper bound
		{604800, KindWeekly},   // 7 days
		{574560, KindWeekly},   // lower bound
		{635040, KindWeekly},   // upper bound
		{2592000, KindMonthly}, // 30 days
		{3600, KindOther},      // 1 hour is not a known band
		{0, KindOther},
	}
	for _, tc := range cases {
		if got := ClassifyWindow(tc.seconds); got != tc.want {
			t.Errorf("ClassifyWindow(%d) = %q, want %q", tc.seconds, got, tc.want)
		}
	}
}

// TestAccountEndpointURL verifies the wham vs api/codex prefix rule.
func TestAccountEndpointURL(t *testing.T) {
	cases := []struct {
		base     string
		resource string
		want     string
	}{
		{"https://chatgpt.com/backend-api", "usage", "https://chatgpt.com/backend-api/wham/usage"},
		{"https://chatgpt.com/backend-api/", "rate-limit-reset-credits", "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"},
		{"https://example.com", "usage", "https://example.com/api/codex/usage"},
		{"https://example.com/backend-api/v1", "usage", "https://example.com/backend-api/v1/wham/usage"},
		{"https://chatgpt.com/backend-api/codex", "usage", "https://chatgpt.com/backend-api/wham/usage"},
		{"https://chatgpt.com/backend-api/codex/responses/", "usage", "https://chatgpt.com/backend-api/wham/usage"},
		{"https://gateway.example/backend-api/codex/responses/?route=/blue/", "usage", "https://gateway.example/backend-api/wham/usage?route=/blue/"},
		{"https://gateway.example/tenant%2Fone/backend-api/?route=blue", "usage", "https://gateway.example/tenant%2Fone/backend-api/wham/usage?route=blue"},
		{"https://gateway.example/?route=blue", "usage", "https://gateway.example/api/codex/usage?route=blue"},
	}
	for _, tc := range cases {
		if got := AccountEndpointURL(tc.base, tc.resource); got != tc.want {
			t.Errorf("AccountEndpointURL(%q, %q) = %q, want %q", tc.base, tc.resource, got, tc.want)
		}
	}
}

// TestParseBalance verifies balance coercion for string, number and null.
func TestParseBalance(t *testing.T) {
	cases := []struct {
		raw  string
		want *string
	}{
		{`null`, nil},
		{`""`, nil},
		{`"12.5"`, ptr("12.5")},
		{`42`, ptr("42")},
		{`1234.5678`, ptr("1234.5678")},
	}
	for _, tc := range cases {
		got := parseBalance(json.RawMessage(tc.raw))
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("parseBalance(%s) = %q, want nil", tc.raw, *got)
		case tc.want != nil && got == nil:
			t.Errorf("parseBalance(%s) = nil, want %q", tc.raw, *tc.want)
		case tc.want != nil && got != nil && *tc.want != *got:
			t.Errorf("parseBalance(%s) = %q, want %q", tc.raw, *got, *tc.want)
		}
	}
}

// TestConsumeErrorMessages verifies the reset-credit result codes are explained.
func TestConsumeErrorMessages(t *testing.T) {
	cases := map[string]string{
		CodeNoCredit:        "no rate-limit reset credit is available",
		CodeNothingToReset:  "no rate-limit window needs resetting",
		CodeAlreadyRedeemed: "this reset request was already redeemed",
	}
	for code, want := range cases {
		err := &ConsumeError{StatusCode: 409, Code: code}
		if err.Error() != want {
			t.Errorf("code %q message = %q, want %q", code, err.Error(), want)
		}
	}
}

func TestNewDefaultsToCodexBackend(t *testing.T) {
	factory := egress.NewFactory(egress.Options{})
	client := New(Config{}, factory, nil)
	if client.cfg.BaseURL != CodexBaseURL || client.cfg.OfficialBaseURL != CodexBaseURL {
		t.Fatalf("quota defaults = %q / %q, want %q", client.cfg.BaseURL, client.cfg.OfficialBaseURL, CodexBaseURL)
	}
	for _, modelBase := range []string{
		"https://api.openai.com/v1",
		"https://api.openai.com/v1/responses",
		"https://api.openai.com/",
		"https://api.openai.com",
		" https://API.OPENAI.COM/v1/?route=blue ",
	} {
		modelClient := New(Config{BaseURL: modelBase, OfficialBaseURL: modelBase}, factory, nil)
		if modelClient.cfg.BaseURL != CodexBaseURL || modelClient.cfg.OfficialBaseURL != CodexBaseURL {
			t.Fatalf("model API base %q must not receive Codex quota credentials", modelBase)
		}
	}
}

func TestQuotaFallsBackOnUnauthorizedAndUsesCodexAccountID(t *testing.T) {
	var firstPath string
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstPath = r.URL.Path
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("chatgpt-account-id"); got != "codex-account" {
			t.Errorf("chatgpt-account-id = %q, want codex-account", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer codex-token" {
			t.Errorf("authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"allowed":true}}`))
	}))
	defer second.Close()

	client := New(Config{BaseURL: first.URL, OfficialBaseURL: second.URL}, egress.NewFactory(egress.Options{}), nil)
	report, err := client.FetchUsage(context.Background(), &store.Account{CodexAccountID: "codex-account"}, "codex-token")
	if err != nil {
		t.Fatalf("FetchUsage: %v", err)
	}
	if report.PlanType != "pro" {
		t.Fatalf("plan_type = %q, want pro", report.PlanType)
	}
	if firstPath != "/api/codex/usage" {
		t.Fatalf("first path = %q, want /api/codex/usage", firstPath)
	}
}

func TestQuotaFallsBackOnForbiddenAndServerError(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer first.Close()
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("chatgpt-account-id"); got != "codex-account" {
					t.Errorf("chatgpt-account-id = %q, want codex-account", got)
				}
				_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"allowed":true}}`))
			}))
			defer second.Close()

			client := New(Config{BaseURL: first.URL, OfficialBaseURL: second.URL}, egress.NewFactory(egress.Options{}), nil)
			report, err := client.FetchUsage(context.Background(), &store.Account{CodexAccountID: "codex-account"}, "codex-token")
			if err != nil {
				t.Fatalf("FetchUsage: %v", err)
			}
			if report.PlanType != "pro" {
				t.Fatalf("plan_type = %q, want pro", report.PlanType)
			}
		})
	}
}

func TestQuotaDoesNotRetryResetOnServerError(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`upstream unavailable`))
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("reset-credit POST must not retry a 5xx response")
	}))
	defer second.Close()

	client := New(Config{BaseURL: first.URL, OfficialBaseURL: second.URL}, egress.NewFactory(egress.Options{}), nil)
	if _, err := client.ConsumeResetCredit(context.Background(), &store.Account{CodexAccountID: "codex-account"}, "codex-token", ""); err == nil {
		t.Fatal("expected reset-credit 5xx error")
	}
}

func TestQuotaRejectsMissingCodexAccountID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("quota request should not be sent without Codex account id")
	}))
	defer server.Close()
	client := New(Config{BaseURL: server.URL}, egress.NewFactory(egress.Options{}), nil)
	if _, err := client.FetchUsage(context.Background(), &store.Account{AccountID: "primary"}, "token"); err == nil {
		t.Fatal("expected missing Codex account id error")
	}
}

// TestQuotaTransportErrorFallback simulates a request reaching the backend, but
// losing its response either before headers or partway through the response body.
func TestQuotaTransportErrorFallback(t *testing.T) {
	for _, failure := range []string{"connection_closed", "truncated_response"} {
		t.Run(failure, func(t *testing.T) { quotaTransportErrorFallbackCase1(t, failure) })
	}
}

// Explicit endpoint-selection refusals retain the existing POST fallback policy:
// unlike a lost response or 5xx, they normally indicate no operation was performed.
func TestQuotaResetFallsBackOnEndpointRejection(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var firstCalls, secondCalls atomic.Int32
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				firstCalls.Add(1)
				reportTestFailure(t, r.Method != http.MethodPost, func() {
					t.Errorf("first method = %q, want POST", r.Method)
				})
				w.WriteHeader(status)
			}))
			defer first.Close()
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secondCalls.Add(1)
				reportTestFailure(t, r.Method != http.MethodPost, func() {
					t.Errorf("second method = %q, want POST", r.Method)
				})
				_, _ = w.Write([]byte(`{"code":"reset"}`))
			}))
			defer second.Close()

			client := New(Config{BaseURL: first.URL, OfficialBaseURL: second.URL}, egress.NewFactory(egress.Options{}), nil)
			result, err := client.ConsumeResetCredit(context.Background(), &store.Account{CodexAccountID: "codex-account"}, "codex-token", "credit-1")
			reportTestFailure(t, err != nil || result == nil || result.Code != CodeReset, func() {
				t.Errorf("ConsumeResetCredit = (%+v, %v), want fallback reset result", result, err)
			})
			{
				got := firstCalls.Load()
				reportTestFailure(t, got != 1, func() {
					t.Errorf("first endpoint received %d POSTs, want 1", got)
				})
			}
			{
				got := secondCalls.Load()
				reportTestFailure(t, got != 1, func() {
					t.Errorf("second endpoint received %d POSTs, want 1", got)
				})
			}
		})
	}
}

func ptr(v string) *string { return &v }

func quotaTransportErrorFallbackCase1(t *testing.T, failure string) {
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		t.Run(method, func(t *testing.T) { quotaTransportErrorFallbackCase1Case1(t, failure, method) })
	}
}

func quotaTransportErrorFallbackCase1Case1(t *testing.T, failure, method string) {
	var firstCalls, secondCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		reportTestFailure(t, r.Method != method, func() {
			t.Errorf("first method = %q, want %q", r.Method, method)
		})
		// Read the complete request before dropping the response, as if
		// the backend had already performed the requested operation.
		{
			_, err := io.Copy(io.Discard, r.Body)
			reportTestFailure(t, err != nil, func() {
				t.Errorf("read request: %v", err)
			})
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		if failure == "truncated_response" {
			_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n{")
			{
				err := rw.Flush()
				reportTestFailure(t, err != nil, func() {
					t.Errorf("flush partial response: %v", err)
				})
			}
		}
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		reportTestFailure(t, r.Method != method, func() {
			t.Errorf("second method = %q, want %q", r.Method, method)
		})
		if method == http.MethodPost {
			_, _ = w.Write([]byte(`{"code":"reset"}`))
		} else {
			_, _ = w.Write([]byte(usageSample))
		}
	}))
	defer second.Close()

	client := New(Config{BaseURL: first.URL, OfficialBaseURL: second.URL}, egress.NewFactory(egress.Options{}), nil)
	account := &store.Account{CodexAccountID: "codex-account"}
	if method == http.MethodPost {
		result, err := client.ConsumeResetCredit(context.Background(), account, "codex-token", "credit-1")
		reportTestFailure(t, err == nil || result != nil, func() {
			t.Errorf("ConsumeResetCredit = (%+v, %v), want nil result and transport/read error", result, err)
		})
		{
			got := secondCalls.Load()
			reportTestFailure(t, got != 0, func() {
				t.Errorf("second endpoint received %d POSTs, want 0", got)
			})
		}
	} else {
		report, err := client.FetchUsage(context.Background(), account, "codex-token")
		reportTestFailure(t, err != nil || report == nil || report.PlanType != "pro", func() {
			t.Errorf("FetchUsage = (%+v, %v), want fallback report", report, err)
		})
		{
			got := secondCalls.Load()
			reportTestFailure(t, got != 1, func() {
				t.Errorf("second endpoint received %d GETs, want 1", got)
			})
		}
	}
	{
		got := firstCalls.Load()
		reportTestFailure(t, got != 1, func() {
			t.Errorf("first endpoint received %d requests, want 1", got)
		})
	}
}
