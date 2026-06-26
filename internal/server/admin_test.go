package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/cluster"
	"github.com/clems4ever/llmbox/internal/docker"
	"github.com/clems4ever/llmbox/testutils"
)

// adminResult is the {ok,msg,err} JSON every admin action returns.
type adminResult struct {
	OK  bool   `json:"ok"`
	Msg string `json:"msg"`
	Err string `json:"err"`
}

// decodeAdminResult asserts a 200 JSON response and returns the decoded result.
//
// @arg t The test, failed on a non-200 status or a body that won't decode.
// @arg rec The recorded response.
// @return adminResult The decoded {ok,msg,err} result.
func decodeAdminResult(t *testing.T, rec *httptest.ResponseRecorder) adminResult {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q), want 200", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	var r adminResult
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decoding result: %v (body %q)", err, rec.Body.String())
	}
	return r
}

// newAdminServer builds an admin-enabled Server (admin@corp.com on the allow
// list) backed by a real bbolt store and a fake box manager.
func newAdminServer(t *testing.T) (*Server, *testutils.FakeMgr, Store) {
	t.Helper()
	st, err := OpenStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	auth := NewTestAuthenticator("admin@corp.com")
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "https://claude.com/x", SubmitURL: "https://claude.ai/code/s/1"}
	return New(f, nil, "https://boxes.example.com", time.Minute, st, auth), f, st
}

// signIn stores a login session and returns its cookie. admin/activate control
// the session's capabilities.
func signIn(t *testing.T, st Store, admin, activate bool) *http.Cookie {
	t.Helper()
	if err := st.SaveLoginSession("SID", LoginSession{
		Email: "admin@corp.com", CSRF: "CSRF", ExpiresAt: time.Now().Add(time.Hour),
		Admin: admin, Activate: activate,
	}); err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: LoginCookie, Value: "SID"}
}

// TestAdminAllowlist checks AdminEnabled/isAdmin honor the allow-list (case-insensitively) and a nil authenticator.
func TestAdminAllowlist(t *testing.T) {
	a := &Authenticator{adminEmails: map[string]bool{"admin@corp.com": true}}
	if !a.AdminEnabled() {
		t.Error("AdminEnabled = false, want true")
	}
	if !a.isAdmin("Admin@Corp.com") {
		t.Error("isAdmin should be case-insensitive")
	}
	if a.isAdmin("nobody@corp.com") {
		t.Error("isAdmin allowed an unlisted email")
	}
	var nilA *Authenticator
	if nilA.AdminEnabled() || nilA.isAdmin("admin@corp.com") {
		t.Error("nil Authenticator should not enable admin")
	}
	if (&Authenticator{}).AdminEnabled() {
		t.Error("empty allow-list should disable admin")
	}
}

// TestAdminButtonsReturnPath checks adminButtons builds a login link carrying the URL-encoded return path.
func TestAdminButtonsReturnPath(t *testing.T) {
	a := &Authenticator{providers: map[string]*provider{"google": {label: "Google"}}, order: []string{"google"}}
	btns := a.adminButtons("/admin")
	if len(btns) != 1 || btns[0].LoginPath != "/auth/google/login?return=%2Fadmin" {
		t.Errorf("adminButtons = %+v", btns)
	}
}

// TestSafeReturnPath checks local paths are accepted and absolute/protocol-relative/backslash ones are rejected.
func TestSafeReturnPath(t *testing.T) {
	cases := map[string]string{
		"/admin":            "/admin",
		"/admin?x=1":        "/admin?x=1",
		"":                  "",
		"//evil.com":        "",
		"https://evil.com":  "",
		"http://evil.com/x": "",
		"relative":          "",
		"/\\evil":           "",
	}
	for in, want := range cases {
		if got := safeReturnPath(in); got != want {
			t.Errorf("safeReturnPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestProviderLoginReturnPath checks the login flow persists a safe return path (admin flow) instead of a box token.
func TestProviderLoginReturnPath(t *testing.T) {
	s, _, st := newAuthServer(t, googleTestProvider(t, idClaims{}, nil))
	h := s.APIHandler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/google/login?return=%2Fadmin", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	flow, ok, err := st.TakeLoginFlow(loc.Query().Get("state"))
	if err != nil || !ok {
		t.Fatalf("flow not persisted: ok=%v err=%v", ok, err)
	}
	if flow.ReturnTo != "/admin" || flow.ReturnToken != "" {
		t.Errorf("flow = %+v, want ReturnTo=/admin", flow)
	}
}

// TestProviderCallbackAdminOnly checks an admin who cannot activate boxes still signs in with Admin=true, Activate=false.
func TestProviderCallbackAdminOnly(t *testing.T) {
	// Admin whose email is in no activation allow rule (domain admin.io is not
	// allowed) still signs in for admin, with Activate=false.
	p := googleTestProvider(t, idClaims{Email: "boss@admin.io", EmailVerified: true}, nil)
	s, _, st := newAuthServer(t, p)
	s.auth.adminEmails = map[string]bool{"boss@admin.io": true}
	h := s.APIHandler()

	if err := st.SaveLoginFlow("STATE", loginFlow{Provider: "google", ReturnTo: "/admin", Nonce: "N", Verifier: "V", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/google/callback?state=STATE&code=CODE", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/admin" {
		t.Errorf("Location = %q, want /admin", loc)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == LoginCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no login cookie set")
	}
	ls, ok, _ := st.LoginSession(cookie.Value)
	if !ok || !ls.Admin || ls.Activate {
		t.Errorf("session = %+v, want Admin=true Activate=false", ls)
	}
}

// TestAdminDashboardGate checks /admin shows sign-in to anonymous, a 403 notice to non-admins, and the dashboard to admins.
func TestAdminDashboardGate(t *testing.T) {
	s, _, st := newAdminServer(t)
	h := s.APIHandler()

	get := func(c *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/admin", nil)
		if c != nil {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := get(nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Sign in with Google") {
		t.Errorf("anonymous: status=%d body lacks sign-in", rec.Code)
	}
	if rec := get(signIn(t, st, false, true)); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "isn't an administrator") {
		t.Errorf("non-admin: status=%d, want 403 with notice", rec.Code)
	}
	if rec := get(signIn(t, st, true, false)); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Cluster admin") {
		t.Errorf("admin: status=%d, want dashboard", rec.Code)
	}
}

// TestAdminActionsRequireAdminAndCSRF checks a mutating admin action rejects no-cookie, non-admin, and bad-CSRF requests.
func TestAdminActionsRequireAdminAndCSRF(t *testing.T) {
	s, _, st := newAdminServer(t)
	h := s.APIHandler()

	post := func(c *http.Cookie, form url.Values) int {
		req := httptest.NewRequest(http.MethodPost, "/admin/spokes", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if c != nil {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := post(nil, url.Values{"name": {"e"}}); code != http.StatusUnauthorized {
		t.Errorf("no cookie: %d, want 401", code)
	}
	if code := post(signIn(t, st, false, true), url.Values{"name": {"e"}, "csrf": {"CSRF"}}); code != http.StatusForbidden {
		t.Errorf("non-admin: %d, want 403", code)
	}
	if code := post(signIn(t, st, true, false), url.Values{"name": {"e"}, "csrf": {"WRONG"}}); code != http.StatusForbidden {
		t.Errorf("bad CSRF: %d, want 403", code)
	}
}

// TestAdminCreateSpokeMintsToken checks creating a spoke mints a join token in the server's own store and shows the command.
func TestAdminCreateSpokeMintsToken(t *testing.T) {
	s, _, st := newAdminServer(t)
	h := s.APIHandler()

	form := url.Values{"name": {"edge-1"}, "ttl": {"2h"}, "csrf": {"CSRF"}}
	req := httptest.NewRequest(http.MethodPost, "/admin/spokes", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(signIn(t, st, true, false))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	tokens, err := st.ListJoinTokens()
	if err != nil || len(tokens) != 1 || tokens[0].Name != "edge-1" {
		t.Fatalf("tokens = %+v err=%v, want one for edge-1", tokens, err)
	}
	var got struct {
		OK       bool `json:"ok"`
		NewSpoke *struct {
			Command string `json:"command"`
		} `json:"newSpoke"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding result: %v (body %q)", err, rec.Body.String())
	}
	if !got.OK || got.NewSpoke == nil {
		t.Fatalf("result = %+v, want ok with a newSpoke", got)
	}
	for _, want := range []string{"docker run", "--group-add", "/var/run/docker.sock", "wss://boxes.example.com/spoke/connect"} {
		if !strings.Contains(got.NewSpoke.Command, want) {
			t.Errorf("spoke command missing %q: %q", want, got.NewSpoke.Command)
		}
	}
}

// TestAdminActionJSON checks that admin actions answer an AJAX caller (Accept:
// application/json) with a JSON result instead of an HTML render or redirect, so
// the page updates in place and a dashboard refresh never resubmits a create.
func TestAdminActionJSON(t *testing.T) {
	s, _, st := newAdminServer(t)
	h := s.APIHandler()

	jsonPost := func(path string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.AddCookie(signIn(t, st, true, false))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// Create-spoke returns the token/command as JSON (not an HTML result page).
	rec := jsonPost("/admin/spokes", url.Values{"name": {"edge-1"}, "csrf": {"CSRF"}})
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q, want json (body %q)", ct, rec.Body.String())
	}
	var ok struct {
		OK       bool `json:"ok"`
		NewSpoke *struct {
			Token   string `json:"token"`
			Command string `json:"command"`
		} `json:"newSpoke"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil {
		t.Fatalf("decode: %v (body %q)", err, rec.Body.String())
	}
	if !ok.OK || ok.NewSpoke == nil || ok.NewSpoke.Token == "" {
		t.Fatalf("unexpected result %+v", ok)
	}
	if !strings.Contains(ok.NewSpoke.Command, "docker run") {
		t.Errorf("command missing docker run: %q", ok.NewSpoke.Command)
	}

	// A validation failure comes back as ok:false with an error message.
	rec = jsonPost("/admin/boxes", url.Values{"box_id": {""}, "csrf": {"CSRF"}})
	var er struct {
		OK  bool   `json:"ok"`
		Err string `json:"err"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &er); err != nil {
		t.Fatalf("decode err: %v (body %q)", err, rec.Body.String())
	}
	if er.OK || er.Err == "" {
		t.Fatalf("want ok:false with an error, got %+v", er)
	}
}

// TestAdminJSServed checks the admin client script is served as JavaScript at
// /admin.js.
func TestAdminJSServed(t *testing.T) {
	s, _, _ := newAdminServer(t)
	h := s.APIHandler()

	req := httptest.NewRequest(http.MethodGet, "/admin.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("content-type = %q, want javascript", ct)
	}
	if !strings.Contains(rec.Body.String(), "addEventListener") {
		t.Error("admin.js body missing expected script content")
	}
}

// TestAdminDashboardShowsActivationURL checks a pending box's activation URL is
// shown in the dashboard table, so it survives a page refresh.
func TestAdminDashboardShowsActivationURL(t *testing.T) {
	s, f, st := newAdminServer(t)
	f.ListResult = []docker.Box{{ContainerID: "abcdef0123456789", BoxID: "foo", Spoke: "local", Phase: "pending"}}
	sess, err := s.createBox(t.Context(), docker.CreateOptions{BoxID: "foo"})
	if err != nil {
		t.Fatal(err)
	}
	h := s.APIHandler()

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(signIn(t, st, true, false))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if want := s.AuthPageURL(sess.Token); !strings.Contains(rec.Body.String(), want) {
		t.Errorf("dashboard missing activation URL %q", want)
	}
}

// TestAdminDropSpokeRemovesAndKicks checks dropping a spoke deletes its record and tokens and disconnects its live link.
func TestAdminDropSpokeRemovesAndKicks(t *testing.T) {
	s, _, st := newAdminServer(t)
	hub := &testutils.FakeHub{Connected: map[string]boxManager{"edge": &testutils.FakeMgr{}}}
	s.SetHub(hub)
	if err := st.PutSpoke("edge", cluster.SpokeRecord{Name: "edge", EnrolledAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := cluster.CreateJoinToken(st, "edge", time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	h := s.APIHandler()

	form := url.Values{"name": {"edge"}, "csrf": {"CSRF"}}
	req := httptest.NewRequest(http.MethodPost, "/admin/spokes/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(signIn(t, st, true, false))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if res := decodeAdminResult(t, rec); !res.OK || res.Err != "" {
		t.Fatalf("result = %+v, want ok with no error", res)
	}
	if _, found, _ := st.GetSpoke("edge"); found {
		t.Error("spoke record not deleted")
	}
	if toks, _ := st.ListJoinTokens(); len(toks) != 0 {
		t.Errorf("join tokens not revoked: %+v", toks)
	}
	if len(hub.Disconnected) != 1 || hub.Disconnected[0] != "edge" {
		t.Errorf("Disconnected = %v, want [edge]", hub.Disconnected)
	}
}

// TestAdminRevokeToken checks revoking a join token by ID removes it from the store.
func TestAdminRevokeToken(t *testing.T) {
	s, _, st := newAdminServer(t)
	if _, err := cluster.CreateJoinToken(st, "edge", time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	toks, _ := st.ListJoinTokens()
	if len(toks) != 1 {
		t.Fatalf("setup: %d tokens", len(toks))
	}
	h := s.APIHandler()

	form := url.Values{"id": {toks[0].ID}, "csrf": {"CSRF"}}
	req := httptest.NewRequest(http.MethodPost, "/admin/tokens/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(signIn(t, st, true, false))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if res := decodeAdminResult(t, rec); !res.OK || res.Err != "" {
		t.Fatalf("result = %+v, want ok with no error", res)
	}
	if toks, _ := st.ListJoinTokens(); len(toks) != 0 {
		t.Errorf("token not revoked: %+v", toks)
	}
}

// TestAdminCreateBox checks creating a box routes to the requested spoke and shows the activation URL.
func TestAdminCreateBox(t *testing.T) {
	s, f, st := newAdminServer(t)
	h := s.APIHandler()

	form := url.Values{"box_id": {"refactor-auth"}, "spoke": {"local"}, "csrf": {"CSRF"}}
	req := httptest.NewRequest(http.MethodPost, "/admin/boxes", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(signIn(t, st, true, false))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var got struct {
		OK     bool `json:"ok"`
		NewBox *struct {
			AuthURL string `json:"authUrl"`
		} `json:"newBox"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding result: %v (body %q)", err, rec.Body.String())
	}
	if !got.OK || got.NewBox == nil {
		t.Fatalf("result = %+v, want ok with a newBox", got)
	}
	if f.GotOpts.BoxID != "refactor-auth" {
		t.Errorf("created box id = %q", f.GotOpts.BoxID)
	}
	if !strings.Contains(got.NewBox.AuthURL, "https://boxes.example.com/auth/") {
		t.Errorf("newBox.authUrl = %q, want the activation URL", got.NewBox.AuthURL)
	}
}

// TestAdminDeleteBox checks removing a box answers with a JSON ok result and
// destroys the box by ID — the path the admin page's urlencoded fetch takes.
func TestAdminDeleteBox(t *testing.T) {
	s, f, st := newAdminServer(t)
	if _, err := s.createBox(t.Context(), docker.CreateOptions{BoxID: "foo"}); err != nil {
		t.Fatal(err)
	}
	h := s.APIHandler()

	form := url.Values{"box_id": {"foo"}, "csrf": {"CSRF"}}
	req := httptest.NewRequest(http.MethodPost, "/admin/boxes/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(signIn(t, st, true, false))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if res := decodeAdminResult(t, rec); !res.OK || res.Err != "" {
		t.Fatalf("result = %+v, want ok with no error", res)
	}
	if len(f.Destroyed) != 1 || f.Destroyed[0] != "foo" {
		t.Errorf("Destroyed = %v, want [foo]", f.Destroyed)
	}
}

// TestToAdminTokens checks join tokens are shortened, sorted by name, and flagged when expired.
func TestToAdminTokens(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := []cluster.JoinTokenInfo{
		{ID: strings.Repeat("b", 64), Name: "zeta", ExpiresAt: now.Add(time.Hour)},
		{ID: strings.Repeat("a", 64), Name: "alpha", ExpiresAt: now.Add(-time.Hour)},
	}
	out := toAdminTokens(in, now)
	if out[0].Name != "alpha" || out[1].Name != "zeta" {
		t.Errorf("not sorted by name: %+v", out)
	}
	if len(out[0].Short) != adminTokenIDLen || !out[0].Expired {
		t.Errorf("alpha = %+v, want short id and expired", out[0])
	}
	if out[1].Expired {
		t.Error("zeta should not be expired")
	}
}

// TestToAdminBoxes checks boxes are sorted by ID and fall back to the container name when no box ID is set.
func TestToAdminBoxes(t *testing.T) {
	out := toAdminBoxes([]docker.Box{
		{Name: "c2", BoxID: "beta", Spoke: "edge", Image: "img", State: "running", Phase: "ready", Created: 0},
		{Name: "c1", Spoke: "local", Created: 0},
	})
	if out[0].BoxID != "beta" || out[1].BoxID != "c1" {
		t.Errorf("not sorted/derived: %+v", out)
	}
}

// TestSpokeConnectURL checks the spoke-connect URL derives ws/wss from the public URL scheme.
func TestSpokeConnectURL(t *testing.T) {
	cases := map[string]string{
		"https://h.example.com":  "wss://h.example.com/spoke/connect",
		"http://localhost:8080":  "ws://localhost:8080/spoke/connect",
		"https://h.example.com/": "wss://h.example.com/spoke/connect",
	}
	for pub, want := range cases {
		s := &Server{publicURL: pub}
		if got := s.spokeConnectURL(); got != want {
			t.Errorf("spokeConnectURL(%q) = %q, want %q", pub, got, want)
		}
	}
}
