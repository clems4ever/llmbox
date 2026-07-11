package store

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestSQLiteStoreRoundTrip checks a box survives save, reload, and close.
func TestSQLiteStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	b := Box{
		Token:         "tok1",
		InstanceID:    "abcdef0123456789",
		BoxID:         "web-box",
		Spoke:         "edge",
		Description:   "front-end",
		Status:        "broken",
		LastError:     "init script failed",
		HookState:     map[string]string{"granular-hook": "subj-1"},
		Lifecycle:     LifecycleRunning,
		CreatedAt:     time.Unix(1700000000, 0).UTC(),
		ObservedName:  "cname",
		ObservedImage: "img:1",
		ObservedState: "running",
		ObservedAt:    time.Unix(1700000100, 0).UTC(),
	}
	if err := st.PutBox(b); err != nil {
		t.Fatalf("PutBox: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen to prove data is on disk, not just in memory.
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()

	got, err := st2.ListBoxes()
	if err != nil {
		t.Fatalf("ListBoxes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 box, got %d", len(got))
	}
	if !reflect.DeepEqual(got[0], b) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got[0], b)
	}
}

// TestSQLiteStoreDelete checks a deleted box is gone and deleting a missing
// token is a harmless no-op.
func TestSQLiteStoreDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.PutBox(Box{Token: "a", InstanceID: "id-a", Status: "ready", Lifecycle: LifecycleRunning}); err != nil {
		t.Fatalf("PutBox: %v", err)
	}
	if err := st.DeleteBox("a"); err != nil {
		t.Fatalf("DeleteBox: %v", err)
	}
	if err := st.DeleteBox("missing"); err != nil {
		t.Errorf("DeleteBox of missing token should be a no-op, got %v", err)
	}
	got, err := st.ListBoxes()
	if err != nil {
		t.Fatalf("ListBoxes: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 boxes after delete, got %d", len(got))
	}
}

// TestOpenDropsActivationColumns checks Open drops the box-activation columns
// (boxes.owner/authorize_url/session_url, identity_sessions.can_activate,
// oidc_flows.return_token) from a database created before llmbox was reduced to
// pure box infrastructure, and that the drop is idempotent across reopens.
func TestOpenDropsActivationColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")

	// Lay down the pre-reduction tables carrying the now-removed columns, as an
	// old build did, each with one row so the drop must survive live data.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	stmts := []string{
		`CREATE TABLE boxes (
			token TEXT PRIMARY KEY,
			owner TEXT NOT NULL DEFAULT '',
			authorize_url TEXT NOT NULL DEFAULT '',
			session_url TEXT NOT NULL DEFAULT ''
		)`,
		`INSERT INTO boxes (token, owner, authorize_url, session_url) VALUES ('t', 'dev@example.com', 'https://a', 'https://s')`,
		`CREATE TABLE identity_sessions (
			session_hash TEXT PRIMARY KEY,
			can_activate INTEGER NOT NULL DEFAULT 0
		)`,
		`INSERT INTO identity_sessions (session_hash, can_activate) VALUES ('h', 1)`,
		`CREATE TABLE oidc_flows (
			state_hash TEXT PRIMARY KEY,
			return_token TEXT NOT NULL DEFAULT ''
		)`,
		`INSERT INTO oidc_flows (state_hash, return_token) VALUES ('s', 'TOK')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seeding old schema (%q): %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing seed db: %v", err)
	}

	dropped := map[string][]string{
		"boxes":             {"owner", "authorize_url", "session_url"},
		"identity_sessions": {"can_activate"},
		"oidc_flows":        {"return_token"},
	}

	// Opening twice proves the DROP COLUMN steps are guarded, not blindly reapplied.
	for i := 0; i < 2; i++ {
		st, err := Open(path)
		if err != nil {
			t.Fatalf("Open (pass %d): %v", i+1, err)
		}
		_ = st.Close()

		check, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatalf("reopening for pragma check: %v", err)
		}
		for table, cols := range dropped {
			for _, col := range cols {
				var n int
				if err := check.QueryRow(
					`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, col).Scan(&n); err != nil {
					t.Fatalf("probing %s.%s: %v", table, col, err)
				}
				if n != 0 {
					t.Errorf("pass %d: %s.%s still present, want dropped", i+1, table, col)
				}
			}
		}
		_ = check.Close()
	}
}

// TestIdentityStoreFlowRoundTrip checks an in-flight OIDC flow can be taken
// exactly once.
func TestIdentityStoreFlowRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	want := OIDCFlow{Provider: "google", ReturnTo: "/admin", Nonce: "N", PKCEVerifier: "V", ExpiresAt: time.Unix(1700000000, 0).UTC()}
	if err := st.PutOIDCFlow("state1", want); err != nil {
		t.Fatalf("PutOIDCFlow: %v", err)
	}
	got, ok, err := st.TakeOIDCFlow("state1")
	if err != nil || !ok {
		t.Fatalf("TakeOIDCFlow: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("flow mismatch:\n got %+v\nwant %+v", got, want)
	}
	// Second take finds nothing (one-time use).
	if _, ok, _ := st.TakeOIDCFlow("state1"); ok {
		t.Error("flow should be consumed after first take")
	}
}

// TestIdentityStoreSessionRoundTrip checks an identity session survives
// save/read and can be deleted.
func TestIdentityStoreSessionRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	want := IdentitySession{Email: "a@corp.com", Provider: "google", CSRFToken: "c", ExpiresAt: time.Unix(1700000000, 0).UTC()}
	if err := st.PutIdentitySession("sid", want); err != nil {
		t.Fatalf("PutIdentitySession: %v", err)
	}
	got, ok, err := st.GetIdentitySession("sid")
	if err != nil || !ok {
		t.Fatalf("GetIdentitySession: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("session mismatch:\n got %+v\nwant %+v", got, want)
	}
	if err := st.DeleteIdentitySession("sid"); err != nil {
		t.Fatalf("DeleteIdentitySession: %v", err)
	}
	if _, ok, _ := st.GetIdentitySession("sid"); ok {
		t.Error("session should be gone after delete")
	}
}

// TestIdentityStorePurgeExpired checks expired sessions and flows are dropped
// while live ones remain.
func TestIdentityStorePurgeExpired(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	now := time.Unix(1700000000, 0).UTC()
	_ = st.PutIdentitySession("live", IdentitySession{Email: "a@corp.com", ExpiresAt: now.Add(time.Hour)})
	_ = st.PutIdentitySession("dead", IdentitySession{Email: "b@corp.com", ExpiresAt: now.Add(-time.Hour)})
	_ = st.PutOIDCFlow("liveflow", OIDCFlow{ExpiresAt: now.Add(time.Hour)})
	_ = st.PutOIDCFlow("deadflow", OIDCFlow{ExpiresAt: now.Add(-time.Hour)})

	if err := st.PurgeExpiredIdentities(now); err != nil {
		t.Fatalf("PurgeExpiredIdentities: %v", err)
	}
	if _, ok, _ := st.GetIdentitySession("live"); !ok {
		t.Error("live session was purged")
	}
	if _, ok, _ := st.GetIdentitySession("dead"); ok {
		t.Error("expired session was not purged")
	}
	if _, ok, _ := st.TakeOIDCFlow("liveflow"); !ok {
		t.Error("live flow was purged")
	}
	if _, ok, _ := st.TakeOIDCFlow("deadflow"); ok {
		t.Error("expired flow was not purged")
	}
}
