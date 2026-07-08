package hub

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/clems4ever/llmbox/internal/hub/apikey"
)

// csrfHeader is the request header a browser session must echo its login
// session's CSRF token in on every API call. The token reaches the page via the
// session endpoint (/api/v1/me), never a cookie, so a cross-site request cannot
// forge it. API-key callers are exempt: a bearer key is not an ambient
// credential, so CSRF does not apply.
const csrfHeader = "X-CSRF-Token"

// principalCtxKey is the private context key under which requireAPIAuth stashes
// the authenticated caller's identity for the handlers it wraps.
type principalCtxKey struct{}

// principalFrom returns the authenticated caller identity requireAPIAuth stored
// on the request context: the signed-in admin's email, or "apikey:<name>" for an
// API-key caller. It is empty outside a wrapped handler.
//
// @arg ctx The request context the API auth middleware populated.
// @return string The caller identity, or "" when absent.
//
// @testcase TestCreateProxyRecordsPrincipal reads the principal a wrapped handler sees.
func principalFrom(ctx context.Context) string {
	p, _ := ctx.Value(principalCtxKey{}).(string)
	return p
}

// requireAPIAuth is the authentication middleware for the box-control API. It
// admits a request on either of two credentials — an API key presented as a
// bearer token (Authorization: Bearer <key>, minted with `llmbox-server apikey
// add`), or a signed-in administrator's login cookie together with a matching
// X-CSRF-Token header — and rejects everything else with a JSON error, so the
// one API serves both headless callers (llmbox-mcp, scripts) and the web app
// under a single gate. The authenticated principal is stamped on the request
// context for handlers that record identity (see principalFrom).
//
// @arg next The API handler to run once the request is authenticated.
// @return http.Handler The auth-gated handler.
//
// @testcase TestAPIAuthAcceptsAPIKey admits a valid bearer key and stamps its principal.
// @testcase TestAPIAuthRejectsBadAPIKey rejects an unknown or expired bearer key.
// @testcase TestAPIAuthAcceptsAdminCookie admits an admin cookie with the CSRF header.
// @testcase TestAPIAuthRejectsBadCSRF rejects an admin cookie whose CSRF header is wrong or missing.
// @testcase TestAPIAuthRejectsNonAdmin rejects a signed-in non-admin session.
// @testcase TestAPIAuthRejectsAnonymous rejects a request with no credentials.
func (s *Server) requireAPIAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1) Bearer API key: the headless-caller path.
		if key, ok := bearerToken(r); ok {
			rec, ok, err := apikey.Authenticate(s.store, key, time.Now())
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "verifying API key: "+err.Error())
				return
			}
			if !ok {
				writeJSONError(w, http.StatusUnauthorized, "invalid or expired API key")
				return
			}
			ctx := context.WithValue(r.Context(), principalCtxKey{}, "apikey:"+rec.Name)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		// 2) Admin login cookie + CSRF header: the browser path. s.auth is nil when
		// no sign-in provider is configured; only API keys work then.
		if s.auth != nil {
			if ls, ok := s.auth.CurrentLogin(r); ok {
				if !ls.Admin {
					writeJSONError(w, http.StatusForbidden, "not an administrator")
					return
				}
				if subtle.ConstantTimeCompare([]byte(r.Header.Get(csrfHeader)), []byte(ls.CSRF)) != 1 {
					writeJSONError(w, http.StatusForbidden, "invalid or missing "+csrfHeader+" header")
					return
				}
				ctx := context.WithValue(r.Context(), principalCtxKey{}, ls.Email)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}
		writeJSONError(w, http.StatusUnauthorized, "authentication required: pass an API key as a bearer token, or sign in")
	})
}

// bearerToken extracts the credential from an "Authorization: Bearer <token>"
// header; ok is false when the header is absent or not a bearer scheme.
//
// @arg r The request whose Authorization header is read.
// @return string The bearer credential (may be empty when malformed).
// @return bool True when a bearer Authorization header was present.
//
// @testcase TestAPIAuthAcceptsAPIKey extracts the key this way on the accept path.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

// writeJSONError writes an API error body ({"error": msg}) with the given
// status, matching the error shape of every other box-control API response so
// clients need a single decoder.
//
// @arg w The response writer.
// @arg status The HTTP status code to send.
// @arg msg The error message for the body.
//
// @testcase TestAPIAuthRejectsAnonymous reads the JSON error body written here.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// meResponse is the body of GET /api/v1/me: who the browser's login cookie says
// the caller is, and the CSRF token their subsequent API calls must echo in the
// X-CSRF-Token header. It is how the web app bootstraps an authenticated API
// session from nothing but the cookie.
type meResponse struct {
	Email string `json:"email"`
	Admin bool   `json:"admin"`
	CSRF  string `json:"csrf"`
}

// handleMe answers GET /api/v1/me for the web app: 401 when no one is signed in
// (or sign-in is not configured), otherwise the session's email, admin
// capability, and CSRF token. It is cookie-only by design — it exists to turn a
// login cookie into an API-usable session — and safe to serve without a CSRF
// check because it changes nothing and its response is unreadable cross-origin.
//
// @arg w The response writer the JSON session is written to.
// @arg r The request whose login cookie identifies the session.
//
// @testcase TestMeReturnsSession returns the signed-in admin's email and CSRF token.
// @testcase TestMeRejectsAnonymous answers 401 with no login cookie.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeJSONError(w, http.StatusUnauthorized, "sign-in is not configured; use an API key")
		return
	}
	ls, ok := s.auth.CurrentLogin(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "not signed in")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// The CSRF token is per-session state; never let a cache serve it across users.
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(meResponse{Email: ls.Email, Admin: ls.Admin, CSRF: ls.CSRF})
}
