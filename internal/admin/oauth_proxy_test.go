package admin

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pi-gateway/internal/accounts"
	"pi-gateway/internal/oauth"
	"pi-gateway/internal/store"
)

type rejectSharedOAuthTransport struct{}

func (rejectSharedOAuthTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("shared OAuth transport must not be used for per-flow egress")
}

func localOAuthJWT(claims map[string]any) string {
	data, _ := json.Marshal(claims)
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(data) + ".sig"
}

// The CONNECT destination is always a local TLS stub, never the requested
// public OAuth host. This proves real factory/proxy traffic without credentials
// or socket connections reaching an external service.
func localOAuthTunnel(t *testing.T, target string, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	targetURL, _ := url.Parse(target)
	var tunnels sync.WaitGroup
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tunnels.Add(1)
		defer tunnels.Done()
		if r.Method != http.MethodConnect || (r.Host != "auth.openai.com:443" && r.Host != "auth0.openai.com:443") {
			http.Error(w, "test only permits OAuth CONNECT", 403)
			return
		}
		remote, err := net.DialTimeout("tcp", targetURL.Host, time.Second)
		if err != nil {
			http.Error(w, "local connect failed", 502)
			return
		}
		downstream, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			remote.Close()
			return
		}
		hits.Add(1)
		_, _ = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffer.Flush()
		copyDone := make(chan struct{})
		go func() {
			defer close(copyDone)
			_, _ = io.Copy(remote, buffer)
			remote.Close()
		}()
		_, _ = io.Copy(downstream, remote)
		downstream.Close()
		<-copyDone
	}))
	t.Cleanup(func() {
		server.Close()
		tunnels.Wait() // httptest.Close itself cannot wait for hijacked connections.
	})
	return server
}

func TestOAuthFlowKeepAndExplicitProxyUseRealTokenTransport(t *testing.T) {
	for _, kind := range []string{oauth.FlowKindCodex, oauth.FlowKindChatGPT} {
		for _, choice := range []string{"keep_saved", "keep_manual", "direct_id", "direct_url", "manual", "saved"} {
			t.Run(kind+"/"+choice, func(t *testing.T) { runOAuthFlowKeepAndExplicitProxyUseRealTokenTransportCase(t, &kind, &choice) })
		}
	}
}

func TestOAuthKeepRejectsMissingAccountAndRefreshNeverFallsBackToDirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid proxy must not make a token request") }))
	defer target.Close()
	h := newProxyHarness(t, target.URL, "sse")
	h.request(t, "POST", "/api/oauth/start", `{"kind":"codex","account_id":"999999"}`, 404, true)
	h.request(t, "POST", "/api/oauth/start", `{"kind":"codex","account_id":"not-an-id"}`, 400, true)
	var dials atomic.Int64
	h.factory.BaseDialer = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("test blocks all network")
	}
	a := *h.account
	a.ProxyURL = "invalid-proxy"
	a.OAuthClientID = "issued-local-client"
	a.RefreshToken = "local-refresh"
	{
		err := h.admin.accounts.Refresh(context.Background(), &a)
		reportTestFailure(t, err == nil, func() {
			t.Fatal("invalid proxy refresh unexpectedly succeeded")
		})
	}
	reportTestFailure(t, dials.Load() != 0, func() {
		t.Fatalf("invalid proxy fell back to a network dial: %d", dials.Load())
	})
	reportTestFailure(t, !strings.Contains(a.LastError, "proxy"), func() {
		t.Fatalf("missing proxy error: %q", a.LastError)
	})
	a.CodexAccessToken = "local-codex-access"
	a.CodexRefreshToken = "local-codex-refresh"
	a.CodexAccountID = "local-codex-account"
	a.CodexExpiresAt = 1
	{
		_, _, _, err := h.admin.accounts.EnsureFreshCodexToken(context.Background(), &a)
		reportTestFailure(t, err == nil || !strings.Contains(err.Error(), "proxy"), func() {
			t.Fatalf("invalid proxy Codex refresh did not fail closed: %v", err)
		})
	}
	reportTestFailure(t, dials.Load() != 0, func() {
		t.Fatalf("invalid proxy Codex refresh fell back to a network dial: %d", dials.Load())
	})
}

// runOAuthFlowKeepAndExplicitProxyUseRealTokenTransportCase exercises one isolated test phase with the original shared state.
func runOAuthFlowKeepAndExplicitProxyUseRealTokenTransportCase(t *testing.T, capturedKind *string, capturedChoice *string) {
	t.Helper()

	ctx := context.Background()
	var tokenCalls, directDials, firstDials, secondDials atomic.Int64
	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveOAuthFlowKeepAndExplicitProxyUseRealTokenTransportRequest(w, r, &t, &tokenCalls)
	}))
	t.Cleanup(tokenServer.Close)
	h := newProxyHarness(t, tokenServer.URL, "sse")
	first := localOAuthTunnel(t, tokenServer.URL, &firstDials)
	second := localOAuthTunnel(t, tokenServer.URL, &secondDials)
	target, _ := url.Parse(tokenServer.URL)
	// Even explicit direct test traffic is forcibly routed to the local stub.
	// Any other non-loopback dial is rejected instead of reaching the Internet.
	h.factory.BaseDialer = func(ctx context.Context, network, address string) (net.Conn, error) {
		return verifyOAuthFlowKeepAndExplicitProxyUseRealTokenTransport(ctx, network, address, &directDials, &target)
	}
	roots := x509.NewCertPool()
	roots.AddCert(tokenServer.Certificate())
	serverName := "127.0.0.1"
	if len(tokenServer.Certificate().DNSNames) > 0 {
		serverName = tokenServer.Certificate().DNSNames[0]
	}
	for _, endpoint := range []string{"", first.URL, second.URL} {
		client, err := h.factory.HTTPClient(endpoint)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
		client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: roots, ServerName: serverName, MinVersion: tls.VersionTLS12}
		t.Cleanup(client.CloseIdleConnections)
	}
	shared := oauth.NewClient(&http.Client{Transport: rejectSharedOAuthTransport{}})
	manager := accounts.New(h.db, shared, h.factory, accounts.Options{Logger: h.admin.logger})
	h.admin.accounts = manager
	h.account.AccountID = "chatgpt:local@example.test"
	h.account.OAuthClientID = "issued-local-client"
	h.account.RefreshToken = "local-refresh"
	{
		err := h.db.UpdateAccount(ctx, h.account)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	oldProxy := &store.Proxy{Name: "existing", URL: first.URL}
	newProxy := &store.Proxy{Name: "selected", URL: second.URL}
	for _, p := range []*store.Proxy{oldProxy, newProxy} {
		{
			err := h.db.CreateProxy(ctx, p)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
	}
	oldID := oldProxy.ID
	if (*capturedChoice) == "keep_manual" {
		oldID = 0
	}
	{
		err := h.db.SetAccountProxy(ctx, h.account.ID, oldID, first.URL)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	flows := oauth.NewFlowManager(shared, oauth.FlowOptions{
		CallbackHost: "127.0.0.1", CallbackPort: 0, Timeout: time.Second, DeviceID: oauth.NewDeviceUUID, Logger: h.admin.logger,
		OnComplete: func(flow *oauth.Flow, token *oauth.Token) error {
			return verifyOAuthFlowKeepAndExplicitProxyUseRealTokenTransport2(flow, token, &ctx, &h, &manager)
		},
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		{
			err := flows.Close(ctx)
			reportTestFailure(t, err != nil, func() {
				t.Errorf("close OAuth flow manager: %v", err)
			})
		}
	})
	h.admin.flows = flows
	h.admin.bindOAuthProxy()
	payload := map[string]any{"kind": (*capturedKind), "account_id": strconv.FormatInt(h.account.ID, 10)}
	expectedURL := first.URL
	switch *capturedChoice {
	case "direct_id":
		payload["proxy_id"] = nil
		expectedURL = ""
	case "direct_url":
		payload["proxy_url"] = ""
		expectedURL = ""
	case "manual":
		payload["proxy_id"] = nil
		payload["proxy_url"] = second.URL
		expectedURL = second.URL
	case "saved":
		payload["proxy_id"] = newProxy.ID
		payload["proxy_url"] = "ignored-invalid-url"
		expectedURL = second.URL
	}
	raw, _ := json.Marshal(payload)
	started := h.request(t, "POST", "/api/oauth/start", string(raw), 200, true)["flow"].(map[string]any)
	flowID := started["id"].(string)
	{
		got := flows.Get(flowID).Meta["proxy_url"]
		reportTestFailure(t, got != expectedURL, func() {
			t.Fatalf("wrong effective flow proxy: %q want %q", got, expectedURL)
		})
	}
	authorize, _ := url.Parse(started["auth_url"].(string))
	callback, _ := url.Parse(started["redirect_uri"].(string))
	query := callback.Query()
	query.Set("code", "local-code")
	query.Set("state", authorize.Query().Get("state"))
	query.Set("client_id", "issued-local-client")
	callback.RawQuery = query.Encode()
	input, _ := json.Marshal(map[string]string{"input": callback.String()})
	completed := h.request(t, "POST", "/api/oauth/"+flowID+"/complete", string(input), 200, true)
	finished := completed["flow"].(map[string]any)
	reportTestFailure(t, finished["status"] != oauth.FlowCompleted, func() {
		t.Fatalf("OAuth failed: %+v", finished)
	})
	reportTestFailure(t, finished["db_account_id"] != float64(h.account.ID), func() {
		t.Fatalf("completed flow lost persisted account id: %+v", finished)
	})
	a, err := h.db.GetAccount(ctx, h.account.ID)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	reportTestFailure(t, a.ProxyURL != expectedURL, func() {
		t.Fatalf("completion persisted wrong proxy: %q want %q", a.ProxyURL, expectedURL)
	})
	reportTestFailure(t, strings.HasPrefix((*capturedChoice), "keep") && ((oldID == 0 && a.ProxyID != nil) || (oldID > 0 && (a.ProxyID == nil || *a.ProxyID != oldID))), func() {
		t.Fatal("keep altered existing proxy binding")
	})
	// Refresh must use exactly the same selected exit, not the shared direct client.
	if (*capturedKind) == oauth.FlowKindCodex {
		{
			err := h.db.SaveCodexCredential(ctx, a.ID, a.CodexAccessToken, a.CodexRefreshToken, a.CodexIDToken, 1, a.CodexAccountID)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		a, err = h.db.GetAccount(ctx, a.ID)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
		{
			_, _, _, err := manager.EnsureFreshCodexToken(ctx, a)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
	} else {
		err := manager.Refresh(ctx, a)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, tokenCalls.Load() != 2, func() {
		t.Fatalf("exchange and refresh did not both reach local token server: %d", tokenCalls.Load())
	})
	switch expectedURL {
	case first.URL:
		reportTestFailure(t, firstDials.Load() < 1 || secondDials.Load() != 0 || directDials.Load() != 0, func() {
			t.Fatalf("keep bypassed old proxy: old=%d new=%d direct=%d", firstDials.Load(), secondDials.Load(), directDials.Load())
		})
	case second.URL:
		reportTestFailure(t, secondDials.Load() < 1 || firstDials.Load() != 0 || directDials.Load() != 0, func() {
			t.Fatalf("explicit proxy choice ignored: old=%d new=%d direct=%d", firstDials.Load(), secondDials.Load(), directDials.Load())
		})
	case "":
		reportTestFailure(t, directDials.Load() < 1 || firstDials.Load() != 0 || secondDials.Load() != 0, func() {
			t.Fatalf("explicit direct inherited a proxy: old=%d new=%d direct=%d", firstDials.Load(), secondDials.Load(), directDials.Load())
		})
	}
}

// serveOAuthFlowKeepAndExplicitProxyUseRealTokenTransportRequest exercises one isolated test phase with the original shared state.
func serveOAuthFlowKeepAndExplicitProxyUseRealTokenTransportRequest(w http.ResponseWriter, r *http.Request, capturedT **testing.T, capturedTokenCalls *atomic.Int64) {
	(*capturedT).Helper()

	(*capturedTokenCalls).Add(1)
	reportTestFailure((*capturedT), r.Method != "POST", func() {
		(*capturedT).Error("token request was not POST")
	})
	if err := r.ParseForm(); err != nil {
		(*capturedT).Error(err)
		return
	}
	reportTestFailure((*capturedT), r.Form.Get("grant_type") != "authorization_code" && r.Form.Get("grant_type") != "refresh_token", func() {
		(*capturedT).Errorf("unexpected grant: %q", r.Form.Get("grant_type"))
	})
	access := "local-chatgpt-access"
	if r.Form.Get("client_id") == oauth.ClientID {
		access = localOAuthJWT(map[string]any{oauth.JWTClaimPath: map[string]any{"chatgpt_account_id": "local-codex-account"}})
	}
	writeJSON(w, 200, map[string]any{"access_token": access, "refresh_token": "local-refresh", "id_token": localOAuthJWT(map[string]any{"email": "local@example.test"}), "expires_in": 3600, "scope": oauth.ChatGPTDirectTokenScope})
}

// verifyOAuthFlowKeepAndExplicitProxyUseRealTokenTransport exercises one isolated test phase with the original shared state.
func verifyOAuthFlowKeepAndExplicitProxyUseRealTokenTransport(ctx context.Context, network, address string, capturedDirectDials *atomic.Int64, capturedTarget **url.URL) (net.Conn, error) {
	if address == "auth.openai.com:443" || address == "auth0.openai.com:443" {
		(*capturedDirectDials).Add(1)
		address = (*capturedTarget).Host
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return nil, errors.New("test rejected non-local network destination")
	}
	return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, address)
}

// verifyOAuthFlowKeepAndExplicitProxyUseRealTokenTransport2 exercises one isolated test phase with the original shared state.
func verifyOAuthFlowKeepAndExplicitProxyUseRealTokenTransport2(flow *oauth.Flow, token *oauth.Token, capturedCtx *context.Context, capturedH **proxyHarness, capturedManager **accounts.Manager) error {
	if flow.Kind == oauth.FlowKindCodex {
		a, err := (*capturedH).db.GetAccount((*capturedCtx), (*capturedH).account.ID)
		if err != nil {
			return err
		}
		if err := (*capturedManager).LinkCodexCredential((*capturedCtx), a, token); err != nil {
			return err
		}
		flow.DBAccountID = a.ID
		return nil
	}
	a, err := (*capturedManager).CreateFromToken((*capturedCtx), "reauthorized", token, flow.Meta["proxy_url"])
	if err == nil {
		flow.DBAccountID = a.ID
	}
	return err
}
