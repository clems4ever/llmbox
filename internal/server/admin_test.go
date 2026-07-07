package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/auth"
	"github.com/clems4ever/llmbox/internal/cluster"
	"github.com/clems4ever/llmbox/internal/sandbox"
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
// list) backed by a real SQLite store and a fake box manager.
func newAdminServer(t *testing.T) (*Server, *testutils.FakeMgr, Store) {
	t.Helper()
	st, err := OpenStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := auth.NewTestAuthenticator("admin@corp.com")
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "https://claude.com/x", SubmitURL: "https://claude.ai/code/s/1"}
	return wireSpoke(New(nil, "https://boxes.example.com", time.Minute, st, a), f), f, st
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
	return &http.Cookie{Name: auth.LoginCookie, Value: "SID"}
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

// TestHomeRedirectsToAdmin checks the bare home page redirects to /admin when the
// admin UI is enabled, and stays a 404 when it is not (nowhere to land).
func TestHomeRedirectsToAdmin(t *testing.T) {
	s, _, _ := newAdminServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.APIHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Errorf("admin-enabled home status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/admin" {
		t.Errorf("Location = %q, want /admin", loc)
	}

	// With no admin allow-list, "/" has no landing page and stays a 404.
	noAdmin := newTestServer(&testutils.FakeMgr{})
	rec = httptest.NewRecorder()
	noAdmin.APIHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("admin-disabled home status = %d, want 404", rec.Code)
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
	// The command is the bare per-backend invocation (no docker-run wrapper), and
	// defaults to the docker backend when none is chosen.
	for _, want := range []string{"llmbox-spoke docker --hub", "wss://boxes.example.com/spoke/connect", "--token "} {
		if !strings.Contains(got.NewSpoke.Command, want) {
			t.Errorf("spoke command missing %q: %q", want, got.NewSpoke.Command)
		}
	}
	if strings.Contains(got.NewSpoke.Command, "docker run") {
		t.Errorf("spoke command should not wrap in docker run: %q", got.NewSpoke.Command)
	}
}

// TestAdminCreateSpokeBackend checks the chosen backend selects the displayed
// command, and an unknown backend is rejected.
func TestAdminCreateSpokeBackend(t *testing.T) {
	s, _, st := newAdminServer(t)
	h := s.APIHandler()

	createSpoke := func(name, backend string) *httptest.ResponseRecorder {
		form := url.Values{"name": {name}, "backend": {backend}, "csrf": {"CSRF"}}
		req := httptest.NewRequest(http.MethodPost, "/admin/spokes", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(signIn(t, st, true, false))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	command := func(rec *httptest.ResponseRecorder) string {
		var got struct {
			NewSpoke *struct {
				Command string `json:"command"`
			} `json:"newSpoke"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.NewSpoke == nil {
			t.Fatalf("decode newSpoke: %v (body %q)", err, rec.Body.String())
		}
		return got.NewSpoke.Command
	}

	if cmd := command(createSpoke("fc-1", "firecracker")); !strings.Contains(cmd, "llmbox-spoke firecracker --hub") {
		t.Errorf("firecracker command = %q, want an llmbox-spoke firecracker invocation", cmd)
	}
	if cmd := command(createSpoke("dk-1", "docker")); !strings.Contains(cmd, "llmbox-spoke docker --hub") {
		t.Errorf("docker command = %q, want an llmbox-spoke docker invocation", cmd)
	}

	// An unknown backend is refused with an error result (and mints no token).
	rec := createSpoke("bad-1", "podman")
	var er struct {
		OK  bool   `json:"ok"`
		Err string `json:"err"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &er); err != nil {
		t.Fatalf("decode err: %v (body %q)", err, rec.Body.String())
	}
	if er.OK || er.Err == "" {
		t.Errorf("unknown backend should be rejected, got %+v", er)
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
	if !strings.Contains(ok.NewSpoke.Command, "llmbox-spoke ") {
		t.Errorf("command missing llmbox-spoke invocation: %q", ok.NewSpoke.Command)
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
	f.ListResult = []sandbox.Box{{InstanceID: "abcdef0123456789", BoxID: "foo", Spoke: testSpoke, Phase: "pending"}}
	sess, err := s.createBox(t.Context(), sandbox.CreateOptions{BoxID: "foo"})
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

// TestAdminDropDefaultSpokeClearsDefault checks that dropping the spoke currently
// set as the default also clears the default, so an unqualified box create fails
// loudly rather than silently targeting a spoke that no longer exists.
func TestAdminDropDefaultSpokeClearsDefault(t *testing.T) {
	s, _, st := newAdminServer(t)
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{"edge": &testutils.FakeMgr{}}})
	if err := st.PutSpoke("edge", cluster.SpokeRecord{Name: "edge", EnrolledAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDefaultSpoke("edge"); err != nil {
		t.Fatalf("SetDefaultSpoke: %v", err)
	}
	h := s.APIHandler()

	rec := postAdmin(t, h, st, "/admin/spokes/delete", url.Values{"name": {"edge"}, "csrf": {"CSRF"}})
	if res := decodeAdminResult(t, rec); !res.OK {
		t.Fatalf("drop result = %+v, want ok", res)
	}
	if def, _ := s.DefaultSpoke(); def != "" {
		t.Errorf("default spoke = %q after dropping it, want cleared", def)
	}
}

// postAdmin submits an admin form as a signed-in admin and returns the recorder.
func postAdmin(t *testing.T, h http.Handler, st Store, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(signIn(t, st, true, false))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAdminSetDefaultSpoke checks an enrolled spoke can be made the default via the
// admin action, and it is persisted.
func TestAdminSetDefaultSpoke(t *testing.T) {
	s, _, st := newAdminServer(t)
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{"edge": &testutils.FakeMgr{}}})
	if err := st.PutSpoke("edge", cluster.SpokeRecord{Name: "edge", EnrolledAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	h := s.APIHandler()

	rec := postAdmin(t, h, st, "/admin/default-spoke", url.Values{"name": {"edge"}, "csrf": {"CSRF"}})
	if res := decodeAdminResult(t, rec); !res.OK || res.Err != "" {
		t.Fatalf("result = %+v, want ok", res)
	}
	if def, _ := s.DefaultSpoke(); def != "edge" {
		t.Errorf("default spoke = %q, want edge", def)
	}
}

// TestAdminSetDefaultSpokeUnknown checks the default cannot be set to a spoke that
// is not enrolled.
func TestAdminSetDefaultSpokeUnknown(t *testing.T) {
	s, _, st := newAdminServer(t)
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{}})
	h := s.APIHandler()

	rec := postAdmin(t, h, st, "/admin/default-spoke", url.Values{"name": {"ghost"}, "csrf": {"CSRF"}})
	if res := decodeAdminResult(t, rec); res.OK || res.Err == "" {
		t.Fatalf("result = %+v, want an error for an unenrolled spoke", res)
	}
	if def, _ := s.DefaultSpoke(); def == "ghost" {
		t.Errorf("default spoke was set to the unenrolled spoke %q", def)
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

	form := url.Values{"box_id": {"refactor-auth"}, "spoke": {""}, "csrf": {"CSRF"}}
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
	if _, err := s.createBox(t.Context(), sandbox.CreateOptions{BoxID: "foo"}); err != nil {
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
	out := toAdminBoxes([]sandbox.Box{
		{Name: "c2", BoxID: "beta", Spoke: "edge", Image: "img", State: "running", Phase: "ready", Created: 0},
		{Name: "c1", Spoke: testSpoke, Created: 0},
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

// newProxyAdminServer builds an admin server with proxying enabled and a
// "web-box" session registered on the local spoke.
func newProxyAdminServer(t *testing.T) (*Server, Store) {
	t.Helper()
	s, _, st := newAdminServer(t)
	s.SetProxyBaseDomain("proxy.example.com")
	if _, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "web-box"}); err != nil {
		t.Fatalf("createBox: %v", err)
	}
	return s, st
}

// TestAdminCreateProxy checks the admin create-proxy action enables a proxy,
// returns its URL, and records the admin as its creator.
func TestAdminCreateProxy(t *testing.T) {
	s, st := newProxyAdminServer(t)
	h := s.APIHandler()

	req := httptest.NewRequest(http.MethodPost, "/admin/proxies", strings.NewReader(url.Values{
		"box_id": {"web-box"}, "port": {"8000"}, "description": {"preview server"}, "csrf": {"CSRF"},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.AddCookie(signIn(t, st, true, false))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var out struct {
		OK       bool `json:"ok"`
		NewProxy *struct {
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"newProxy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (body %q)", err, rec.Body.String())
	}
	if !out.OK || out.NewProxy == nil || !strings.HasSuffix(out.NewProxy.URL, ".proxy.example.com/") {
		t.Fatalf("unexpected result %+v", out)
	}
	if out.NewProxy.Description != "preview server" {
		t.Errorf("newProxy description = %q, want %q", out.NewProxy.Description, "preview server")
	}
	proxies, _ := st.ListProxies()
	if len(proxies) != 1 || proxies[0].CreatedBy != "admin@corp.com" {
		t.Errorf("proxies = %+v, want one created by admin@corp.com", proxies)
	}
	if proxies[0].Description != "preview server" {
		t.Errorf("stored description = %q, want %q", proxies[0].Description, "preview server")
	}
}

// TestAdminProxyRendersDescription checks the rendered admin dashboard shows a
// proxy's description in the proxies card.
func TestAdminProxyRendersDescription(t *testing.T) {
	s, _ := newProxyAdminServer(t)
	if _, err := s.createProxy("web-box", 8000, "admin@corp.com", "preview server"); err != nil {
		t.Fatalf("createProxy: %v", err)
	}
	proxies, _ := s.listProxies("")
	rows := toAdminProxies(proxies, s)
	if len(rows) != 1 || rows[0].Description != "preview server" {
		t.Errorf("admin rows = %+v, want one with description %q", rows, "preview server")
	}
}

// TestAdminCreateProxyValidates checks the admin create-proxy action rejects a
// missing box ID or a non-numeric port.
func TestAdminCreateProxyValidates(t *testing.T) {
	s, st := newProxyAdminServer(t)
	h := s.APIHandler()

	post := func(form url.Values) adminResult {
		req := httptest.NewRequest(http.MethodPost, "/admin/proxies", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(signIn(t, st, true, false))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return decodeAdminResult(t, rec)
	}
	if r := post(url.Values{"port": {"8000"}, "csrf": {"CSRF"}}); r.OK || r.Err == "" {
		t.Error("expected an error for a missing box id")
	}
	if r := post(url.Values{"box_id": {"web-box"}, "port": {"nope"}, "csrf": {"CSRF"}}); r.OK || r.Err == "" {
		t.Error("expected an error for a non-numeric port")
	}
}

// TestAdminDeleteProxy checks the admin delete-proxy action removes a proxy by
// its slug.
func TestAdminDeleteProxy(t *testing.T) {
	s, st := newProxyAdminServer(t)
	h := s.APIHandler()
	rec, err := s.createProxy("web-box", 8000, "admin@corp.com", "")
	if err != nil {
		t.Fatalf("createProxy: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/proxies/delete", strings.NewReader(url.Values{
		"slug": {rec.Slug}, "csrf": {"CSRF"},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(signIn(t, st, true, false))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if res := decodeAdminResult(t, rr); !res.OK {
		t.Fatalf("delete result not ok: %+v", res)
	}
	if proxies, _ := st.ListProxies(); len(proxies) != 0 {
		t.Errorf("proxy survived delete: %+v", proxies)
	}
}

// TestAdminDashboardShowsProxies checks the dashboard renders the proxies card
// for an admin when proxying is enabled.
func TestAdminDashboardShowsProxies(t *testing.T) {
	s, st := newProxyAdminServer(t)
	if _, err := s.createProxy("web-box", 8000, "admin@corp.com", ""); err != nil {
		t.Fatal(err)
	}
	h := s.APIHandler()
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(signIn(t, st, true, false))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "HTTP proxies") || !strings.Contains(body, "proxies-card") {
		t.Error("dashboard missing proxies card")
	}
	if !strings.Contains(body, ".proxy.example.com/") {
		t.Error("dashboard missing proxy URL")
	}
}
