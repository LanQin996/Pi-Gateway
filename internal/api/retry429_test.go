package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"pi-gateway/internal/store"
	"pi-gateway/internal/upstream"
)

func Test429AccountSwitchAndCooldown(t *testing.T) {
	for _, tc := range []struct {
		name                                         string
		global                                       bool
		group                                        string
		stream, eventError, priorOutput, exhaust, ws bool
		longCooldown                                 bool
		attempts, wantCalls, wantStatus              int
	}{
		{name: "default enabled", global: true, attempts: 2, wantCalls: 2, wantStatus: 200},
		{name: "websocket", global: true, ws: true, attempts: 2, wantCalls: 2, wantStatus: 200},
		{name: "stream", global: true, stream: true, attempts: 2, wantCalls: 2, wantStatus: 200},
		{name: "event error", global: true, stream: true, eventError: true, attempts: 2, wantCalls: 2, wantStatus: 200},
		{name: "global disabled", stream: true, eventError: true, attempts: 2, wantCalls: 2, wantStatus: 200},
		{name: "group enables", group: "enabled", attempts: 2, wantCalls: 2, wantStatus: 200},
		{name: "group disables", global: true, group: "disabled", attempts: 2, wantCalls: 2, wantStatus: 200},
		{name: "disabled attempt limit", attempts: 1, wantCalls: 1, wantStatus: 429},
		{name: "disabled exhausted", exhaust: true, attempts: 2, wantCalls: 2, wantStatus: 429},
		{name: "disabled websocket", ws: true, attempts: 2, wantCalls: 2, wantStatus: 200},
		{name: "disabled cooldown exceeds wait budget", longCooldown: true, attempts: 2, wantCalls: 1, wantStatus: 429},
		{name: "disabled already output", stream: true, priorOutput: true, eventError: true, attempts: 2, wantCalls: 1, wantStatus: 200},
		{name: "group inherits", global: true, group: "inherit", attempts: 2, wantCalls: 2, wantStatus: 200},
		{name: "attempt limit", global: true, attempts: 1, wantCalls: 1, wantStatus: 429},
		{name: "all limited", global: true, exhaust: true, attempts: 4, wantCalls: 2, wantStatus: 429},
		{name: "already output", global: true, stream: true, priorOutput: true, eventError: true, attempts: 2, wantCalls: 1, wantStatus: 200},
	} {
		t.Run(tc.name, func(t *testing.T) { run429AccountSwitchAndCooldownCase(t, &tc) })
	}
}

func Test429SameAccountWaitCancellation(t *testing.T) {
	for _, eventError := range []bool{false, true} {
		name := "HTTP error"
		if eventError {
			name = "SSE error"
		}
		t.Run(name, func(t *testing.T) { run429SameAccountWaitCancellationCase(t, &eventError) })
	}
}
func Test429SameAccountSharedWaitBudget(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After-Ms", "400")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"error\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"limited\"}}\n\n")
	}), "sse")
	rt := h.dataPlane.settings.Get()
	rt.SwitchOn429, rt.MaxAttempts = false, 10
	{
		err := h.dataPlane.settings.Set(context.Background(), rt)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	zero := 0
	{
		err := h.store.PatchAccountManagementFields(context.Background(), h.accountID, store.AccountManagementPatch{Cooldown429Seconds: &zero})
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	r.Header.Set("Authorization", "Bearer "+h.key)
	p, apiErr := h.dataPlane.prepare(context.Background(), r, []byte(`{"model":"test","input":[]}`), "sse")
	reportTestFailure(t, apiErr != nil, func() {
		t.Fatal(apiErr)
	})
	defer p.Release()
	req, err := h.dataPlane.newUpstreamRequest(context.Background(), p)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var events int
	before := time.Now()
	_, err = h.dataPlane.streamWith429RetryWindow(ctx, p, req, func(event *upstream.Event) error {
		events++
		reportTestFailure(t, upstream.FailureFromEvent(event) == nil, func() {
			t.Errorf("unexpected event: %+v", event)
		})
		return nil
	}, 600*time.Millisecond)
	reportTestFailure(t, err == nil || errorFromUpstream(err).Status != http.StatusTooManyRequests, func() {
		t.Fatalf("exhausted wait budget did not preserve the upstream 429: %v", err)
	})
	{
		elapsed := time.Since(before)
		reportTestFailure(t, elapsed < 600*time.Millisecond || elapsed > time.Second, func() {
			t.Fatalf("retry waits reset the shared budget: %v", elapsed)
		})
	}
	reportTestFailure(t, calls.Load() != 2 || events != 1, func() {
		t.Fatalf("calls=%d events=%d; want one replay and only the final failure", calls.Load(), events)
	})
	reportTestFailure(t, p.ResponseHeaders.Get("Retry-After-Ms") != "400" || h.dataPlane.accounts.Inflight(h.accountID) != 0, func() {
		t.Fatal("wait timeout lost retry metadata or leaked a slot")
	})
}
func TestRetryAfterDuration(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		h    http.Header
		want time.Duration
	}{
		{http.Header{"Retry-After": {"7"}, "Retry-After-Ms": {"8000"}}, 8 * time.Second},
		{http.Header{"Retry-After": {now.Add(time.Minute).Format(http.TimeFormat)}}, time.Minute},
		{http.Header{"Retry-After": {"invalid"}, "Retry-After-Ms": {"-1"}}, 0},
	} {
		{
			got := retryAfterDuration(tc.h, now)
			reportTestFailure(t, got != tc.want, func() {
				t.Fatalf("delay=%v want=%v", got, tc.want)
			})
		}
	}
}

func Test429GroupOverrideScopeAndConflict(t *testing.T) {
	h := newHarness(t, sseUpstream(&upstreamCapture{}), "sse")
	ctx := context.Background()
	enabled := &store.AccountGroup{Name: "enable", Enabled: true, AccountIDs: []int64{h.accountID}, SwitchOn429: "enabled"}
	disabled := &store.AccountGroup{Name: "disable", Enabled: true, AccountIDs: []int64{h.accountID}, SwitchOn429: "disabled"}
	for _, g := range []*store.AccountGroup{enabled, disabled} {
		{
			err := h.store.CreateAccountGroup(ctx, g)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
	}
	a, err := h.store.GetAccount(ctx, h.accountID)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	rt := h.dataPlane.settings.Get()
	rt.SwitchOn429 = false
	{
		err := h.dataPlane.settings.Set(ctx, rt)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	for _, tc := range []struct {
		ids  []int64
		want bool
	}{
		{nil, false},
		{[]int64{enabled.ID}, true},
		{[]int64{disabled.ID}, false},
		{[]int64{enabled.ID, disabled.ID}, false},
	} {
		p := &prepared{Account: a, Key: &store.APIKey{GroupIDs: tc.ids}}
		{
			got := h.dataPlane.switchOn429(ctx, p)
			reportTestFailure(t, got != tc.want, func() {
				t.Fatalf("scope=%v switch=%v want=%v", tc.ids, got, tc.want)
			})
		}
	}
	// A disabled group neither authorizes nor imposes an override.
	off := false
	{
		err := h.store.PatchAccountGroup(ctx, disabled.ID, store.AccountGroupPatch{Enabled: &off})
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, !h.dataPlane.switchOn429(ctx, &prepared{Account: a, Key: &store.APIKey{}}), func() {
		t.Fatal("disabled group imposed override")
	})
}

// run429AccountSwitchAndCooldownCase exercises one isolated test phase with the original shared state.
func run429AccountSwitchAndCooldownCase(t *testing.T, capturedTc *struct {
	name         string
	global       bool
	group        string
	stream       bool
	eventError   bool
	priorOutput  bool
	exhaust      bool
	ws           bool
	longCooldown bool
	attempts     int
	wantCalls    int
	wantStatus   int
}) {
	t.Helper()

	t.Parallel()
	waitSame := (*capturedTc).group == "disabled" || (!(*capturedTc).global && (*capturedTc).group != "enabled")
	var mu sync.Mutex
	var called []string
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serve429AccountSwitchAndCooldownRequest(w, r, &capturedTc, &waitSame, &mu, &called)
	}), "sse")
	ctx := context.Background()
	a := &store.Account{Name: "second", AccountID: "acct-second", AccessToken: fakeJWT(t, "acct-second"), ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Enabled: true, Weight: 1, Concurrency: 3, Status: store.AccountStatusReady}
	{
		err := h.store.CreateAccount(ctx, a)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	// Switching keeps the existing floor; same-account waits use upstream
	// Retry-After directly when the administrator sets the floor to zero.
	seconds := 120
	if waitSame {
		seconds = 0
	}
	for _, id := range []int64{h.accountID, a.ID} {
		{
			err := h.store.PatchAccountManagementFields(ctx, id, store.AccountManagementPatch{Cooldown429Seconds: &seconds})
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
	}
	if (*capturedTc).group != "" {
		g := &store.AccountGroup{Name: "policy", Enabled: true, AccountIDs: []int64{h.accountID, a.ID}, SwitchOn429: (*capturedTc).group}
		{
			err := h.store.CreateAccountGroup(ctx, g)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
	}
	rt := h.dataPlane.settings.Get()
	rt.SwitchOn429 = (*capturedTc).global
	rt.MaxAttempts = (*capturedTc).attempts
	{
		err := h.dataPlane.settings.Set(ctx, rt)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	h.dataPlane.accounts.SetSettings(rt)
	body := `{"model":"test","input":[],"stream":false}`
	if (*capturedTc).stream {
		body = `{"model":"test","input":[],"stream":true}`
	}
	req, _ := http.NewRequest("POST", h.server.URL+"/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+h.key)
	before := time.Now()
	// Bound the complete request separately from the retry wait budget.
	// This catches accidentally waiting out the 90/120-second cooldown
	// while allowing preparation and persistence under race instrumentation.
	requestTimeout := 2 * sameAccount429WaitTimeout
	if (*capturedTc).ws {
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.server.URL, "http")+"/v1/responses", http.Header{"Authorization": {"Bearer " + h.key}})
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(requestTimeout))
		{
			err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "test", "input": []any{}})
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		_, raw, err := conn.ReadMessage()
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
		reportTestFailure(t, !strings.Contains(string(raw), "response.completed"), func() {
			t.Fatalf("ws output=%s", raw)
		})
		mu.Lock()
		ids := append([]string(nil), called...)
		mu.Unlock()
		reportTestFailure(t, len(ids) != 2 || (ids[0] == ids[1]) != !(*capturedTc).global, func() {
			t.Fatalf("ws attempts=%v switch=%v", ids, (*capturedTc).global)
		})
		reportTestFailure(t, !(*capturedTc).global && time.Since(before) < 200*time.Millisecond, func() {
			t.Fatal("same-account websocket retry ignored the short upstream cooldown")
		})
		return
	}
	client := &http.Client{Timeout: requestTimeout}
	resp, err := client.Do(req)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	raw, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	reportTestFailure(t, readErr != nil, func() {
		t.Fatal(readErr)
	})
	reportTestFailure(t, resp.StatusCode != (*capturedTc).wantStatus, func() {
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, (*capturedTc).wantStatus, raw)
	})
	mu.Lock()
	ids := append([]string(nil), called...)
	mu.Unlock()
	reportTestFailure(t, len(ids) != (*capturedTc).wantCalls, func() {
		t.Fatalf("calls=%v want=%d", ids, (*capturedTc).wantCalls)
	})
	func() { verify429AccountSwitchAndCooldown(&t, &capturedTc, &before, &ids) }()
	if (*capturedTc).longCooldown {
		// This includes preparation, upstream IO and usage persistence,
		// not just the retry wait. Verify the wait is not cut short here;
		// Test429SameAccountSharedWaitBudget checks budget exhaustion at
		// the retry boundary without HTTP response/finalization overhead.
		{
			elapsed := time.Since(before)
			reportTestFailure(t, elapsed < sameAccount429WaitTimeout, func() {
				t.Fatalf("long cooldown returned before the retry wait budget: %v", elapsed)
			})
		}
		reportTestFailure(t, resp.Header.Get("Retry-After") != "90", func() {
			t.Fatalf("upstream cooldown header lost: %v", resp.Header)
		})
	}
	reportTestFailure(t, (*capturedTc).wantStatus == 200 && !(*capturedTc).priorOutput && strings.Contains(string(raw), "limited"), func() {
		t.Fatalf("failed attempt leaked: %s", raw)
	})
	all, err := h.store.ListAccounts(ctx)
	var first *store.Account
	func() { verify429AccountSwitchAndCooldown2(&ids, &all, &first) }()
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	reportTestFailure(t, first == nil, func() {
		t.Fatal("attempt account not found")
	})
	cooldown := 120 * time.Second
	if waitSame {
		cooldown = 200 * time.Millisecond
		if (*capturedTc).longCooldown {
			cooldown = 90 * time.Second
		}
	}
	reportTestFailure(t, first.CooldownUntil < before.Add(cooldown).UnixMilli(), func() {
		t.Fatalf("cooldown not recorded: %+v", first)
	})
	reportTestFailure(t, h.dataPlane.accounts.Inflight(h.accountID) != 0 || h.dataPlane.accounts.Inflight(a.ID) != 0, func() {
		t.Fatal("account slots leaked")
	})
}

// run429SameAccountWaitCancellationCase exercises one isolated test phase with the original shared state.
func run429SameAccountWaitCancellationCase(t *testing.T, capturedEventError *bool) {
	t.Helper()

	t.Parallel()
	var calls atomic.Int32
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if *capturedEventError {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"error\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"limited\"}}\n\n")
		} else {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"limited"}}`)
		}
	}), "sse")
	rt := h.dataPlane.settings.Get()
	rt.SwitchOn429, rt.MaxAttempts = false, 2
	{
		err := h.dataPlane.settings.Set(context.Background(), rt)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	r.Header.Set("Authorization", "Bearer "+h.key)
	p, apiErr := h.dataPlane.prepare(context.Background(), r, []byte(`{"model":"test","input":[]}`), "sse")
	reportTestFailure(t, apiErr != nil, func() {
		t.Fatal(apiErr)
	})
	defer p.Release()
	req, err := h.dataPlane.newUpstreamRequest(context.Background(), p)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events atomic.Int32
	done := make(chan error, 1)
	go func() {
		_, err := h.dataPlane.streamWith429Retry(ctx, p, req, func(*upstream.Event) error {
			events.Add(1)
			return nil
		})
		done <- err
	}()
	// Observe the persisted 429 to know the request has entered its wait.
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			t.Fatalf("request returned instead of waiting: %v", err)
		case <-deadline.C:
			t.Fatal("request did not enter the retry wait")
		case <-ticker.C:
			a, err := h.store.GetAccount(context.Background(), h.accountID)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			if a.ConsecutiveFailures == 0 || h.dataPlane.accounts.Inflight(h.accountID) != 0 {
				continue
			}
		}
		break
	}
	reportTestFailure(t, events.Load() != 0 || calls.Load() != 1 || h.dataPlane.accounts.Inflight(h.accountID) != 0, func() {
		t.Fatalf("invalid waiting state: events=%d calls=%d inflight=%d", events.Load(), calls.Load(), h.dataPlane.accounts.Inflight(h.accountID))
	})
	cancel()
	select {
	case err := <-done:
		reportTestFailure(t, !errors.Is(err, context.Canceled), func() {
			t.Fatalf("wait cancellation=%v", err)
		})
	case <-time.After(time.Second):
		t.Fatal("retry wait ignored cancellation")
	}
	p.Release()
	reportTestFailure(t, events.Load() != 0 || calls.Load() != 1 || h.dataPlane.accounts.Inflight(h.accountID) != 0, func() {
		t.Fatal("canceled retry leaked a failure event, retried, or retained the account slot")
	})
}

// serve429AccountSwitchAndCooldownRequest exercises one isolated test phase with the original shared state.
func serve429AccountSwitchAndCooldownRequest(w http.ResponseWriter, r *http.Request, capturedCapturedTc **struct {
	name         string
	global       bool
	group        string
	stream       bool
	eventError   bool
	priorOutput  bool
	exhaust      bool
	ws           bool
	longCooldown bool
	attempts     int
	wantCalls    int
	wantStatus   int
}, capturedWaitSame *bool, capturedMu *sync.Mutex, capturedCalled *[]string) {
	(*capturedMu).Lock()
	(*capturedCalled) = append((*capturedCalled), r.Header.Get("Authorization"))
	n := len((*capturedCalled))
	(*capturedMu).Unlock()
	if n == 1 || (*(*capturedCapturedTc)).exhaust {
		if (*capturedWaitSame) && !(*(*capturedCapturedTc)).longCooldown {
			w.Header().Set("Retry-After-Ms", "200")
		} else {
			w.Header().Set("Retry-After", "90")
		}
		if (*(*capturedCapturedTc)).eventError {
			w.Header().Set("Content-Type", "text/event-stream")
			if (*(*capturedCapturedTc)).priorOutput {
				_, _ = io.WriteString(w, deltaSSE)
			}
			_, _ = io.WriteString(w, "data: {\"type\":\"error\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"limited\"}}\n\n")
		} else {
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"limited"}}`)
		}
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, terminalSSE)
}

// verify429AccountSwitchAndCooldown exercises one isolated test phase with the original shared state.
func verify429AccountSwitchAndCooldown(capturedT **testing.T, capturedCapturedTc **struct {
	name         string
	global       bool
	group        string
	stream       bool
	eventError   bool
	priorOutput  bool
	exhaust      bool
	ws           bool
	longCooldown bool
	attempts     int
	wantCalls    int
	wantStatus   int
}, capturedBefore *time.Time, capturedIds *[]string) {
	(*capturedT).Helper()

	if len((*capturedIds)) > 1 {
		wantSame := (*(*capturedCapturedTc)).group == "disabled" || (!(*(*capturedCapturedTc)).global && (*(*capturedCapturedTc)).group != "enabled")
		{
			same := (*capturedIds)[0] == (*capturedIds)[1]
			reportTestFailure((*capturedT), same != wantSame, func() {
				(*capturedT).Fatalf("attempt accounts=%v same=%v want=%v", (*capturedIds), same, wantSame)
			})
		}
		reportTestFailure((*capturedT), wantSame && time.Since((*capturedBefore)) < 200*time.Millisecond, func() {
			(*capturedT).Fatal("same-account retry ignored the short upstream cooldown")
		})
	}
}

// verify429AccountSwitchAndCooldown2 exercises one isolated test phase with the original shared state.
func verify429AccountSwitchAndCooldown2(capturedIds *[]string, capturedAll *[]*store.Account, capturedFirst **store.Account) {
	for _, a := range *capturedAll {
		if "Bearer "+a.AccessToken == (*capturedIds)[0] {
			(*capturedFirst) = a
		}
	}
}
