//go:build e2e

package e2e

import (
	"fmt"
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

	"github.com/clems4ever/llmbox/internal/auth"
	"github.com/clems4ever/llmbox/internal/server"
)

// TestProxySignInRedirectInBrowser drives the proxy sign-in gate through a real
// headless Chrome, reproducing the user's path: opening a box's proxy URL while
// signed out must bounce the browser to the public sign-in page (carrying the
// proxy URL as the return target), and once a shared login cookie is present the
// same URL must reverse-proxy to the box. Chrome's host resolver is pinned so the
// public host and the <slug>.proxy.example.com sub-domain both reach the single
// test listener; the shared cookie is scoped to the parent domain (example.com),
// exactly as the real activation cookie is. The unit tests pin the same behaviour
// at the HTTP layer (TestHandleProxyRedirectsBrowserToSignIn, TestSignInPage*);
// this proves the redirect chain actually renders and round-trips in a browser.
//
// It is opt-in via `-tags e2e` (it needs Chrome + ChromeDriver), like the rest of
// the e2e suite, so a missing browser is a fatal failure rather than a skip.
//
// @arg t The test, failed on any setup, navigation, or assertion error.
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
		t.Fatalf("listen ui: %v", err)
	}
	mcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen mcp: %v", err)
	}
	uiHostPort := uiLn.Addr().String()
	base := "http://" + uiHostPort
	mcpBase := "http://" + mcpLn.Addr().String()

	st, err := server.OpenStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Activation auth on, with the login cookie scoped to the parent domain so it
	// spans the proxy sub-domains — the configuration the redirect relies on.
	a := auth.NewTestAuthenticator("admin@corp.com")
	a.SetCookieDomain("example.com")

	// publicURL is a named host (not the listener IP): the sign-in redirect is
	// built from it, and Chrome's host-resolver rule maps it — and every proxy
	// sub-domain — back to the listener below.
	srv := server.New(mgr, nil, "http://app.example.com", 5*time.Minute, st, a)
	srv.SetProxyBaseDomain("proxy.example.com")
	apiSrv := &http.Server{Handler: srv.APIHandler()}
	mcpSrv := &http.Server{Handler: srv.MCPHandler(srv.MCPServer("llmbox", "e2e"))}
	go func() { _ = apiSrv.Serve(uiLn) }()
	go func() { _ = mcpSrv.Serve(mcpLn) }()
	t.Cleanup(func() { _ = apiSrv.Close() })
	t.Cleanup(func() { _ = mcpSrv.Close() })
	waitHealthy(t, base)

	// Create the box and enable a proxy for its port over MCP, as the chatbot does.
	cs := connectMCP(t, mcpBase)
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

	b := newBrowserResolving(t, uiHostPort)
	t.Cleanup(b.close)

	// --- signed out: opening the proxy URL bounces the browser to the sign-in page ---
	if err := b.wd.Get(proxyURL); err != nil {
		t.Fatalf("opening proxy URL: %v", err)
	}
	signInBtn := b.waitFor(t, selenium.ByXPATH, "//a[contains(normalize-space(.),'Sign in with Google')]")
	cur, _ := b.wd.CurrentURL()
	if !strings.Contains(cur, "/signin") {
		t.Fatalf("after opening proxy signed out, URL = %q, want the /signin page", cur)
	}
	href, _ := signInBtn.GetAttribute("href")
	if !strings.Contains(href, "return=") || !strings.Contains(href, u.Host) {
		t.Fatalf("sign-in button href = %q, want a login link returning to the proxy host %q", href, u.Host)
	}

	// Capture the proxy sign-in page for the docs when a screenshot dir is set (CI
	// does this); a plain run writes nothing. Desktop first, then a phone viewport,
	// then back to desktop so the size is restored for the rest of the test.
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

	// --- sign in: drop the shared login cookie (scoped to example.com, as the real
	// OIDC callback does), skipping the OIDC dance like the admin browser test ---
	cookie := signIn(t, st, false, true) // a box-activator session "SID"
	setCookie := fmt.Sprintf("document.cookie = %q;", auth.LoginCookie+"="+cookie.Value+"; domain=example.com; path=/")
	if _, err := b.wd.ExecuteScript(setCookie, nil); err != nil {
		t.Fatalf("setting login cookie: %v", err)
	}

	// --- signed in: the same proxy URL now reverse-proxies through to the box ---
	if err := b.wd.Get(proxyURL + "hello"); err != nil {
		t.Fatalf("reopening proxy URL after sign-in: %v", err)
	}
	src := b.waitForText(t, "box server says hi at /hello")
	if strings.Contains(src, "Sign in with Google") {
		t.Fatalf("after sign-in the proxy still showed the sign-in page:\n%s", src)
	}
}
