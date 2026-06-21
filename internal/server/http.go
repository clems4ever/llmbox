package server

import (
	"crypto/subtle"
	_ "embed"
	"fmt"
	"html/template"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Handler builds the HTTP handler serving the MCP endpoint (at the root), the
// auth web pages (at /auth/{token}), a /healthz probe, and the server favicon.
// When clustering is enabled (a hub was set), it also serves the spoke
// connection endpoint at /spoke/connect, and when an admin allow-list is
// configured it serves the admin UI at /admin. mcpServer is reused across
// sessions.
//
// @arg mcpServer The MCP server shared across all requests to the root endpoint.
// @return http.Handler A mux routing the MCP, auth, health, and favicon endpoints.
//
// @testcase TestAuthPageRendersAndSubmits drives the auth routes through this handler.
// @testcase TestHealthz checks the /healthz route returns ok.
// @testcase TestFaviconServed checks the favicon route returns the embedded SVG.
func (s *Server) Handler(mcpServer *mcp.Server) http.Handler {
	mux := http.NewServeMux()

	// MCP is served at the root. The more specific routes below take precedence
	// over this catch-all (Go's ServeMux matches the most specific pattern).
	//
	// SECURITY — the MCP endpoint is intentionally UNAUTHENTICATED here. The MCP
	// tools (create/get/list/destroy/logs/exec) carry no caller identity and bind
	// boxes to no owner: any client that can reach this handler can act on ANY box
	// by its box_id (including exec'ing into a box another user activated). This is
	// by design — llmbox is meant to run behind an authenticating reverse proxy
	// (e.g. oauth2-proxy / an API gateway with mTLS) that performs authn/authz on
	// every request before it reaches this mux, AND in front of a trusted set of
	// callers. Do NOT expose this port directly to untrusted networks. The OIDC
	// activation flow below (/auth) only gates who can bind a Claude account to a
	// box; it does NOT protect the MCP verbs. If per-user box isolation is ever
	// required, authenticate here and bind each box to an owner identity.
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)
	mux.Handle("/", mcpHandler)

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
		mux.HandleFunc("GET /auth/{provider}/login", s.handleProviderLogin)
		mux.HandleFunc("GET /auth/{provider}/callback", s.handleProviderCallback)
	}

	// Admin web UI (only when an admin allow-list is configured): manage cluster
	// spokes and boxes through the running hub process.
	if s.auth.AdminEnabled() {
		s.registerAdminRoutes(mux)
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

	return mux
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
	}
	// When activation auth is enabled, gate the whole page behind sign-in: an
	// unauthenticated visitor (e.g. someone who only has the leaked token) sees
	// only the sign-in buttons, never the activation form or the session URL.
	if s.auth != nil {
		data.AuthEnabled = true
		// Only a session authorized to activate boxes unlocks the activation form;
		// a signed-in admin who isn't a box-activator is told so rather than shown
		// the form (and an unauthenticated visitor sees only the sign-in buttons).
		if ls, ok := s.currentLogin(r); ok && ls.Activate {
			data.LoggedIn = true
			data.Email = ls.Email
			data.CSRF = ls.CSRF
		} else if ok {
			data.Email = ls.Email
			data.NotAuthorized = true
			data.Providers = s.auth.buttons(sess.Token)
		} else {
			data.Providers = s.auth.buttons(sess.Token)
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
		ls, ok := s.currentLogin(r)
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
	_ = s.SubmitCode(r.Context(), token, r.PostFormValue("code"))

	status, sessionURL, errMsg := sess.snapshot()
	s.render(w, authPageData{
		Token:        sess.Token,
		AuthorizeURL: template.URL(sess.AuthorizeURL),
		Status:       status,
		SessionURL:   sessionURL,
		Error:        errMsg,
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
	Providers     []providerButton
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
//go:embed auth.html.tmpl
var authTmplSrc string

// authTmpl is the parsed auth page template.
var authTmpl = template.Must(template.New("auth").Parse(authTmplSrc))

// faviconSVG is the server favicon, embedded into the binary at build time from
// favicon.svg and served at /favicon.ico and /favicon.svg.
//
//go:embed favicon.svg
var faviconSVG []byte
