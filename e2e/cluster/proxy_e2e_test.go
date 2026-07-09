//go:build e2e

package clustere2e

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/clems4ever/llmbox/internal/hub/auth"
	storepkg "github.com/clems4ever/llmbox/internal/hub/store"
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
	if err := f.store.PutIdentitySession("ACT", storepkg.IdentitySession{
		Email: "dev@corp.com", ExpiresAt: time.Now().Add(time.Hour), CanActivate: true,
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

// TestWebSocketProxyThroughSpoke proves the headline of the streaming tunnel: a
// live WebSocket to a box on a REAL remote spoke works end to end. The upgrade and
// every frame travel browser → hub reverse proxy → the cluster stream tunnel →
// spoke → the box's WebSocket server, and the echo comes back the same way. A
// buffered request/response proxy could not carry this.
func TestWebSocketProxyThroughSpoke(t *testing.T) {
	// The "server running inside the box": a real WebSocket echo server.
	boxWS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(websocket.StatusNormalClosure, "") }()
		for {
			typ, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if err := c.Write(r.Context(), typ, append([]byte("echo: "), data...)); err != nil {
				return
			}
		}
	}))
	defer boxWS.Close()

	f := newClusterFixture(t)
	r := f.connectSpoke("edge-1")
	r.mgr.setDialTarget(boxWS.Listener.Addr().String())

	f.createBoxViaAPI("ws-box", "edge-1")
	proxyURL := f.createProxyViaAPI("ws-box", 8000)
	u, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatalf("parse proxy url %q: %v", proxyURL, err)
	}

	// A box-activator session, presented as a cookie on the WebSocket handshake.
	if err := f.store.PutIdentitySession("ACT", storepkg.IdentitySession{
		Email: "dev@corp.com", ExpiresAt: time.Now().Add(time.Hour), CanActivate: true,
	}); err != nil {
		t.Fatalf("seed activator session: %v", err)
	}

	// Dial the WebSocket at the proxy sub-domain host, but route the TCP connection
	// to the hub listener (mirroring how a browser reaches <slug>.<base>/ that a
	// wildcard DNS points at the hub).
	hubAddr := strings.TrimPrefix(f.baseURL, "http://")
	wsClient := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, hubAddr)
		},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+u.Host+"/socket", &websocket.DialOptions{
		HTTPClient: wsClient,
		HTTPHeader: http.Header{"Cookie": {auth.LoginCookie + "=ACT"}},
	})
	if err != nil {
		t.Fatalf("websocket dial through the proxy failed: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	// Round-trip several messages to prove the connection is live and bidirectional.
	for _, msg := range []string{"hello", "streamed", "over the tunnel"} {
		if err := conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			t.Fatalf("ws write %q: %v", msg, err)
		}
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		if typ != websocket.MessageText || string(data) != "echo: "+msg {
			t.Errorf("ws echo = %q (type %v), want %q", data, typ, "echo: "+msg)
		}
	}
}
