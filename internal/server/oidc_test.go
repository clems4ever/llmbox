package server

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

	"github.com/clems4ever/llmbox/internal/config"
	"github.com/clems4ever/llmbox/internal/docker"
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

// newAuthServer builds an auth-enabled Server backed by a real bbolt store.
func newAuthServer(t *testing.T, p *provider) (*Server, *fakeMgr, Store) {
	t.Helper()
	st, err := OpenStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	auth := &Authenticator{
		providers:  map[string]*provider{p.name: p},
		order:      []string{p.name},
		sessionTTL: time.Hour,
	}
	f := &fakeMgr{createID: "abcdef0123456789", createURL: "https://claude.com/cai/oauth/authorize?x=1", submitURL: "https://claude.ai/code/s/1"}
	return New(f, nil, "https://boxes.example.com", time.Minute, st, auth), f, st
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

// TestNewAuthenticatorDisabled checks that no enabled provider yields a nil
// Authenticator (activation stays unauthenticated) with no error.
func TestNewAuthenticatorDisabled(t *testing.T) {
	a, err := NewAuthenticator(context.Background(), config.AuthConfig{})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	if a != nil {
		t.Errorf("want nil authenticator when no provider enabled, got %v", a)
	}
}

// TestProviderLoginRedirects checks /auth/google/login persists a flow and
// redirects to the provider with state and a PKCE challenge.
func TestProviderLoginRedirects(t *testing.T) {
	s, _, st := newAuthServer(t, googleTestProvider(t, idClaims{}, nil))
	h := s.Handler(s.MCPServer("t", "v"))

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

// TestProviderCallbackActivates checks an authorized identity gets a login
// session cookie and is redirected back to the box's auth page.
func TestProviderCallbackActivates(t *testing.T) {
	s, _, st := newAuthServer(t, googleTestProvider(t, idClaims{Email: "alice@corp.com", EmailVerified: true}, nil))
	h := s.Handler(s.MCPServer("t", "v"))

	if err := st.SaveLoginFlow("STATE", loginFlow{Provider: "google", ReturnToken: "TOK", Nonce: "N", Verifier: "V", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
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
		if c.Name == loginCookie {
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
	s, _, st := newAuthServer(t, googleTestProvider(t, idClaims{Email: "mallory@evil.com", EmailVerified: true}, nil))
	h := s.Handler(s.MCPServer("t", "v"))

	if err := st.SaveLoginFlow("STATE", loginFlow{Provider: "google", ReturnToken: "TOK", Nonce: "N", Verifier: "V", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
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

// TestAuthPageRequiresLogin checks that, with auth enabled, an unauthenticated
// visitor sees the sign-in buttons and not the code-entry form.
func TestAuthPageRequiresLogin(t *testing.T) {
	s, _, _ := newAuthServer(t, googleTestProvider(t, idClaims{}, nil))
	sess, err := s.CreateBox(context.Background(), docker.CreateOptions{})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	h := s.Handler(s.MCPServer("t", "v"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/"+sess.Token, nil))

	body := rec.Body.String()
	if !strings.Contains(body, "Sign in with Google") || !strings.Contains(body, "/auth/google/login?token=") {
		t.Errorf("sign-in buttons missing from page:\n%s", body)
	}
	if strings.Contains(body, `name="code"`) {
		t.Error("code form should be hidden until signed in")
	}
}

// TestActivationGatedByLogin checks the activation POST is refused without a
// valid login session and matching CSRF, and proceeds (recording who activated)
// when both are present.
func TestActivationGatedByLogin(t *testing.T) {
	s, f, st := newAuthServer(t, googleTestProvider(t, idClaims{}, nil))
	sess, err := s.CreateBox(context.Background(), docker.CreateOptions{})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	h := s.Handler(s.MCPServer("t", "v"))

	post := func(cookie *http.Cookie, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/auth/"+sess.Token, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// No login session -> 401, code never submitted.
	if rec := post(nil, url.Values{"code": {"X"}}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth POST status = %d, want 401", rec.Code)
	}
	if f.gotCode != "" {
		t.Fatal("SubmitCode should not run without a login session")
	}

	// Seed a login session and post with the right CSRF.
	if err := st.SaveLoginSession("SID", loginSession{Email: "alice@corp.com", CSRF: "CSRF", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: loginCookie, Value: "SID"}

	// Wrong CSRF -> 403, still no submission.
	if rec := post(cookie, url.Values{"code": {"X"}, "csrf": {"WRONG"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("bad-CSRF POST status = %d, want 403", rec.Code)
	}
	if f.gotCode != "" {
		t.Fatal("SubmitCode should not run with a bad CSRF token")
	}

	// Correct session + CSRF -> activation proceeds and records who activated.
	if rec := post(cookie, url.Values{"code": {"THECODE"}, "csrf": {"CSRF"}}); rec.Code != http.StatusOK {
		t.Fatalf("authorized POST status = %d, want 200", rec.Code)
	}
	if f.gotCode != "THECODE" {
		t.Errorf("submitted code = %q, want THECODE", f.gotCode)
	}
	got := s.lookup(sess.Token)
	got.mu.Lock()
	activatedBy := got.ActivatedBy
	got.mu.Unlock()
	if activatedBy != "alice@corp.com" {
		t.Errorf("ActivatedBy = %q, want alice@corp.com", activatedBy)
	}
}
