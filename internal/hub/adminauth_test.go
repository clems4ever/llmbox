package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// wrappedAdmin wraps a sentinel handler in requireAdmin and returns the gated
// handler plus pointers recording whether the sentinel ran and the login session
// it observed, so a test can assert the middleware's allow/deny decision and what
// it hands through.
func wrappedAdmin(s *Server) (h http.HandlerFunc, called *bool, seen *LoginSession) {
	called = new(bool)
	seen = new(LoginSession)
	h = s.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		*called = true
		*seen = adminLogin(r)
		w.WriteHeader(http.StatusOK)
	})
	return h, called, seen
}

// adminPost drives one urlencoded POST through h with an optional login cookie
// and returns the recorded response.
func adminPost(t *testing.T, h http.HandlerFunc, c *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/spokes", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestRequireAdminRejectsUnauthenticated checks an unauthenticated request is
// answered 401 and never reaches the wrapped handler.
func TestRequireAdminRejectsUnauthenticated(t *testing.T) {
	s, _, _ := newAdminServer(t)
	h, called, _ := wrappedAdmin(s)

	rec := adminPost(t, h, nil, "csrf=CSRF")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
	if *called {
		t.Error("next ran for an unauthenticated request")
	}
}

// TestRequireAdminRejectsNonAdmin checks a signed-in non-admin is answered 403
// and never reaches the wrapped handler.
func TestRequireAdminRejectsNonAdmin(t *testing.T) {
	s, _, st := newAdminServer(t)
	h, called, _ := wrappedAdmin(s)

	rec := adminPost(t, h, signIn(t, st, false, true), "csrf=CSRF")
	if rec.Code != http.StatusForbidden {
		t.Errorf("code = %d, want 403", rec.Code)
	}
	if *called {
		t.Error("next ran for a non-admin request")
	}
}

// TestRequireAdminRejectsBadCSRF checks a signed-in admin whose CSRF token does
// not match is answered 403 and never reaches the wrapped handler.
func TestRequireAdminRejectsBadCSRF(t *testing.T) {
	s, _, st := newAdminServer(t)
	h, called, _ := wrappedAdmin(s)

	rec := adminPost(t, h, signIn(t, st, true, false), "csrf=WRONG")
	if rec.Code != http.StatusForbidden {
		t.Errorf("code = %d, want 403", rec.Code)
	}
	if *called {
		t.Error("next ran despite a bad CSRF token")
	}
}

// TestRequireAdminRejectsBadForm checks a malformed request body is answered 400
// before the CSRF check and never reaches the wrapped handler.
func TestRequireAdminRejectsBadForm(t *testing.T) {
	s, _, st := newAdminServer(t)
	h, called, _ := wrappedAdmin(s)

	// "%zz" is an invalid percent-escape, so ParseForm fails.
	rec := adminPost(t, h, signIn(t, st, true, false), "%zz")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", rec.Code)
	}
	if *called {
		t.Error("next ran for a malformed form")
	}
}

// TestRequireAdminAllowsAdmin checks a valid admin request reaches the wrapped
// handler and exposes the authorized login session via adminLogin.
func TestRequireAdminAllowsAdmin(t *testing.T) {
	s, _, st := newAdminServer(t)
	h, called, seen := wrappedAdmin(s)

	rec := adminPost(t, h, signIn(t, st, true, false), "csrf=CSRF")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if !*called {
		t.Fatal("next did not run for a valid admin request")
	}
	if !seen.Admin || seen.Email != "admin@corp.com" {
		t.Errorf("handler saw login %+v, want the authorized admin session", *seen)
	}
}
