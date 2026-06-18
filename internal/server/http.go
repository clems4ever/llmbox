package server

import (
	_ "embed"
	"html/template"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Handler builds the HTTP handler serving the MCP endpoint (at the root), the
// auth web pages (at /auth/{token}), a /healthz probe, and the server favicon.
// mcpServer is reused across sessions.
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
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)
	mux.Handle("/", mcpHandler)

	mux.HandleFunc("GET /auth/{token}", s.handleAuthPage)
	mux.HandleFunc("POST /auth/{token}", s.handleAuthSubmit)
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
	s.render(w, authPageData{
		Token:        sess.Token,
		AuthorizeURL: template.URL(sess.AuthorizeURL),
		Status:       status,
		SessionURL:   sessionURL,
		Error:        errMsg,
	})
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
	})
}

type authPageData struct {
	Token        string
	AuthorizeURL template.URL
	Status       string
	SessionURL   string
	Error        string
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
