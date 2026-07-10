package hub

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/hub/auth"
	"github.com/clems4ever/llmbox/internal/shared/cluster"
	"github.com/clems4ever/llmbox/testutils"
)

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
	if err := st.PutIdentitySession(hashTok("SID"), IdentitySession{
		Email: "admin@corp.com", CSRFToken: "CSRF", ExpiresAt: time.Now().Add(time.Hour),
		CanAdmin: admin, CanActivate: activate,
	}); err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: auth.LoginCookie, Value: "SID"}
}

// TestAdminSPAServed checks the admin single-page app is served: the shell at
// /admin as HTML referencing a hashed script, and that script under
// /admin/assets/ with an immutable cache header.
func TestAdminSPAServed(t *testing.T) {
	s, _, _ := newAdminServer(t)
	h := s.APIHandler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type = %q, want html", ct)
	}
	body := rec.Body.String()
	// The shell references its content-hashed bundle under /admin/assets/.
	start := strings.Index(body, "/admin/assets/")
	if start < 0 {
		t.Fatalf("shell does not reference /admin/assets/: %q", body)
	}
	end := start
	for end < len(body) && body[end] != '"' {
		end++
	}
	assetPath := body[start:end]

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, assetPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", assetPath, rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("asset Cache-Control = %q, want immutable", cc)
	}
}

// TestAssetsServedWithoutAdmin checks the web assets are mounted even when the
// admin UI is disabled: the public activation page is served from the same
// built app, so its hashed bundle must resolve for a server with no admin
// allow-list.
func TestAssetsServedWithoutAdmin(t *testing.T) {
	s := newTestServer(&testutils.FakeMgr{}) // nil auth: admin UI disabled
	h := s.APIHandler()

	// Discover a hashed asset path from the activation shell.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/sometoken", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/{token} = %d, want 200 (is the web app built? run `make web`)", rec.Code)
	}
	body := rec.Body.String()
	start := strings.Index(body, "/admin/assets/")
	if start < 0 {
		t.Fatalf("activation shell does not reference /admin/assets/: %q", body)
	}
	end := start
	for end < len(body) && body[end] != '"' {
		end++
	}
	assetPath := body[start:end]

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, assetPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 without the admin UI", assetPath, rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("asset Cache-Control = %q, want immutable", cc)
	}

	// The admin shell itself stays absent without an admin allow-list.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /admin = %d, want 404 with the admin UI disabled", rec.Code)
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

// TestDropSpokeRemovesAndKicks checks dropping a spoke deletes its enrollment,
// revokes its outstanding join tokens, and force-closes its live connection.
func TestDropSpokeRemovesAndKicks(t *testing.T) {
	s, _, st := newAdminServer(t)
	hub := &testutils.FakeHub{Connected: map[string]boxManager{"edge": &testutils.FakeMgr{}}}
	s.SetHub(hub)
	if err := st.PutSpoke("edge", cluster.SpokeRecord{Name: "edge", EnrolledAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := cluster.CreateJoinToken(st, "edge", time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := s.dropSpoke("edge"); err != nil {
		t.Fatalf("dropSpoke: %v", err)
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

// TestDropDefaultSpokeClearsDefault checks that dropping the spoke currently
// set as the default also clears the default, so an unqualified box create fails
// loudly rather than silently targeting a spoke that no longer exists.
func TestDropDefaultSpokeClearsDefault(t *testing.T) {
	s, _, st := newAdminServer(t)
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{"edge": &testutils.FakeMgr{}}})
	if err := st.PutSpoke("edge", cluster.SpokeRecord{Name: "edge", EnrolledAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDefaultSpoke("edge"); err != nil {
		t.Fatalf("SetDefaultSpoke: %v", err)
	}

	if err := s.dropSpoke("edge"); err != nil {
		t.Fatalf("dropSpoke: %v", err)
	}
	if def, _ := s.DefaultSpoke(); def != "" {
		t.Errorf("default spoke = %q after dropping it, want cleared", def)
	}
}

// TestSpokeConnectURL checks the ws(s) spoke-connect URL is derived from the
// public URL's scheme.
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
