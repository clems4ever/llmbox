//go:build e2e

package e2e

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clems4ever/llmbox/internal/hub"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// TestEndToEndProxy drives the HTTP-proxy feature end to end against the real
// server, box-control API, and reverse-proxy routing, with only Docker faked: a
// box is created over the API, a proxy is enabled for it over CreateProxy, and a
// browser-style request to the returned proxy sub-domain is reverse-proxied to a
// real upstream "box" server. This is the user's core scenario — "start a server
// in the box, expose it, open the URL" — exercised through the whole stack.
func TestEndToEndProxy(t *testing.T) {
	// The "server running inside the box": a real loopback HTTP server the fake
	// box manager's DialBox points at.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-From", "box")
		_, _ = w.Write([]byte("box server says hi at " + r.URL.Path))
	}))
	t.Cleanup(upstream.Close)

	mgr := newFakeBoxManager()
	mgr.dialTarget = upstream.Listener.Addr().String()

	uiLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	base := "http://" + uiLn.Addr().String()

	store, err := hub.OpenStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	srv := hub.New(nil, base, store, nil)
	wireDefaultSpoke(t, srv, store, mgr)
	srv.SetProxyBaseDomain("proxy.example.com")
	httpSrv := &http.Server{Handler: srv.APIHandler()}
	go func() { _ = httpSrv.Serve(uiLn) }()
	t.Cleanup(func() { _ = httpSrv.Close() })
	waitHealthy(t, base)

	// --- chatbot side: create the box, then enable a proxy for its port ---
	ctx := context.Background()
	c := newBoxClient(t, base, store)
	if _, err := c.CreateBox(ctx, sandbox.CreateOptions{BoxID: "proxy-box"}); err != nil {
		t.Fatalf("CreateBox: %v", err)
	}

	proxy, err := c.CreateProxy(ctx, "proxy-box", 8000, "hello server")
	if err != nil {
		t.Fatalf("CreateProxy: %v", err)
	}
	proxyURL := proxy.URL
	if proxyURL == "" {
		t.Fatalf("CreateProxy returned no url: %+v", proxy)
	}
	if proxy.Description != "hello server" {
		t.Errorf("CreateProxy description = %q, want %q", proxy.Description, "hello server")
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatalf("parsing proxy url %q: %v", proxyURL, err)
	}
	if !strings.HasSuffix(u.Hostname(), ".proxy.example.com") {
		t.Fatalf("proxy host = %q, want a *.proxy.example.com sub-domain", u.Host)
	}

	// --- user side: open the proxy URL (resolved to the UI listener, Host set
	// to the proxy sub-domain so the host router forwards it to the box) ---
	req, _ := http.NewRequest(http.MethodGet, base+"/hello", nil)
	req.Host = u.Host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("X-From") != "box" {
		t.Errorf("missing upstream header X-From; headers: %v", resp.Header)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := string(body); got != "box server says hi at /hello" {
		t.Errorf("proxied body = %q", got)
	}

	// --- the proxy is listed, then disabled, after which the URL stops working ---
	proxies, err := c.ListProxies(ctx, "proxy-box")
	if err != nil {
		t.Fatalf("ListProxies: %v", err)
	}
	if len(proxies) != 1 {
		t.Errorf("ListProxies returned %d proxies, want 1", len(proxies))
	} else if proxies[0].Description != "hello server" {
		t.Errorf("listed proxy description = %q, want %q", proxies[0].Description, "hello server")
	}

	if err := c.DeleteProxy(ctx, "proxy-box", 8000); err != nil {
		t.Fatalf("DeleteProxy: %v", err)
	}
	req2, _ := http.NewRequest(http.MethodGet, base+"/hello", nil)
	req2.Host = u.Host
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("post-delete proxy request: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("after delete, proxy status = %d, want 404", resp2.StatusCode)
	}
}
