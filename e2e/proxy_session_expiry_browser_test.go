//go:build e2e

package e2e

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tebeka/selenium"

	"github.com/clems4ever/llmbox/internal/hub"
	"github.com/clems4ever/llmbox/internal/hub/auth"
	"github.com/clems4ever/llmbox/internal/hub/store"
)

// TestProxySessionExpiryRedirectInBrowser is the regression test for
// https://github.com/clems4ever/llmbox/issues/116: when an admin's session
// expires while they are sitting on a proxied box app, the browser must be sent to
// the sign-in page rather than left on an app whose background requests silently
// start failing ("appearing disconnected").
//
// It drives a real headless Chrome all the way through the proxy. A signed-in
// admin opens a proxied single-page app (a static page that never navigates on its
// own); the hub serves it with the injected session watcher. The test then expires
// the session server-side and does nothing else in the browser — the injected
// watcher's poll of the proxy's auth-check endpoint sees the 401 and navigates the
// tab to the public sign-in page on its own. The test asserts the browser landed
// on sign-in, carrying the proxied app as the return target, and screenshots the
// before/after for the docs.
//
// The browser resolves both the main host and the proxy sub-domains to the test
// listener via --host-resolver-rules, and the login cookie is scoped to the shared
// parent domain, exactly as the documented cookie_domain deployment does — so no
// DNS, TLS, or real OIDC provider is needed.
//
// @arg t The test, failed on any setup, navigation, or assertion error.
func TestProxySessionExpiryRedirectInBrowser(t *testing.T) {
	// The "app running inside the box": a static single-page app that makes no
	// navigation of its own, so any redirect to sign-in must come from the injected
	// watcher — never from the app.
	const appMarker = "box-app-is-running"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>Box App</title>` +
			`<style>body{font-family:sans-serif;display:flex;align-items:center;justify-content:center;height:90vh;background:#0f172a;color:#e2e8f0}` +
			`h1{font-size:2rem}</style></head>` +
			`<body><h1 id="` + appMarker + `">📦 Box app is running</h1></body></html>`))
	}))
	t.Cleanup(upstream.Close)

	mgr := newFakeBoxManager()
	mgr.dialTarget = upstream.Listener.Addr().String()

	// Listen first so the listener's port can be baked into the public URL: the main
	// host and the proxy sub-domains share it, and the browser maps both to loopback.
	uiLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := uiLn.Addr().(*net.TCPAddr).Port
	loopback := uiLn.Addr().String()                              // 127.0.0.1:PORT — for health/MCP
	publicURL := "http://boxes.example.com:" + strconv.Itoa(port) // the browser-facing main host

	st, err := hub.OpenStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Admin sign-in on, login cookie scoped to the shared parent domain so it spans
	// the main host and the proxy sub-domains (the documented cookie_domain setup).
	a := auth.NewTestAuthenticator("admin@corp.com")
	a.SetCookieDomain("example.com")

	srv := hub.New(nil, publicURL, st, a)
	wireDefaultSpoke(t, srv, st, mgr)
	srv.SetProxyBaseDomain("proxy.example.com")
	// Poll briskly so the post-expiry redirect happens within the test's patience
	// (production defaults to a slower cadence).
	srv.SetProxyAuthCheckInterval(2 * time.Second)

	httpSrv := &http.Server{Handler: srv.APIHandler()}
	go func() { _ = httpSrv.Serve(uiLn) }()
	t.Cleanup(func() { _ = httpSrv.Close() })
	waitHealthy(t, "http://"+loopback)

	// Create the box and enable a proxy for its port over MCP, as the chatbot does.
	cs := connectMCP(t, "http://"+loopback, st)
	callTool(t, cs, "create_llmbox", map[string]any{"box_id": "proxy-box"})
	proxyOut := callTool(t, cs, "create_llmbox_proxy", map[string]any{"box_id": "proxy-box", "port": 8000})
	proxyURL, _ := proxyOut["url"].(string)
	if proxyURL == "" {
		t.Fatalf("create_llmbox_proxy returned no url: %+v", proxyOut)
	}
	pu, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatalf("parsing proxy url %q: %v", proxyURL, err)
	}

	// Seed a live admin session; the browser presents its cookie to stand in for a
	// completed OIDC sign-in (which needs a real provider).
	const sid = "SESSION-ID"
	putSession := func(expires time.Time) {
		if err := st.PutIdentitySession(store.HashToken(sid), store.IdentitySession{
			Email: "admin@corp.com", Provider: "google", CSRFToken: "CSRF", ExpiresAt: expires, CanAdmin: true,
		}); err != nil {
			t.Fatalf("put session: %v", err)
		}
	}
	putSession(time.Now().Add(time.Hour))

	// Map the main host and every proxy sub-domain to the loopback listener; the port
	// in each URL is preserved, so the browser reaches the one test server.
	b := newBrowser(t, "--host-resolver-rules=MAP *example.com 127.0.0.1")
	t.Cleanup(b.close)
	slowmo := os.Getenv("LLMBOX_E2E_HEADED") != "" // pace the demo video; no-op for CI

	// Plant the shared login cookie the same way a completed OIDC sign-in would: on
	// the main host, scoped to the parent cookie domain so it also reaches the proxy
	// sub-domains. It is set via document.cookie on the real sign-in page (WebDriver's
	// AddCookie silently drops cookies for these hosts in headless Chrome, while
	// document.cookie is honoured — and the test cookie is not HttpOnly).
	if err := b.wd.Get(publicURL + "/signin?return=%2Fadmin"); err != nil {
		t.Fatalf("opening the sign-in page: %v", err)
	}
	if _, err := b.wd.ExecuteScript(
		`document.cookie=arguments[0]+"=`+sid+`; domain=example.com; path=/";`,
		[]any{auth.LoginCookie},
	); err != nil {
		t.Fatalf("planting login cookie: %v", err)
	}

	// --- signed in: the proxied box app renders through the proxy ---
	if err := b.wd.Get(proxyURL); err != nil {
		t.Fatalf("opening the proxied app: %v", err)
	}
	b.waitFor(t, selenium.ByID, appMarker) // the box app is visible
	src, _ := b.wd.PageSource()
	if !strings.Contains(src, "/.llmbox/proxy-auth-check") {
		t.Fatalf("proxied document is missing the injected session watcher:\n%s", src)
	}
	pause(slowmo, 3*time.Second)
	shotDir := screenshotDir(t)
	if shotDir != "" {
		b.saveScreenshot(t, shotDir, "session-expiry-1-app.png")
	}

	// --- the session expires while the user sits on the app ---
	putSession(time.Now().Add(-time.Minute))

	// The watcher polls, sees 401, and navigates the tab to sign-in — with no further
	// action in the browser. Wait for that redirect.
	signInURL := b.waitForURLContains(t, "/signin", 20*time.Second)
	if !strings.Contains(signInURL, "boxes.example.com") {
		t.Fatalf("redirected to %q, want the sign-in page on the main host", signInURL)
	}
	if !strings.Contains(signInURL, url.QueryEscape(pu.Hostname())) && !strings.Contains(signInURL, pu.Hostname()) {
		t.Fatalf("sign-in URL %q does not carry the proxied app as its return target", signInURL)
	}
	// The sign-in page renders its provider button — the user can sign back in.
	b.waitFor(t, selenium.ByXPATH, "//a[contains(normalize-space(.),'Sign in with Google')]")
	pause(slowmo, 2*time.Second)
	if shotDir != "" {
		b.saveScreenshot(t, shotDir, "session-expiry-2-signin.png")
	}
}

// waitForURLContains polls the browser's current URL until it contains sub or the
// timeout elapses, returning the matching URL. It is how the test waits for the
// injected watcher to navigate the tab after the session expires.
//
// @arg t The test, failed if the URL never matches.
// @arg sub The substring the URL must contain.
// @arg timeout How long to wait.
// @return string The current URL once it contains sub.
func (b *browser) waitForURLContains(t *testing.T, sub string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cur, err := b.wd.CurrentURL(); err == nil && strings.Contains(cur, sub) {
			return cur
		}
		if time.Now().After(deadline) {
			cur, _ := b.wd.CurrentURL()
			t.Fatalf("timed out waiting for the URL to contain %q; still at %q", sub, cur)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// screenshotDir resolves $LLMBOX_E2E_SCREENSHOT_DIR (empty when unset), so a plain
// run writes nothing and a recording/CI run captures the before/after frames.
//
// @arg t The test, failed if the configured dir cannot be resolved.
// @return string The resolved screenshot directory, or "" when none is configured.
func screenshotDir(t *testing.T) string {
	t.Helper()
	dir, err := resolveScreenshotDir(os.Getenv("LLMBOX_E2E_SCREENSHOT_DIR"))
	if err != nil {
		t.Fatalf("resolving screenshot dir: %v", err)
	}
	return dir
}

// pause sleeps only when on (headed demo runs), so the recorded video lingers on
// each state; a normal headless run skips it and stays fast.
//
// @arg on Whether to actually sleep.
// @arg d How long to sleep when on.
func pause(on bool, d time.Duration) {
	if on {
		time.Sleep(d)
	}
}
