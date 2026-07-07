package auth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/clems4ever/llmbox/internal/shared/config"
	"github.com/clems4ever/llmbox/internal/shared/store"
)

// fakeVerifier stands in for a real OIDC ID-token verifier so the HTTP flow can
// be exercised without a live provider.
type fakeVerifier struct {
	claims idClaims
	err    error
}

// verify returns the canned claims/error regardless of the token.
func (f fakeVerifier) verify(context.Context, string, string) (idClaims, error) {
	return f.claims, f.err
}

// googleTestProvider builds a Google-shaped provider whose token endpoint is a
// local test server (returning an id_token) and whose verifier is canned.
func googleTestProvider(t *testing.T, claims idClaims, verr error) *provider {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"a","token_type":"Bearer","id_token":"dummy","expires_in":3600}`)
	}))
	t.Cleanup(ts.Close)
	return &provider{
		name:  "google",
		label: "Google",
		oauth2: &oauth2.Config{
			ClientID:     "cid",
			ClientSecret: "sec",
			Endpoint:     oauth2.Endpoint{AuthURL: "https://accounts.google.test/authorize", TokenURL: ts.URL},
			RedirectURL:  "https://boxes.example.com/auth/google/callback",
			Scopes:       []string{"openid", "email"},
		},
		verifier:       fakeVerifier{claims: claims, err: verr},
		allowedDomains: map[string]bool{"corp.com": true},
	}
}

// newTestAuth builds an Authenticator wrapping p and bound to a fresh SQLite store,
// plus a mux mounting the login/callback handlers, so the OIDC flow can be driven
// over HTTP without a Server.
func newTestAuth(t *testing.T, p *provider) (*Authenticator, http.Handler, store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := &Authenticator{
		providers:  map[string]*provider{p.name: p},
		order:      []string{p.name},
		sessionTTL: time.Hour,
	}
	a.Bind(st, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/{provider}/login", a.HandleLogin)
	mux.HandleFunc("GET /auth/{provider}/callback", a.HandleCallback)
	return a, mux, st
}

// TestAuthorize checks the allow rules: verified email in an allowed domain or
// the email allowlist passes; unverified, wrong-domain, or hd-mismatch fails.
func TestAuthorize(t *testing.T) {
	p := &provider{
		allowedDomains: map[string]bool{"corp.com": true},
		allowedEmails:  map[string]bool{"ext@gmail.com": true},
	}
	cases := []struct {
		name string
		c    idClaims
		want bool
	}{
		{"domain ok", idClaims{Email: "a@corp.com", EmailVerified: true}, true},
		{"domain ok, hd matches", idClaims{Email: "a@corp.com", EmailVerified: true, HostedDomain: "corp.com"}, true},
		{"hd mismatch", idClaims{Email: "a@corp.com", EmailVerified: true, HostedDomain: "evil.com"}, false},
		{"email allowlist", idClaims{Email: "ext@gmail.com", EmailVerified: true}, true},
		{"unverified", idClaims{Email: "a@corp.com", EmailVerified: false}, false},
		{"other domain", idClaims{Email: "a@other.com", EmailVerified: true}, false},
		{"empty", idClaims{}, false},
	}
	for _, tc := range cases {
		if got := p.authorize(tc.c); got != tc.want {
			t.Errorf("%s: authorize = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestNewDisabled checks that no enabled provider yields a nil Authenticator
// (activation stays unauthenticated) with no error.
func TestNewDisabled(t *testing.T) {
	a, err := New(context.Background(), config.AuthConfig{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a != nil {
		t.Errorf("want nil authenticator when no provider enabled, got %v", a)
	}
}

// TestProviderLoginRedirects checks /auth/google/login persists a flow and
// redirects to the provider with state and a PKCE challenge.
func TestProviderLoginRedirects(t *testing.T) {
	_, h, st := newTestAuth(t, googleTestProvider(t, idClaims{}, nil))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/google/login?token=TOK", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("bad Location: %v", err)
	}
	if !strings.HasPrefix(rec.Header().Get("Location"), "https://accounts.google.test/authorize") {
		t.Errorf("redirect target = %q", rec.Header().Get("Location"))
	}
	state := loc.Query().Get("state")
	if state == "" || loc.Query().Get("code_challenge") == "" {
		t.Fatalf("missing state/PKCE in %q", loc.RawQuery)
	}
	flow, ok, err := st.TakeLoginFlow(state)
	if err != nil || !ok {
		t.Fatalf("flow not persisted for state: ok=%v err=%v", ok, err)
	}
	if flow.ReturnToken != "TOK" || flow.Provider != "google" {
		t.Errorf("flow = %+v", flow)
	}
}

// TestProviderLoginReturnPath checks the login flow persists a safe return path
// (the admin flow) instead of a box token.
func TestProviderLoginReturnPath(t *testing.T) {
	_, h, st := newTestAuth(t, googleTestProvider(t, idClaims{}, nil))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/google/login?return=%2Fadmin", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	flow, ok, err := st.TakeLoginFlow(loc.Query().Get("state"))
	if err != nil || !ok {
		t.Fatalf("flow not persisted: ok=%v err=%v", ok, err)
	}
	if flow.ReturnTo != "/admin" || flow.ReturnToken != "" {
		t.Errorf("flow = %+v, want ReturnTo=/admin", flow)
	}
}

// TestProviderCallbackActivates checks an authorized identity gets a login
// session cookie and is redirected back to the box's auth page.
func TestProviderCallbackActivates(t *testing.T) {
	_, h, st := newTestAuth(t, googleTestProvider(t, idClaims{Email: "alice@corp.com", EmailVerified: true}, nil))

	if err := st.SaveLoginFlow("STATE", store.LoginFlow{Provider: "google", ReturnToken: "TOK", Nonce: "N", Verifier: "V", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/google/callback?state=STATE&code=CODE", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/auth/TOK" {
		t.Errorf("Location = %q, want /auth/TOK", loc)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == LoginCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no login cookie set")
	}
	ls, ok, err := st.LoginSession(cookie.Value)
	if err != nil || !ok {
		t.Fatalf("login session not stored: ok=%v err=%v", ok, err)
	}
	if ls.Email != "alice@corp.com" {
		t.Errorf("session email = %q, want alice@corp.com", ls.Email)
	}
}

// TestProviderCallbackRejectsUnauthorized checks an identity outside the allow
// rule is refused with 403 and gets no login cookie.
func TestProviderCallbackRejectsUnauthorized(t *testing.T) {
	_, h, st := newTestAuth(t, googleTestProvider(t, idClaims{Email: "mallory@evil.com", EmailVerified: true}, nil))

	if err := st.SaveLoginFlow("STATE", store.LoginFlow{Provider: "google", ReturnToken: "TOK", Nonce: "N", Verifier: "V", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/google/callback?state=STATE&code=CODE", nil))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("unauthorized identity should get no cookie")
	}
}

// TestProviderCallbackAdminOnly checks an admin who cannot activate boxes still
// signs in with Admin=true, Activate=false.
func TestProviderCallbackAdminOnly(t *testing.T) {
	// Admin whose email is in no activation allow rule (domain admin.io is not
	// allowed) still signs in for admin, with Activate=false.
	a, h, st := newTestAuth(t, googleTestProvider(t, idClaims{Email: "boss@admin.io", EmailVerified: true}, nil))
	a.adminEmails = map[string]bool{"boss@admin.io": true}

	if err := st.SaveLoginFlow("STATE", store.LoginFlow{Provider: "google", ReturnTo: "/admin", Nonce: "N", Verifier: "V", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/google/callback?state=STATE&code=CODE", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/admin" {
		t.Errorf("Location = %q, want /admin", loc)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == LoginCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no login cookie set")
	}
	ls, ok, _ := st.LoginSession(cookie.Value)
	if !ok || !ls.Admin || ls.Activate {
		t.Errorf("session = %+v, want Admin=true Activate=false", ls)
	}
}

// TestAdminAllowlist checks AdminEnabled/isAdmin honor the allow-list (case-insensitively) and a nil authenticator.
func TestAdminAllowlist(t *testing.T) {
	a := &Authenticator{adminEmails: map[string]bool{"admin@corp.com": true}}
	if !a.AdminEnabled() {
		t.Error("AdminEnabled = false, want true")
	}
	if !a.isAdmin("Admin@Corp.com") {
		t.Error("isAdmin should be case-insensitive")
	}
	if a.isAdmin("nobody@corp.com") {
		t.Error("isAdmin allowed an unlisted email")
	}
	var nilA *Authenticator
	if nilA.AdminEnabled() || nilA.isAdmin("admin@corp.com") {
		t.Error("nil Authenticator should not enable admin")
	}
	if (&Authenticator{}).AdminEnabled() {
		t.Error("empty allow-list should disable admin")
	}
}

// TestReturnButtonsPath checks ReturnButtons builds a login link carrying the URL-encoded return target.
func TestReturnButtonsPath(t *testing.T) {
	a := &Authenticator{providers: map[string]*provider{"google": {label: "Google"}}, order: []string{"google"}}
	btns := a.ReturnButtons("/admin")
	if len(btns) != 1 || btns[0].LoginPath != "/auth/google/login?return=%2Fadmin" {
		t.Errorf("ReturnButtons = %+v", btns)
	}
}

// TestSafeReturnURL checks local paths and cookie-domain sub-domains are accepted
// while foreign hosts and non-http schemes are rejected (no open redirect).
func TestSafeReturnURL(t *testing.T) {
	a := &Authenticator{cookieDomain: "example.com"}
	cases := map[string]string{
		"/admin":                          "/admin",
		"https://x.proxy.example.com/app": "https://x.proxy.example.com/app",
		"https://example.com/y":           "https://example.com/y",
		"http://x.example.com/z":          "http://x.example.com/z",
		"https://evil.com/x":              "",
		"https://example.com.evil.com/":   "",
		"ftp://x.example.com/":            "",
		"//x.example.com/":                "",
		"":                                "",
	}
	for in, want := range cases {
		if got := a.SafeReturnURL(in); got != want {
			t.Errorf("SafeReturnURL(%q) = %q, want %q", in, got, want)
		}
	}
	// With no cookie domain, only local paths are safe.
	noDomain := &Authenticator{}
	if got := noDomain.SafeReturnURL("https://x.example.com/"); got != "" {
		t.Errorf("SafeReturnURL without cookie domain = %q, want \"\"", got)
	}
}

// TestSafeReturnPath checks local paths are accepted and absolute/protocol-relative/backslash ones are rejected.
func TestSafeReturnPath(t *testing.T) {
	cases := map[string]string{
		"/admin":            "/admin",
		"/admin?x=1":        "/admin?x=1",
		"":                  "",
		"//evil.com":        "",
		"https://evil.com":  "",
		"http://evil.com/x": "",
		"relative":          "",
		"/\\evil":           "",
	}
	for in, want := range cases {
		if got := safeReturnPath(in); got != want {
			t.Errorf("safeReturnPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNewTestAuthenticator checks the test-helper authenticator is admin-enabled,
// recognizes its admin emails case-insensitively, and exposes the single stub
// "Google" sign-in button.
func TestNewTestAuthenticator(t *testing.T) {
	a := NewTestAuthenticator("Admin@Corp.com")
	if !a.AdminEnabled() {
		t.Error("AdminEnabled() = false, want true")
	}
	if !a.isAdmin("admin@corp.com") {
		t.Error("isAdmin should match the configured admin email (case-insensitive)")
	}
	if a.isAdmin("nobody@corp.com") {
		t.Error("isAdmin should reject an unlisted email")
	}
	if btns := a.ReturnButtons("/admin"); len(btns) != 1 || btns[0].Label != "Google" {
		t.Errorf("ReturnButtons = %+v, want a single Google button", btns)
	}
}

// TestButtons checks Buttons renders one login button per provider, labelled and
// carrying the box token in the login path.
func TestButtons(t *testing.T) {
	a := NewTestAuthenticator()
	btns := a.Buttons("tok 123")
	if len(btns) != 1 {
		t.Fatalf("got %d buttons, want 1", len(btns))
	}
	if btns[0].Label != "Google" {
		t.Errorf("label = %q, want Google", btns[0].Label)
	}
	if !strings.Contains(btns[0].LoginPath, "/auth/google/login?token=tok+123") {
		t.Errorf("login path = %q, want it to carry the escaped token", btns[0].LoginPath)
	}
}

// TestCurrentLogin checks CurrentLogin resolves a live login session from the
// request cookie and treats a missing or expired session as not-signed-in.
func TestCurrentLogin(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := &Authenticator{sessionTTL: time.Hour}
	a.Bind(st, nil)

	// No cookie -> not signed in.
	if _, ok := a.CurrentLogin(httptest.NewRequest(http.MethodGet, "/", nil)); ok {
		t.Error("CurrentLogin with no cookie should be not-signed-in")
	}

	// A live session resolves via its cookie.
	if err := st.SaveLoginSession("SID", store.LoginSession{Email: "dev@corp.com", ExpiresAt: time.Now().Add(time.Hour), Activate: true}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: LoginCookie, Value: "SID"})
	if ls, ok := a.CurrentLogin(req); !ok || ls.Email != "dev@corp.com" || !ls.Activate {
		t.Errorf("CurrentLogin = (%+v, %v), want the signed-in session", ls, ok)
	}

	// An expired session is treated as not-signed-in.
	if err := st.SaveLoginSession("OLD", store.LoginSession{Email: "x@corp.com", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	reqOld := httptest.NewRequest(http.MethodGet, "/", nil)
	reqOld.AddCookie(&http.Cookie{Name: LoginCookie, Value: "OLD"})
	if _, ok := a.CurrentLogin(reqOld); ok {
		t.Error("expired session should be not-signed-in")
	}
}
