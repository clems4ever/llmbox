package hub

import (
	"crypto/subtle"
	_ "embed"
	"fmt"
	"html/template"
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

// registerAppRoutes mounts the UI/API routes on mux: the auth web pages, the
// optional provider sign-in, spoke connect, and admin routes, plus the health
// probe and favicon.
//
// @arg mux The mux to register the UI/API routes on.
//
// @testcase TestAuthPageRendersAndSubmits drives the auth routes registered here.
// @testcase TestHealthz checks the /healthz route returns ok.
// @testcase TestFaviconServed checks the favicon route returns the embedded SVG.
// @testcase TestHomeRedirectsToAdmin redirects "/" to /admin when the admin UI is enabled.
func (s *Server) registerAppRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/{token}", s.handleAuthPage)
	mux.HandleFunc("POST /auth/{token}", s.handleAuthSubmit)

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
		// Stand-alone sign-in page an unauthenticated proxy visitor is bounced to.
		mux.HandleFunc("GET /signin", s.handleSignIn)
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

// handleAuthPage renders the current state of an auth session, looked up by the
// {token} path value. It 404s when no session matches the token.
//
// @arg w The response writer the page is rendered to.
// @arg r The request whose {token} path value identifies the auth session.
//
// @testcase TestAuthPageRendersAndSubmits renders a pending session's page.
// @testcase TestAuthPageUnknownToken 404s for an unknown token.
func (s *Server) handleAuthPage(w http.ResponseWriter, r *http.Request) {
	sess := s.lookup(r.PathValue("token"))
	if sess == nil {
		http.Error(w, "Unknown or expired authentication session.", http.StatusNotFound)
		return
	}
	status, sessionURL, errMsg := sess.snapshot()
	data := authPageData{
		Token:        sess.Token,
		AuthorizeURL: template.URL(sess.AuthorizeURL),
		Status:       status,
		SessionURL:   sessionURL,
		Error:        errMsg,
		BoxID:        sess.BoxID,
		Spoke:        sess.SpokeName,
	}
	// When activation auth is enabled, gate the whole page behind sign-in: an
	// unauthenticated visitor (e.g. someone who only has the leaked token) sees
	// only the sign-in buttons, never the activation form or the session URL.
	if s.auth != nil {
		data.AuthEnabled = true
		// Only a session authorized to activate boxes unlocks the activation form;
		// a signed-in admin who isn't a box-activator is told so rather than shown
		// the form (and an unauthenticated visitor sees only the sign-in buttons).
		if ls, ok := s.auth.CurrentLogin(r); ok && ls.Activate {
			data.LoggedIn = true
			data.Email = ls.Email
			data.CSRF = ls.CSRF
		} else if ok {
			data.Email = ls.Email
			data.NotAuthorized = true
			data.Providers = s.auth.Buttons(sess.Token)
		} else {
			data.Providers = s.auth.Buttons(sess.Token)
		}
	}
	s.render(w, data)
}

// handleAuthSubmit feeds the pasted code to the box (blocking until login
// completes or fails), then re-renders the page with the result. The code is
// never logged. It 404s when no session matches the {token} path value.
//
// @arg w The response writer the result page is rendered to.
// @arg r The request carrying the {token} path value and the posted code form field.
//
// @testcase TestAuthPageRendersAndSubmits submits a code and renders the session URL.
func (s *Server) handleAuthSubmit(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	sess := s.lookup(token)
	if sess == nil {
		http.Error(w, "Unknown or expired authentication session.", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form.", http.StatusBadRequest)
		return
	}

	// When activation auth is enabled, require a valid login session and a matching
	// CSRF token before accepting the code, and record who activated the box.
	var email string
	if s.auth != nil {
		ls, ok := s.auth.CurrentLogin(r)
		if !ok {
			http.Error(w, "Please sign in to activate this box.", http.StatusUnauthorized)
			return
		}
		if !ls.Activate {
			http.Error(w, fmt.Sprintf("Signed in as %s, but that account is not authorized to activate boxes here.", ls.Email), http.StatusForbidden)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(ls.CSRF)) != 1 {
			http.Error(w, "Invalid or missing form token; reload the page and try again.", http.StatusForbidden)
			return
		}
		email = ls.Email
		sess.mu.Lock()
		sess.ActivatedBy = email
		sess.mu.Unlock()
	}

	// SubmitCode blocks until login completes (or fails); it records the result
	// (including any error) on the session, which we then render — so the returned
	// error needs no separate handling here. The code itself is never logged.
	_ = s.submitCode(r.Context(), token, r.PostFormValue("code"))

	status, sessionURL, errMsg := sess.snapshot()
	s.render(w, authPageData{
		Token:        sess.Token,
		AuthorizeURL: template.URL(sess.AuthorizeURL),
		Status:       status,
		SessionURL:   sessionURL,
		Error:        errMsg,
		BoxID:        sess.BoxID,
		Spoke:        sess.SpokeName,
		AuthEnabled:  s.auth != nil,
		LoggedIn:     s.auth == nil || email != "",
		Email:        email,
	})
}

type authPageData struct {
	Token        string
	AuthorizeURL template.URL
	Status       string
	SessionURL   string
	Error        string

	// BoxID and Spoke identify which box (and which cluster spoke it runs on)
	// this activation page is for, shown so the user can tell boxes apart.
	BoxID string
	Spoke string

	// Activation-auth fields. AuthEnabled is true when a provider is configured;
	// when so and LoggedIn is false, the template shows only the sign-in buttons.
	// NotAuthorized is true when the visitor is signed in but not allowed to
	// activate boxes (e.g. an admin-only identity), so the page explains that
	// instead of offering the activation form.
	AuthEnabled   bool
	LoggedIn      bool
	NotAuthorized bool
	Email         string
	CSRF          string
	Providers     []auth.ProviderButton
}

// render writes the auth page for data, with no-store caching since the page
// carries live session state. A template-execution failure is logged (the
// response is already partway written, so it can't be turned into an error page).
//
// @arg w The response writer the page is written to.
// @arg data The auth page state to render.
//
// @testcase TestAuthPageRendersAndSubmits renders pages via this helper.
func (s *Server) render(w http.ResponseWriter, data authPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Don't let intermediaries cache an auth page (it contains live state).
	w.Header().Set("Cache-Control", "no-store")
	if data.Status == "ready" {
		w.WriteHeader(http.StatusOK)
	}
	if err := authTmpl.Execute(w, data); err != nil {
		s.logger().Warn("failed to render auth page", "err", err)
	}
}

// authTmplSrc is the auth page template, embedded into the binary at build time
// from auth.html.tmpl so the server ships as a single self-contained executable.
//
//go:embed templates/auth.html.tmpl
var authTmplSrc string

// authTmpl is the parsed auth page template.
var authTmpl = template.Must(template.New("auth").Parse(authTmplSrc))

// signInData is the state rendered by the stand-alone sign-in page.
type signInData struct {
	// Providers is one sign-in button per configured provider, each returning to
	// the sanitized destination after login; empty when the return target is
	// missing or unsafe, in which case the page shows an explanatory notice.
	Providers []auth.ProviderButton
}

// handleSignIn renders the stand-alone sign-in page an unauthenticated proxy
// visitor is bounced to: it lists each provider's sign-in button, all returning
// to the ?return= target once the shared login cookie is set. A visitor who is
// already signed in is redirected straight to the return target. The return
// target is sanitized via SafeReturnURL; a missing or unsafe one yields a page
// with no buttons (and no redirect), never an open redirect.
//
// @arg w The response writer the page is rendered to.
// @arg r The request whose ?return= names where to go after sign-in.
//
// @testcase TestSignInPageRendersButtons renders a provider button carrying the return target.
// @testcase TestSignInPageRedirectsWhenSignedIn bounces an already-signed-in visitor to the return target.
// @testcase TestSignInPageRejectsUnsafeReturn renders no buttons for an unsafe return target.
func (s *Server) handleSignIn(w http.ResponseWriter, r *http.Request) {
	returnTo := s.auth.SafeReturnURL(r.URL.Query().Get("return"))
	if _, ok := s.auth.CurrentLogin(r); ok && returnTo != "" {
		http.Redirect(w, r, returnTo, http.StatusFound)
		return
	}
	var buttons []auth.ProviderButton
	if returnTo != "" {
		buttons = s.auth.ReturnButtons(returnTo)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := signInTmpl.Execute(w, signInData{Providers: buttons}); err != nil {
		s.logger().Warn("failed to render sign-in page", "err", err)
	}
}

// signInTmplSrc is the sign-in page template, embedded at build time from
// signin.html.tmpl so the server ships as a single self-contained executable.
//
//go:embed templates/signin.html.tmpl
var signInTmplSrc string

// signInTmpl is the parsed sign-in page template.
var signInTmpl = template.Must(template.New("signin").Parse(signInTmplSrc))

// faviconSVG is the server favicon, embedded into the binary at build time from
// favicon.svg and served at /favicon.ico and /favicon.svg.
//
//go:embed static/favicon.svg
var faviconSVG []byte
