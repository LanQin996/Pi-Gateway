package accounts

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pi-gateway/internal/config"
	"pi-gateway/internal/egress"
)

type codexVersionTestClock struct{ nanos atomic.Int64 }

func (c *codexVersionTestClock) now() time.Time          { return time.Unix(0, c.nanos.Load()) }
func (c *codexVersionTestClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }

func newCodexVersionTestManager(t *testing.T, proxy string, roundTrip catalogRoundTrip) (*Manager, *codexVersionTestClock) {
	t.Helper()
	clock := &codexVersionTestClock{}
	clock.nanos.Store(time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	factory := egress.NewFactory(egress.Options{})
	// Even accidentally selecting another HTTP client cannot reach the network.
	factory.BaseDialer = func(context.Context, string, string) (net.Conn, error) {
		t.Error("unexpected real network dial in release unit test")
		return nil, errors.New("network disabled in release test")
	}
	client, err := factory.HTTPClient(proxy)
	if err != nil {
		t.Fatal(err)
	}
	client.Transport = roundTrip
	return &Manager{factory: factory, now: clock.now, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, clock
}

func codexVersionTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func codexVersionTestRelease(version string) *http.Response {
	return codexVersionTestResponse(http.StatusOK, `{"tag_name":"rust-v`+version+`"}`)
}

func TestParseCodexReleaseVersion(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"rust_tag", `{"tag_name":"rust-v0.161.0"}`, "0.161.0"},
		{"v_tag", `{"tag_name":"v1.2.3"}`, "1.2.3"},
		{"bare_tag", `{"tag_name":"1.2.3"}`, "1.2.3"},
		{"zero", `{"tag_name":"rust-v0.0.0"}`, "0.0.0"},
		{"name", `{"name":"0.161.0"}`, "0.161.0"},
		{"name_v", `{"name":"v0.161.0"}`, "0.161.0"},
		{"name_rust_v", `{"name":"rust-v0.161.0"}`, "0.161.0"},
		{"empty_tag_name", `{"tag_name":"","name":"0.161.0"}`, "0.161.0"},
		{"whitespace_tag_name", `{"tag_name":" \t","name":"  v0.161.0  "}`, "0.161.0"},
		{"trim_tag", `{"tag_name":"  rust-v0.161.0\n"}`, "0.161.0"},
		{"tag_wins", `{"tag_name":"rust-v0.161.0","name":"0.999.0"}`, "0.161.0"},
		{"stable_flags", `{"tag_name":"v0.161.0","draft":false,"prerelease":false}`, "0.161.0"},
		{"unknown_fields", `{"tag_name":"v0.161.0","assets":[],"body":"ignored"}`, "0.161.0"},
		{"draft", `{"tag_name":"v0.161.0","draft":true}`, ""},
		{"prerelease", `{"tag_name":"v0.161.0","prerelease":true}`, ""},
		{"alpha_tag_not_rescued_by_name", `{"tag_name":"rust-v0.162.0-alpha.1","name":"0.161.0"}`, ""},
		{"invalid_tag_not_rescued_by_name", `{"tag_name":"latest","name":"0.161.0"}`, ""},
		{"malformed_json", `{"tag_name":`, ""},
		{"empty", ``, ""},
		{"null", `null`, ""},
		{"array", `[{"tag_name":"v0.161.0"}]`, ""},
		{"missing_version", `{}`, ""},
		{"numeric_tag", `{"tag_name":161,"name":"0.161.0"}`, ""},
		{"wrong_flag_type", `{"tag_name":"0.161.0","draft":"false"}`, ""},
		{"trailing_json", `{"tag_name":"v0.161.0"}{}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCodexReleaseVersion([]byte(tc.body))
			if tc.want == "" {
				requireDataTruef(t, !(err == nil || got != ""), "invalid release accepted: version=%q err=%v", got, err)
				return
			}
			reportTestFailure(t, err != nil || got != tc.want, func() {
				t.Fatalf("version=%q err=%v, want %q", got, err, tc.want)
			})
		})
	}
	for _, version := range []string{
		"1.2", "1.2.3.4", "1..3", ".2.3", "1.2.", "01.2.3", "1.02.3", "1.2.03",
		"-1.2.3", "+1.2.3", "1.2.-3", "1.2.3-alpha.1", "1.2.3-rc.1", "1.2.3+build",
		"1. 2.3", "１.2.3", "Codex 1.2.3", "vv1.2.3", "RUST-v1.2.3",
		"18446744073709551616.0.0", "0.18446744073709551616.0", "0.0.18446744073709551616",
		strings.Repeat("1", 65) + ".2.3",
	} {
		t.Run("invalid_version_"+version, func(t *testing.T) {
			raw, err := json.Marshal(map[string]string{"tag_name": version, "name": "0.161.0"})
			requireDataNoError(t, err)
			{
				got, err := parseCodexReleaseVersion(raw)
				requireDataTruef(t, !(err == nil || got != ""), "invalid tag %q accepted or rescued by name: %q %v", version, got, err)
			}
		})
	}
}

func TestCompareCodexVersionsNumeric(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"0.160.0", "0.160.0", 0},
		{"0.160.10", "0.160.9", 1},
		{"0.99.99", "0.160.0", -1},
		{"1.10.0", "1.9.99", 1},
		{"10.0.0", "9.999.999", 1},
		{"0.999.999", "1.0.0", -1},
		{"9007199254740993.0.0", "9007199254740992.0.0", 1},
		{"18446744073709551615.0.0", "18446744073709551614.0.0", 1},
	} {
		if got := compareCodexVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compare(%q, %q)=%d, want %d", tc.a, tc.b, got, tc.want)
		}
		if got := compareCodexVersions(tc.b, tc.a); got != -tc.want {
			t.Errorf("reverse compare(%q, %q)=%d, want %d", tc.b, tc.a, got, -tc.want)
		}
	}
}

type codexVersionTestBody struct {
	io.Reader
	closed bool
	read   int
}

func (b *codexVersionTestBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}
func (b *codexVersionTestBody) Close() error { b.closed = true; return nil }

type codexVersionTestReadError struct{}

func (codexVersionTestReadError) Read([]byte) (int, error) {
	return 0, errors.New("raw-read-error response-body-secret")
}

func TestFetchLatestCodexClientVersionResponses(t *testing.T) {
	const limit = 256 << 10
	const valid = `{"tag_name":"rust-v0.161.0"}`
	const padded = `{"tag_name":"rust-v0.161.0","padding":""}`
	atLimit := strings.Replace(padded, `"padding":""`, `"padding":"`+strings.Repeat("x", limit-len(padded))+`"`, 1)
	for _, tc := range []struct {
		name, body, encoding, wantError string
		status                          int
		corrupt, readError              bool
	}{
		{name: "plain", body: valid},
		{name: "gzip", body: valid, encoding: "gzip"},
		{name: "deflate", body: valid, encoding: "deflate"},
		{name: "exact_limit", body: atLimit},
		{name: "over_limit", body: atLimit + " ", wantError: "exceeds size limit"},
		{name: "gzip_expansion_limit", body: atLimit + " ", encoding: "gzip", wantError: "exceeds size limit"},
		{name: "deflate_expansion_limit", body: atLimit + " ", encoding: "deflate", wantError: "exceeds size limit"},
		{name: "corrupt_gzip", body: "response-body-secret", encoding: "gzip", corrupt: true, wantError: "could not be decompressed"},
		{name: "read_error", readError: true, wantError: "could not be read"},
		{name: "invalid_json", body: "response-body-secret", wantError: "invalid JSON"},
		{name: "invalid_release", body: `{"tag_name":"latest","name":"response-body-secret"}`, wantError: "valid stable version"},
		{name: "draft", body: `{"tag_name":"rust-v0.161.0","draft":true}`, wantError: "not stable"},
		{name: "prerelease", body: `{"tag_name":"rust-v0.161.0","prerelease":true}`, wantError: "not stable"},
		{name: "unauthorized", body: "response-body-secret", status: http.StatusUnauthorized, wantError: "HTTP 401"},
		{name: "forbidden", body: "response-body-secret", status: http.StatusForbidden, wantError: "HTTP 403"},
		{name: "not_found", body: "response-body-secret", status: http.StatusNotFound, wantError: "HTTP 404"},
		{name: "rate_limited", body: "response-body-secret", status: http.StatusTooManyRequests, wantError: "HTTP 429"},
		{name: "server_error", body: "response-body-secret", status: http.StatusInternalServerError, wantError: "HTTP 500"},
	} {
		t.Run(tc.name, func(t *testing.T) { testFetchLatestCodexClientVersionResponsesPhase1(t, limit, &tc) })
	}
}

func TestFetchLatestCodexClientVersionNeverFollowsRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls, redirectPolicyCalls atomic.Int64
			body := &codexVersionTestBody{Reader: strings.NewReader("redirect-body-secret")}
			manager, _ := newCodexVersionTestManager(t, "", func(r *http.Request) (*http.Response, error) {
				if calls.Add(1) != 1 {
					t.Error("redirect request was sent")
					return codexVersionTestRelease("0.999.0"), nil
				}
				resp := codexVersionTestResponse(status, "")
				resp.Header.Set("Location", "https://redirect.invalid/credential-trap")
				resp.Body = body
				return resp, nil
			})
			client, err := manager.factory.HTTPClient("")
			requireDataNoError(t, err)
			client.CheckRedirect = func(*http.Request, []*http.Request) error {
				redirectPolicyCalls.Add(1)
				return nil
			}
			got, err := manager.fetchLatestCodexClientVersion(context.Background(), "")
			requireDataTruef(t, !(err == nil || got != "" || !strings.Contains(err.Error(), fmt.Sprint(status))), "redirect accepted: version=%q err=%v", got, err)
			reportTestFailure(t, calls.Load() != 1 || redirectPolicyCalls.Load() != 0 || !body.closed, func() {
				t.Fatalf("calls=%d shared redirect policy calls=%d closed=%v", calls.Load(), redirectPolicyCalls.Load(), body.closed)
			})
			{
				err := client.CheckRedirect(nil, nil)
				requireDataTrue(t, !(err != nil || redirectPolicyCalls.Load() != 1), "release lookup mutated the shared client's redirect policy")
			}
		})
	}
}

func TestResolveCodexClientVersionFixedSkipsNetwork(t *testing.T) {
	var calls atomic.Int64
	manager, clock := newCodexVersionTestManager(t, "", func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("fixed version must not fetch")
	})
	manager.codexVersions.version = "9.9.9"
	manager.codexVersions.nextCheck = clock.now().Add(time.Hour)
	for _, proxy := range []string{"", "not-a-valid-proxy"} {
		got, err := manager.resolveCodexClientVersion(context.Background(), "0.123.4-custom", proxy)
		if err != nil || got != "0.123.4-custom" {
			t.Fatalf("fixed version=%q err=%v", got, err)
		}
	}
	if calls.Load() != 0 || manager.codexVersions.version != "9.9.9" || !manager.codexVersions.nextCheck.Equal(clock.now().Add(time.Hour)) || manager.codexVersions.inflight != nil {
		t.Fatal("fixed version performed a lookup or mutated automatic cache state")
	}
	manager.factory = nil
	if got, err := manager.resolveCodexClientVersion(context.Background(), "0.123.4", ""); err != nil || got != "0.123.4" {
		t.Fatalf("fixed version required an HTTP factory: %q %v", got, err)
	}
}

func TestResolveCodexClientVersionCacheExpiresAfterOneHour(t *testing.T) {
	for _, configured := range []string{"", config.AutoCodexClientVersion} {
		t.Run("configured_"+configured, func(t *testing.T) {
			var calls atomic.Int64
			manager, clock := newCodexVersionTestManager(t, "", func(*http.Request) (*http.Response, error) {
				return codexVersionTestRelease(fmt.Sprintf("0.%d.0", 160+calls.Add(1))), nil
			})
			for _, step := range []struct {
				advance time.Duration
				want    string
				calls   int64
			}{
				{0, "0.161.0", 1},
				{0, "0.161.0", 1},
				{time.Hour - time.Nanosecond, "0.161.0", 1},
				{time.Nanosecond, "0.162.0", 2},
				{0, "0.162.0", 2},
			} {
				clock.advance(step.advance)
				got, err := manager.resolveCodexClientVersion(context.Background(), configured, "")
				if err != nil || got != step.want || calls.Load() != step.calls {
					t.Fatalf("time=%s version=%q err=%v calls=%d, want %q calls=%d", clock.now(), got, err, calls.Load(), step.want, step.calls)
				}
			}
		})
	}
}

func TestResolveCodexClientVersionFailureBackoffAndRecovery(t *testing.T) {
	var calls atomic.Int64
	manager, clock := newCodexVersionTestManager(t, "", func(*http.Request) (*http.Response, error) {
		switch calls.Add(1) {
		case 1:
			return codexVersionTestResponse(http.StatusBadGateway, "response-body-secret"), nil
		case 2:
			return codexVersionTestRelease("0.161.0"), nil
		case 3:
			return nil, errors.New("raw-transport-error")
		default:
			return codexVersionTestRelease("0.162.0"), nil
		}
	})
	for _, step := range []struct {
		name    string
		advance time.Duration
		want    string
		calls   int64
	}{
		{"first_failure_fallback", 0, config.DefaultCodexClientVersion, 1},
		{"initial_backoff", time.Minute - time.Nanosecond, config.DefaultCodexClientVersion, 1},
		{"initial_recovery", time.Nanosecond, "0.161.0", 2},
		{"failure_keeps_success", time.Hour, "0.161.0", 3},
		{"success_backoff", time.Minute - time.Nanosecond, "0.161.0", 3},
		{"later_recovery", time.Nanosecond, "0.162.0", 4},
	} {
		t.Run(step.name, func(t *testing.T) {
			clock.advance(step.advance)
			got, err := manager.resolveCodexClientVersion(context.Background(), config.AutoCodexClientVersion, "")
			if err != nil || got != step.want || calls.Load() != step.calls {
				t.Fatalf("version=%q err=%v calls=%d, want %q calls=%d", got, err, calls.Load(), step.want, step.calls)
			}
		})
	}
}

func TestResolveCodexClientVersionNeverDowngrades(t *testing.T) {
	steps := []struct{ release, want string }{
		{"0.159.999", config.DefaultCodexClientVersion},
		{"0.160.9", "0.160.9"},
		{"0.160.10", "0.160.10"},
		{"0.160.2", "0.160.10"},
		{"0.161.0", "0.161.0"},
		{"0.99.999", "0.161.0"},
		{"1.9.99", "1.9.99"},
		{"1.10.0", "1.10.0"},
		{"1.9.999", "1.10.0"},
		{"10.0.0", "10.0.0"},
		{"9.999.999", "10.0.0"},
	}
	var calls atomic.Int64
	manager, clock := newCodexVersionTestManager(t, "", func(*http.Request) (*http.Response, error) {
		index := calls.Add(1) - 1
		if index >= int64(len(steps)) {
			return nil, errors.New("unexpected extra release request")
		}
		return codexVersionTestRelease(steps[index].release), nil
	})
	for index, step := range steps {
		clock.advance(time.Hour)
		got, err := manager.resolveCodexClientVersion(context.Background(), config.AutoCodexClientVersion, "")
		if err != nil || got != step.want || calls.Load() != int64(index+1) {
			t.Fatalf("release=%q resolved=%q err=%v calls=%d, want %q", step.release, got, err, calls.Load(), step.want)
		}
	}
}

// Observing Done proves a waiter reached the pending-request select, without sleeps.
// The resolver only calls Err before that point; no HTTP request is created for it.
type codexVersionTestWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *codexVersionTestWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

type codexVersionTestResult struct {
	version string
	err     error
}

func startCodexVersionTestResolve(manager *Manager, ctx context.Context) <-chan codexVersionTestResult {
	result := make(chan codexVersionTestResult, 1)
	go func() {
		version, err := manager.resolveCodexClientVersion(ctx, config.AutoCodexClientVersion, "")
		result <- codexVersionTestResult{version, err}
	}()
	return result
}

func receiveCodexVersionTest[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for release test synchronization")
		var zero T
		return zero
	}
}

func TestResolveCodexClientVersionConcurrentRequestsCoalesce(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	releaseAll := sync.OnceFunc(func() { close(release) })
	defer releaseAll()
	var calls atomic.Int64
	manager, _ := newCodexVersionTestManager(t, "", func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return codexVersionTestRelease("0.161.0"), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := []<-chan codexVersionTestResult{startCodexVersionTestResolve(manager, ctx)}
	receiveCodexVersionTest(t, started)
	for range 16 {
		waiter := &codexVersionTestWaitContext{Context: ctx, waiting: make(chan struct{})}
		results = append(results, startCodexVersionTestResolve(manager, waiter))
		receiveCodexVersionTest(t, waiter.waiting)
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent cache misses caused %d HTTP requests", calls.Load())
	}
	releaseAll()
	for _, result := range results {
		got := receiveCodexVersionTest(t, result)
		if got.err != nil || got.version != "0.161.0" {
			t.Fatalf("concurrent result=%+v", got)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("waiters started %d requests instead of sharing one", calls.Load())
	}
}

func TestResolveCodexClientVersionCanceledWaiterDoesNotCancelLeader(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	releaseAll := sync.OnceFunc(func() { close(release) })
	defer releaseAll()
	var calls atomic.Int64
	manager, _ := newCodexVersionTestManager(t, "", func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return codexVersionTestRelease("0.161.0"), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	leader := startCodexVersionTestResolve(manager, leaderCtx)
	receiveCodexVersionTest(t, started)
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	defer cancelWaiter()
	waiter := &codexVersionTestWaitContext{Context: waiterCtx, waiting: make(chan struct{})}
	result := startCodexVersionTestResolve(manager, waiter)
	receiveCodexVersionTest(t, waiter.waiting)
	cancelWaiter()
	if got := receiveCodexVersionTest(t, result); !errors.Is(got.err, context.Canceled) || got.version != "" {
		t.Fatalf("canceled waiter=%+v", got)
	}
	manager.codexVersions.mu.Lock()
	untouched := manager.codexVersions.version == "" && manager.codexVersions.nextCheck.IsZero() && manager.codexVersions.inflight != nil
	manager.codexVersions.mu.Unlock()
	if !untouched || calls.Load() != 1 || leaderCtx.Err() != nil {
		t.Fatal("canceling a waiter modified the cache or interrupted the leader")
	}
	releaseAll()
	if got := receiveCodexVersionTest(t, leader); got.err != nil || got.version != "0.161.0" {
		t.Fatalf("leader=%+v", got)
	}
	if got, err := manager.resolveCodexClientVersion(context.Background(), config.AutoCodexClientVersion, ""); err != nil || got != "0.161.0" || calls.Load() != 1 {
		t.Fatalf("success was not cached: %q %v calls=%d", got, err, calls.Load())
	}
}

func TestResolveCodexClientVersionCanceledLeaderAllowsWaiterRetry(t *testing.T) {
	for _, cached := range []string{"", "0.161.0"} {
		t.Run("previous_"+cached, func(t *testing.T) { testResolveCodexClientVersionCanceledLeaderAllowsWaiterRetryPhase1(t, &cached) })
	}
}

func TestResolveCodexClientVersionAlreadyCanceled(t *testing.T) {
	var calls atomic.Int64
	manager, _ := newCodexVersionTestManager(t, "", func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return codexVersionTestRelease("0.161.0"), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, configured := range []string{config.AutoCodexClientVersion, "0.123.4"} {
		if got, err := manager.resolveCodexClientVersion(ctx, configured, ""); !errors.Is(err, context.Canceled) || got != "" {
			t.Fatalf("already canceled call=%q %v", got, err)
		}
	}
	if calls.Load() != 0 || manager.codexVersions.version != "" || !manager.codexVersions.nextCheck.IsZero() || manager.codexVersions.inflight != nil {
		t.Fatal("already canceled call performed I/O or populated the cache")
	}
}

// The underlying reader remains the real HTTP response body. Signaling its first
// Read distinguishes cancellation during body consumption from header/transport I/O.
type codexVersionTestObservedBody struct {
	io.ReadCloser
	ctx     context.Context
	started chan<- context.Context
	once    sync.Once
}

func (b *codexVersionTestObservedBody) Read(p []byte) (int, error) {
	b.once.Do(func() { b.started <- b.ctx })
	return b.ReadCloser.Read(p)
}

func newCodexVersionBodyTestManager(t *testing.T, handler http.HandlerFunc) (*Manager, *codexVersionTestClock, <-chan context.Context) {
	t.Helper()
	serverCtx, cancelServer := context.WithCancel(context.Background())
	t.Cleanup(cancelServer) // Register immediately; the server cleanup also cancels before closing.
	server := httptest.NewUnstartedServer(handler)
	server.Config.BaseContext = func(net.Listener) context.Context { return serverCtx }
	server.Start()
	t.Cleanup(func() {
		cancelServer()
		server.CloseClientConnections()
		server.Close()
	})
	localURL, err := url.Parse(server.URL)
	requireDataNoError(t, err)
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != localURL.Host {
			return nil, errors.New("body test refuses any dial except its local release server")
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	bodyStarted := make(chan context.Context, 4)
	manager, clock := newCodexVersionTestManager(t, "", func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.String() != "https://api.github.com/repos/openai/codex/releases/latest" {
			return nil, errors.New("body test refuses unexpected release request")
		}
		clone := r.Clone(r.Context())
		endpoint := *r.URL
		endpoint.Scheme, endpoint.Host = localURL.Scheme, localURL.Host
		clone.URL, clone.Host = &endpoint, localURL.Host
		response, err := transport.RoundTrip(clone)
		if err != nil {
			return nil, err
		}
		response.Body = &codexVersionTestObservedBody{ReadCloser: response.Body, ctx: r.Context(), started: bodyStarted}
		return response, nil
	})
	return manager, clock, bodyStarted
}

func TestResolveCodexClientVersionRequestTimeoutIsFiveSeconds(t *testing.T) {
	var calls atomic.Int64
	serverStopped := make(chan struct{})
	manager, clock, bodyStarted := newCodexVersionBodyTestManager(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			close(serverStopped)
			return
		}
		_, _ = io.WriteString(w, `{"tag_name":"rust-v0.161.0"}`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	started := time.Now()
	result := startCodexVersionTestResolve(manager, ctx)
	bodyCtx := receiveCodexVersionTest(t, bodyStarted)
	deadline, ok := bodyCtx.Deadline()
	remaining := time.Until(deadline)
	if !ok || remaining <= 0 || remaining > 5*time.Second || deadline.Before(started.Add(4*time.Second)) {
		t.Fatalf("body read lacks the five-second request deadline: deadline=%v remaining=%v", deadline, remaining)
	}
	// This is the only real deadline wait; the headers have already arrived and
	// the real HTTP response body, rather than a fake RoundTripper, is blocked.
	select {
	case got := <-result:
		if got.err != nil || got.version != config.DefaultCodexClientVersion || !errors.Is(bodyCtx.Err(), context.DeadlineExceeded) || calls.Load() != 1 {
			t.Fatalf("body timeout fallback=%+v request error=%v calls=%d", got, bodyCtx.Err(), calls.Load())
		}
	case <-time.After(6 * time.Second):
		t.Fatal("body read did not finish after the five-second request deadline")
	}
	if elapsed := time.Since(started); elapsed > 6*time.Second {
		t.Fatalf("five-second body read was not bounded: %v", elapsed)
	}
	receiveCodexVersionTest(t, serverStopped)
	if !manager.codexVersions.nextCheck.Equal(clock.now().Add(time.Minute)) {
		t.Fatal("body timeout did not install the normal failure retry delay")
	}
	clock.advance(time.Minute - time.Nanosecond)
	if got, err := manager.resolveCodexClientVersion(context.Background(), config.AutoCodexClientVersion, ""); err != nil || got != config.DefaultCodexClientVersion || calls.Load() != 1 {
		t.Fatalf("body timeout did not respect retry backoff: %q %v calls=%d", got, err, calls.Load())
	}
	clock.advance(time.Nanosecond)
	if got, err := manager.resolveCodexClientVersion(context.Background(), config.AutoCodexClientVersion, ""); err != nil || got != "0.161.0" || calls.Load() != 2 {
		t.Fatalf("body timeout did not recover after one minute: %q %v calls=%d", got, err, calls.Load())
	}
}

func TestResolveCodexClientVersionBodyCancellationAllowsWaiterRetry(t *testing.T) {
	for _, cached := range []string{"", "0.161.0"} {
		t.Run("previous_"+cached, func(t *testing.T) { testResolveCodexClientVersionBodyCancellationAllowsWaiterRetryPhase1(t, &cached) })
	}
}

func TestFetchLatestCodexClientVersionSelectedProxyAndCredentialBoundary(t *testing.T) {
	var targetCalls, proxyCalls, directCalls atomic.Int64
	assertPublicHeaders := func(headers http.Header) {
		testFetchLatestCodexClientVersionSelectedProxyAndCredentialBoundaryPhase1(headers, &t)
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		assertPublicHeaders(r.Header)
		reportTestFailure(t, r.Method != http.MethodGet || r.URL.Path != "/repos/openai/codex/releases/latest" || r.URL.RawQuery != "" || r.Header.Get("Proxy-Authorization") != "", func() {
			t.Errorf("incorrect public release request: %s %s", r.Method, r.URL)
		})
		_, _ = io.WriteString(w, `{"tag_name":"rust-v0.161.0"}`)
	}))
	defer target.Close()
	targetURL, err := url.Parse(target.URL)
	requireDataNoError(t, err)
	forwardTransport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != targetURL.Host {
			return nil, errors.New("proxy test refuses non-local target")
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	defer forwardTransport.CloseIdleConnections()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		testFetchLatestCodexClientVersionSelectedProxyAndCredentialBoundaryPhase2(w, r, &t, &proxyCalls, &assertPublicHeaders, &targetURL, &forwardTransport)
	}))
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	requireDataNoError(t, err)
	proxyURL.User = url.UserPassword("proxy-user", "proxy-password")
	factory := egress.NewFactory(egress.Options{})
	factory.BaseDialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != proxyURL.Host {
			return nil, errors.New("release test refuses any dial except the selected local proxy")
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	direct, err := factory.HTTPClient("")
	requireDataNoError(t, err)
	direct.Transport = catalogRoundTrip(func(*http.Request) (*http.Response, error) {
		directCalls.Add(1)
		return nil, errors.New("direct release requests are forbidden")
	})
	client, err := factory.HTTPClient(proxyURL.String())
	requireDataNoError(t, err)
	transport := client.Transport.(*http.Transport)
	defer transport.CloseIdleConnections()
	jar, err := cookiejar.New(nil)
	requireDataNoError(t, err)
	publicURL, err := url.Parse(codexLatestReleaseURL)
	requireDataNoError(t, err)
	jar.SetCookies(publicURL, []*http.Cookie{{Name: "session", Value: "cookie-secret"}})
	client.Jar = jar
	client.Transport = catalogRoundTrip(func(r *http.Request) (*http.Response, error) {
		return testFetchLatestCodexClientVersionSelectedProxyAndCredentialBoundaryPhase3(r, &assertPublicHeaders, &targetURL, &transport)
	})
	manager := &Manager{factory: factory}
	got, err := manager.fetchLatestCodexClientVersion(context.Background(), proxyURL.String())
	reportTestFailure(t, err != nil || got != "0.161.0" || targetCalls.Load() != 1 || proxyCalls.Load() != 1 || directCalls.Load() != 0, func() {
		t.Fatalf("version=%q err=%v target=%d proxy=%d direct=%d", got, err, targetCalls.Load(), proxyCalls.Load(), directCalls.Load())
	})
	requireDataCondition(t, !(client.Jar != jar || len(client.Jar.Cookies(publicURL)) != 1), "release lookup mutated the shared client's cookie jar")
}

func TestResolveCodexClientVersionFailureLogsAreSanitized(t *testing.T) {
	const proxy = "http://proxy-user:proxy-password@proxy.invalid:8080"
	for _, failure := range []string{"transport", "http", "json", "version", "gzip", "read", "invalid_proxy", "missing_factory"} {
		t.Run(failure, func(t *testing.T) { testResolveCodexClientVersionFailureLogsAreSanitizedPhase1(t, proxy, &failure) })
	}
}

// testFetchLatestCodexClientVersionResponsesPhase1 exercises one isolated test phase with the original shared state.
func testFetchLatestCodexClientVersionResponsesPhase1(t *testing.T, capturedLimit int, capturedTc *struct {
	name      string
	body      string
	encoding  string
	wantError string
	status    int
	corrupt   bool
	readError bool
}) {
	wire := []byte((*capturedTc).body)
	if (*capturedTc).encoding != "" && !(*capturedTc).corrupt {
		var compressed bytes.Buffer
		var writer io.WriteCloser = gzip.NewWriter(&compressed)
		if (*capturedTc).encoding == "deflate" {
			writer = zlib.NewWriter(&compressed)
		}
		{
			_, err := io.WriteString(writer, (*capturedTc).body)
			requireDataTrue(t, !(err != nil), err)
		}
		requireDataNoError(t, writer.Close())
		wire = compressed.Bytes()
		requireDataCondition(t, !(strings.Contains((*capturedTc).name, "expansion_limit") && len(wire) >= capturedLimit), "test payload must exceed the limit only after decompression")
	}
	body := &codexVersionTestBody{Reader: bytes.NewReader(wire)}
	if (*capturedTc).readError {
		body.Reader = codexVersionTestReadError{}
	}
	var calls atomic.Int64
	manager, _ := newCodexVersionTestManager(t, "", func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		status := (*capturedTc).status
		if status == 0 {
			status = http.StatusOK
		}
		resp := codexVersionTestResponse(status, "")
		resp.Header.Set("Content-Encoding", (*capturedTc).encoding)
		resp.Body = body
		return resp, nil
	})
	got, err := manager.fetchLatestCodexClientVersion(context.Background(), "")
	if (*capturedTc).wantError == "" {
		requireDataTruef(t, !(err != nil || got != "0.161.0"), "version=%q err=%v", got, err)
	} else {
		reportTestFailure(t, err == nil || got != "" || !strings.Contains(err.Error(), (*capturedTc).wantError), func() {
			t.Fatalf("version=%q err=%v, want error containing %q", got, err, (*capturedTc).wantError)
		})
	}
	requireDataCondition(t, !(err != nil && strings.Contains(err.Error(), "response-body-secret")), "response body leaked through the public error")
	reportTestFailure(t, calls.Load() != 1 || !body.closed, func() {
		t.Fatalf("calls=%d body.closed=%v", calls.Load(), body.closed)
	})
	reportTestFailure(t, (*capturedTc).encoding == "" && body.read > capturedLimit+1, func() {
		t.Fatalf("read %d bytes beyond the bounded response reader", body.read)
	})
}

// testResolveCodexClientVersionCanceledLeaderAllowsWaiterRetryPhase1 exercises one isolated test phase with the original shared state.
func testResolveCodexClientVersionCanceledLeaderAllowsWaiterRetryPhase1(t *testing.T, capturedCached *string) {
	started, retryStarted, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseAll := sync.OnceFunc(func() { close(release) })
	defer releaseAll()
	var calls atomic.Int64
	manager, clock := newCodexVersionTestManager(t, "", func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		close(retryStarted)
		select {
		case <-release:
			return codexVersionTestRelease("0.162.0"), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	manager.codexVersions.version = (*capturedCached)
	if (*capturedCached) != "" {
		manager.codexVersions.nextCheck = clock.now()
	}
	previousDeadline := manager.codexVersions.nextCheck
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	leader := startCodexVersionTestResolve(manager, leaderCtx)
	receiveCodexVersionTest(t, started)
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	defer cancelWaiter()
	waiter := &codexVersionTestWaitContext{Context: waiterCtx, waiting: make(chan struct{})}
	result := startCodexVersionTestResolve(manager, waiter)
	receiveCodexVersionTest(t, waiter.waiting)
	cancelLeader()
	{
		got := receiveCodexVersionTest(t, leader)
		requireDataTruef(t, !(!errors.Is(got.err, context.Canceled) || got.version != ""), "canceled leader=%+v", got)
	}
	receiveCodexVersionTest(t, retryStarted)
	manager.codexVersions.mu.Lock()
	untouched := manager.codexVersions.version == (*capturedCached) && manager.codexVersions.nextCheck.Equal(previousDeadline) && manager.codexVersions.inflight != nil
	manager.codexVersions.mu.Unlock()
	requireDataCondition(t, !(!untouched || calls.Load() != 2), "leader cancellation installed fallback/backoff or prevented immediate waiter retry")
	releaseAll()
	{
		got := receiveCodexVersionTest(t, result)
		requireDataTruef(t, !(got.err != nil || got.version != "0.162.0"), "retrying waiter=%+v", got)
	}
	{
		got, err := manager.resolveCodexClientVersion(context.Background(), config.AutoCodexClientVersion, "")
		reportTestFailure(t, err != nil || got != "0.162.0" || calls.Load() != 2, func() {
			t.Fatalf("retry result not cached: %q %v calls=%d", got, err, calls.Load())
		})
	}
}

// testResolveCodexClientVersionBodyCancellationAllowsWaiterRetryPhase1 exercises one isolated test phase with the original shared state.
func testResolveCodexClientVersionBodyCancellationAllowsWaiterRetryPhase1(t *testing.T, capturedCached *string) {
	var calls atomic.Int64
	serverStopped, releaseRetry := make(chan struct{}), make(chan struct{})
	releaseAll := sync.OnceFunc(func() { close(releaseRetry) })
	t.Cleanup(releaseAll)
	manager, clock, bodyStarted := newCodexVersionBodyTestManager(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if call == 1 {
			<-r.Context().Done()
			close(serverStopped)
			return
		}
		select {
		case <-releaseRetry:
			_, _ = io.WriteString(w, `{"tag_name":"rust-v0.162.0"}`)
		case <-r.Context().Done():
		}
	})
	manager.codexVersions.version = (*capturedCached)
	if (*capturedCached) != "" {
		manager.codexVersions.nextCheck = clock.now()
	}
	previousDeadline := manager.codexVersions.nextCheck
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	t.Cleanup(cancelLeader)
	leader := startCodexVersionTestResolve(manager, leaderCtx)
	firstBodyCtx := receiveCodexVersionTest(t, bodyStarted)
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	t.Cleanup(cancelWaiter)
	waiter := &codexVersionTestWaitContext{Context: waiterCtx, waiting: make(chan struct{})}
	result := startCodexVersionTestResolve(manager, waiter)
	receiveCodexVersionTest(t, waiter.waiting)
	cancelLeader()
	{
		got := receiveCodexVersionTest(t, leader)
		requireDataTruef(t, !(!errors.Is(got.err, context.Canceled) || got.version != ""), "leader did not immediately cancel its real body read: %+v", got)
	}
	reportTestFailure(t, !errors.Is(firstBodyCtx.Err(), context.Canceled), func() {
		t.Fatalf("first body context=%v, want parent cancellation", firstBodyCtx.Err())
	})
	receiveCodexVersionTest(t, serverStopped)
	retryBodyCtx := receiveCodexVersionTest(t, bodyStarted)
	manager.codexVersions.mu.Lock()
	untouched := manager.codexVersions.version == (*capturedCached) && manager.codexVersions.nextCheck.Equal(previousDeadline) && manager.codexVersions.inflight != nil
	manager.codexVersions.mu.Unlock()
	requireDataCondition(t, !(!untouched || calls.Load() != 2 || retryBodyCtx.Err() != nil), "body cancellation polluted the cache or failed to start the live waiter's retry")
	releaseAll()
	{
		got := receiveCodexVersionTest(t, result)
		requireDataTruef(t, !(got.err != nil || got.version != "0.162.0"), "waiter could not finish reading the retried body: %+v", got)
	}
	{
		got, err := manager.resolveCodexClientVersion(context.Background(), config.AutoCodexClientVersion, "")
		reportTestFailure(t, err != nil || got != "0.162.0" || calls.Load() != 2, func() {
			t.Fatalf("retried body result not cached: %q %v calls=%d", got, err, calls.Load())
		})
	}
	requireDataCondition(t, !(!manager.codexVersions.nextCheck.Equal(clock.now().Add(time.Hour))), "successful body retry did not establish the normal cache TTL")
}

// testFetchLatestCodexClientVersionSelectedProxyAndCredentialBoundaryPhase1 exercises one isolated test phase with the original shared state.
func testFetchLatestCodexClientVersionSelectedProxyAndCredentialBoundaryPhase1(headers http.Header, capturedT **testing.T) {
	(*capturedT).Helper()
	for _, key := range []string{"Authorization", "Cookie", "ChatGPT-Account-Id", "OpenAI-Organization", "OpenAI-Project", "X-Api-Key", "OpenAI-Beta", "Version", "Originator"} {
		reportTestFailure((*capturedT), headers.Get(key) != "", func() {
			(*capturedT).Errorf("public release request included user credential/header %s", key)
		})
	}
	reportTestFailure((*capturedT), headers.Get("Accept") != "application/vnd.github+json" || headers.Get("User-Agent") != "pi-gateway-codex-version", func() {
		(*capturedT).Errorf("incorrect release metadata headers: %v", headers)
	})
}

// testFetchLatestCodexClientVersionSelectedProxyAndCredentialBoundaryPhase2 exercises one isolated test phase with the original shared state.
func testFetchLatestCodexClientVersionSelectedProxyAndCredentialBoundaryPhase2(w http.ResponseWriter, r *http.Request, capturedT **testing.T, capturedProxyCalls *atomic.Int64, capturedAssertPublicHeaders *func(headers http.Header), capturedTargetURL **url.URL, capturedForwardTransport **http.Transport) {
	(*capturedProxyCalls).Add(1)
	(*capturedAssertPublicHeaders)(r.Header)
	reportTestFailure((*capturedT), r.Header.Get("Proxy-Authorization") == "", func() {
		(*capturedT).Error("selected proxy credentials were not used")
	})
	if r.URL.Host != (*capturedTargetURL).Host || r.URL.Scheme != (*capturedTargetURL).Scheme {
		(*capturedT).Error("proxy received an unexpected destination")
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	forward := r.Clone(r.Context())
	forward.RequestURI = ""
	forward.Header.Del("Proxy-Authorization")
	resp, err := (*capturedForwardTransport).RoundTrip(forward)
	if err != nil {
		(*capturedT).Errorf("local proxy forwarding: %v", err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// testFetchLatestCodexClientVersionSelectedProxyAndCredentialBoundaryPhase3 exercises one isolated test phase with the original shared state.
func testFetchLatestCodexClientVersionSelectedProxyAndCredentialBoundaryPhase3(r *http.Request, capturedAssertPublicHeaders *func(headers http.Header), capturedTargetURL **url.URL, capturedTransport **http.Transport) (*http.Response, error) {
	(*capturedAssertPublicHeaders)(r.Header)
	if r.URL.String() != "https://api.github.com/repos/openai/codex/releases/latest" || r.Header.Get("Proxy-Authorization") != "" {
		return nil, errors.New("unexpected public release destination or proxy credential leakage")
	}
	clone := r.Clone(r.Context())
	endpoint := *r.URL
	endpoint.Scheme, endpoint.Host = (*capturedTargetURL).Scheme, (*capturedTargetURL).Host
	clone.URL, clone.Host = &endpoint, (*capturedTargetURL).Host
	return (*capturedTransport).RoundTrip(clone)
}

// testResolveCodexClientVersionFailureLogsAreSanitizedPhase1 exercises one isolated test phase with the original shared state.
func testResolveCodexClientVersionFailureLogsAreSanitizedPhase1(t *testing.T, capturedProxy string, capturedFailure *string) {
	var logs bytes.Buffer
	var calls atomic.Int64
	manager, _ := newCodexVersionTestManager(t, capturedProxy, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		switch *capturedFailure {
		case "transport":
			return nil, errors.New("raw-transport-error " + capturedProxy + " Authorization=account-secret")
		case "http":
			return codexVersionTestResponse(http.StatusServiceUnavailable, "response-body-secret account-secret"), nil
		case "json":
			return codexVersionTestResponse(http.StatusOK, "response-body-secret"), nil
		case "version":
			return codexVersionTestResponse(http.StatusOK, `{"tag_name":"response-body-secret"}`), nil
		case "gzip":
			resp := codexVersionTestResponse(http.StatusOK, "response-body-secret")
			resp.Header.Set("Content-Encoding", "gzip")
			return resp, nil
		case "read":
			resp := codexVersionTestResponse(http.StatusOK, "")
			resp.Body = io.NopCloser(codexVersionTestReadError{})
			return resp, nil
		default:
			t.Error("invalid proxy or absent factory unexpectedly reached transport")
			return nil, errors.New("unexpected transport call")
		}
	})
	manager.logger = slog.New(slog.NewTextHandler(&logs, nil))
	selectedProxy := capturedProxy
	if (*capturedFailure) == "invalid_proxy" {
		selectedProxy = "invalid://proxy-user:proxy-password@proxy.invalid:8080"
	}
	if (*capturedFailure) == "missing_factory" {
		manager.factory = nil
	}
	got, err := manager.resolveCodexClientVersion(context.Background(), config.AutoCodexClientVersion, selectedProxy)
	requireDataTruef(t, !(err != nil || got != config.DefaultCodexClientVersion), "fallback=%q err=%v", got, err)
	reportTestFailure(t, !strings.Contains(logs.String(), "Codex client version lookup failed") || !strings.Contains(logs.String(), "reason="), func() {
		t.Fatalf("missing classified failure log: %s", logs.String())
	})
	for _, secret := range []string{"proxy-password", "proxy-user", "proxy.invalid", "response-body-secret", "account-secret", "raw-transport-error", "raw-read-error"} {
		reportTestFailure(t, strings.Contains(logs.String(), secret), func() {
			t.Fatalf("failure log leaked %q: %s", secret, logs.String())
		})
	}
	wantCalls := int64(1)
	if (*capturedFailure) == "invalid_proxy" || (*capturedFailure) == "missing_factory" {
		wantCalls = 0
	}
	reportTestFailure(t, calls.Load() != wantCalls, func() {
		t.Fatalf("calls=%d, want %d", calls.Load(), wantCalls)
	})
}
