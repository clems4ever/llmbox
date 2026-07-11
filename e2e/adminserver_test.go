//go:build e2e

package e2e

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/hub"
	"github.com/clems4ever/llmbox/internal/hub/auth"
	"github.com/clems4ever/llmbox/internal/hub/store"
	"github.com/clems4ever/llmbox/internal/shared/cluster"
	"github.com/clems4ever/llmbox/testutils"
)

// e2eDefaultSpoke is the spoke the plain (non-cluster) e2e servers register their
// fake box manager under and set as the default, so a box created with no explicit
// spoke routes to it.
const e2eDefaultSpoke = "spoke-e2e"

// wireDefaultSpoke registers mgr as the single connected spoke, enrolls it in the
// store, and makes it the default, so the hub-less-backend server routes an
// unqualified box create to it. Use it for the plain e2e servers that stand in a
// single fake box manager for the whole cluster.
//
// @arg t The test the wiring is scoped to.
// @arg srv The server to attach the hub and default to.
// @arg st The server's store (for enrolling the spoke).
// @arg mgr The box manager to serve as the default spoke.
func wireDefaultSpoke(t *testing.T, srv *hub.Server, st hub.Store, mgr cluster.BoxManager) {
	t.Helper()
	srv.SetHub(&testutils.FakeHub{Connected: map[string]cluster.BoxManager{e2eDefaultSpoke: mgr}})
	if err := st.PutSpoke(e2eDefaultSpoke, cluster.SpokeRecord{Name: e2eDefaultSpoke, EnrolledAt: time.Now()}); err != nil {
		t.Fatalf("PutSpoke: %v", err)
	}
	if err := srv.SetDefaultSpoke(e2eDefaultSpoke); err != nil {
		t.Fatalf("SetDefaultSpoke: %v", err)
	}
}

// newAdminServer builds an admin-enabled Server (admin@corp.com on the allow
// list) backed by a real SQLite store and a fake box manager, using the exported
// server test seams so the e2e package can wire it from outside package server.
//
// @arg t The test, failed if the store cannot be opened.
// @return *hub.Server The admin-enabled server.
// @return *testutils.FakeMgr The fake box manager backing it, for seeding/assertions.
// @return hub.Store The backing store, for seeding login sessions.
func newAdminServer(t *testing.T) (*hub.Server, *testutils.FakeMgr, hub.Store) {
	t.Helper()
	st, err := hub.OpenStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := auth.NewTestAuthenticator("admin@corp.com")
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789"}
	srv := hub.New(nil, "https://boxes.example.com", st, a)
	wireDefaultSpoke(t, srv, st, f)
	return srv, f, st
}

// signIn stores a login session and returns its cookie. admin controls whether
// the session may use the admin UI and reach the per-box HTTP proxies.
//
// @arg t The test, failed if the session cannot be saved.
// @arg st The store to persist the login session in.
// @arg admin Whether the session has admin capability.
// @return *http.Cookie The login cookie naming the persisted session.
func signIn(t *testing.T, st hub.Store, admin bool) *http.Cookie {
	t.Helper()
	if err := st.PutIdentitySession(store.HashToken("SID"), store.IdentitySession{
		Email: "admin@corp.com", CSRFToken: "CSRF", ExpiresAt: time.Now().Add(time.Hour),
		CanAdmin: admin,
	}); err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: auth.LoginCookie, Value: "SID"}
}
