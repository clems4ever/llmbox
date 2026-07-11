package hub

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

// defaultAdminTokenTTL is the join-token validity used when a create-spoke
// request leaves the TTL unset.
const defaultAdminTokenTTL = time.Hour

// webDist is the built admin single-page app (Vite + React, sources under
// web/ at the repo root), embedded so the server ships as one binary. The dist
// is generated, not committed (only webdist/.gitkeep is tracked so the embed
// pattern always matches): build it with `make web` before compiling a binary
// that should serve the UI. CI and the Dockerfile do this automatically.
//
//go:embed all:webdist
var webDist embed.FS

// uiAssets returns the built web app's file tree (the embedded webdist).
//
// @return fs.FS The embedded web dist filesystem.
//
// @testcase TestAdminSPAServed serves shells and assets resolved through this helper.
func uiAssets() fs.FS {
	assets, err := fs.Sub(webDist, "webdist")
	if err != nil {
		// The embedded tree always contains webdist; failing here is a build defect.
		panic("webdist embed missing: " + err.Error())
	}
	return assets
}

// servePage writes one built page shell (e.g. index.html) from the web dist.
// Every shell is a secret-free static page whose live state travels over JSON
// endpoints; the shell references content-hashed assets, so it is served
// no-cache while the assets themselves are immutable (see registerAssetRoutes).
//
// @arg w The response writer the shell is written to.
// @arg r The request (used only for content negotiation by the file server).
// @arg page The shell file name inside the web dist.
//
// @testcase TestAdminSPAServed serves the admin page shell via this helper.
func (s *Server) servePage(w http.ResponseWriter, r *http.Request, page string) {
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFileFS(w, r, uiAssets(), page)
}

// registerAssetRoutes mounts the web app's content-hashed assets under
// /admin/assets/ (the bundler's base path, shared by every page shell — admin,
// sign-in). They are registered unconditionally: the proxy sign-in page must
// render even when the admin UI is disabled, and the assets are public static
// files carrying no data.
//
// @arg mux The mux the asset route is added to.
//
// @testcase TestAdminSPAServed serves a hashed asset with the immutable cache header.
// @testcase TestAssetsServedWithoutAdmin serves assets when the admin UI is disabled.
func (s *Server) registerAssetRoutes(mux *http.ServeMux) {
	fileServer := http.FileServer(http.FS(uiAssets()))
	mux.Handle("GET /admin/assets/", http.StripPrefix("/admin/", cacheForever(fileServer)))
}

// registerAdminRoutes mounts the admin web app's shell at /admin. It is only
// called when the admin UI is enabled (the shared assets are mounted separately
// by registerAssetRoutes). The shell is a secret-free static page — the app
// itself bootstraps its session from GET /api/v1/me and drives every read and
// action through the authenticated box-control API, so no admin data or
// operation is served outside that single API.
//
// @arg mux The mux the admin routes are added to.
//
// @testcase TestAdminSPAServed serves the SPA shell at /admin and its assets.
func (s *Server) registerAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		// Serve the SPA shell. Its asset URLs are content-hashed, so the shell
		// itself must not be cached hard or a stale page would 404 on new assets.
		s.servePage(w, r, "index.html")
	})
}

// cacheForever marks a response as immutable for HTTP caches. The SPA's assets
// are content-hashed by the bundler, so a URL's body can never change and the
// browser may cache it indefinitely.
//
// @arg next The file-serving handler whose responses are immutable.
// @return http.Handler The handler with the immutable cache header applied.
//
// @testcase TestAdminSPAServed checks assets are served with the immutable cache header.
func cacheForever(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		next.ServeHTTP(w, r)
	})
}

// spokeRunCommand builds the copy-pasteable command that starts a spoke on the
// chosen backend and enrolls it with token. It is the bare `llmbox-spoke <backend>`
// invocation — the operator runs the installed binary directly (a firecracker spoke
// needs a KVM host, not a container) — carrying only the hub URL and the one-time
// token. The credential lands at the spoke's built-in default
// (~/.llmbox/llmbox-spoke.json), so no --state flag is needed; the spoke reads no
// config file, and every other setting (including --state) is an optional flag.
//
// @arg token The one-time join token to enroll with.
// @arg backend The box backend the spoke runs ("docker" or "firecracker").
// @return string A single-line shell command to start the spoke.
//
// @testcase TestBackendCreateSpoke renders the run command with the hub URL, backend, and token.
func (s *Server) spokeRunCommand(token, backend string) string {
	return "llmbox-spoke " + backend + " --hub " + s.spokeConnectURL() + " --token " + token
}

// spokeConnectURL is the WebSocket URL a spoke dials to join this hub, derived
// from the public URL (https→wss, http→ws) with the /spoke/connect path.
//
// @return string The spoke-connect URL embedded in the generated spoke command.
//
// @testcase TestSpokeConnectURL derives the ws(s) scheme from the public URL.
func (s *Server) spokeConnectURL() string {
	u := s.publicURL
	switch {
	case strings.HasPrefix(u, "https://"):
		u = "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		u = "ws://" + strings.TrimPrefix(u, "http://")
	}
	return strings.TrimRight(u, "/") + "/spoke/connect"
}
