package hub

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/clems4ever/llmbox/internal/hub/auth"
	"github.com/clems4ever/llmbox/internal/shared/api"
)

// APIHandler builds the server's single HTTP handler: the box-control JSON API
// (under /api/v1/) plus the UI (auth pages, provider sign-in, admin UI, spoke
// connect, health, favicon). Everything is served on one port; with proxying
// enabled, requests to a proxy sub-domain are reverse-proxied to the box and all
// other Hosts fall through to these routes.
//
// @return http.Handler A mux routing the box-control API and UI endpoints.
//
// @testcase TestAPIHandlerServesUIAndAPI serves both the UI routes and the box-control API.
func (s *Server) APIHandler() http.Handler {
	mux := http.NewServeMux()
	s.registerAppRoutes(mux)
	// The box-control API shares this mux under /api/v1/, gated by the API auth
	// middleware (bearer API key, or admin login cookie + CSRF header). The one
	// exception is the session endpoint /api/v1/me, which is cookie-only by
	// design: it is how the web app turns its login cookie into the CSRF token
	// the gated calls require. Its more specific pattern wins over the subtree.
	mux.HandleFunc("GET /api/v1/me", s.handleMe)
	mux.Handle("/api/v1/", s.requireAPIAuth(api.NewHandler(s.boxBackend())))
	if !s.ProxyEnabled() {
		return mux
	}
	// With proxying enabled, dispatch by Host: a request whose Host is a proxy
	// sub-domain (<slug>.<base-domain>) is reverse-proxied to the box; everything
	// else (the main UI/API host) falls through to the normal routes.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slug, ok := s.proxySlugFromHost(r.Host); ok {
			s.handleProxy(w, r, slug)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// registerAppRoutes mounts the UI/API routes on mux: the activation page and
// its JSON state/submit endpoints, the optional provider sign-in, spoke
// connect, and admin routes, plus the health probe and favicon. Every page is
// a static shell from the built web app (Vite); live state travels only over
// JSON endpoints, so the server renders no HTML.
//
// @arg mux The mux to register the UI/API routes on.
//
// @testcase TestAuthPageRendersAndSubmits drives the auth routes registered here.
// @testcase TestHealthz checks the /healthz route returns ok.
// @testcase TestFaviconServed checks the favicon route returns the embedded SVG.
// @testcase TestHomeRedirectsToAdmin redirects "/" to /admin when the admin UI is enabled.
func (s *Server) registerAppRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/{token}", s.handleAuthPage)
	mux.HandleFunc("GET /auth/{token}/state", s.handleAuthState)
	mux.HandleFunc("POST /auth/{token}/code", s.handleAuthSubmit)
	// The web app's hashed assets are shared by every page shell (admin, auth,
	// sign-in), so they are served whether or not the admin UI is enabled.
	s.registerAssetRoutes(mux)

	// Spoke connection endpoint (only when clustering is enabled): a spoke dials
	// this to enroll and then serve box verbs over the upgraded WebSocket.
	if s.hub != nil {
		mux.HandleFunc("/spoke/connect", s.hub.ConnectHandler)
	}

	// Provider sign-in routes (only when activation auth is configured). The
	// 3-segment patterns don't collide with the 2-segment /auth/{token} above.
	if s.auth != nil {
		mux.HandleFunc("GET /auth/{provider}/login", s.auth.HandleLogin)
		mux.HandleFunc("GET /auth/{provider}/callback", s.auth.HandleCallback)
		// Stand-alone sign-in page an unauthenticated proxy visitor is bounced to,
		// plus the JSON state it renders from.
		mux.HandleFunc("GET /signin", s.handleSignIn)
		mux.HandleFunc("GET /signin/state", s.handleSignInState)
	}

	// Admin web UI (only when an admin allow-list is configured): manage cluster
	// spokes and boxes through the running hub process. The bare home page then
	// redirects there so a visitor landing on "/" reaches the dashboard (which
	// itself gates on sign-in); without the admin UI there is no landing page to
	// send them to, so "/" stays a 404 as before.
	if s.auth.AdminEnabled() {
		s.registerAdminRoutes(mux)
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/admin", http.StatusFound)
		})
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		// A failed write to the client is unactionable here; nothing to recover.
		_, _ = w.Write([]byte("ok"))
	})

	// The server favicon (also referenced by the auth page), served as SVG at the
	// conventional /favicon.ico path and at /favicon.svg.
	favicon := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		// A failed write to the client is unactionable here; nothing to recover.
		_, _ = w.Write(faviconSVG)
	}
	mux.HandleFunc("GET /favicon.ico", favicon)
	mux.HandleFunc("GET /favicon.svg", favicon)
}

// handleAuthPage serves the activation page shell — a static, secret-free page
// from the built web app. All live state (box identity, auth status, sign-in
// gating) is fetched by the page from GET /auth/{token}/state, so nothing about
// the session is decided here; even an unknown token serves the shell and the
// page surfaces the state endpoint's 404.
//
// @arg w The response writer the shell is written to.
// @arg r The request (its path is ignored; the shell is the same for every token).
//
// @testcase TestAuthPageServesShell serves the activation page shell.
func (s *Server) handleAuthPage(w http.ResponseWriter, r *http.Request) {
	s.servePage(w, r, "auth.html")
}

// handleAuthState reports an auth session's state as JSON — everything the
// activation page renders. When activation auth is enabled, the response is
// gated exactly like the old server-rendered page: an unauthenticated visitor
// (e.g. someone who only has a leaked token) gets the box identity and the
// sign-in buttons, never the authorize URL, the CSRF token, the status, or the
// session URL; a signed-in non-activator additionally learns it is not
// authorized. It 404s when no session matches the token.
//
// @arg w The response writer the JSON state is written to.
// @arg r The request whose {token} path value identifies the auth session.
//
// @testcase TestAuthPageRendersAndSubmits reads a pending session's state.
// @testcase TestAuthPageShowsBoxAndSpoke carries the box ID and runner name.
// @testcase TestAuthPageUnknownToken 404s for an unknown token.
// @testcase TestAuthPageRequiresLogin hides the authorize URL and CSRF from an unauthenticated visitor.
func (s *Server) handleAuthState(w http.ResponseWriter, r *http.Request) {
	sess := s.lookup(r.PathValue("token"))
	if sess == nil {
		writeJSONError(w, http.StatusNotFound, "unknown or expired authentication session")
		return
	}
	writeNoStoreJSON(w, s.authStateFor(sess, r))
}

// authStateFor builds the activation page's JSON state for one session,
// applying the sign-in gating: the sensitive fields (authorize URL, CSRF,
// status, session URL, error) are included only when the visitor may activate —
// either activation auth is disabled, or the request carries a login session
// allowed to activate. Everyone else gets the box identity plus the sign-in
// buttons (and, for a signed-in non-activator, the not-authorized flag).
//
// @arg sess The auth session to describe.
// @arg r The request whose login cookie decides the gating.
// @return authState The gated JSON state.
//
// @testcase TestAuthPageRendersAndSubmits returns the full state when auth is disabled.
// @testcase TestAuthPageRequiresLogin returns only identity and sign-in buttons when not signed in.
func (s *Server) authStateFor(sess *session, r *http.Request) authState {
	st := authState{
		BoxID: sess.BoxID,
		Spoke: sess.SpokeName,
	}
	// The sign-in buttons must carry the plaintext token so sign-in returns to
	// /auth/{token}; sess.Token is only the hash. The plaintext is the {token} path
	// value on this /auth/{token}/... request.
	pageToken := r.PathValue("token")
	allowed := true
	if s.auth != nil {
		st.AuthEnabled = true
		allowed = false
		if ls, ok := s.auth.CurrentLogin(r); ok && ls.CanActivate {
			st.LoggedIn = true
			st.Email = ls.Email
			st.CSRF = ls.CSRFToken
			allowed = true
		} else if ok {
			st.Email = ls.Email
			st.NotAuthorized = true
			st.Providers = providerButtons(s.auth.Buttons(pageToken))
		} else {
			st.Providers = providerButtons(s.auth.Buttons(pageToken))
		}
	}
	if allowed {
		status, sessionURL, errMsg := sess.snapshot()
		st.Status = status
		st.SessionURL = sessionURL
		st.Error = errMsg
		st.AuthorizeURL = sess.AuthorizeURL
	}
	return st
}

// handleAuthSubmit feeds the pasted code to the box (blocking until login
// completes or fails), then responds with the session's fresh JSON state. The
// body is JSON ({"code", "csrf"}); the code is never logged. It 404s when no
// session matches the {token} path value, 401s when activation auth is enabled
// and the visitor is not signed in, and 403s for a non-activator identity or a
// CSRF mismatch. A submit error the session does not record (e.g. an empty
// code) is surfaced in the response's error field.
//
// @arg w The response writer the JSON result state is written to.
// @arg r The request carrying the {token} path value and the JSON code/csrf body.
//
// @testcase TestAuthPageRendersAndSubmits submits a code and returns the session URL.
// @testcase TestActivationGatedByLogin rejects missing logins and CSRF mismatches, and records the activator.
func (s *Server) handleAuthSubmit(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	sess := s.lookup(token)
	if sess == nil {
		writeJSONError(w, http.StatusNotFound, "unknown or expired authentication session")
		return
	}
	var req struct {
		Code string `json:"code"`
		CSRF string `json:"csrf"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad request body")
		return
	}

	// When activation auth is enabled, require a valid login session and a matching
	// CSRF token before accepting the code, and record who activated the box.
	if s.auth != nil {
		ls, ok := s.auth.CurrentLogin(r)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "Please sign in to activate this workspace.")
			return
		}
		if !ls.CanActivate {
			writeJSONError(w, http.StatusForbidden, fmt.Sprintf("Signed in as %s, but that account is not authorized to activate workspaces here.", ls.Email))
			return
		}
		if subtle.ConstantTimeCompare([]byte(req.CSRF), []byte(ls.CSRFToken)) != 1 {
			writeJSONError(w, http.StatusForbidden, "Invalid or missing form token; reload the page and try again.")
			return
		}
		sess.mu.Lock()
		sess.ActivatedBy = ls.Email
		sess.mu.Unlock()
	}

	// SubmitCode blocks until login completes (or fails); it records the result
	// (including any error) on the session, which the state below reflects. The
	// code itself is never logged. An error the session does not record (e.g. an
	// empty code never submitted to the box) is patched into the response so the
	// page always has something to show.
	err := s.submitCode(r.Context(), token, req.Code)
	st := s.authStateFor(sess, r)
	if err != nil && st.Error == "" {
		st.Error = err.Error()
	}
	writeNoStoreJSON(w, st)
}

// authState is the JSON the activation page renders from — the box identity,
// the sign-in gating, and (only when the visitor may activate) the live auth
// status and URLs. It is the SPA-era successor of the old server-rendered
// template's data.
type authState struct {
	// BoxID and Spoke identify which box (and which runner it runs on) this
	// activation page is for, shown so the user can tell workspaces apart.
	BoxID string `json:"box_id,omitempty"`
	Spoke string `json:"spoke,omitempty"`

	// Activation-auth fields. AuthEnabled is true when a provider is configured;
	// when so and LoggedIn is false, the page shows only the sign-in buttons.
	// NotAuthorized is true when the visitor is signed in but not allowed to
	// activate workspaces (e.g. an admin-only identity).
	AuthEnabled   bool             `json:"auth_enabled"`
	LoggedIn      bool             `json:"logged_in"`
	NotAuthorized bool             `json:"not_authorized,omitempty"`
	Email         string           `json:"email,omitempty"`
	Providers     []providerButton `json:"providers,omitempty"`

	// Gated fields, present only when the visitor may activate (auth disabled,
	// or signed in with the activate capability): the auth lifecycle status
	// (pending/ready/error), the session URL once ready, the error detail, the
	// provider authorize URL for step 1, and the CSRF token the submit needs.
	Status       string `json:"status,omitempty"`
	SessionURL   string `json:"session_url,omitempty"`
	Error        string `json:"error,omitempty"`
	AuthorizeURL string `json:"authorize_url,omitempty"`
	CSRF         string `json:"csrf,omitempty"`
}

// providerButton is one sign-in button in the JSON state: the provider's
// human label and the login path that starts its OIDC flow.
type providerButton struct {
	Label     string `json:"label"`
	LoginPath string `json:"login_path"`
}

// providerButtons converts the auth layer's buttons to their JSON wire form.
//
// @arg in The auth layer's provider buttons.
// @return []providerButton The same buttons with JSON field tags.
//
// @testcase TestAuthPageRequiresLogin reads sign-in buttons converted by this helper.
// @testcase TestSignInPageRendersButtons reads return buttons converted by this helper.
func providerButtons(in []auth.ProviderButton) []providerButton {
	out := make([]providerButton, 0, len(in))
	for _, b := range in {
		out = append(out, providerButton{Label: b.Label, LoginPath: b.LoginPath})
	}
	return out
}

// writeNoStoreJSON writes v as a JSON response marked uncacheable — the shape
// used by the activation and sign-in state endpoints, whose payloads carry live
// per-visitor session state no intermediary may cache.
//
// @arg w The response writer.
// @arg v The value to encode.
//
// @testcase TestAuthPageRendersAndSubmits reads state responses written by this helper.
func writeNoStoreJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Default().Warn("failed to encode JSON state", "err", err)
	}
}

// signInState is the JSON the stand-alone sign-in page renders from: whether
// the visitor is already signed in, the sanitized return target, and one
// sign-in button per configured provider. Providers is empty when the return
// target is missing or unsafe, in which case the page shows an explanatory
// notice — never an open redirect.
type signInState struct {
	SignedIn  bool             `json:"signed_in"`
	ReturnTo  string           `json:"return_to,omitempty"`
	Providers []providerButton `json:"providers,omitempty"`
}

// handleSignIn serves the stand-alone sign-in page an unauthenticated proxy
// visitor is bounced to. A visitor who is already signed in (with a safe
// ?return= target) is redirected straight to the target, exactly as before;
// everyone else gets the static page shell from the built web app, which
// fetches its buttons from GET /signin/state. The return target is sanitized
// via SafeReturnURL, so an unsafe one never redirects.
//
// @arg w The response writer the shell (or redirect) is written to.
// @arg r The request whose ?return= names where to go after sign-in.
//
// @testcase TestSignInPageRendersButtons serves the shell and its state carries the return target.
// @testcase TestSignInPageRedirectsWhenSignedIn bounces an already-signed-in visitor to the return target.
// @testcase TestSignInPageRejectsUnsafeReturn returns no buttons for an unsafe return target.
func (s *Server) handleSignIn(w http.ResponseWriter, r *http.Request) {
	returnTo := s.auth.SafeReturnURL(r.URL.Query().Get("return"))
	if _, ok := s.auth.CurrentLogin(r); ok && returnTo != "" {
		http.Redirect(w, r, returnTo, http.StatusFound)
		return
	}
	s.servePage(w, r, "signin.html")
}

// handleSignInState reports the sign-in page's JSON state: the sanitized
// ?return= target, whether the visitor is already signed in, and the provider
// buttons returning there after login. An unsafe or missing return target
// yields no buttons, mirroring the redirect handler's open-redirect guard.
//
// @arg w The response writer the JSON state is written to.
// @arg r The request whose ?return= names where to go after sign-in.
//
// @testcase TestSignInPageRendersButtons carries a provider button with the return target.
// @testcase TestSignInPageRejectsUnsafeReturn returns no buttons for an unsafe return target.
func (s *Server) handleSignInState(w http.ResponseWriter, r *http.Request) {
	returnTo := s.auth.SafeReturnURL(r.URL.Query().Get("return"))
	st := signInState{ReturnTo: returnTo}
	if _, ok := s.auth.CurrentLogin(r); ok {
		st.SignedIn = true
	}
	if returnTo != "" {
		st.Providers = providerButtons(s.auth.ReturnButtons(returnTo))
	}
	writeNoStoreJSON(w, st)
}

// faviconSVG is the server favicon, embedded into the binary at build time from
// favicon.svg and served at /favicon.ico and /favicon.svg.
//
//go:embed static/favicon.svg
var faviconSVG []byte
