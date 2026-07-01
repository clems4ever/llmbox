//go:build e2e

package e2e

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/mcpapi"
	"github.com/clems4ever/llmbox/internal/server"
)

// TestEndToEndProxy drives the HTTP-proxy feature end to end against the real
// server, MCP tools, and reverse-proxy routing, with only Docker faked: a box is
// created over MCP, a proxy is enabled for it over the create_llmbox_proxy tool,
// and a browser-style request to the returned proxy sub-domain is reverse-proxied
// to a real upstream "box" server. This is the user's core scenario — "start a
// server in the box, expose it, open the URL" — exercised through the whole stack.
func TestEndToEndProxy(t *testing.T) {
	// The "server running inside the box": a real loopback HTTP server the fake
	// box manager's DialBox points at.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-From", "box")
		_, _ = w.Write([]byte("box server says hi at " + r.URL.Path))
	}))
	t.Cleanup(upstream.Close)

	platform := newFakeAnthropic()
	t.Cleanup(platform.close)
	mgr := newFakeBoxManager(platform)
	mgr.dialTarget = upstream.Listener.Addr().String()

	uiLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen ui: %v", err)
	}
	mcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen mcp: %v", err)
	}
	base := "http://" + uiLn.Addr().String()
	mcpBase := "http://" + mcpLn.Addr().String()

	store, err := server.OpenStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	srv := server.New(mgr, nil, base, 5*time.Minute, store, nil)
	srv.SetProxyBaseDomain("proxy.example.com")
	apiSrv := &http.Server{Handler: srv.APIHandler()}
	mcpSrv := &http.Server{Handler: mcpapi.NewHandler(srv.MCPBackend())}
	go func() { _ = apiSrv.Serve(uiLn) }()
	go func() { _ = mcpSrv.Serve(mcpLn) }()
	t.Cleanup(func() { _ = apiSrv.Close() })
	t.Cleanup(func() { _ = mcpSrv.Close() })
	waitHealthy(t, base)

	// --- chatbot side: create the box, then enable a proxy for its port ---
	cs := connectMCP(t, mcpBase)
	callTool(t, cs, "create_llmbox", map[string]any{"box_id": "proxy-box"})

	proxyOut := callTool(t, cs, "create_llmbox_proxy", map[string]any{
		"box_id":      "proxy-box",
		"port":        8000,
		"description": "hello server",
	})
	proxyURL, _ := proxyOut["url"].(string)
	if proxyURL == "" {
		t.Fatalf("create_llmbox_proxy returned no url: %+v", proxyOut)
	}
	if desc, _ := proxyOut["description"].(string); desc != "hello server" {
		t.Errorf("create_llmbox_proxy description = %q, want %q", desc, "hello server")
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatalf("parsing proxy url %q: %v", proxyURL, err)
	}
	if !strings.HasSuffix(u.Host, ".proxy.example.com") {
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
	listOut := callTool(t, cs, "list_llmbox_proxies", map[string]any{"box_id": "proxy-box"})
	proxies, _ := listOut["proxies"].([]any)
	if len(proxies) != 1 {
		t.Errorf("list_llmbox_proxies returned %d proxies, want 1", len(proxies))
	} else if first, ok := proxies[0].(map[string]any); ok {
		if desc, _ := first["description"].(string); desc != "hello server" {
			t.Errorf("listed proxy description = %q, want %q", desc, "hello server")
		}
	}

	callTool(t, cs, "delete_llmbox_proxy", map[string]any{"box_id": "proxy-box", "port": 8000})
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
