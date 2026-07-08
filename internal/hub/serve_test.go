package hub

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/hub/config"
)

// TestServe checks Serve stands up the hub on a loopback listener, serves the
// health endpoint, and returns cleanly once the parent context is cancelled.
func TestServe(t *testing.T) {
	cfg := &config.Config{
		HTTPAddr:  freeAddr(t),
		PublicURL: "http://" + "127.0.0.1",
		StateFile: filepath.Join(t.TempDir(), "sessions.db"),
	}
	cfg.AuthTTL = config.Duration(5 * time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- Serve(ctx, cfg, "llmbox-server", "test") }()

	base := "http://" + cfg.HTTPAddr
	deadline := time.Now().Add(3 * time.Second)
	var healthy bool
	for time.Now().Before(deadline) {
		if resp, err := http.Get(base + "/healthz"); err == nil {
			_ = resp.Body.Close()
			healthy = resp.StatusCode == http.StatusOK
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !healthy {
		cancel()
		<-errc
		t.Fatal("hub never served a healthy /healthz")
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("Serve returned %v, want nil on graceful shutdown", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Serve did not return after context cancel")
	}
}

// TestInsecureTransportWarning checks the plaintext-HTTP startup banner is
// non-empty and points the operator at the tls config block.
func TestInsecureTransportWarning(t *testing.T) {
	lines := insecureTransportWarning()
	if len(lines) == 0 {
		t.Fatal("insecureTransportWarning returned no lines")
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"WARNING", "NOT encrypted", "tls.enabled"} {
		if !strings.Contains(joined, want) {
			t.Errorf("banner missing %q:\n%s", want, joined)
		}
	}
}

// writeSelfSignedCert generates a self-signed certificate valid for 127.0.0.1,
// writes the PEM cert and key into t.TempDir(), and returns their paths.
func writeSelfSignedCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// freeAddr returns a currently-free 127.0.0.1 address to bind a test server to.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// serveTest runs listenAndServe(srv, tlsCfg) in the background against a "pong"
// handler, waits for the port to accept connections, and registers a graceful
// shutdown for cleanup. It returns the reachable base URL.
func serveTest(t *testing.T, tlsCfg config.TLSConfig) string {
	t.Helper()
	addr := freeAddr(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "pong")
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: time.Second}

	errc := make(chan error, 1)
	go func() { errc <- listenAndServe(srv, tlsCfg) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		if err := <-errc; err != nil {
			t.Errorf("listenAndServe returned %v", err)
		}
	})

	scheme := "http"
	if tlsCfg.Enabled {
		scheme = "https"
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errc:
			t.Fatalf("server exited early: %v", err)
		default:
		}
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return scheme + "://" + addr
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server at %s never became reachable", addr)
	return ""
}

// TestListenAndServeTLS checks listenAndServe terminates TLS with the configured
// cert and key so an HTTPS request succeeds.
func TestListenAndServeTLS(t *testing.T) {
	cert, key := writeSelfSignedCert(t)
	base := serveTest(t, config.TLSConfig{Enabled: true, CertFile: cert, KeyFile: key})

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed test cert
	}}
	resp, err := client.Get(base + "/ping")
	if err != nil {
		t.Fatalf("HTTPS GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "pong" {
		t.Errorf("body = %q, want pong", body)
	}
}

// TestListenAndServePlaintext checks listenAndServe serves plaintext HTTP when
// TLS is disabled.
func TestListenAndServePlaintext(t *testing.T) {
	base := serveTest(t, config.TLSConfig{Enabled: false})
	resp, err := http.Get(base + "/ping")
	if err != nil {
		t.Fatalf("HTTP GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "pong" {
		t.Errorf("body = %q, want pong", body)
	}
}

// TestListenAndServeTLSMissingCert checks an absent certificate file makes
// listenAndServe return an error rather than serving.
func TestListenAndServeTLSMissingCert(t *testing.T) {
	dir := t.TempDir()
	tlsCfg := config.TLSConfig{
		Enabled:  true,
		CertFile: filepath.Join(dir, "nope-cert.pem"),
		KeyFile:  filepath.Join(dir, "nope-key.pem"),
	}
	srv := &http.Server{Addr: freeAddr(t), ReadHeaderTimeout: time.Second}
	if err := listenAndServe(srv, tlsCfg); err == nil {
		t.Error("listenAndServe with missing cert = nil, want error")
	}
}
