package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/hub/apikey"
	"github.com/clems4ever/llmbox/internal/hub/auth"
	"github.com/clems4ever/llmbox/internal/hub/store"
)

// apiPost drives one POST to an /api/v1 path through the server's handler with
// optional bearer key, login cookie, and CSRF header, returning the response.
func apiPost(t *testing.T, s *Server, path, bearer string, c *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if c != nil {
		req.AddCookie(c)
	}
	if csrf != "" {
		req.Header.Set(csrfHeader, csrf)
	}
	rec := httptest.NewRecorder()
	s.APIHandler().ServeHTTP(rec, req)
	return rec
}

// TestAPIAuthAcceptsAPIKey checks a freshly minted API key admits an API call as
// a bearer token.
func TestAPIAuthAcceptsAPIKey(t *testing.T) {
	s, _, st := newAdminServer(t)
	key, err := apikey.Create(st, "test", time.Hour, time.Now())
	if err != nil {
		t.Fatalf("mint key: %v", err)
	}
	if rec := apiPost(t, s, "/api/v1/list-boxes", key, nil, ""); rec.Code != http.StatusOK {
		t.Errorf("keyed call = %d (%s), want 200", rec.Code, rec.Body.String())
	}
}

// TestAPIAuthRejectsBadAPIKey checks unknown and expired bearer keys are
// rejected with 401.
func TestAPIAuthRejectsBadAPIKey(t *testing.T) {
	s, _, st := newAdminServer(t)

	if rec := apiPost(t, s, "/api/v1/list-boxes", "lbx_bogus", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown key = %d, want 401", rec.Code)
	}

	// An expired key (stored directly with a past expiry) is rejected identically.
	if err := st.PutAPIKey(apikey.HashSecret("lbx_old"), store.APIKeyRecord{
		Name: "old", CreatedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	if rec := apiPost(t, s, "/api/v1/list-boxes", "lbx_old", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("expired key = %d, want 401", rec.Code)
	}
}

// TestAPIAuthAcceptsAdminCookie checks a signed-in admin with the matching CSRF
// header is admitted.
func TestAPIAuthAcceptsAdminCookie(t *testing.T) {
	s, _, st := newAdminServer(t)
	c := signIn(t, st, true, false)
	if rec := apiPost(t, s, "/api/v1/list-boxes", "", c, "CSRF"); rec.Code != http.StatusOK {
		t.Errorf("admin cookie call = %d (%s), want 200", rec.Code, rec.Body.String())
	}
}

// TestAPIAuthRejectsBadCSRF checks an admin cookie without the matching CSRF
// header is rejected with 403.
func TestAPIAuthRejectsBadCSRF(t *testing.T) {
	s, _, st := newAdminServer(t)
	c := signIn(t, st, true, false)
	if rec := apiPost(t, s, "/api/v1/list-boxes", "", c, "WRONG"); rec.Code != http.StatusForbidden {
		t.Errorf("wrong CSRF = %d, want 403", rec.Code)
	}
	if rec := apiPost(t, s, "/api/v1/list-boxes", "", c, ""); rec.Code != http.StatusForbidden {
		t.Errorf("missing CSRF = %d, want 403", rec.Code)
	}
}

// TestAPIAuthRejectsNonAdmin checks a signed-in non-admin session is rejected
// with 403 even with its correct CSRF token.
func TestAPIAuthRejectsNonAdmin(t *testing.T) {
	s, _, st := newAdminServer(t)
	c := signIn(t, st, false, true)
	if rec := apiPost(t, s, "/api/v1/list-boxes", "", c, "CSRF"); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin = %d, want 403", rec.Code)
	}
}

// TestAPIAuthRejectsAnonymous checks a request with no credentials is rejected
// with a 401 JSON error body.
func TestAPIAuthRejectsAnonymous(t *testing.T) {
	s, _, _ := newAdminServer(t)
	rec := apiPost(t, s, "/api/v1/list-boxes", "", nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d, want 401", rec.Code)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error == "" {
		t.Errorf("body = %q (%v), want a JSON error", rec.Body.String(), err)
	}
}

// TestMeReturnsSession checks GET /api/v1/me turns a login cookie into the
// session's email, admin flag, and CSRF token.
func TestMeReturnsSession(t *testing.T) {
	s, _, st := newAdminServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.AddCookie(signIn(t, st, true, false))
	rec := httptest.NewRecorder()
	s.APIHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("me = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var me meResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if me.Email != "admin@corp.com" || !me.Admin || me.CSRF != "CSRF" {
		t.Errorf("me = %+v", me)
	}
}

// TestLogoutClearsSession checks POST /api/v1/logout deletes the login session
// (so /api/v1/me answers 401 afterwards) and expires the login cookie. It also
// covers the non-admin path: any signed-in session may end itself.
func TestLogoutClearsSession(t *testing.T) {
	s, _, st := newAdminServer(t)
	c := signIn(t, st, false, true) // non-admin: logout must still be allowed
	rec := apiPost(t, s, "/api/v1/logout", "", c, "CSRF")
	if rec.Code != http.StatusOK {
		t.Fatalf("logout = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	cleared := false
	for _, sc := range rec.Result().Cookies() {
		if sc.Name == auth.LoginCookie && sc.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("logout did not expire the %s cookie: %v", auth.LoginCookie, rec.Result().Cookies())
	}
	// The session is gone from the store, so the cookie no longer authenticates.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.AddCookie(c)
	mrec := httptest.NewRecorder()
	s.APIHandler().ServeHTTP(mrec, req)
	if mrec.Code != http.StatusUnauthorized {
		t.Errorf("me after logout = %d, want 401", mrec.Code)
	}
}

// TestLogoutRejectsBadCSRF checks a logout without the session's CSRF token is
// rejected with 403 and the session survives.
func TestLogoutRejectsBadCSRF(t *testing.T) {
	s, _, st := newAdminServer(t)
	c := signIn(t, st, true, false)
	if rec := apiPost(t, s, "/api/v1/logout", "", c, "WRONG"); rec.Code != http.StatusForbidden {
		t.Errorf("wrong CSRF logout = %d, want 403", rec.Code)
	}
	if rec := apiPost(t, s, "/api/v1/logout", "", c, ""); rec.Code != http.StatusForbidden {
		t.Errorf("missing CSRF logout = %d, want 403", rec.Code)
	}
	// The session is untouched: the cookie still authenticates.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	s.APIHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("me after rejected logout = %d, want 200", rec.Code)
	}
}

// TestLogoutRejectsAnonymous checks POST /api/v1/logout answers 401 when no one
// is signed in.
func TestLogoutRejectsAnonymous(t *testing.T) {
	s, _, _ := newAdminServer(t)
	if rec := apiPost(t, s, "/api/v1/logout", "", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous logout = %d, want 401", rec.Code)
	}
}

// TestMeRejectsAnonymous checks GET /api/v1/me answers 401 without a login
// cookie.
func TestMeRejectsAnonymous(t *testing.T) {
	s, _, _ := newAdminServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	rec := httptest.NewRecorder()
	s.APIHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous me = %d, want 401", rec.Code)
	}
}
