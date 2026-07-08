//go:build e2e

package e2e

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tebeka/selenium"

	"github.com/clems4ever/llmbox/internal/hub"
	"github.com/clems4ever/llmbox/internal/hub/auth"
)

// TestProxySignInRedirectInBrowser exercises the proxy sign-in gate end to end and
// captures the sign-in page for the docs. With activation auth on, a signed-out
// request to a box's proxy URL must be redirected to the public sign-in page
// (carrying the proxy URL as the return target), and a request bearing a valid
// shared session must reverse-proxy through to the box. The redirect and the
// proxied round-trip are driven with an HTTP client (which sets the proxy
// sub-domain Host header against the one test listener — no DNS or browser host
// resolution needed), and the sign-in page the redirect points at is then opened
// in a real headless Chrome to assert it renders and to screenshot it.
//
// It is opt-in via `-tags e2e` (it needs Chrome + ChromeDriver), like the rest of
// the e2e suite, so a missing browser is a fatal failure rather than a skip.
//
// @arg t The test, failed on any setup, request, or assertion error.
func TestProxySignInRedirectInBrowser(t *testing.T) {
	// The "server running inside the box": a real loopback HTTP server the fake
	// box manager's DialBox points at, so an authorized proxy request reaches it.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("box server says hi at " + r.URL.Path))
	}))
	t.Cleanup(upstream.Close)

	platform := newFakeAnthropic()
	t.Cleanup(platform.close)
	mgr := newFakeBoxManager(platform)
	mgr.dialTarget = upstream.Listener.Addr().String()

	uiLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	base := "http://" + uiLn.Addr().String()

	st, err := hub.OpenStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Activation auth on, with the login cookie scoped to the parent domain so it
	// spans the proxy sub-domains — the configuration the redirect relies on.
	a := auth.NewTestAuthenticator("admin@corp.com")
	a.SetCookieDomain("example.com")

	srv := hub.New(nil, base, 5*time.Minute, st, a)
	wireDefaultSpoke(t, srv, st, mgr)
	srv.SetProxyBaseDomain("proxy.example.com")
	httpSrv := &http.Server{Handler: srv.APIHandler()}
	go func() { _ = httpSrv.Serve(uiLn) }()
	t.Cleanup(func() { _ = httpSrv.Close() })
	waitHealthy(t, base)

	// Create the box and enable a proxy for its port over the API, as the chatbot does.
	cs := connectMCP(t, base, st)
	callTool(t, cs, "create_llmbox", map[string]any{"box_id": "proxy-box"})
	proxyOut := callTool(t, cs, "create_llmbox_proxy", map[string]any{"box_id": "proxy-box", "port": 8000})
	proxyURL, _ := proxyOut["url"].(string)
	if proxyURL == "" {
		t.Fatalf("create_llmbox_proxy returned no url: %+v", proxyOut)
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatalf("parsing proxy url %q: %v", proxyURL, err)
	}

	// A request to the listener carrying the proxy sub-domain as its Host is what
	// the host router keys on; no DNS or browser host-resolution is involved.
	proxyReq := func(t *testing.T, path string, cookie *http.Cookie) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		req.Host = u.Host
		req.Header.Set("Accept", "text/html") // a top-level browser navigation
		if cookie != nil {
			req.AddCookie(cookie)
		}
		noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatalf("proxy request %q: %v", path, err)
		}
		return resp
	}

	// --- signed out: the proxy bounces a browser navigation to the sign-in page ---
	resp := proxyReq(t, "/", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("signed-out proxy status = %d, want 302", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, base+"/signin?return=") {
		t.Fatalf("redirect Location = %q, want the sign-in page on %s", loc, base)
	}
	if !strings.Contains(loc, url.QueryEscape(proxyURL)) {
		t.Fatalf("redirect Location = %q, want it to return to the proxy URL %q", loc, proxyURL)
	}

	// --- signed in: the same proxy URL now reverse-proxies through to the box ---
	cookie := signIn(t, st, false, true) // a box-activator session
	authed := proxyReq(t, "/hello", cookie)
	defer authed.Body.Close()
	if authed.StatusCode != http.StatusOK {
		t.Fatalf("signed-in proxy status = %d, want 200", authed.StatusCode)
	}
	if body, _ := io.ReadAll(authed.Body); string(body) != "box server says hi at /hello" {
		t.Fatalf("proxied body = %q, want the box response", string(body))
	}

	// --- the sign-in page the redirect points at: render it in a real browser and
	// capture it for the docs. It lives on the public host (the listener IP), so no
	// DNS/host-resolution is needed and there is nothing flaky to wait on. ---
	b := newBrowser(t)
	t.Cleanup(b.close)
	if err := b.wd.Get(loc); err != nil {
		t.Fatalf("opening the sign-in page: %v", err)
	}
	signInBtn := b.waitFor(t, selenium.ByXPATH, "//a[contains(normalize-space(.),'Sign in with Google')]")
	href, _ := signInBtn.GetAttribute("href")
	// The return URL carries the proxy host with its port percent-encoded (:%3A),
	// so match on the bare hostname, which appears literally in the href.
	if !strings.Contains(href, "/auth/google/login") || !strings.Contains(href, u.Hostname()) {
		t.Fatalf("sign-in button href = %q, want a login link returning to the proxy host %q", href, u.Hostname())
	}

	// Capture the proxy sign-in page for the docs when a screenshot dir is set (CI
	// does this); a plain run writes nothing. Desktop first, then a phone viewport.
	shotDir, err := resolveScreenshotDir(os.Getenv("LLMBOX_E2E_SCREENSHOT_DIR"))
	if err != nil {
		t.Fatalf("resolving screenshot dir: %v", err)
	}
	if shotDir != "" {
		b.resizeForScreenshot(t)
		b.saveScreenshot(t, shotDir, "signin-page.png")
		b.resizeForMobileScreenshot(t)
		b.saveScreenshot(t, shotDir, "signin-page-mobile.png")
		b.resizeForScreenshot(t)
	}
}
