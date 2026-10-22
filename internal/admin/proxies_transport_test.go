package admin

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
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

	"pi-gateway/internal/oauth"
	"pi-gateway/internal/store"
)

// This tiny SOCKS server is test-only: production uses the existing egress
// factory and golang.org/x/net/proxy. It refuses every non-local test target.
func localSOCKSProxy(t *testing.T, target string, hits *atomic.Int64) string {
	t.Helper()
	targetURL, _ := url.Parse(target)
	targetHost, targetPort, _ := net.SplitHostPort(targetURL.Host)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	var mu sync.Mutex
	active := map[net.Conn]struct{}{}
	var workers sync.WaitGroup
	t.Cleanup(func() {
		listener.Close()
		mu.Lock()
		for conn := range active {
			conn.Close()
		}
		mu.Unlock()
		workers.Wait()
	})
	workers.Add(1)
	go func() {
		serveLocalSOCKSListener(serveLocalSOCKSListenerState{capturedHits: &hits, capturedTargetURL: &targetURL, capturedTargetHost: &targetHost, capturedTargetPort: &targetPort, capturedListener: &listener, capturedMu: &mu, capturedActive: &active, capturedWorkers: &workers})
	}()
	return listener.Addr().String()
}

func TestSavedProxyAuthenticatedSOCKS5AndSOCKS5H(t *testing.T) {
	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		reportTestFailure(t, r.Header.Get("Authorization") != "" || r.Header.Get("Proxy-Authorization") != "", func() {
			t.Error("probe target received credentials")
		})
		w.WriteHeader(204)
	}))
	defer target.Close()
	h := newProxyHarness(t, target.URL, "sse")
	var socksHits atomic.Int64
	address := localSOCKSProxy(t, target.URL, &socksHits)
	for _, scheme := range []string{"socks5", "socks5h"} {
		endpoint := scheme + "://local-user:local-secret@" + address
		result := h.request(t, "POST", "/api/proxies", proxyPayload(scheme, endpoint), 201, true)
		id := savedProxyID(result)
		test := h.request(t, "POST", fmt.Sprintf("/api/proxies/%d/test", id), "", 200, true)["test"].(map[string]any)
		reportTestFailure(t, test["success"] != true || test["status"] != float64(204), func() {
			t.Fatalf("%s did not reach fake upstream through authenticated SOCKS: %+v", scheme, test)
		})
	}
	reportTestFailure(t, socksHits.Load() != 2 || targetHits.Load() != 2, func() {
		t.Fatalf("SOCKS or target bypassed: proxy=%d target=%d", socksHits.Load(), targetHits.Load())
	})
}

func TestSavedProxyOAuthCompletionPersistsLatestBindingAndDoesNotExposeMeta(t *testing.T) {
	ctx := context.Background()
	db := newAdminTestStore(t)
	p := &store.Proxy{Name: "oauth proxy", URL: "http://user:old-secret@127.0.0.1:1234"}
	{
		err := db.CreateProxy(ctx, p)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	var persisted int
	manager := oauth.NewFlowManager(nil, oauth.FlowOptions{OnComplete: func(flow *oauth.Flow, _ *oauth.Token) error {
		persisted++
		if flow.DBAccountID > 0 {
			return nil
		} // Reauthorization of an existing row.
		a := &store.Account{Name: "new oauth account", Enabled: true, ProxyURL: flow.Meta["proxy_url"]}
		if err := db.CreateAccount(ctx, a); err != nil {
			return err
		}
		flow.DBAccountID = a.ID
		return nil
	}})
	s := &Server{store: db, flows: manager}
	s.bindOAuthProxy()
	// Intercept the same callback used by OAuth finish, without any external login.
	var completion func(*oauth.Flow, *oauth.Token) error
	manager.WrapCompletion(func(next func(*oauth.Flow, *oauth.Token) error) func(*oauth.Flow, *oauth.Token) error {
		completion = next
		return next
	})
	p.URL = "http://user:updated-secret@127.0.0.1:1235"
	{
		err := db.UpdateProxy(ctx, p)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	flow := &oauth.Flow{Meta: map[string]string{"proxy_id": strconv.FormatInt(p.ID, 10), "proxy_url": "http://old:old-secret@127.0.0.1:1234"}}
	{
		err := completion(flow, &oauth.Token{})
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	a, err := db.GetAccount(ctx, flow.DBAccountID)
	reportTestFailure(t, err != nil || a == nil || a.ProxyID == nil || *a.ProxyID != p.ID || a.ProxyURL != p.URL, func() {
		t.Fatalf("OAuth proxy not persisted: %+v %v", a, err)
	})
	raw, _ := json.Marshal(flow)
	reportTestFailure(t, strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "proxy_url"), func() {
		t.Fatalf("flow metadata exposed proxy credentials: %s", raw)
	})
	flow.Meta["proxy_id"] = "999999"
	{
		err := completion(flow, &oauth.Token{})
		reportTestFailure(t, err == nil || persisted != 1, func() {
			t.Fatal("missing proxy should fail before the account persistence callback")
		})
	}
	flow.Meta["proxy_id"] = "null"
	flow.Meta["proxy_url"] = ""
	{
		err := completion(flow, &oauth.Token{})
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	a, err = db.GetAccount(ctx, flow.DBAccountID)
	reportTestFailure(t, err != nil || a.ProxyID != nil || a.ProxyURL != "", func() {
		t.Fatalf("explicit direct OAuth selection retained a saved proxy: %+v %v", a, err)
	})
}

type serveLocalSOCKSListenerState struct {
	capturedHits       **atomic.Int64
	capturedTargetURL  **url.URL
	capturedTargetHost *string
	capturedTargetPort *string
	capturedListener   *net.Listener
	capturedMu         *sync.Mutex
	capturedActive     *map[net.Conn]struct{}
	capturedWorkers    *sync.WaitGroup
}

// serveLocalSOCKSListener exercises one isolated test phase with the original shared state.
func serveLocalSOCKSListener(state serveLocalSOCKSListenerState) {
	defer (*state.capturedWorkers).Done()
	for {
		conn, err := (*state.capturedListener).Accept()
		if err != nil {
			return
		}
		(*state.capturedMu).Lock()
		(*state.capturedActive)[conn] = struct{}{}
		(*state.capturedMu).Unlock()
		(*state.capturedWorkers).Add(1)
		go func() { handleLocalSOCKSConnection(&state, &conn) }()
	}
}

// handleLocalSOCKSConnection exercises one isolated test phase with the original shared state.
func handleLocalSOCKSConnection(capturedState *serveLocalSOCKSListenerState, capturedConn *net.Conn) {
	defer (*(*capturedState).capturedWorkers).Done()
	defer (*capturedConn).Close()
	defer func() {
		(*(*capturedState).capturedMu).Lock()
		delete((*(*capturedState).capturedActive), (*capturedConn))
		(*(*capturedState).capturedMu).Unlock()
	}()
	_ = (*capturedConn).SetDeadline(time.Now().Add(5 * time.Second))
	if !authenticateLocalSOCKS(*capturedConn) {
		return
	}
	connect := make([]byte, 4)
	if _, err := io.ReadFull((*capturedConn), connect); err != nil || connect[0] != 5 || connect[1] != 1 {
		return
	}
	host, ok := readLocalSOCKSHost(*capturedConn, connect[3])
	if !ok {
		return
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull((*capturedConn), port); err != nil {
		return
	}
	if host != (*(*capturedState).capturedTargetHost) || strconv.Itoa(int(binary.BigEndian.Uint16(port))) != (*(*capturedState).capturedTargetPort) {
		return
	}
	remote, err := net.DialTimeout("tcp", (*(*capturedState).capturedTargetURL).Host, time.Second)
	if err != nil {
		return
	}
	defer remote.Close()
	(*(*capturedState).capturedHits).Add(1)
	_, _ = (*capturedConn).Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
	done := make(chan struct{})
	go func() { _, _ = io.Copy(remote, (*capturedConn)); remote.Close(); close(done) }()
	_, _ = io.Copy((*capturedConn), remote)
	(*capturedConn).Close()
	<-done
}

func authenticateLocalSOCKS(conn net.Conn) bool {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil || header[0] != 5 {
		return false
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return false
	}
	hasAuth := false
	func() { findLocalSOCKSAuthMethod(&methods, &hasAuth) }()
	if !hasAuth {
		_, _ = conn.Write([]byte{5, 255})
		return false
	}
	_, _ = conn.Write([]byte{5, 2})
	if _, err := io.ReadFull(conn, header); err != nil || header[0] != 1 {
		return false
	}
	username := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, username); err != nil {
		return false
	}
	length := make([]byte, 1)
	if _, err := io.ReadFull(conn, length); err != nil {
		return false
	}
	password := make([]byte, int(length[0]))
	if _, err := io.ReadFull(conn, password); err != nil {
		return false
	}
	if string(username) != "local-user" || string(password) != "local-secret" {
		_, _ = conn.Write([]byte{1, 1})
		return false
	}
	_, _ = conn.Write([]byte{1, 0})

	return true
}

func readLocalSOCKSHost(conn net.Conn, kind byte) (string, bool) {
	length := make([]byte, 1)
	host := ""
	switch kind {
	case 1:
		data := make([]byte, 4)
		if _, err := io.ReadFull(conn, data); err != nil {
			return "", false
		}
		host = net.IP(data).String()
	case 3:
		if _, err := io.ReadFull(conn, length); err != nil {
			return "", false
		}
		data := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, data); err != nil {
			return "", false
		}
		host = string(data)
	case 4:
		data := make([]byte, 16)
		if _, err := io.ReadFull(conn, data); err != nil {
			return "", false
		}
		host = net.IP(data).String()
	default:
		return "", false
	}
	return host, true
}

// findLocalSOCKSAuthMethod exercises one isolated test phase with the original shared state.
func findLocalSOCKSAuthMethod(capturedMethods *[]byte, capturedHasAuth *bool) {
	for _, method := range *capturedMethods {
		if method == 2 {
			(*capturedHasAuth) = true
		}
	}
}
