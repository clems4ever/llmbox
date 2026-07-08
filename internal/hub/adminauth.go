package hub

import (
	"context"
	"crypto/subtle"
	"net/http"
)

// adminLoginCtxKey is the private context key under which requireAdmin stashes
// the authorized admin login session for the handler it wraps.
type adminLoginCtxKey struct{}

// requireAdmin is the authentication middleware for a mutating admin action. It
// gates the wrapped handler behind the checks every admin POST needs — a
// signed-in administrator, a parseable form, and a matching CSRF token —
// answering 401/403/400 itself and calling next only once the request is
// authorized. Applying it at route registration keeps the set of authenticated
// endpoints visible in one place instead of each handler re-checking auth; the
// authorized login session is carried to next on the request context, readable
// with adminLogin.
//
// @arg next The admin action handler to run once the request is authorized.
// @return http.HandlerFunc The auth-gated handler.
//
// @testcase TestRequireAdminRejectsUnauthenticated answers 401 and does not call next when no one is signed in.
// @testcase TestRequireAdminRejectsNonAdmin answers 403 for a signed-in non-admin.
// @testcase TestRequireAdminRejectsBadCSRF answers 403 when the CSRF token does not match.
// @testcase TestRequireAdminAllowsAdmin calls next and exposes the login session for a valid admin request.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ls, ok := s.auth.CurrentLogin(r)
		if !ok {
			http.Error(w, "Please sign in.", http.StatusUnauthorized)
			return
		}
		if !ls.Admin {
			http.Error(w, "Not authorized.", http.StatusForbidden)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad form.", http.StatusBadRequest)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(ls.CSRF)) != 1 {
			http.Error(w, "Invalid or missing form token; reload the page and try again.", http.StatusForbidden)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), adminLoginCtxKey{}, ls)))
	}
}

// adminLogin returns the authorized admin login session requireAdmin stored on
// the request context. It is meaningful only inside a handler wrapped by
// requireAdmin; anywhere else it returns the zero LoginSession.
//
// @arg r The request whose context requireAdmin populated.
// @return LoginSession The authorized admin login session, or the zero value when absent.
//
// @testcase TestRequireAdminAllowsAdmin reads the login session a wrapped handler sees.
func adminLogin(r *http.Request) LoginSession {
	ls, _ := r.Context().Value(adminLoginCtxKey{}).(LoginSession)
	return ls
}
