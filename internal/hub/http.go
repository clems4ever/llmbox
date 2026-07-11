package hub

import (
	_ "embed"
	"encoding/json"
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
	// middleware (bearer API key, or admin login cookie + CSRF header). Two
	// session endpoints sit outside that gate, their more specific patterns
	// winning over the subtree: /api/v1/me is cookie-only by design — it is how
	// the web app turns its login cookie into the CSRF token the gated calls
	// require — and /api/v1/logout ends the login session itself.
	mux.HandleFunc("GET /api/v1/me", s.handleMe)
	// Logout is likewise cookie-based (plus the CSRF header) rather than gated by
	// requireAPIAuth: any signed-in session may end itself, admin or not.
	mux.HandleFunc("POST /api/v1/logout", s.handleLogout)
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

// registerAppRoutes mounts the UI/API routes on mux: the optional provider
// sign-in, spoke connect, and admin routes, plus the health probe and favicon.
// Every page is a static shell from the built web app (Vite); live state travels
// only over JSON endpoints, so the server renders no HTML.
//
// @arg mux The mux to register the UI/API routes on.
//
// @testcase TestHealthz checks the /healthz route returns ok.
// @testcase TestFaviconServed checks the favicon route returns the embedded SVG.
// @testcase TestHomeRedirectsToAdmin redirects "/" to /admin when the admin UI is enabled.
func (s *Server) registerAppRoutes(mux *http.ServeMux) {
	// The web app's hashed assets are shared by every page shell (admin,
	// sign-in), so they are served whether or not the admin UI is enabled.
	s.registerAssetRoutes(mux)

	// Spoke connection endpoint (only when clustering is enabled): a spoke dials
	// this to enroll and then serve box verbs over the upgraded WebSocket.
	if s.hub != nil {
		mux.HandleFunc("/spoke/connect", s.hub.ConnectHandler)
	}

	// Provider sign-in routes (only when auth is configured): the OIDC
	// login/callback handshake plus the stand-alone sign-in page.
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

	// The server favicon, served as SVG at the conventional /favicon.ico path and
	// at /favicon.svg.
	favicon := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		// A failed write to the client is unactionable here; nothing to recover.
		_, _ = w.Write(faviconSVG)
	}
	mux.HandleFunc("GET /favicon.ico", favicon)
	mux.HandleFunc("GET /favicon.svg", favicon)
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
// @testcase TestSignInPageRendersButtons reads return buttons converted by this helper.
func providerButtons(in []auth.ProviderButton) []providerButton {
	out := make([]providerButton, 0, len(in))
	for _, b := range in {
		out = append(out, providerButton{Label: b.Label, LoginPath: b.LoginPath})
	}
	return out
}

// writeNoStoreJSON writes v as a JSON response marked uncacheable — the shape
// used by the sign-in state endpoint, whose payload carries live per-visitor
// session state no intermediary may cache.
//
// @arg w The response writer.
// @arg v The value to encode.
//
// @testcase TestSignInPageRendersButtons reads state responses written by this helper.
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
