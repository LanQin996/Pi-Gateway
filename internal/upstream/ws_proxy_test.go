package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pi-gateway/internal/egress"

	"github.com/gorilla/websocket"
)

type wsProxyFixture struct {
	server   *httptest.Server
	requests chan *http.Request
	connects atomic.Int32
}

// This is a real TCP CONNECT forwarder; it does not terminate the target TLS.
func newWSProxyFixture(t *testing.T, secure bool, forwardAddr string) *wsProxyFixture {
	t.Helper()
	fixture := &wsProxyFixture{requests: make(chan *http.Request, 32)}
	var handlers sync.WaitGroup
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		if r.Method != http.MethodConnect {
			t.Errorf("proxy received %s, want CONNECT", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		fixture.connects.Add(1)
		fixture.requests <- r
		addr := r.Host
		if forwardAddr != "" {
			addr = forwardAddr
		}
		// This fixture only forwards to local test servers, never arbitrary destinations.
		host, _, splitErr := net.SplitHostPort(addr)
		ip, parseErr := netip.ParseAddr(host)
		if splitErr != nil || parseErr != nil || !ip.IsLoopback() {
			http.Error(w, "only loopback test targets are allowed", http.StatusForbidden)
			return
		}
		target, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			http.Error(w, "target unavailable", http.StatusBadGateway)
			return
		}
		defer target.Close()
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(target, rw)
			_ = target.Close()
			close(done)
		}()
		_, _ = io.Copy(conn, target)
		_ = conn.Close()
		<-done
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	if secure {
		server.StartTLS()
	} else {
		server.Start()
	}
	fixture.server = server
	t.Cleanup(func() {
		server.Close()
		handlers.Wait()
	})
	return fixture
}

func newWSProxyTarget(t *testing.T, secure bool) (*httptest.Server, *atomic.Int32, <-chan http.Header, <-chan []byte) {
	t.Helper()
	handshakes := &atomic.Int32{}
	headers := make(chan http.Header, 32)
	frames := make(chan []byte, 32)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakes.Add(1)
		headers <- r.Header.Clone()
		socket, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer socket.Close()
		for {
			_ = socket.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, payload, err := socket.ReadMessage()
			if err != nil {
				return
			}
			frames <- payload
			if err := socket.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-proxy","status":"completed","output":[]}}`)); err != nil {
				return
			}
		}
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	if secure {
		server.StartTLS()
	} else {
		server.Start()
	}
	t.Cleanup(server.Close)
	return server, handshakes, headers, frames
}

func wsProxyURL(t *testing.T, endpoint, password string) string {
	t.Helper()
	u, err := url.Parse(endpoint)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	u.User = url.UserPassword("proxy-user", password)
	return u.String()
}

func TestWSProxyRoundTripAndAuthenticationIsolation(t *testing.T) {
	for _, scheme := range []string{"direct", "http", "https"} {
		for _, secureTarget := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/wss=%t", scheme, secureTarget), func(t *testing.T) { runWSProxyRoundTripAndAuthenticationIsolationCase(t, &scheme, &secureTarget) })
		}
	}
}

func TestWSHTTPSProxyStillVerifiesTargetCertificate(t *testing.T) {
	target, handshakes, _, _ := newWSProxyTarget(t, true)
	proxy := newWSProxyFixture(t, true, target.Listener.Addr().String())
	roots := x509.NewCertPool()
	roots.AddCert(proxy.server.Certificate())
	roots.AddCert(target.Certificate())
	_, port, _ := net.SplitHostPort(target.Listener.Addr().String())
	c := New(Config{WSURL: "wss://" + net.JoinHostPort("wrong-target.invalid", port)}, egress.NewFactory(egress.Options{RootCAs: roots}))
	t.Cleanup(c.Close)
	conn, _, err := c.dialWS(context.Background(), &Request{ProxyURL: wsProxyURL(t, proxy.server.URL, "private-password")})
	if conn != nil {
		conn.Close()
		t.Fatal("target with wrong certificate was accepted")
	}
	var certErr *tls.CertificateVerificationError
	var hostnameErr x509.HostnameError
	reportTestFailure(t, !errors.As(err, &certErr) || !errors.As(err, &hostnameErr) || proxy.connects.Load() != 1 || handshakes.Load() != 0, func() {
		t.Fatalf("target verification bypassed: error=%v connects=%d handshakes=%d", err, proxy.connects.Load(), handshakes.Load())
	})
	reportTestFailure(t, strings.Contains(err.Error(), "private-password"), func() {
		t.Fatal("target TLS error contains proxy password")
	})
}

func TestWSHTTPSProxyPoolFingerprintIsolation(t *testing.T) {
	target, handshakes, _, _ := newWSProxyTarget(t, true)
	proxyA := newWSProxyFixture(t, true, "")
	proxyB := newWSProxyFixture(t, true, "")
	roots := x509.NewCertPool()
	roots.AddCert(target.Certificate())
	roots.AddCert(proxyA.server.Certificate())
	roots.AddCert(proxyB.server.Certificate())
	c := New(Config{WSURL: "wss" + strings.TrimPrefix(target.URL, "https"), Pool: true, ConnectTimeout: time.Second, IdleTimeout: 5 * time.Second}, egress.NewFactory(egress.Options{RootCAs: roots}))
	t.Cleanup(c.Close)
	a1 := wsProxyURL(t, proxyA.server.URL, "password-one")
	a2 := wsProxyURL(t, proxyA.server.URL, "password-two")
	b := wsProxyURL(t, proxyB.server.URL, "password-one")
	// Same session/account, switching exit and credentials, with direct mixed in.
	for _, proxyURL := range []string{a1, a1, b, b, "", "", a2, a2, a1} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := c.Stream(ctx, &Request{
			Transport: "websocket-cached", ProxyURL: proxyURL, SessionID: "proxy-session", PoolSessionID: "proxy-internal", AccountID: 17, ClientKeyID: 1,
			Body: []byte(`{"model":"test","input":[]}`),
		}, func(*Event) error { return nil })
		cancel() // A finished handshake/request must not close its pooled socket.
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, handshakes.Load() != 4 || proxyA.connects.Load() != 2 || proxyB.connects.Load() != 1 || c.PoolSize() != 4, func() {
		t.Fatalf("exit or credentials reused incorrectly: handshakes=%d proxyA=%d proxyB=%d pool=%d", handshakes.Load(), proxyA.connects.Load(), proxyB.connects.Load(), c.PoolSize())
	})
	c.pool.mu.Lock()
	defer c.pool.mu.Unlock()
	for _, proxyURL := range []string{a1, a2, b, ""} {
		key := c.requestPoolKey(&Request{ClientKeyID: 1, SessionID: "proxy-session", PoolSessionID: "proxy-internal", AccountID: 17, ProxyURL: proxyURL})
		reportTestFailure(t, c.pool.entries[key] == nil, func() {
			t.Fatal("proxy fingerprint missing from connection pool key")
		})
	}
	for key := range c.pool.entries {
		reportTestFailure(t, strings.Contains(key, "password-") || strings.Contains(key, "proxy-user") || strings.Contains(key, "https://"), func() {
			t.Fatal("raw proxy credentials exposed in pool key")
		})
	}
}

func TestWSHTTPSProxyHandshakeDeadlineAndCancellation(t *testing.T) {
	for _, phase := range []string{"target-tls", "ws-upgrade", "wss-upgrade"} {
		for _, mode := range []string{"handshake-timeout", "context-deadline", "cancel"} {
			t.Run(phase+"/"+mode, func(t *testing.T) { runWSHTTPSProxyHandshakeDeadlineAndCancellationCase(t, &phase, &mode) })
		}
	}
}

// runWSProxyRoundTripAndAuthenticationIsolationCase exercises one isolated test phase with the original shared state.
func runWSProxyRoundTripAndAuthenticationIsolationCase(t *testing.T, capturedScheme *string, capturedSecureTarget *bool) {
	t.Helper()

	target, handshakes, headers, frames := newWSProxyTarget(t, (*capturedSecureTarget))
	roots := x509.NewCertPool()
	if *capturedSecureTarget {
		roots.AddCert(target.Certificate())
	}
	var proxy *wsProxyFixture
	rawProxy := ""
	if (*capturedScheme) != "direct" {
		proxy = newWSProxyFixture(t, (*capturedScheme) == "https", "")
		rawProxy = wsProxyURL(t, proxy.server.URL, "p@ss:word")
		if (*capturedScheme) == "https" {
			roots.AddCert(proxy.server.Certificate())
		}
	}
	c := New(Config{WSURL: "ws" + strings.TrimPrefix(target.URL, "http") + "/responses?route=proxy", ConnectTimeout: time.Second}, egress.NewFactory(egress.Options{RootCAs: roots}))
	t.Cleanup(c.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	events := 0
	res, err := c.Stream(ctx, &Request{
		ProxyURL: rawProxy, Transport: "websocket", Body: []byte(`{"model":"test","input":[]}`),
		WSHeaders: http.Header{
			"Authorization":       []string{"Bearer upstream-only"},
			"pRoXy-AuThOrIzAtIoN": []string{"Basic must-not-reach-target"},
		},
	}, func(event *Event) error {
		reportTestFailure(t, event.Type != "response.completed", func() {
			t.Errorf("unexpected event %q", event.Type)
		})
		events++
		return nil
	})
	reportTestFailure(t, err != nil || res == nil || res.Status != http.StatusSwitchingProtocols || events != 1 || handshakes.Load() != 1, func() {
		t.Fatalf("WS round trip failed: result=%+v err=%v events=%d handshakes=%d", res, err, events, handshakes.Load())
	})
	header := <-headers
	reportTestFailure(t, header.Get("Authorization") != "Bearer upstream-only" || header.Get("Proxy-Authorization") != "", func() {
		t.Fatal("upstream/proxy authorization boundaries were not preserved")
	})
	var frame map[string]any
	{
		err := json.Unmarshal(<-frames, &frame)
		reportTestFailure(t, err != nil || frame["type"] != "response.create" || frame["model"] != "test", func() {
			t.Fatalf("wrong tunneled WS frame: %v %v", frame, err)
		})
	}
	if proxy != nil {
		r := <-proxy.requests
		u, _ := url.Parse(rawProxy)
		reportTestFailure(t, r.Host != target.Listener.Addr().String() || r.Header.Get("Proxy-Authorization") != egress.BasicAuthHeader(u) || r.Header.Get("Authorization") != "", func() {
			t.Fatal("CONNECT target or authentication incorrect")
		})
		reportTestFailure(t, (*capturedScheme) == "https" && r.TLS == nil, func() {
			t.Fatal("HTTPS proxy CONNECT used plaintext")
		})
	}
}

// runWSHTTPSProxyHandshakeDeadlineAndCancellationCase exercises one isolated test phase with the original shared state.
func runWSHTTPSProxyHandshakeDeadlineAndCancellationCase(t *testing.T, capturedPhase *string, capturedMode *string) {
	t.Helper()

	started, closed := make(chan struct{}), make(chan struct{})
	roots := x509.NewCertPool()
	var targetURL string
	func() {
		verifyWSHTTPSProxyHandshakeDeadlineAndCancellation(&t, &capturedPhase, &started, &closed, &roots, &targetURL)
	}()
	proxy := newWSProxyFixture(t, true, "")
	roots.AddCert(proxy.server.Certificate())
	handshakeTimeout := 3 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	if (*capturedMode) == "handshake-timeout" {
		handshakeTimeout = 150 * time.Millisecond
	} else if (*capturedMode) == "context-deadline" {
		cancel()
		ctx, cancel = context.WithTimeout(context.Background(), 150*time.Millisecond)
	}
	defer cancel()
	c := New(Config{WSURL: targetURL, ConnectTimeout: handshakeTimeout}, egress.NewFactory(egress.Options{RootCAs: roots}))
	t.Cleanup(c.Close)
	result := make(chan error, 1)
	go func() {
		conn, _, err := c.dialWS(ctx, &Request{ProxyURL: proxy.server.URL})
		if conn != nil {
			conn.Close()
		}
		result <- err
	}()
	select {
	case <-started:
	case err := <-result:
		t.Fatalf("handshake failed before %s: %v", (*capturedPhase), err)
	case <-time.After(2 * time.Second):
		t.Fatal("handshake did not reach target")
	}
	if (*capturedMode) == "cancel" {
		cancel()
	}
	select {
	case err := <-result:
		var timeout net.Error
		if (*capturedMode) == "cancel" {
			reportTestFailure(t, !errors.Is(err, context.Canceled), func() {
				t.Fatalf("cancellation not preserved: %v", err)
			})
		} else {
			reportTestFailure(t, !errors.Is(err, context.DeadlineExceeded) && !(errors.As(err, &timeout) && timeout.Timeout()), func() {
				t.Fatalf("handshake timeout not preserved: %v", err)
			})
		}
	case <-time.After(time.Second):
		t.Fatal("handshake cancellation/deadline did not close tunnel")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("aborted handshake leaked target connection")
	}
}

// verifyWSHTTPSProxyHandshakeDeadlineAndCancellation exercises one isolated test phase with the original shared state.
func verifyWSHTTPSProxyHandshakeDeadlineAndCancellation(capturedT **testing.T, capturedCapturedPhase **string, capturedStarted *chan struct{}, capturedClosed *chan struct{}, capturedRoots **x509.CertPool, capturedTargetURL *string) {
	(*capturedT).Helper()

	if (*(*capturedCapturedPhase)) == "target-tls" {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		reportTestFailure((*capturedT), err != nil, func() {
			(*capturedT).Fatal(err)
		})
		(*capturedT).Cleanup(func() { _ = listener.Close() })
		(*capturedTargetURL) = "wss://" + listener.Addr().String()
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			defer close((*capturedClosed))
			close((*capturedStarted))
			_, _ = io.Copy(io.Discard, conn)
		}()
	} else {
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				(*capturedT).Error(err)
				return
			}
			defer conn.Close()
			defer close((*capturedClosed))
			close((*capturedStarted))
			_, _ = io.Copy(io.Discard, conn)
		}))
		server.Config.ErrorLog = log.New(io.Discard, "", 0)
		if (*(*capturedCapturedPhase)) == "wss-upgrade" {
			server.StartTLS()
			(*capturedRoots).AddCert(server.Certificate())
		} else {
			server.Start()
		}
		(*capturedT).Cleanup(server.Close)
		(*capturedTargetURL) = "ws" + strings.TrimPrefix(server.URL, "http")
	}
}

// Rejected destinations must never reach the fixture's TCP forwarding path.
func TestWSProxyFixtureRejectsNonLoopbackTargets(t *testing.T) {
	fixture := newWSProxyFixture(t, false, "")
	for _, target := range []string{"192.0.2.1:80", "example.invalid:80", "[2001:db8::1]:80", "localhost:80", "invalid-address"} {
		t.Run(target, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodConnect, fixture.server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = target
			response, err := fixture.server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "only loopback test targets are allowed") {
				t.Fatalf("unsafe CONNECT target %q: status=%d body=%s", target, response.StatusCode, body)
			}
		})
	}
}
