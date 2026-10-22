package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fixtureCertificates(t *testing.T, ips ...net.IP) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "certs")
	if err := createCertificates(directory, ips); err != nil {
		t.Fatal(err)
	}
	return directory
}

func fixtureClient(t *testing.T, directory string) *http.Client {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		t.Fatal("invalid CA")
	}
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport}
}

func fixtureServer(t *testing.T, directory string, handler http.Handler) *httptest.Server {
	t.Helper()
	certificate, err := tls.LoadX509KeyPair(filepath.Join(directory, "server.crt"), filepath.Join(directory, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func TestCertificateTrustAndIPVerification(t *testing.T) {
	correct := fixtureCertificates(t, net.ParseIP("127.0.0.1"))
	otherCA := fixtureCertificates(t, net.ParseIP("127.0.0.1"))
	wrongIP := fixtureCertificates(t, net.ParseIP("127.0.0.2"))
	var requests atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(http.StatusNoContent) })
	server := fixtureServer(t, correct, handler)
	response, err := fixtureClient(t, correct).Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatal("unexpected status")
	}
	requireCertificateRejection(t, fixtureClient(t, otherCA), server.URL)
	mismatch := fixtureServer(t, wrongIP, handler)
	requireCertificateRejection(t, fixtureClient(t, wrongIP), mismatch.URL)
	if requests.Load() != 1 {
		t.Fatal("untrusted TLS requests reached HTTP handler")
	}
}

func requireCertificateRejection(t *testing.T, client *http.Client, address string) {
	t.Helper()
	response, err := client.Get(address)
	if response != nil {
		response.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("expected certificate verification error, got %v", err)
	}
}

func TestRelayPreservesRequestsAndRefusesPlaintext(t *testing.T) {
	var requests atomic.Int64
	backend := relayBackend(t, &requests)
	defer backend.Close()
	_, port, err := net.SplitHostPort(backend.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	relay, err := relayServer("127.0.0.1:18443", number)
	if err != nil {
		t.Fatal(err)
	}
	certs := fixtureCertificates(t, net.ParseIP("127.0.0.1"))
	server := fixtureServer(t, certs, relay.Handler)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/auth/login", strings.NewReader(`{"fixture":true}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer disposable")
	response, err := fixtureClient(t, certs).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != `{"ok":true}` || response.StatusCode != http.StatusCreated {
		t.Fatal("response changed")
	}
	plaintext := "http://" + server.Listener.Addr().String() + "/healthz"
	denied, err := http.Get(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	denied.Body.Close()
	if denied.StatusCode != http.StatusBadRequest || requests.Load() != 1 {
		t.Fatal("plaintext reached backend")
	}
}

func TestCertificateFilesArePrivateAndExclusive(t *testing.T) {
	directory := fixtureCertificates(t, net.ParseIP("127.0.0.1"))
	if err := createCertificates(directory, nil); err == nil {
		t.Fatal("must not replace existing certificates")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatal("only CA certificate and server certificate/key may persist")
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Errorf("insecure permissions for %s", entry.Name())
		}
	}
}

func TestRelayTargetBoundaries(t *testing.T) {
	for _, address := range []string{"example.com:18443", "192.0.2.1:18443", "127.0.0.1:0", "127.0.0.1:65536"} {
		if _, err := relayServer(address, 8317); err == nil {
			t.Errorf("accepted unsafe listen address %s", address)
		}
	}
	if _, err := relayServer("127.0.0.1:18443", 0); err == nil {
		t.Fatal("accepted invalid backend port")
	}
	if err := validateAddress("0.0.0.0:18443", false); err == nil {
		t.Fatal("health client must not target wildcard")
	}
}

func relayBackend(t *testing.T, requests *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/auth/login" || r.Header.Get("Authorization") != "Bearer disposable" {
			t.Error("request fields changed")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != `{"fixture":true}` {
			t.Error("request body changed")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
}

func readCertificate(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("missing certificate PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func TestCertificateExpires(t *testing.T) {
	directory := fixtureCertificates(t, net.ParseIP("127.0.0.1"))
	roots := x509.NewCertPool()
	roots.AddCert(readCertificate(t, filepath.Join(directory, "ca.crt")))
	certificate := readCertificate(t, filepath.Join(directory, "server.crt"))
	_, err := certificate.Verify(x509.VerifyOptions{Roots: roots, DNSName: "127.0.0.1", CurrentTime: time.Now().Add(48 * time.Hour)})
	var expired x509.CertificateInvalidError
	if !errors.As(err, &expired) || expired.Reason != x509.Expired {
		t.Fatalf("expected expiry rejection, got %v", err)
	}
}

func TestTLSHealthcheckVerifiesIdentityAndStatus(t *testing.T) {
	directory := fixtureCertificates(t, net.ParseIP("127.0.0.1"))
	untrusted := fixtureCertificates(t, net.ParseIP("127.0.0.1"))
	var status atomic.Int32
	status.Store(http.StatusOK)
	var calls atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/healthz" {
			t.Error("health client changed path")
		}
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(int(status.Load()))
	})
	server := fixtureServer(t, directory, handler)
	address := server.Listener.Addr().String()
	if err := checkTLS(address, directory); err != nil {
		t.Fatal(err)
	}
	if err := checkTLS(address, untrusted); err == nil {
		t.Fatal("healthcheck accepted unknown CA")
	}
	status.Store(http.StatusFound)
	if err := checkTLS(address, directory); err == nil {
		t.Fatal("healthcheck accepted redirect")
	}
	if calls.Load() != 2 {
		t.Fatal("healthcheck followed redirect or bypassed CA verification")
	}
}
