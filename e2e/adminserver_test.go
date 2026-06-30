//go:build e2e

package e2e

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/auth"
	"github.com/clems4ever/llmbox/internal/server"
	"github.com/clems4ever/llmbox/internal/store"
	"github.com/clems4ever/llmbox/testutils"
)

// newAdminServer builds an admin-enabled Server (admin@corp.com on the allow
// list) backed by a real SQLite store and a fake box manager, using the exported
// server test seams so the e2e package can wire it from outside package server.
//
// @arg t The test, failed if the store cannot be opened.
// @return *server.Server The admin-enabled server.
// @return *testutils.FakeMgr The fake box manager backing it, for seeding/assertions.
// @return server.Store The backing store, for seeding login sessions.
func newAdminServer(t *testing.T) (*server.Server, *testutils.FakeMgr, server.Store) {
	t.Helper()
	st, err := server.OpenStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := auth.NewTestAuthenticator("admin@corp.com")
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "https://claude.com/x", SubmitURL: "https://claude.ai/code/s/1"}
	return server.New(f, nil, "https://boxes.example.com", time.Minute, st, a), f, st
}

// signIn stores a login session and returns its cookie. admin/activate control
// the session's capabilities.
//
// @arg t The test, failed if the session cannot be saved.
// @arg st The store to persist the login session in.
// @arg admin Whether the session has admin capability.
// @arg activate Whether the session may activate boxes.
// @return *http.Cookie The login cookie naming the persisted session.
func signIn(t *testing.T, st server.Store, admin, activate bool) *http.Cookie {
	t.Helper()
	if err := st.SaveLoginSession("SID", store.LoginSession{
		Email: "admin@corp.com", CSRF: "CSRF", ExpiresAt: time.Now().Add(time.Hour),
		Admin: admin, Activate: activate,
	}); err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: auth.LoginCookie, Value: "SID"}
}
