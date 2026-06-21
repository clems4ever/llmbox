package server

import (
	"crypto/subtle"
	_ "embed"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/clems4ever/llmbox/internal/cluster"
	"github.com/clems4ever/llmbox/internal/docker"
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
	mux.HandleFunc("POST /admin/spokes", s.handleAdminCreateSpoke)
	mux.HandleFunc("POST /admin/spokes/delete", s.handleAdminDropSpoke)
	mux.HandleFunc("POST /admin/tokens/delete", s.handleAdminRevokeToken)
	mux.HandleFunc("POST /admin/boxes", s.handleAdminCreateBox)
	mux.HandleFunc("POST /admin/boxes/delete", s.handleAdminDeleteBox)
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
}

// newSpokeResult is the one-time output shown after minting a join token.
type newSpokeResult struct {
	Name    string
	Token   string
	Command string
}

// newBoxResult is the one-time output shown after creating a box.
type newBoxResult struct {
	BoxID   string
	Spoke   string
	AuthURL string
}

// adminPageData is the admin page template context.
type adminPageData struct {
	// Sign-in state. SignIn holds the provider buttons when the visitor is not
	// signed in; NotAdmin is set when signed in without admin rights.
	SignIn   []providerButton
	SignedIn bool
	NotAdmin bool
	Email    string
	CSRF     string

	// Dashboard data (only populated for an admin).
	Spokes          []SpokeStatus
	Tokens          []adminToken
	Boxes           []adminBox
	ConnectedSpokes []string

	// Flash banner from a redirect (e.g. after a delete) and one-time results.
	Flash    string
	FlashOK  bool
	NewSpoke *newSpokeResult
	NewBox   *newBoxResult
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
	ls, ok := s.currentLogin(r)
	if !ok {
		s.renderAdmin(w, adminPageData{SignIn: s.auth.adminButtons("/admin")})
		return
	}
	if !ls.Admin {
		w.WriteHeader(http.StatusForbidden)
		s.renderAdmin(w, adminPageData{SignedIn: true, NotAdmin: true, Email: ls.Email})
		return
	}
	data := s.adminDashboard(r, ls)
	if msg := r.URL.Query().Get("msg"); msg != "" {
		data.Flash, data.FlashOK = msg, true
	} else if e := r.URL.Query().Get("err"); e != "" {
		data.Flash, data.FlashOK = e, false
	}
	s.renderAdmin(w, data)
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
func (s *Server) adminDashboard(r *http.Request, ls loginSession) adminPageData {
	data := adminPageData{SignedIn: true, Email: ls.Email, CSRF: ls.CSRF}

	if spokes, err := s.SpokeStatuses(r.Context()); err != nil {
		s.logger().Warn("admin: listing spokes", "err", err)
	} else {
		data.Spokes = spokes
		for _, sp := range spokes {
			// The template offers "local" explicitly, so only list connected
			// remote spokes here to avoid a duplicate option.
			if sp.Connected && !sp.Local {
				data.ConnectedSpokes = append(data.ConnectedSpokes, sp.Name)
			}
		}
	}

	if tokens, err := s.store.ListJoinTokens(); err != nil {
		s.logger().Warn("admin: listing join tokens", "err", err)
	} else {
		data.Tokens = toAdminTokens(tokens, time.Now())
	}

	if boxes, err := s.ListBoxes(r.Context()); err != nil {
		s.logger().Warn("admin: listing boxes", "err", err)
	} else {
		data.Boxes = toAdminBoxes(boxes)
	}
	return data
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
func toAdminBoxes(boxes []docker.Box) []adminBox {
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
// @return loginSession The admin's session when authorized.
// @return bool True when the request may proceed.
//
// @testcase TestAdminActionsRequireAdminAndCSRF rejects non-admins and bad CSRF tokens.
func (s *Server) requireAdminPost(w http.ResponseWriter, r *http.Request) (loginSession, bool) {
	ls, ok := s.currentLogin(r)
	if !ok {
		http.Error(w, "Please sign in.", http.StatusUnauthorized)
		return loginSession{}, false
	}
	if !ls.Admin {
		http.Error(w, "Not authorized.", http.StatusForbidden)
		return loginSession{}, false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form.", http.StatusBadRequest)
		return loginSession{}, false
	}
	if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(ls.CSRF)) != 1 {
		http.Error(w, "Invalid or missing form token; reload the page and try again.", http.StatusForbidden)
		return loginSession{}, false
	}
	return ls, true
}

// handleAdminCreateSpoke mints a one-time join token for a named spoke and
// renders the token and ready-to-run spoke command once. Because the token is
// minted through the running hub, it lands in the very store the hub reads.
//
// @arg w The response writer the result page is rendered to.
// @arg r The request carrying the spoke name and optional TTL.
//
// @testcase TestAdminCreateSpokeMintsToken mints a token in the server's own store.
func (s *Server) handleAdminCreateSpoke(w http.ResponseWriter, r *http.Request) {
	ls, ok := s.requireAdminPost(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		s.redirectAdmin(w, r, "", "spoke name is required")
		return
	}
	ttl := defaultAdminTokenTTL
	if v := strings.TrimSpace(r.PostFormValue("ttl")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			s.redirectAdmin(w, r, "", "invalid TTL (use e.g. 1h, 30m)")
			return
		}
		ttl = d
	}
	token, err := cluster.CreateJoinToken(s.store, name, ttl, time.Now())
	if err != nil {
		s.redirectAdmin(w, r, "", "creating token: "+err.Error())
		return
	}
	data := s.adminDashboard(r, ls)
	data.NewSpoke = &newSpokeResult{
		Name:    name,
		Token:   token,
		Command: "llmbox spoke --hub " + s.spokeConnectURL() + " --token " + token,
	}
	s.renderAdmin(w, data)
}

// handleAdminDropSpoke removes a spoke's enrollment and any of its outstanding
// join tokens, then force-closes its live connection so it is dropped at once
// and cannot reconnect.
//
// @arg w The response writer (redirected back to the dashboard).
// @arg r The request carrying the spoke name.
//
// @testcase TestAdminDropSpokeRemovesAndKicks deletes the record and disconnects the live link.
func (s *Server) handleAdminDropSpoke(w http.ResponseWriter, r *http.Request) {
	_, ok := s.requireAdminPost(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		s.redirectAdmin(w, r, "", "spoke name is required")
		return
	}
	if err := s.store.DeleteSpoke(name); err != nil {
		s.redirectAdmin(w, r, "", "dropping spoke: "+err.Error())
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
	s.redirectAdmin(w, r, "dropped spoke "+name, "")
}

// handleAdminRevokeToken deletes a single outstanding join token by its full ID.
//
// @arg w The response writer (redirected back to the dashboard).
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
		s.redirectAdmin(w, r, "", "token id is required")
		return
	}
	if err := s.store.DeleteJoinToken(id); err != nil {
		s.redirectAdmin(w, r, "", "revoking token: "+err.Error())
		return
	}
	s.redirectAdmin(w, r, "revoked join token", "")
}

// handleAdminCreateBox creates a box on the chosen spoke and renders its auth URL
// once (the box still needs a user to complete activation).
//
// @arg w The response writer the result page is rendered to.
// @arg r The request carrying the box ID, optional image/description, and spoke.
//
// @testcase TestAdminCreateBox creates a box on the requested spoke.
func (s *Server) handleAdminCreateBox(w http.ResponseWriter, r *http.Request) {
	ls, ok := s.requireAdminPost(w, r)
	if !ok {
		return
	}
	boxID := strings.TrimSpace(r.PostFormValue("box_id"))
	if boxID == "" {
		s.redirectAdmin(w, r, "", "box id is required")
		return
	}
	sess, err := s.CreateBox(r.Context(), docker.CreateOptions{
		BoxID:       boxID,
		Image:       strings.TrimSpace(r.PostFormValue("image")),
		Description: strings.TrimSpace(r.PostFormValue("description")),
		SpokeName:   strings.TrimSpace(r.PostFormValue("spoke")),
	})
	if err != nil {
		s.redirectAdmin(w, r, "", "creating box: "+err.Error())
		return
	}
	data := s.adminDashboard(r, ls)
	data.NewBox = &newBoxResult{
		BoxID:   sess.BoxID,
		Spoke:   sess.SpokeName,
		AuthURL: s.AuthPageURL(sess.Token),
	}
	s.renderAdmin(w, data)
}

// handleAdminDeleteBox removes a box by its box ID.
//
// @arg w The response writer (redirected back to the dashboard).
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
		s.redirectAdmin(w, r, "", "box id is required")
		return
	}
	if err := s.DestroyBox(r.Context(), boxID); err != nil {
		s.redirectAdmin(w, r, "", "removing box: "+err.Error())
		return
	}
	s.redirectAdmin(w, r, "removed box "+boxID, "")
}

// redirectAdmin redirects back to the dashboard carrying a one-line flash message
// (msg for success, err for failure) as a query parameter (post/redirect/get).
//
// @arg w The response writer.
// @arg r The request being handled.
// @arg msg A success message, or "".
// @arg errMsg An error message, or "".
//
// @testcase TestAdminDropSpokeRemovesAndKicks follows the redirect after an action.
func (s *Server) redirectAdmin(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	u := "/admin"
	if msg != "" {
		u += "?msg=" + url.QueryEscape(msg)
	} else if errMsg != "" {
		u += "?err=" + url.QueryEscape(errMsg)
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
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
//go:embed admin.html.tmpl
var adminTmplSrc string

// adminTmpl is the parsed admin page template.
var adminTmpl = template.Must(template.New("admin").Parse(adminTmplSrc))
