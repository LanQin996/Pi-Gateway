// compose_tls is a disposable CI fixture, not a production gateway TLS setting.
// It terminates verified TLS and forwards to fixed loopback HTTP in the same
// network namespace. The final local hop is deliberately not claimed encrypted.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("expected certs, serve or check subcommand")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	directory := flags.String("dir", "", "private certificate directory")
	listen := flags.String("listen", "127.0.0.1:18443", "TLS listening address")
	backend := flags.Int("backend-port", 8317, "fixed loopback HTTP gateway port")
	san := flags.String("san-ip", "", "optional loopback IP SAN for negative certificate tests")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *directory == "" || flags.NArg() != 0 {
		return errors.New("a certificate directory and no extra arguments are required")
	}
	switch args[0] {
	case "certs":
		ips, err := certificateIPs(*san)
		if err != nil {
			return err
		}
		return createCertificates(*directory, ips)
	case "check":
		return checkTLS(*listen, *directory)
	case "serve":
		server, err := relayServer(*listen, *backend)
		if err != nil {
			return err
		}
		return serve(ctx, server, *directory)
	default:
		return errors.New("unsupported fixture subcommand")
	}
}

func serialNumber() (*big.Int, error) {
	limit := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, err
	}
	return serial.Add(serial, big.NewInt(1)), nil
}

func certificateTemplate() (*x509.Certificate, error) {
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	return &x509.Certificate{SerialNumber: serial, NotBefore: now.Add(-time.Minute),
		NotAfter: now.Add(24 * time.Hour), BasicConstraintsValid: true}, nil
}

func createCertificates(directory string, ips []net.IP) error {
	// Refuse an existing directory; never overwrite another run's keys or follow
	// an existing directory symlink. The caller owns cleanup of this new fixture.
	if err := os.Mkdir(directory, 0700); err != nil {
		return err
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	ca, err := certificateTemplate()
	if err != nil {
		return err
	}
	ca.Subject = pkix.Name{CommonName: "Disposable Compose CI CA"}
	ca.IsCA = true
	ca.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	if err := writePEM(filepath.Join(directory, "ca.crt"), "CERTIFICATE", caDER); err != nil {
		return err
	}
	return createServerCertificate(directory, ips, ca, caKey)
}

func createServerCertificate(directory string, ips []net.IP, ca *x509.Certificate, caKey *ecdsa.PrivateKey) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	certificate, err := certificateTemplate()
	if err != nil {
		return err
	}
	certificate.Subject = pkix.Name{CommonName: "Disposable Compose TLS endpoint"}
	certificate.IPAddresses = ips
	certificate.KeyUsage = x509.KeyUsageDigitalSignature
	certificate.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	der, err := x509.CreateCertificate(rand.Reader, certificate, ca, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	if err := writePEM(filepath.Join(directory, "server.crt"), "CERTIFICATE", der); err != nil {
		return err
	}
	encodedKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	// The CA private key stays only in memory and is never persisted.
	return writePEM(filepath.Join(directory, "server.key"), "PRIVATE KEY", encodedKey)
}

func writePEM(path, kind string, der []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	encodeErr := pem.Encode(file, &pem.Block{Type: kind, Bytes: der})
	return errors.Join(encodeErr, file.Close())
}

func relayServer(address string, backendPort int) (*http.Server, error) {
	if err := validateAddress(address, true); err != nil {
		return nil, err
	}
	if backendPort < 1 || backendPort > 65535 {
		return nil, errors.New("invalid fixture backend port")
	}
	// Explicit HTTP, confined to the shared namespace's loopback. Never target a
	// Docker service DNS name or another bridge address with plaintext credentials.
	target, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", backendPort))
	if err != nil {
		return nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		ResponseHeaderTimeout: 15 * time.Second, IdleConnTimeout: 30 * time.Second, MaxResponseHeaderBytes: 64 << 10}
	return &http.Server{Addr: address, Handler: proxy, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}, nil
}

func serve(ctx context.Context, server *http.Server, directory string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
	}()
	err := server.ListenAndServeTLS(filepath.Join(directory, "server.crt"), filepath.Join(directory, "server.key"))
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func validateAddress(address string, wildcard bool) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return errors.New("fixture address must use a literal IP")
	}
	if !ip.IsLoopback() && !(wildcard && ip.IsUnspecified()) {
		return errors.New("fixture address must be loopback or an allowed wildcard")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return errors.New("invalid fixture port")
	}
	return nil
}

func checkTLS(address, directory string) error {
	if err := validateAddress(address, false); err != nil {
		return err
	}
	ca, err := os.ReadFile(filepath.Join(directory, "ca.crt"))
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return errors.New("invalid fixture CA")
	}
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get("https://" + address + "/healthz")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("TLS healthcheck returned status %d", response.StatusCode)
	}
	return nil
}

func certificateIPs(value string) ([]net.IP, error) {
	if value == "" {
		return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, nil
	}
	ip := net.ParseIP(value)
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("certificate SAN must be a loopback IP")
	}
	return []net.IP{ip}, nil
}
