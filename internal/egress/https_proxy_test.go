package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func httpsProxyTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	return server, roots
}

func TestHTTPSProxyTunnelAuthBufferedDataAndLifetime(t *testing.T) {
	requests := make(chan *http.Request, 1)
	server, roots := httpsProxyTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests <- r
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		// Tunnel bytes can be buffered along with the CONNECT response.
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\nready")
		_, _ = io.Copy(conn, rw)
	})
	u, _ := url.Parse(server.URL)
	u.User = url.UserPassword("proxy-user", "secret:p@ss")
	factory := NewFactory(Options{RootCAs: roots, ConnectTimeout: 200 * time.Millisecond})
	cfg, err := factory.WSDialConfig(u.String())
	if err != nil || cfg.Proxy != nil || cfg.NetDialContext == nil {
		t.Fatalf("HTTPS must use a custom tunnel dialer: cfg=%+v err=%v", cfg, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := cfg.NetDialContext(ctx, "tcp", "target.invalid:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancel() // The completed dial must no longer be owned by this context.
	request := <-requests
	if request.Method != http.MethodConnect || request.Host != "target.invalid:443" || request.RequestURI != request.Host {
		t.Fatalf("wrong CONNECT target: method=%s host=%s uri=%s", request.Method, request.Host, request.RequestURI)
	}
	if request.Header.Get("Proxy-Authorization") != BasicAuthHeader(u) || request.Header.Get("Authorization") != "" {
		t.Fatal("proxy authentication missing or sent as upstream authorization")
	}
	if request.TLS == nil || request.TLS.NegotiatedProtocol != "http/1.1" {
		t.Fatalf("CONNECT did not use HTTP/1.1 over verified TLS: %+v", request.TLS)
	}
	ready := make([]byte, len("ready"))
	if _, err := io.ReadFull(conn, ready); err != nil || string(ready) != "ready" {
		t.Fatalf("buffered tunnel bytes lost: %q %v", ready, err)
	}
	// A dial deadline must not remain on a long-lived WebSocket tunnel.
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatalf("dial context or deadline closed a successful tunnel: %v", err)
	}
	data := make([]byte, 4)
	if _, err := io.ReadFull(conn, data); err != nil || string(data) != "ping" {
		t.Fatalf("tunnel echo failed: %q %v", data, err)
	}
}

func TestHTTPSProxyRejectsUntrustedCertificateAndWrongHostname(t *testing.T) {
	for _, wrongHost := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrong-host=%t", wrongHost), func(t *testing.T) {
			var connects atomic.Int32
			server, roots := httpsProxyTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				connects.Add(1)
				w.WriteHeader(http.StatusBadGateway)
			})
			u, _ := url.Parse(server.URL)
			u.User = url.UserPassword("private-user", "private-password")
			if !wrongHost {
				roots = x509.NewCertPool()
			}
			factory := NewFactory(Options{RootCAs: roots})
			if wrongHost {
				port := u.Port()
				u.Host = net.JoinHostPort("wrong-proxy.invalid", port)
				factory.BaseDialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
				}
			}
			cfg, err := factory.WSDialConfig(u.String())
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			conn, err := cfg.NetDialContext(context.Background(), "tcp", "upstream.invalid:443")
			if conn != nil {
				conn.Close()
				t.Fatal("unverified proxy was accepted")
			}
			var certErr *tls.CertificateVerificationError
			reportTestFailure(t, !errors.As(err, &certErr) || connects.Load() != 0, func() {
				t.Fatalf("certificate failure required before CONNECT: err=%v connects=%d", err, connects.Load())
			})
			reportTestFailure(t, strings.Contains(err.Error(), "private-"), func() {
				t.Fatal("credential leaked into certificate error")
			})
		})
	}
}

func TestHTTPSProxyRejectsCONNECTWithoutLeakingCredentials(t *testing.T) {
	const user, password = "private-user", "private-password"
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
	for _, response := range []string{
		"HTTP/1.1 407 " + user + ":" + password + "\r\nContent-Length: 999999\r\n\r\n" + auth,
		"HTTP/1.1 200 OK\r\n" + user + ":" + password + "\x00\r\n\r\n",
		"not-http " + auth + "\r\n\r\n",
	} {
		t.Run(strings.SplitN(response, "\r\n", 2)[0][:8], func(t *testing.T) { rejectCONNECTWithoutLeakingCredentials(t, response, user, password, auth) })
	}
}

func TestHTTPSProxyDeadlineAndCancellation(t *testing.T) {
	for _, phase := range []string{"tcp", "tls", "connect"} {
		for _, mode := range []string{"connect-timeout", "context-deadline", "cancel"} {
			t.Run(phase+"/"+mode, func(t *testing.T) { checkHTTPSProxyDeadlineAndCancellation(t, phase, mode) })
		}
	}
}

func TestWSDialConfigSOCKSDNSAndAuthUnchanged(t *testing.T) {
	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			defer listener.Close()
			result := make(chan error, 1)
			go func() {
				result <- serveSOCKSTestConnection(listener, scheme)
			}()
			cfg, err := NewFactory(Options{}).WSDialConfig(scheme + "://user:password@" + listener.Addr().String())
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			host := "localhost"
			if scheme == "socks5h" {
				host = "remote-only.invalid"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			conn, err := cfg.NetDialContext(ctx, "tcp", net.JoinHostPort(host, "443"))
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			conn.Close()
			{
				err := <-result
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
		})
	}
}

func serveSOCKSTestConnection(listener net.Listener, scheme string) error {
	conn, err := listener.Accept()
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	reader := bufio.NewReader(conn)
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(reader, greeting); err != nil {
		return err
	}
	if greeting[0] != 5 {
		return fmt.Errorf("not SOCKS5")
	}
	if _, err := io.CopyN(io.Discard, reader, int64(greeting[1])); err != nil {
		return err
	}
	_, _ = conn.Write([]byte{5, 2})
	version, _ := reader.ReadByte()
	user, err := readSOCKSString(reader)
	if err != nil {
		return err
	}
	password, err := readSOCKSString(reader)
	if err != nil || version != 1 || user != "user" || password != "password" {
		return fmt.Errorf("SOCKS authentication mismatch: %v", err)
	}
	_, _ = conn.Write([]byte{1, 0})
	host, err := readSOCKSHost(reader)
	if err != nil {
		return err
	}
	if scheme == "socks5" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) {
		return fmt.Errorf("socks5 did not resolve locally: %s", host)
	}
	if scheme == "socks5h" && host != "remote-only.invalid" {
		return fmt.Errorf("socks5h did not preserve remote hostname: %s", host)
	}
	if _, err := io.CopyN(io.Discard, reader, 2); err != nil {
		return err
	}
	_, err = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
	return err
}

func rejectCONNECTWithoutLeakingCredentials(t *testing.T, response, user, password, auth string) {
	closed := make(chan struct{})
	server, roots := httpsProxyTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		defer close(closed)
		_, _ = io.WriteString(conn, response)
		_, _ = io.Copy(io.Discard, conn)
	})
	u, _ := url.Parse(server.URL)
	u.User = url.UserPassword(user, password)
	factory := NewFactory(Options{RootCAs: roots, ConnectTimeout: time.Second})
	cfg, err := factory.WSDialConfig(u.String())
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	conn, err := cfg.NetDialContext(context.Background(), "tcp", "upstream.invalid:443")
	if conn != nil || err == nil {
		if conn != nil {
			conn.Close()
		}
		t.Fatalf("bad CONNECT accepted: %v", err)
	}
	for _, secret := range []string{user, password, auth, u.String()} {
		reportTestFailure(t, strings.Contains(err.Error(), secret), func() {
			t.Fatal("proxy response leaked credentials into error")
		})
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("failed CONNECT connection was not closed")
	}
}

func checkHTTPSProxyDeadlineAndCancellation(t *testing.T, phase, mode string) {
	started, closed := make(chan struct{}), make(chan struct{})
	rawProxy, roots := stalledHTTPSProxy(t, phase, started, closed)
	connectTimeout := 3 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	if mode == "connect-timeout" {
		connectTimeout = 150 * time.Millisecond
	} else if mode == "context-deadline" {
		cancel()
		ctx, cancel = context.WithTimeout(context.Background(), 150*time.Millisecond)
	}
	defer cancel()
	factory := NewFactory(Options{RootCAs: roots, ConnectTimeout: connectTimeout})
	if phase == "tcp" {
		factory.BaseDialer = func(ctx context.Context, _, _ string) (net.Conn, error) {
			close(started)
			<-ctx.Done()
			close(closed)
			return nil, ctx.Err()
		}
	}
	cfg, err := factory.WSDialConfig(rawProxy)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	result := make(chan error, 1)
	go func() {
		conn, err := cfg.NetDialContext(ctx, "tcp", "target.invalid:443")
		if conn != nil {
			conn.Close()
		}
		result <- err
	}()
	select {
	case <-started:
	case err := <-result:
		t.Fatalf("dial failed before reaching %s: %v", phase, err)
	case <-time.After(2 * time.Second):
		t.Fatal("dial never reached expected phase")
	}
	want := context.DeadlineExceeded
	if mode == "cancel" {
		cancel()
		want = context.Canceled
	}
	select {
	case err := <-result:
		reportTestFailure(t, !errors.Is(err, want), func() {
			t.Fatalf("got %v, want %v", err, want)
		})
	case <-time.After(time.Second):
		t.Fatal("deadline/cancellation did not interrupt dial")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("aborted dial leaked its connection")
	}
}

func readSOCKSString(reader *bufio.Reader) (string, error) {
	n, err := reader.ReadByte()
	if err != nil {
		return "", err
	}
	data := make([]byte, int(n))
	_, err = io.ReadFull(reader, data)
	return string(data), err
}

func readSOCKSHost(reader *bufio.Reader) (string, error) {
	var err error
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return "", err
	}
	var host string
	switch header[3] {
	case 1, 4:
		size := 4
		if header[3] == 4 {
			size = 16
		}
		ip := make(net.IP, size)
		if _, err := io.ReadFull(reader, ip); err != nil {
			return "", err
		}
		host = ip.String()
	case 3:
		host, err = readSOCKSString(reader)
	default:
		return "", fmt.Errorf("unknown SOCKS address type")
	}
	if err != nil {
		return "", err
	}
	return host, nil
}

func stalledHTTPSProxy(t *testing.T, phase string, started, closed chan struct{}) (string, *x509.CertPool) {
	rawProxy := "https://proxy.invalid:443"
	roots := x509.NewCertPool()
	if phase == "tls" {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
		t.Cleanup(func() { _ = listener.Close() })
		rawProxy = "https://" + listener.Addr().String()
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			defer close(closed)
			close(started)
			_, _ = io.Copy(io.Discard, conn)
		}()
	} else if phase == "connect" {
		server, pool := httpsProxyTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			defer close(closed)
			close(started)
			_, _ = io.Copy(io.Discard, conn)
		})
		rawProxy, roots = server.URL, pool
	}
	return rawProxy, roots
}
