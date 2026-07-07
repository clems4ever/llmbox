package server

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/clems4ever/llmbox/internal/auth"
	"github.com/clems4ever/llmbox/internal/cluster"
	"github.com/clems4ever/llmbox/internal/sandbox"
	"github.com/clems4ever/llmbox/internal/store"
)

// adminTokenIDLen is how many leading hash characters the admin UI shows for a
// join token; the full ID is still submitted in the revoke form.
const adminTokenIDLen = 12

// defaultAdminTokenTTL is the join-token validity used when the admin form leaves
// the TTL blank.
const defaultAdminTokenTTL = time.Hour

// registerAdminRoutes wires the admin UI (dashboard plus spoke/token/box actions)
// onto mux. It is only called when the admin UI is enabled.
//
// @arg mux The mux the admin routes are added to.
//
// @testcase TestAdminDashboardGate drives the registered routes through the handler.
func (s *Server) registerAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", s.handleAdmin)
	mux.HandleFunc("GET /admin.js", s.handleAdminJS)
	mux.HandleFunc("POST /admin/spokes", s.handleAdminCreateSpoke)
	mux.HandleFunc("POST /admin/spokes/delete", s.handleAdminDropSpoke)
	mux.HandleFunc("POST /admin/default-spoke", s.handleAdminSetDefaultSpoke)
	mux.HandleFunc("POST /admin/tokens/delete", s.handleAdminRevokeToken)
	mux.HandleFunc("POST /admin/boxes", s.handleAdminCreateBox)
	mux.HandleFunc("POST /admin/boxes/delete", s.handleAdminDeleteBox)
	if s.ProxyEnabled() {
		mux.HandleFunc("POST /admin/proxies", s.handleAdminCreateProxy)
		mux.HandleFunc("POST /admin/proxies/delete", s.handleAdminDeleteProxy)
	}
}

// handleAdminJS serves the admin page's client script. It is a
// static, secret-free asset (kept in its own file so its braces don't collide
// with the HTML template's delimiters), so it needs no auth.
//
// @arg w The response writer.
// @arg r The request (unused beyond routing).
//
// @testcase TestAdminJSServed serves the script with a javascript content type.
func (s *Server) handleAdminJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(adminJS))
}

// adminToken is one outstanding join token rendered in the admin UI.
type adminToken struct {
	ID      string // full hash, submitted by the revoke form
	Short   string // shortened hash for display
	Name    string
	Expires string
	Expired bool
}

// adminBox is one box rendered in the admin UI (pre-formatted for the template).
type adminBox struct {
	BoxID   string
	Spoke   string
	Image   string
	State   string
	Phase   string
	Created string
	// AuthURL is the activation URL for a box still awaiting sign-in; SessionURL
	// is the drive-the-box URL once it is ready. Resolved from the live session
	// on every render so the activation link survives a page refresh (a box's
	// one-time creation result is otherwise lost on reload).
	AuthURL    string
	SessionURL string
}

// adminProxy is one enabled proxy rendered in the admin UI (pre-formatted).
type adminProxy struct {
	Slug        string
	URL         string
	BoxID       string
	Port        int
	Spoke       string
	CreatedBy   string
	Created     string
	Description string
}

// newProxyResult is the one-time output shown after enabling a proxy.
type newProxyResult struct {
	BoxID       string `json:"boxId"`
	Port        int    `json:"port"`
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}

// newSpokeResult is the one-time output shown after minting a join token.
type newSpokeResult struct {
	Name    string `json:"name"`
	Token   string `json:"token"`
	Command string `json:"command"`
}

// newBoxResult is the one-time output shown after creating a box.
type newBoxResult struct {
	BoxID   string `json:"boxId"`
	Spoke   string `json:"spoke"`
	AuthURL string `json:"authUrl"`
}

// adminPageData is the admin page template context.
type adminPageData struct {
	// Sign-in state. SignIn holds the provider buttons when the visitor is not
	// signed in; NotAdmin is set when signed in without admin rights.
	SignIn   []auth.ProviderButton
	SignedIn bool
	NotAdmin bool
	Email    string
	CSRF     string

	// Dashboard data (only populated for an admin).
	Spokes          []SpokeStatus
	Tokens          []adminToken
	Boxes           []adminBox
	ConnectedSpokes []string
	// DefaultSpoke is the spoke an unqualified box create runs on ("" when unset).
	DefaultSpoke string

	// ProxyEnabled gates the proxies card; Proxies are the enabled HTTP proxies.
	ProxyEnabled bool
	Proxies      []adminProxy
}

// handleAdmin renders the admin dashboard for an authorized admin, the admin
// sign-in buttons for an unauthenticated visitor, or a not-authorized notice for
// a signed-in non-admin.
//
// @arg w The response writer the page is rendered to.
// @arg r The incoming request (read for the login cookie and flash query params).
//
// @testcase TestAdminDashboardGate shows sign-in to anonymous, 403 notice to non-admins, dashboard to admins.
func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	ls, ok := s.auth.CurrentLogin(r)
	if !ok {
		s.renderAdmin(w, adminPageData{SignIn: s.auth.ReturnButtons("/admin")})
		return
	}
	if !ls.Admin {
		w.WriteHeader(http.StatusForbidden)
		s.renderAdmin(w, adminPageData{SignedIn: true, NotAdmin: true, Email: ls.Email})
		return
	}
	s.renderAdmin(w, s.adminDashboard(r, ls))
}

// adminDashboard builds the base dashboard data (spokes, tokens, boxes) for an
// authorized admin. Listing errors degrade gracefully to empty sections rather
// than failing the whole page.
//
// @arg r The request (for the request context used by box listing).
// @arg ls The signed-in admin's login session (for the CSRF token and email).
// @return adminPageData The populated dashboard context.
//
// @testcase TestAdminDashboardGate renders the dashboard sections for an admin.
func (s *Server) adminDashboard(r *http.Request, ls LoginSession) adminPageData {
	data := adminPageData{SignedIn: true, Email: ls.Email, CSRF: ls.CSRF}

	if spokes, err := s.SpokeStatuses(r.Context()); err != nil {
		s.logger().Warn("admin: listing spokes", "err", err)
	} else {
		data.Spokes = spokes
		for _, sp := range spokes {
			if sp.Connected {
				data.ConnectedSpokes = append(data.ConnectedSpokes, sp.Name)
			}
			if sp.Default {
				data.DefaultSpoke = sp.Name
			}
		}
	}

	if tokens, err := s.store.ListJoinTokens(); err != nil {
		s.logger().Warn("admin: listing join tokens", "err", err)
	} else {
		data.Tokens = toAdminTokens(tokens, time.Now())
	}

	if boxes, err := s.listBoxes(r.Context()); err != nil {
		s.logger().Warn("admin: listing boxes", "err", err)
	} else {
		rows := toAdminBoxes(boxes)
		// Resolve each box's activation/session URL from its live session so the
		// link is always present after a refresh, not just on the create result.
		for i := range rows {
			sess := s.lookupByBoxID(rows[i].BoxID)
			if sess == nil {
				continue
			}
			status, sessionURL, _ := sess.snapshot()
			if status == "ready" {
				rows[i].SessionURL = sessionURL
			} else {
				rows[i].AuthURL = s.AuthPageURL(sess.Token)
			}
		}
		data.Boxes = rows
	}

	if s.ProxyEnabled() {
		data.ProxyEnabled = true
		if proxies, err := s.listProxies(""); err != nil {
			s.logger().Warn("admin: listing proxies", "err", err)
		} else {
			data.Proxies = toAdminProxies(proxies, s)
		}
	}
	return data
}

// toAdminProxies maps stored proxies to their display form, sorted by box ID
// then port, resolving each proxy's public URL via the server.
//
// @arg proxies The enabled proxies.
// @arg s The server used to resolve each proxy's public URL.
// @return []adminProxy The display rows.
//
// @testcase TestAdminCreateProxy renders the proxies card (including the description) for an admin.
func toAdminProxies(proxies []store.ProxyRecord, s *Server) []adminProxy {
	out := make([]adminProxy, 0, len(proxies))
	for _, p := range proxies {
		out = append(out, adminProxy{
			Slug:        p.Slug,
			URL:         s.proxyURL(p.Slug),
			BoxID:       p.BoxID,
			Port:        p.Port,
			Spoke:       p.Spoke,
			CreatedBy:   p.CreatedBy,
			Created:     p.CreatedAt.UTC().Format(time.RFC3339),
			Description: p.Description,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BoxID != out[j].BoxID {
			return out[i].BoxID < out[j].BoxID
		}
		return out[i].Port < out[j].Port
	})
	return out
}

// toAdminTokens maps store join-token records to their display form, sorted by
// spoke name then expiry, flagging expired tokens against now.
//
// @arg tokens The outstanding join tokens.
// @arg now The current time, to flag expired tokens.
// @return []adminToken The display rows.
//
// @testcase TestToAdminTokens shortens IDs, sorts by name, and flags expiry.
func toAdminTokens(tokens []cluster.JoinTokenInfo, now time.Time) []adminToken {
	out := make([]adminToken, 0, len(tokens))
	for _, t := range tokens {
		short := t.ID
		if len(short) > adminTokenIDLen {
			short = short[:adminTokenIDLen]
		}
		out = append(out, adminToken{
			ID:      t.ID,
			Short:   short,
			Name:    t.Name,
			Expires: t.ExpiresAt.Format(time.RFC3339),
			Expired: now.After(t.ExpiresAt),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Expires < out[j].Expires
	})
	return out
}

// toAdminBoxes maps docker boxes to their display form, sorted by box ID.
//
// @arg boxes The boxes to display.
// @return []adminBox The display rows.
//
// @testcase TestToAdminBoxes formats and sorts boxes for display.
func toAdminBoxes(boxes []sandbox.Box) []adminBox {
	out := make([]adminBox, 0, len(boxes))
	for _, b := range boxes {
		id := b.BoxID
		if id == "" {
			id = b.Name
		}
		out = append(out, adminBox{
			BoxID:   id,
			Spoke:   b.Spoke,
			Image:   b.Image,
			State:   b.State,
			Phase:   b.Phase,
			Created: time.Unix(b.Created, 0).UTC().Format(time.RFC3339),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BoxID < out[j].BoxID })
	return out
}

// requireAdminPost authorizes a mutating admin request: it requires a signed-in
// admin session and a matching CSRF token, and parses the form. On failure it
// writes the response and returns ok=false.
//
// @arg w The response writer (an error is written on failure).
// @arg r The request to authorize and parse.
// @return LoginSession The admin's session when authorized.
// @return bool True when the request may proceed.
//
// @testcase TestAdminActionsRequireAdminAndCSRF rejects non-admins and bad CSRF tokens.
// @testcase TestAdminDeleteBox accepts the admin page's urlencoded fetch submit.
func (s *Server) requireAdminPost(w http.ResponseWriter, r *http.Request) (LoginSession, bool) {
	ls, ok := s.auth.CurrentLogin(r)
	if !ok {
		http.Error(w, "Please sign in.", http.StatusUnauthorized)
		return LoginSession{}, false
	}
	if !ls.Admin {
		http.Error(w, "Not authorized.", http.StatusForbidden)
		return LoginSession{}, false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form.", http.StatusBadRequest)
		return LoginSession{}, false
	}
	if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(ls.CSRF)) != 1 {
		http.Error(w, "Invalid or missing form token; reload the page and try again.", http.StatusForbidden)
		return LoginSession{}, false
	}
	return ls, true
}

// handleAdminCreateSpoke mints a one-time join token for a named spoke and
// returns the token and ready-to-run spoke command as JSON. Because the token is
// minted through the running hub, it lands in the very store the hub reads.
//
// @arg w The response writer the JSON result is written to.
// @arg r The request carrying the spoke name and optional TTL.
//
// @testcase TestAdminCreateSpokeMintsToken mints a token in the server's own store.
func (s *Server) handleAdminCreateSpoke(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdminPost(w, r); !ok {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		writeResult(w, "", "spoke name is required")
		return
	}
	ttl := defaultAdminTokenTTL
	if v := strings.TrimSpace(r.PostFormValue("ttl")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			writeResult(w, "", "invalid TTL (use e.g. 1h, 30m)")
			return
		}
		ttl = d
	}
	token, err := cluster.CreateJoinToken(s.store, name, ttl, time.Now())
	if err != nil {
		writeResult(w, "", "creating token: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "newSpoke": &newSpokeResult{
		Name:    name,
		Token:   token,
		Command: s.spokeRunCommand(name, token),
	}})
}

// defaultSpokeImage is the image named in the spoke command when none was
// configured (it is display-only — see config.DefaultSpokeImage).
const defaultSpokeImage = "ghcr.io/clems4ever/granular-llmbox:latest"

// spokeRunCommand builds the full, copy-pasteable `docker run …` command that
// starts a spoke (the llmbox-spoke image) and enrolls it with token. It is a
// single line so it pastes and runs as one command, and bakes in the things
// operators routinely get wrong: a persistent state volume (so the credential
// survives and the one-time token isn't needed again), the Docker socket mount,
// and --group-add for the socket's group (the spoke runs as a non-root user and
// otherwise gets "permission denied" on the socket). The spoke reads no config
// file, so every other setting is an optional flag on this command.
//
// @arg name The spoke name (used to name the container and its state volume).
// @arg token The one-time join token to enroll with.
// @return string A single-line shell command to start the spoke.
//
// @testcase TestAdminCreateSpokeMintsToken renders the run command with the hub URL and token.
func (s *Server) spokeRunCommand(name, token string) string {
	img := s.spokeImage
	if img == "" {
		img = defaultSpokeImage
	}
	return strings.Join([]string{
		"docker run -d --name llmbox-spoke-" + name + " --restart unless-stopped",
		"-v llmbox-spoke-" + name + ":/state",
		"-v /var/run/docker.sock:/var/run/docker.sock",
		"--group-add \"$(stat -c '%g' /var/run/docker.sock)\"",
		img,
		"--hub " + s.spokeConnectURL() + " --token " + token + " --state /state/llmbox-spoke.json",
	}, " ")
}

// handleAdminDropSpoke removes a spoke's enrollment and any of its outstanding
// join tokens, then force-closes its live connection so it is dropped at once
// and cannot reconnect.
//
// @arg w The response writer the JSON result is written to.
// @arg r The request carrying the spoke name.
//
// @testcase TestAdminDropSpokeRemovesAndKicks deletes the record and disconnects the live link.
// @testcase TestAdminDropDefaultSpokeClearsDefault clears the default when the dropped spoke was it.
func (s *Server) handleAdminDropSpoke(w http.ResponseWriter, r *http.Request) {
	_, ok := s.requireAdminPost(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		writeResult(w, "", "spoke name is required")
		return
	}
	if err := s.store.DeleteSpoke(name); err != nil {
		writeResult(w, "", "dropping spoke: "+err.Error())
		return
	}
	// Revoke any outstanding join tokens for this spoke so it can't re-enroll.
	if tokens, err := s.store.ListJoinTokens(); err == nil {
		for _, t := range tokens {
			if t.Name == name {
				if derr := s.store.DeleteJoinToken(t.ID); derr != nil {
					s.logger().Warn("admin: deleting join token", "spoke", name, "err", derr)
				}
			}
		}
	}
	if s.hub != nil {
		s.hub.Disconnect(name)
	}
	// A box created with no spoke routes to the default spoke; if the one just
	// dropped was the default, clear it so unqualified creates fail loudly rather
	// than silently targeting a spoke that no longer exists.
	if def, err := s.DefaultSpoke(); err == nil && def == name {
		if cerr := s.SetDefaultSpoke(""); cerr != nil {
			s.logger().Warn("admin: clearing default spoke after drop", "spoke", name, "err", cerr)
		}
	}
	writeResult(w, "dropped spoke "+name, "")
}

// handleAdminSetDefaultSpoke makes a named enrolled spoke the default that a box
// created with no explicit spoke runs on. The spoke must be currently enrolled.
//
// @arg w The response writer the JSON result is written to.
// @arg r The request carrying the spoke name.
//
// @testcase TestAdminSetDefaultSpoke persists the chosen default spoke.
// @testcase TestAdminSetDefaultSpokeUnknown rejects a spoke that is not enrolled.
func (s *Server) handleAdminSetDefaultSpoke(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdminPost(w, r); !ok {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		writeResult(w, "", "spoke name is required")
		return
	}
	// Only allow enrolled spokes as the default so a typo can't silently disable
	// unqualified box creation.
	enrolled, err := s.store.ListSpokes()
	if err != nil {
		writeResult(w, "", "listing spokes: "+err.Error())
		return
	}
	known := false
	for _, rec := range enrolled {
		if rec.Name == name {
			known = true
			break
		}
	}
	if !known {
		writeResult(w, "", "spoke "+name+" is not enrolled")
		return
	}
	if err := s.SetDefaultSpoke(name); err != nil {
		writeResult(w, "", "setting default spoke: "+err.Error())
		return
	}
	writeResult(w, "default spoke is now "+name, "")
}

// handleAdminRevokeToken deletes a single outstanding join token by its full ID.
//
// @arg w The response writer the JSON result is written to.
// @arg r The request carrying the token ID.
//
// @testcase TestAdminRevokeToken deletes the token by ID.
func (s *Server) handleAdminRevokeToken(w http.ResponseWriter, r *http.Request) {
	_, ok := s.requireAdminPost(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PostFormValue("id"))
	if id == "" {
		writeResult(w, "", "token id is required")
		return
	}
	if err := s.store.DeleteJoinToken(id); err != nil {
		writeResult(w, "", "revoking token: "+err.Error())
		return
	}
	writeResult(w, "revoked join token", "")
}

// handleAdminCreateBox creates a box on the chosen spoke and returns its auth URL
// as JSON (the box still needs a user to complete activation).
//
// @arg w The response writer the JSON result is written to.
// @arg r The request carrying the box ID, optional image/description, and spoke.
//
// @testcase TestAdminCreateBox creates a box on the requested spoke.
func (s *Server) handleAdminCreateBox(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdminPost(w, r); !ok {
		return
	}
	boxID := strings.TrimSpace(r.PostFormValue("box_id"))
	if boxID == "" {
		writeResult(w, "", "box id is required")
		return
	}
	sess, err := s.createBox(r.Context(), sandbox.CreateOptions{
		BoxID:       boxID,
		Image:       strings.TrimSpace(r.PostFormValue("image")),
		Description: strings.TrimSpace(r.PostFormValue("description")),
		SpokeName:   strings.TrimSpace(r.PostFormValue("spoke")),
	})
	if err != nil {
		writeResult(w, "", "creating box: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "newBox": &newBoxResult{
		BoxID:   sess.BoxID,
		Spoke:   sess.SpokeName,
		AuthURL: s.AuthPageURL(sess.Token),
	}})
}

// handleAdminDeleteBox removes a box by its box ID.
//
// @arg w The response writer the JSON result is written to.
// @arg r The request carrying the box ID.
//
// @testcase TestAdminDeleteBox destroys the box by ID.
func (s *Server) handleAdminDeleteBox(w http.ResponseWriter, r *http.Request) {
	_, ok := s.requireAdminPost(w, r)
	if !ok {
		return
	}
	boxID := strings.TrimSpace(r.PostFormValue("box_id"))
	if boxID == "" {
		writeResult(w, "", "box id is required")
		return
	}
	if err := s.destroyBox(r.Context(), boxID); err != nil {
		writeResult(w, "", "removing box: "+err.Error())
		return
	}
	writeResult(w, "removed box "+boxID, "")
}

// handleAdminCreateProxy enables an HTTP proxy to a box's port and returns its
// URL as JSON. The signed-in admin's email is recorded as the proxy's creator.
//
// @arg w The response writer the JSON result is written to.
// @arg r The request carrying the box ID, port, and optional description.
//
// @testcase TestAdminCreateProxy enables a proxy, records the description, and returns its URL.
// @testcase TestAdminCreateProxyValidates rejects a missing box ID or bad port.
func (s *Server) handleAdminCreateProxy(w http.ResponseWriter, r *http.Request) {
	ls, ok := s.requireAdminPost(w, r)
	if !ok {
		return
	}
	boxID := strings.TrimSpace(r.PostFormValue("box_id"))
	if boxID == "" {
		writeResult(w, "", "box id is required")
		return
	}
	port, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("port")))
	if err != nil || port <= 0 {
		writeResult(w, "", "a valid port is required")
		return
	}
	description := strings.TrimSpace(r.PostFormValue("description"))
	rec, err := s.createProxy(boxID, port, ls.Email, description)
	if err != nil {
		writeResult(w, "", "creating proxy: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "newProxy": &newProxyResult{
		BoxID:       rec.BoxID,
		Port:        rec.Port,
		URL:         s.proxyURL(rec.Slug),
		Description: rec.Description,
	}})
}

// handleAdminDeleteProxy disables a proxy by its slug.
//
// @arg w The response writer the JSON result is written to.
// @arg r The request carrying the proxy slug.
//
// @testcase TestAdminDeleteProxy disables a proxy by its slug.
func (s *Server) handleAdminDeleteProxy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdminPost(w, r); !ok {
		return
	}
	slug := strings.TrimSpace(r.PostFormValue("slug"))
	if slug == "" {
		writeResult(w, "", "proxy slug is required")
		return
	}
	if err := s.deleteProxyBySlug(slug); err != nil {
		writeResult(w, "", "removing proxy: "+err.Error())
		return
	}
	writeResult(w, "removed proxy", "")
}

// writeResult reports an action's outcome to the admin page as a small
// {ok,msg,err} JSON object. The page is driven entirely over fetch(), so this
// is the only response shape an action produces — there is no non-JS redirect
// fallback to keep in sync.
//
// @arg w The response writer the JSON result is written to.
// @arg msg A success message, or "".
// @arg errMsg An error message, or "".
//
// @testcase TestAdminDropSpokeRemovesAndKicks reads the ok result after an action.
// @testcase TestAdminActionJSON reads an action's JSON result.
func writeResult(w http.ResponseWriter, msg, errMsg string) {
	writeJSON(w, map[string]any{"ok": errMsg == "", "msg": msg, "err": errMsg})
}

// writeJSON writes v as a JSON body with a 200 status. Action outcomes carry an
// "ok" flag, so failures are reported in the body rather than via the status.
//
// @arg w The response writer.
// @arg v The value to encode.
//
// @testcase TestAdminActionJSON decodes a body written here.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, "encoding response", http.StatusInternalServerError)
	}
}

// spokeConnectURL is the WebSocket URL a spoke dials to join this hub, derived
// from the public URL (https→wss, http→ws) with the /spoke/connect path.
//
// @return string The spoke-connect URL for display in the admin UI.
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

// renderAdmin writes the admin page for data with no-store caching.
//
// @arg w The response writer.
// @arg data The admin page context.
//
// @testcase TestAdminDashboardGate renders pages via this helper.
func (s *Server) renderAdmin(w http.ResponseWriter, data adminPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := adminTmpl.Execute(w, data); err != nil {
		s.logger().Warn("failed to render admin page", "err", err)
	}
}

// adminTmplSrc is the admin page template, embedded at build time so the server
// ships as a single self-contained executable.
//
//go:embed templates/admin.html.tmpl
var adminTmplSrc string

// adminTmpl is the parsed admin page template.
var adminTmpl = template.Must(template.New("admin").Parse(adminTmplSrc))

// adminJS is the admin page's client script, served at
// /admin.js. It lives in its own file (not inlined in the template) so its JS
// braces never collide with the html/template {{ }} delimiters.
//
//go:embed static/admin.js
var adminJS string
