package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/clems4ever/llmbox/internal/auth"
	"github.com/clems4ever/llmbox/testutils"
)

// TestIsBrowserNavigation checks an HTML GET is treated as a navigation while an
// XHR, a WebSocket handshake, and a non-GET are not.
func TestIsBrowserNavigation(t *testing.T) {
	html := func(method string) *http.Request {
		r := httptest.NewRequest(method, "http://x/", nil)
		r.Header.Set("Accept", "text/html,application/xhtml+xml")
		return r
	}
	if !isBrowserNavigation(html(http.MethodGet)) {
		t.Error("HTML GET should be a navigation")
	}
	if isBrowserNavigation(html(http.MethodPost)) {
		t.Error("POST should not be a navigation")
	}
	xhr := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	xhr.Header.Set("Accept", "application/json")
	if isBrowserNavigation(xhr) {
		t.Error("JSON GET (XHR) should not be a navigation")
	}
	ws := html(http.MethodGet)
	ws.Header.Set("Upgrade", "websocket")
	if isBrowserNavigation(ws) {
		t.Error("WebSocket handshake should not be a navigation")
	}
}

// TestSignInURLCarriesReturn checks signInURL points at the public sign-in page
// with the current proxy request encoded as the return target.
func TestSignInURLCarriesReturn(t *testing.T) {
	s, _ := newProxyServer(t, &testutils.FakeMgr{CreateID: "abcdef0123456789"}, nil)
	r := httptest.NewRequest(http.MethodGet, "http://abc.proxy.example.com/foo?x=1", nil)
	want := "https://boxes.example.com/signin?return=" + url.QueryEscape("https://abc.proxy.example.com/foo?x=1")
	if got := s.signInURL(r); got != want {
		t.Errorf("signInURL = %q, want %q", got, want)
	}
}

// TestHandleProxyRedirectsBrowserToSignIn checks an unauthenticated browser
// navigation to a proxy is bounced to the sign-in page (302) rather than 401.
func TestHandleProxyRedirectsBrowserToSignIn(t *testing.T) {
	a := auth.NewTestAuthenticator("admin@corp.com")
	s, _ := newProxyServer(t, &dialMgr{FakeMgr: &testutils.FakeMgr{CreateID: "abcdef0123456789"}}, a)
	registerBox(t, s, "web-box", "")
	rec, err := s.createProxy("web-box", 8000, "")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://"+rec.Slug+".proxy.example.com/", nil)
	req.Host = rec.Slug + ".proxy.example.com"
	req.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	s.APIHandler().ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "https://boxes.example.com/signin?return=") {
		t.Errorf("Location = %q, want a sign-in redirect", loc)
	}
}

// signInServer builds a server with activation auth for the sign-in page tests.
func signInServer(t *testing.T) (*Server, Store) {
	t.Helper()
	return newProxyServer(t, &testutils.FakeMgr{CreateID: "abcdef0123456789"}, auth.NewTestAuthenticator("admin@corp.com"))
}

// TestSignInPageRendersButtons checks the sign-in page lists a provider button
// whose login link carries the return target.
func TestSignInPageRendersButtons(t *testing.T) {
	s, _ := signInServer(t)
	req := httptest.NewRequest(http.MethodGet, "/signin?return=%2Fdash", nil)
	w := httptest.NewRecorder()
	s.APIHandler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Sign in with Google") {
		t.Errorf("body lacks provider button: %s", body)
	}
	if !strings.Contains(body, "/auth/google/login?return=%2Fdash") {
		t.Errorf("button lacks return target: %s", body)
	}
}

// TestSignInPageRedirectsWhenSignedIn checks an already-signed-in visitor is sent
// straight to the return target instead of seeing the sign-in buttons.
func TestSignInPageRedirectsWhenSignedIn(t *testing.T) {
	s, st := signInServer(t)
	req := httptest.NewRequest(http.MethodGet, "/signin?return=%2Fdash", nil)
	req.AddCookie(signIn(t, st, false, true))
	w := httptest.NewRecorder()
	s.APIHandler().ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/dash" {
		t.Errorf("Location = %q, want /dash", loc)
	}
}

// TestSignInPageRejectsUnsafeReturn checks an unsafe return target yields a page
// with no sign-in buttons (and no redirect), never an open redirect.
func TestSignInPageRejectsUnsafeReturn(t *testing.T) {
	s, _ := signInServer(t)
	req := httptest.NewRequest(http.MethodGet, "/signin?return=https://evil.com/x", nil)
	w := httptest.NewRecorder()
	s.APIHandler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if body := w.Body.String(); strings.Contains(body, "Sign in with Google") {
		t.Errorf("body should not offer sign-in for an unsafe return: %s", body)
	}
}
