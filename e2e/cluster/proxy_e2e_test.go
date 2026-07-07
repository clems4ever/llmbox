//go:build e2e

package clustere2e

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/auth"
	storepkg "github.com/clems4ever/llmbox/internal/store"
)

// TestProxyThroughSpoke exercises the HTTP-proxy feature against a box on a REAL
// remote spoke: the request travels browser → hub → over the live WebSocket (a
// streaming stream_open/stream_data tunnel) → spoke → the box's server, and the
// response streams back the same way. Only the box's Docker layer is simulated
// (the spoke dials a real loopback server standing in for the in-box HTTP server);
// the hub, enrollment, WebSocket transport, routing, and reverse proxy are all real.
func TestProxyThroughSpoke(t *testing.T) {
	// The "server running inside the box": a real loopback HTTP server the spoke's
	// DialBox points at.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Box-Server", "edge-1")
		_, _ = w.Write([]byte("box reply: " + r.Method + " " + r.URL.RequestURI() + " body=" + string(body)))
	}))
	defer upstream.Close()

	f := newClusterFixture(t)
	r := f.connectSpoke("edge-1")
	r.mgr.setDialTarget(upstream.Listener.Addr().String())

	// Create a box on the remote spoke and enable a proxy for its port.
	f.createBoxViaAPI("web-box", "edge-1")
	proxyURL := f.createProxyViaAPI("web-box", 8000)
	u, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatalf("parse proxy url %q: %v", proxyURL, err)
	}

	// A proxy request must carry a signed-in box-activator session (the admin
	// session has admin rights but not box-activation). Seed one and use its cookie.
	if err := f.store.SaveLoginSession("ACT", storepkg.LoginSession{
		Email: "dev@corp.com", ExpiresAt: time.Now().Add(time.Hour), Activate: true,
	}); err != nil {
		t.Fatalf("seed activator session: %v", err)
	}
	actCookie := &http.Cookie{Name: auth.LoginCookie, Value: "ACT"}

	// Open the proxy URL: connect to the hub listener but set Host to the proxy
	// sub-domain so the hub's host router forwards it to the box on the spoke.
	req, _ := http.NewRequest(http.MethodPost, f.baseURL+"/widgets?id=7", strings.NewReader("ping"))
	req.Host = u.Host
	req.AddCookie(actCookie)
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("X-Box-Server") != "edge-1" {
		t.Errorf("missing box-server header (response not from the spoke's box); headers: %v", resp.Header)
	}
	got, _ := io.ReadAll(resp.Body)
	if want := "box reply: POST /widgets?id=7 body=ping"; string(got) != want {
		t.Errorf("proxied body = %q, want %q", got, want)
	}

	// An unauthenticated request to the same proxy is refused (auth is enforced
	// at the hub before anything reaches the spoke).
	anon, _ := http.NewRequest(http.MethodGet, f.baseURL+"/widgets", nil)
	anon.Host = u.Host
	aresp, err := f.client.Do(anon)
	if err != nil {
		t.Fatalf("anon proxy request: %v", err)
	}
	defer func() { _ = aresp.Body.Close() }()
	if aresp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anon proxy status = %d, want 401", aresp.StatusCode)
	}
}
