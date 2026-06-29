package cluster

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/sandbox"
)

// errDial is a canned dial failure.
var errDial = errors.New("dial refused")

// fixedDialer dials a fixed address, or returns err when set.
type fixedDialer struct {
	target string
	err    error
}

// DialBox dials the fixed target or returns the canned error.
func (d fixedDialer) DialBox(ctx context.Context, _ string, _ int) (net.Conn, error) {
	if d.err != nil {
		return nil, d.err
	}
	var n net.Dialer
	return n.DialContext(ctx, "tcp", d.target)
}

// TestRoundTripToBox checks roundTripToBox forwards a request to the box server
// (reached through the dialer) and returns its status, headers, and body.
func TestRoundTripToBox(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Box", "yes")
		_, _ = w.Write([]byte("hi " + r.Method + " " + r.URL.RequestURI()))
	}))
	defer upstream.Close()

	resp, err := roundTripToBox(context.Background(), fixedDialer{target: upstream.Listener.Addr().String()}, proxyHTTPReq{
		BoxID:  "web-box",
		Port:   8000,
		Method: http.MethodGet,
		Path:   "/a/b?q=1",
	})
	if err != nil {
		t.Fatalf("roundTripToBox: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.Status)
	}
	if resp.Header.Get("X-Box") != "yes" {
		t.Errorf("missing upstream header; got %v", resp.Header)
	}
	if string(resp.Body) != "hi GET /a/b?q=1" {
		t.Errorf("body = %q", resp.Body)
	}
}

// TestRoundTripToBoxDialError checks roundTripToBox surfaces a dial failure.
func TestRoundTripToBoxDialError(t *testing.T) {
	if _, err := roundTripToBox(context.Background(), fixedDialer{err: errDial}, proxyHTTPReq{
		BoxID: "web-box", Port: 8000, Method: http.MethodGet, Path: "/",
	}); err == nil {
		t.Fatal("expected an error when the dial fails")
	}
}

// TestRemoteSpokeProxyHTTP checks the proxy_http verb round-trips end to end: a
// hub-side remoteSpoke forwards a request over the in-memory pipe to the spoke,
// which dials its (fake) box server and returns the buffered response.
func TestRemoteSpokeProxyHTTP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Echo", string(body))
		_, _ = w.Write([]byte("box at " + r.URL.RequestURI()))
	}))
	defer upstream.Close()

	rs := startSpoke(t, &fakeManager{dialTarget: upstream.Listener.Addr().String()})

	status, header, body, err := rs.ProxyHTTP(context.Background(), "web-box", 8000, http.MethodPost, "/hi?x=1", http.Header{"Content-Type": {"text/plain"}}, []byte("ping"))
	if err != nil {
		t.Fatalf("ProxyHTTP: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if header.Get("X-Echo") != "ping" {
		t.Errorf("X-Echo = %q, want the echoed request body", header.Get("X-Echo"))
	}
	if string(body) != "box at /hi?x=1" {
		t.Errorf("body = %q", body)
	}
}

// TestProxyHTTPUnsupportedSpoke checks the verb errors when the spoke's manager
// cannot dial boxes (does not implement BoxDialer).
func TestProxyHTTPUnsupportedSpoke(t *testing.T) {
	payload, err := encodePayload(proxyHTTPReq{BoxID: "b", Port: 80, Method: "GET", Path: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatch(context.Background(), bareManager{}, frame{Type: frameReq, Method: methodProxyHTTP, Payload: payload}, ValidationPolicy{}); err == nil {
		t.Fatal("expected an error for a spoke that cannot dial boxes")
	}
}

// bareManager implements BoxManager with no-op verbs and deliberately does NOT
// implement BoxDialer, so the proxy verb is refused.
type bareManager struct{}

// Create is a no-op stub.
func (bareManager) Create(context.Context, sandbox.CreateOptions) (string, string, error) {
	return "", "", nil
}

// SubmitCode is a no-op stub.
func (bareManager) SubmitCode(context.Context, string, string) (string, error) { return "", nil }

// List is a no-op stub.
func (bareManager) List(context.Context) ([]sandbox.Box, error) { return nil, nil }

// Destroy is a no-op stub.
func (bareManager) Destroy(context.Context, string) error { return nil }

// Logs is a no-op stub.
func (bareManager) Logs(context.Context, string, int) (string, error) { return "", nil }

// Exec is a no-op stub.
func (bareManager) Exec(context.Context, string, []string) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}

// ReapOrphans is a no-op stub.
func (bareManager) ReapOrphans(context.Context, time.Duration) ([]string, error) { return nil, nil }
