package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/auth"
	"github.com/clems4ever/llmbox/internal/sandbox"
	"github.com/clems4ever/llmbox/testutils"
)

// newAuthServer builds an auth-enabled Server backed by a real bbolt store, using
// the offline test authenticator (a single "google" sign-in button, no admin
// allow-list). The OIDC handshake itself is covered in the auth package; these
// tests exercise the server's auth-page gating around a signed-in session.
func newAuthServer(t *testing.T) (*Server, *testutils.FakeMgr, Store) {
	t.Helper()
	st, err := OpenStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := auth.NewTestAuthenticator()
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "https://claude.com/cai/oauth/authorize?x=1", SubmitURL: "https://claude.ai/code/s/1"}
	return New(f, nil, "https://boxes.example.com", time.Minute, st, a), f, st
}

// TestAuthPageRequiresLogin checks that, with auth enabled, an unauthenticated
// visitor sees the sign-in buttons and not the code-entry form.
func TestAuthPageRequiresLogin(t *testing.T) {
	s, _, _ := newAuthServer(t)
	sess, err := s.createBox(context.Background(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	h := s.APIHandler()

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
	s, f, st := newAuthServer(t)
	sess, err := s.createBox(context.Background(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	h := s.APIHandler()

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
	if f.GotCode != "" {
		t.Fatal("SubmitCode should not run without a login session")
	}

	// Seed a login session (authorized to activate) and post with the right CSRF.
	if err := st.SaveLoginSession("SID", LoginSession{Email: "alice@corp.com", CSRF: "CSRF", ExpiresAt: time.Now().Add(time.Hour), Activate: true}); err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: auth.LoginCookie, Value: "SID"}

	// Wrong CSRF -> 403, still no submission.
	if rec := post(cookie, url.Values{"code": {"X"}, "csrf": {"WRONG"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("bad-CSRF POST status = %d, want 403", rec.Code)
	}
	if f.GotCode != "" {
		t.Fatal("SubmitCode should not run with a bad CSRF token")
	}

	// Correct session + CSRF -> activation proceeds and records who activated.
	if rec := post(cookie, url.Values{"code": {"THECODE"}, "csrf": {"CSRF"}}); rec.Code != http.StatusOK {
		t.Fatalf("authorized POST status = %d, want 200", rec.Code)
	}
	if f.GotCode != "THECODE" {
		t.Errorf("submitted code = %q, want THECODE", f.GotCode)
	}
	got := s.lookup(sess.Token)
	got.mu.Lock()
	activatedBy := got.ActivatedBy
	got.mu.Unlock()
	if activatedBy != "alice@corp.com" {
		t.Errorf("ActivatedBy = %q, want alice@corp.com", activatedBy)
	}
}
