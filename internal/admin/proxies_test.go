package admin

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pi-gateway/internal/accounts"
	"pi-gateway/internal/api"
	"pi-gateway/internal/config"
	"pi-gateway/internal/egress"
	"pi-gateway/internal/oauth"
	"pi-gateway/internal/settings"
	"pi-gateway/internal/store"
	"pi-gateway/internal/upstream"

	"github.com/gorilla/websocket"
)

type proxyHarness struct {
	server  *httptest.Server
	admin   *Server
	db      *store.Store
	token   string
	account *store.Account
	factory *egress.Factory
}

func newProxyHarness(t *testing.T, target, transport string) *proxyHarness {
	t.Helper()
	ctx := context.Background()
	st := newAdminTestStore(t)
	cfg := config.Default()
	cfg.Models.CodexClientVersion = config.DefaultCodexClientVersion
	cfg.Admin.Password = "local-admin-password"
	cfg.Upstream.BaseURL = target
	cfg.Upstream.Transport = transport
	cfg.Upstream.SSEZstd = false
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	factory := egress.NewFactory(egress.Options{ConnectTimeout: time.Second, ResponseHeaderTimeout: time.Second})
	manager := accounts.New(st, oauth.NewClient(nil), factory, accounts.Options{Logger: logger})
	holder := settings.New(st, cfg)
	{
		err := holder.Load(ctx)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	client := upstream.New(upstream.Config{SSEURL: cfg.UpstreamSSEURL(), WSURL: cfg.UpstreamWSURL(), Pool: true, Logger: logger}, factory)
	t.Cleanup(client.Close)
	account := &store.Account{Name: "Local test account", Email: "local@example.test", AccessToken: "local-model-token", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Enabled: true, Status: store.AccountStatusReady}
	{
		err := st.CreateAccount(ctx, account)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	{
		err := st.CreateKey(ctx, &store.APIKey{Name: "local API key", Key: "sk-local-api", Enabled: true})
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	s := New(Options{Config: cfg, Store: st, Accounts: manager, Settings: holder, Factory: factory, Upstream: client, Logger: logger})
	{
		err := s.BootstrapPassword(ctx)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	mux := http.NewServeMux()
	s.Routes(mux, nil)
	api.New(api.Options{Config: cfg, Store: st, Accounts: manager, Settings: holder, Upstream: client, Logger: logger}).Routes(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	h := &proxyHarness{server: server, admin: s, db: st, account: account, factory: factory}
	result := h.request(t, "POST", "/api/auth/login", `{"username":"admin","password":"local-admin-password"}`, 200, false)
	h.token = result["token"].(string)
	return h
}

func (h *proxyHarness) request(t *testing.T, method, path, body string, want int, auth bool) map[string]any {
	t.Helper()
	req, err := http.NewRequest(method, h.server.URL+path, strings.NewReader(body))
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	req.Header.Set("Content-Type", "application/json")
	if auth {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	res, err := h.server.Client().Do(req)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	reportTestFailure(t, res.StatusCode != want, func() {
		t.Fatalf("%s %s status %d want %d: %s", method, path, res.StatusCode, want, raw)
	})
	out := map[string]any{}
	{
		err := json.Unmarshal(raw, &out)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	return out
}

func proxyPayload(name, endpoint string) string {
	b, _ := json.Marshal(map[string]string{"name": name, "url": endpoint})
	return string(b)
}
func savedProxyID(result map[string]any) int64 {
	return int64(result["proxy"].(map[string]any)["id"].(float64))
}

func TestProxyAdminCRUDValidationAuthenticationAndRedaction(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer target.Close()
	h := newProxyHarness(t, target.URL, "sse")
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/proxies"}, {"POST", "/api/proxies"}, {"GET", "/api/proxies/1"}, {"PATCH", "/api/proxies/1"}, {"DELETE", "/api/proxies/1"}, {"POST", "/api/proxies/probe"}, {"POST", "/api/proxies/1/test"}, {"GET", "/api/proxies/1/accounts"}, {"PUT", "/api/proxies/1/accounts"},
	} {
		h.request(t, route.method, route.path, "{}", 401, false)
	}
	for _, endpoint := range []string{"", "localhost:8080", "ftp://localhost:8080", "http://localhost", "http://localhost:0", "http://localhost:65536", "http://localhost:8080/path", "http://localhost:8080?token=secret", "http://user:topsecret@localhost:invalid", "http://%xx:topsecret@localhost:80"} {
		out := h.request(t, "POST", "/api/proxies", proxyPayload("invalid", endpoint), 400, true)
		raw, _ := json.Marshal(out)
		reportTestFailure(t, strings.Contains(string(raw), "topsecret"), func() {
			t.Fatal("URL validation leaked credentials")
		})
	}
	for _, scheme := range []string{"http", "https", "socks5", "socks5h"} {
		h.request(t, "POST", "/api/proxies", proxyPayload(scheme, scheme+"://user:topsecret@localhost:18080"), 201, true)
	}
	h.request(t, "GET", "/api/proxies?page_size=101", "", 400, true)
	{
		result := h.request(t, "GET", "/api/proxies?search=topsecret", "", 200, true)
		reportTestFailure(t, result["total"] != float64(0), func() {
			t.Fatal("search must not act as a password-discovery oracle")
		})
	}
	{
		result := h.request(t, "GET", "/api/proxies?search=localhost:18080", "", 200, true)
		reportTestFailure(t, result["total"] != float64(4), func() {
			t.Fatal("public endpoint search did not match saved proxies")
		})
	}
	list := h.request(t, "GET", "/api/proxies?search=socks&page_size=1&page=2", "", 200, true)
	reportTestFailure(t, list["total"] != float64(2) || len(list["items"].([]any)) != 1, func() {
		t.Fatalf("bad pagination: %v", list)
	})
	made := h.request(t, "POST", "/api/proxies", proxyPayload("saved", "http://user:topsecret@localhost:18081"), 201, true)
	id := savedProxyID(made)
	path := fmt.Sprintf("/api/proxies/%d", id)
	for _, out := range []map[string]any{made, h.request(t, "GET", path, "", 200, true), h.request(t, "GET", "/api/proxies", "", 200, true)} {
		raw, _ := json.Marshal(out)
		reportTestFailure(t, strings.Contains(string(raw), "topsecret") || strings.Contains(string(raw), "user:"), func() {
			t.Fatal("proxy response leaked credentials")
		})
	}
	h.request(t, "PATCH", path, `{"name":"renamed","url":"  "}`, 200, true)
	p, err := h.db.GetProxy(context.Background(), id)
	reportTestFailure(t, err != nil || p.Name != "renamed" || p.URL != "http://user:topsecret@localhost:18081", func() {
		t.Fatalf("blank edit lost saved secret: %+v %v", p, err)
	})
	h.request(t, "PATCH", fmt.Sprintf("/api/accounts/%d", h.account.ID), fmt.Sprintf(`{"proxy_id":%d,"proxy_url":"invalid is ignored"}`, id), 200, true)
	accountOut := h.request(t, "GET", "/api/accounts", "", 200, true)
	raw, _ := json.Marshal(accountOut)
	reportTestFailure(t, strings.Contains(string(raw), "topsecret"), func() {
		t.Fatal("account output leaked saved proxy password")
	})
	linked := h.request(t, "GET", path+"/accounts", "", 200, true)
	reportTestFailure(t, linked["total"] != float64(1), func() {
		t.Fatalf("missing account binding: %v", linked)
	})
	h.request(t, "DELETE", path, "", 409, true)
	h.request(t, "PATCH", path, `{"url":"http://new:nextsecret@localhost:18082"}`, 200, true)
	a, _ := h.db.GetAccount(context.Background(), h.account.ID)
	reportTestFailure(t, a.ProxyURL != "http://new:nextsecret@localhost:18082", func() {
		t.Fatalf("bound endpoint did not update: %q", a.ProxyURL)
	})
	// A normal management edit must preserve both the saved binding and password.
	h.request(t, "PATCH", fmt.Sprintf("/api/accounts/%d", a.ID), `{"name":"still-bound"}`, 200, true)
	a, _ = h.db.GetAccount(context.Background(), a.ID)
	reportTestFailure(t, a.ProxyID == nil || *a.ProxyID != id, func() {
		t.Fatal("unrelated edit cleared proxy")
	})
	h.request(t, "PUT", path+"/accounts", fmt.Sprintf(`{"account_ids":[%d,999999]}`, a.ID), 400, true)
	a, _ = h.db.GetAccount(context.Background(), a.ID)
	reportTestFailure(t, a.ProxyID == nil, func() {
		t.Fatal("failed bulk assignment was not atomic")
	})
	h.request(t, "PATCH", fmt.Sprintf("/api/accounts/%d", a.ID), `{"proxy_id":999999}`, 400, true)
	h.request(t, "PATCH", fmt.Sprintf("/api/accounts/%d", a.ID), `{"proxy_id":null}`, 200, true)
	a, _ = h.db.GetAccount(context.Background(), a.ID)
	reportTestFailure(t, a.ProxyID != nil || a.ProxyURL != "", func() {
		t.Fatal("explicit detach did not use direct egress")
	})
	h.request(t, "DELETE", path, "", 200, true)
	h.request(t, "GET", path, "", 404, true)
}

// localForwardProxy implements only a test fixture. Production proxy protocol
// handling always remains in egress.Factory. Requests are restricted to target.
func localForwardProxy(t *testing.T, target string, hits *atomic.Int64, tlsServer bool) *httptest.Server {
	t.Helper()
	targetURL, _ := url.Parse(target)
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveLocalForwardProxyRequest(w, r, &hits, &targetURL, &transport)
	})
	var server *httptest.Server
	if tlsServer {
		server = httptest.NewTLSServer(handler)
	} else {
		server = httptest.NewServer(handler)
	}
	t.Cleanup(server.Close)
	return server
}

func TestSavedProxyProbeUsesActualTransportAndNoAccountOrAdminCredentials(t *testing.T) {
	var seen atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		for _, header := range []string{"Authorization", "Cookie", "Proxy-Authorization", "Chatgpt-Account-Id"} {
			{
				got := r.Header.Get(header)
				reportTestFailure(t, got != "", func() {
					t.Errorf("probe target received %s: %q", header, got)
				})
			}
		}
		w.WriteHeader(401)
	}))
	defer target.Close()
	h := newProxyHarness(t, target.URL, "sse")
	for _, encrypted := range []bool{false, true} {
		var hits atomic.Int64
		proxy := localForwardProxy(t, target.URL, &hits, encrypted)
		if encrypted {
			client, err := h.factory.HTTPClient(proxy.URL)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: proxy.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
		}
		made := h.request(t, "POST", "/api/proxies", proxyPayload("probe", proxy.URL), 201, true)
		path := fmt.Sprintf("/api/proxies/%d/test", savedProxyID(made))
		result := h.request(t, "POST", path, "", 200, true)["test"].(map[string]any)
		reportTestFailure(t, result["success"] != true || result["status"] != float64(401) || result["latency_ms"] == nil || hits.Load() != 1, func() {
			t.Fatalf("proxy probe was not real: %+v hits=%d", result, hits.Load())
		})
	}
	reportTestFailure(t, seen.Load() != 2, func() {
		t.Fatalf("target was not reached twice: %d", seen.Load())
	})
	reject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(407) }))
	defer reject.Close()
	failure := h.request(t, "POST", "/api/proxies/probe", proxyPayload("", reject.URL), 200, true)["test"].(map[string]any)
	reportTestFailure(t, failure["success"] != false || failure["status"] != float64(407), func() {
		t.Fatalf("proxy auth failure reported success: %v", failure)
	})
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := closed.URL
	closed.Close()
	failure = h.request(t, "POST", "/api/proxies/probe", proxyPayload("", strings.Replace(endpoint, "http://", "http://private-user:private-password@", 1)), 200, true)["test"].(map[string]any)
	reportTestFailure(t, failure["success"] != false, func() {
		t.Fatal("closed proxy reported connected")
	})
	raw, _ := json.Marshal(failure)
	reportTestFailure(t, strings.Contains(string(raw), "private-"), func() {
		t.Fatal("failure message exposed proxy credentials")
	})
}

func TestSavedProxyRealAccountRequestsSwitchExitIncludingPooledWebsocket(t *testing.T) {
	for _, mode := range []string{"sse", "websocket-cached"} {
		t.Run(mode, func(t *testing.T) {
			runSavedProxyRealAccountRequestsSwitchExitIncludingPooledWebsocketCase(t, &mode)
		})
	}
}

// serveLocalForwardProxyRequest exercises one isolated test phase with the original shared state.
func serveLocalForwardProxyRequest(w http.ResponseWriter, r *http.Request, capturedHits **atomic.Int64, capturedTargetURL **url.URL, capturedTransport **http.Transport) {
	if r.Host != (*capturedTargetURL).Host {
		http.Error(w, "only the local fake upstream is allowed", 403)
		return
	}
	(*capturedHits).Add(1)
	if r.Method == http.MethodConnect {
		upstreamConn, err := net.DialTimeout("tcp", (*capturedTargetURL).Host, time.Second)
		if err != nil {
			http.Error(w, "connect failed", 502)
			return
		}
		hijacker := w.(http.Hijacker)
		downstream, buffer, err := hijacker.Hijack()
		if err != nil {
			upstreamConn.Close()
			return
		}
		_, _ = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffer.Flush()
		go func() { _, _ = io.Copy(upstreamConn, buffer); _ = upstreamConn.Close() }()
		_, _ = io.Copy(downstream, upstreamConn)
		_ = downstream.Close()
		return
	}
	clone := r.Clone(r.Context())
	clone.RequestURI = ""
	clone.Header.Del("Proxy-Authorization")
	response, err := (*capturedTransport).RoundTrip(clone)
	if err != nil {
		http.Error(w, "forward failed", 502)
		return
	}
	defer response.Body.Close()
	for key, values := range response.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

// runSavedProxyRealAccountRequestsSwitchExitIncludingPooledWebsocketCase exercises one isolated test phase with the original shared state.
func runSavedProxyRealAccountRequestsSwitchExitIncludingPooledWebsocketCase(t *testing.T, capturedMode *string) {
	var requests atomic.Int64
	upgrader := websocket.Upgrader{}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveSavedProxyRealAccountRequestsSwitchExitIncludingPooledWebsocketRequest(w, r, &t, &requests, &upgrader)
	}))
	defer target.Close()
	h := newProxyHarness(t, target.URL, (*capturedMode))
	var firstHits, secondHits atomic.Int64
	first := localForwardProxy(t, target.URL, &firstHits, false)
	second := localForwardProxy(t, target.URL, &secondHits, false)
	made := h.request(t, "POST", "/api/proxies", proxyPayload("real selected proxy", first.URL), 201, true)
	id := savedProxyID(made)
	path := fmt.Sprintf("/api/proxies/%d", id)
	h.request(t, "PATCH", fmt.Sprintf("/api/accounts/%d", h.account.ID), fmt.Sprintf(`{"proxy_id":%d}`, id), 200, true)
	invoke := func() {
		t.Helper()
		req, _ := http.NewRequest("POST", h.server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.1-codex","input":"hi","prompt_cache_key":"same-session","stream":true}`))
		req.Header.Set("Authorization", "Bearer sk-local-api")
		req.Header.Set("Content-Type", "application/json")
		response, err := h.server.Client().Do(req)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		reportTestFailure(t, response.StatusCode != 200 || !strings.Contains(string(raw), "response.completed"), func() {
			t.Fatalf("real account request failed: %d %s", response.StatusCode, raw)
		})
	}
	invoke()
	reportTestFailure(t, firstHits.Load() != 1 || secondHits.Load() != 0, func() {
		t.Fatalf("account did not use selected proxy: first=%d second=%d", firstHits.Load(), secondHits.Load())
	})
	h.request(t, "PATCH", path, proxyPayload("new exit", second.URL), 200, true)
	invoke()
	reportTestFailure(t, firstHits.Load() != 1 || secondHits.Load() != 1, func() {
		t.Fatalf("changed proxy not used (old WS may have been reused): first=%d second=%d", firstHits.Load(), secondHits.Load())
	})
	h.request(t, "DELETE", path, "", 409, true)
	invoke()
	reportTestFailure(t, firstHits.Load() != 1 || secondHits.Load() < 1 || requests.Load() != 3, func() {
		t.Fatalf("protected deletion changed account egress: requests=%d", requests.Load())
	})
}

// serveSavedProxyRealAccountRequestsSwitchExitIncludingPooledWebsocketRequest exercises one isolated test phase with the original shared state.
func serveSavedProxyRealAccountRequestsSwitchExitIncludingPooledWebsocketRequest(w http.ResponseWriter, r *http.Request, capturedT **testing.T, capturedRequests *atomic.Int64, capturedUpgrader *websocket.Upgrader) {
	(*capturedT).Helper()

	reportTestFailure((*capturedT), r.Header.Get("Authorization") != "Bearer local-model-token", func() {
		(*capturedT).Errorf("wrong account token sent: %q", r.Header.Get("Authorization"))
	})
	if websocket.IsWebSocketUpgrade(r) {
		conn, err := (*capturedUpgrader).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			(*capturedRequests).Add(1)
			if err := conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "local-ws-response", "status": "completed"}}); err != nil {
				return
			}
		}
	}
	(*capturedRequests).Add(1)
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"local-response\",\"status\":\"completed\"}}\n\n")
}
