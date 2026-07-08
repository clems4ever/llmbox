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

// webDist is the built admin single-page app (Vite + TypeScript, sources under
// web/ at the repo root), embedded so the server ships as one binary. The dist
// is committed; rebuild it with `make web` after changing anything under web/.
//
//go:embed all:webdist
var webDist embed.FS

// registerAdminRoutes mounts the admin web app onto mux: the SPA shell at
// /admin and its hashed assets under /admin/assets/. It is only called when the
// admin UI is enabled. The shell is a secret-free static page — the app itself
// bootstraps its session from GET /api/v1/me and drives every read and action
// through the authenticated box-control API, so no admin data or operation is
// served outside that single API.
//
// @arg mux The mux the admin routes are added to.
//
// @testcase TestAdminSPAServed serves the SPA shell at /admin and its assets.
func (s *Server) registerAdminRoutes(mux *http.ServeMux) {
	assets, err := fs.Sub(webDist, "webdist")
	if err != nil {
		// The embedded tree always contains webdist; failing here is a build defect.
		panic("webdist embed missing: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(assets))
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		// Serve the SPA shell. Its asset URLs are content-hashed, so the shell
		// itself must not be cached hard or a stale page would 404 on new assets.
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, assets, "index.html")
	})
	mux.Handle("GET /admin/assets/", http.StripPrefix("/admin/", cacheForever(fileServer)))
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

// spokeStateFile is the credential path shown in the generated spoke command. It
// matches the llmbox-spoke default so the command is truthful, and spelling it out
// makes the credential location visible — the operator can repoint it at a
// persistent path — and after first enrollment the spoke reconnects from it without
// the one-time token.
const spokeStateFile = "llmbox-spoke.json"

// spokeRunCommand builds the copy-pasteable command that starts a spoke on the
// chosen backend and enrolls it with token. It is the bare `llmbox-spoke <backend>`
// invocation — the operator runs the installed binary directly (a firecracker spoke
// needs a KVM host, not a container) — carrying the hub URL, one-time token, and the
// credential state path. The spoke reads no config file, so every other setting is
// an optional flag.
//
// @arg token The one-time join token to enroll with.
// @arg backend The box backend the spoke runs ("docker" or "firecracker").
// @return string A single-line shell command to start the spoke.
//
// @testcase TestBackendCreateSpoke renders the run command with the hub URL, backend, token, and state file.
func (s *Server) spokeRunCommand(token, backend string) string {
	return "llmbox-spoke " + backend + " --hub " + s.spokeConnectURL() +
		" --token " + token + " --state " + spokeStateFile
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
