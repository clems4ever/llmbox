package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/hub/auth"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/testutils"
)

// newAuthServer builds an auth-enabled Server backed by a real SQLite store, using
// the offline test authenticator (a single "google" sign-in button, no admin
// allow-list). The OIDC handshake itself is covered in the auth package; these
// tests exercise the server's activation-state gating around a signed-in session.
func newAuthServer(t *testing.T) (*Server, *testutils.FakeMgr, Store) {
	t.Helper()
	st, err := OpenStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := auth.NewTestAuthenticator()
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "https://claude.com/cai/oauth/authorize?x=1", SubmitURL: "https://claude.ai/code/s/1"}
	return wireSpoke(New(nil, "https://boxes.example.com", time.Minute, st, a), f), f, st
}

// TestAuthPageRequiresLogin checks that, with auth enabled, an unauthenticated
// visitor's state carries the sign-in buttons and none of the gated fields
// (authorize URL, CSRF, status) — the JSON analogue of hiding the code form.
func TestAuthPageRequiresLogin(t *testing.T) {
	s, _, _ := newAuthServer(t)
	sess, err := s.createBox(context.Background(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}

	code, st := authStateJSON(t, s.APIHandler(), sess.Token)
	if code != http.StatusOK {
		t.Fatalf("GET state status %d", code)
	}
	providers, _ := st["providers"].([]any)
	if len(providers) != 1 {
		t.Fatalf("providers = %v, want the google sign-in button", st["providers"])
	}
	button, _ := providers[0].(map[string]any)
	if button["label"] != "Google" || !strings.Contains(fmt.Sprint(button["login_path"]), "/auth/google/login?token=") {
		t.Errorf("sign-in button = %v, want the google login path with the token", button)
	}
	for _, gated := range []string{"authorize_url", "csrf", "status", "session_url"} {
		if v, ok := st[gated]; ok {
			t.Errorf("%s = %v should be hidden until signed in", gated, v)
		}
	}
	if st["logged_in"] != false || st["auth_enabled"] != true {
		t.Errorf("gating flags wrong: %v", st)
	}
}

// TestActivationGatedByLogin checks the code submit is refused without a valid
// login session and matching CSRF, and proceeds (recording who activated) when
// both are present.
func TestActivationGatedByLogin(t *testing.T) {
	s, f, st := newAuthServer(t)
	sess, err := s.createBox(context.Background(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	h := s.APIHandler()

	post := func(cookie *http.Cookie, body map[string]string) *httptest.ResponseRecorder {
		payload, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/auth/"+sess.Token+"/code", strings.NewReader(string(payload)))
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// No login session -> 401, code never submitted.
	if rec := post(nil, map[string]string{"code": "X"}); rec.Code != http.StatusUnauthorized {
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
	if rec := post(cookie, map[string]string{"code": "X", "csrf": "WRONG"}); rec.Code != http.StatusForbidden {
		t.Fatalf("bad-CSRF POST status = %d, want 403", rec.Code)
	}
	if f.GotCode != "" {
		t.Fatal("SubmitCode should not run with a bad CSRF token")
	}

	// Correct session + CSRF -> activation proceeds and records who activated.
	if rec := post(cookie, map[string]string{"code": "THECODE", "csrf": "CSRF"}); rec.Code != http.StatusOK {
		t.Fatalf("authorized POST status = %d, want 200: %s", rec.Code, rec.Body.String())
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
