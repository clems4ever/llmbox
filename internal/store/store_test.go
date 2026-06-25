package store

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestBoltStoreRoundTrip checks a session survives save, reload, and close.
func TestBoltStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	ps := PersistedSession{
		Token:        "tok1",
		ContainerID:  "abcdef0123456789",
		AuthorizeURL: "https://claude.com/cai/oauth/authorize?x=1",
		CreatedAt:    time.Unix(1700000000, 0).UTC(),
		HookState:    map[string]string{"granular-hook": "subj-1"},
		BoxID:        "web-box",
		Description:  "front-end",
		Status:       "pending",
	}
	if err := st.Save(ps); err != nil {
		t.Fatalf("Save: %v", err)
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

	got, err := st2.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 session, got %d", len(got))
	}
	if !reflect.DeepEqual(got[0], ps) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got[0], ps)
	}
}

// TestBoltStoreDelete checks a deleted session is gone and deleting a missing
// token is a harmless no-op.
func TestBoltStoreDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.Save(PersistedSession{Token: "a", ContainerID: "id-a", Status: "pending"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := st.Delete("a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := st.Delete("missing"); err != nil {
		t.Errorf("Delete of missing token should be a no-op, got %v", err)
	}
	got, err := st.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 sessions after delete, got %d", len(got))
	}
}

// TestLoginStoreFlowRoundTrip checks an in-flight OIDC flow can be taken exactly
// once.
func TestLoginStoreFlowRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	want := LoginFlow{Provider: "google", ReturnToken: "TOK", Nonce: "N", Verifier: "V", ExpiresAt: time.Unix(1700000000, 0).UTC()}
	if err := st.SaveLoginFlow("state1", want); err != nil {
		t.Fatalf("SaveLoginFlow: %v", err)
	}
	got, ok, err := st.TakeLoginFlow("state1")
	if err != nil || !ok {
		t.Fatalf("TakeLoginFlow: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("flow mismatch:\n got %+v\nwant %+v", got, want)
	}
	// Second take finds nothing (one-time use).
	if _, ok, _ := st.TakeLoginFlow("state1"); ok {
		t.Error("flow should be consumed after first take")
	}
}

// TestLoginStoreSessionRoundTrip checks a login session survives save/read and
// can be deleted.
func TestLoginStoreSessionRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	want := LoginSession{Email: "a@corp.com", Provider: "google", CSRF: "c", ExpiresAt: time.Unix(1700000000, 0).UTC()}
	if err := st.SaveLoginSession("sid", want); err != nil {
		t.Fatalf("SaveLoginSession: %v", err)
	}
	got, ok, err := st.LoginSession("sid")
	if err != nil || !ok {
		t.Fatalf("LoginSession: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("session mismatch:\n got %+v\nwant %+v", got, want)
	}
	if err := st.DeleteLoginSession("sid"); err != nil {
		t.Fatalf("DeleteLoginSession: %v", err)
	}
	if _, ok, _ := st.LoginSession("sid"); ok {
		t.Error("session should be gone after delete")
	}
}

// TestLoginStorePurgeExpired checks expired sessions and flows are dropped while
// live ones remain.
func TestLoginStorePurgeExpired(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	now := time.Unix(1700000000, 0).UTC()
	_ = st.SaveLoginSession("live", LoginSession{Email: "a@corp.com", ExpiresAt: now.Add(time.Hour)})
	_ = st.SaveLoginSession("dead", LoginSession{Email: "b@corp.com", ExpiresAt: now.Add(-time.Hour)})
	_ = st.SaveLoginFlow("liveflow", LoginFlow{ExpiresAt: now.Add(time.Hour)})
	_ = st.SaveLoginFlow("deadflow", LoginFlow{ExpiresAt: now.Add(-time.Hour)})

	if err := st.PurgeExpiredLogins(now); err != nil {
		t.Fatalf("PurgeExpiredLogins: %v", err)
	}
	if _, ok, _ := st.LoginSession("live"); !ok {
		t.Error("live session was purged")
	}
	if _, ok, _ := st.LoginSession("dead"); ok {
		t.Error("expired session was not purged")
	}
	if _, ok, _ := st.TakeLoginFlow("liveflow"); !ok {
		t.Error("live flow was purged")
	}
	if _, ok, _ := st.TakeLoginFlow("deadflow"); ok {
		t.Error("expired flow was not purged")
	}
}
