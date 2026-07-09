package store

import (
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
		Owner:         "dev@example.com",
		Description:   "front-end",
		AuthorizeURL:  "https://claude.com/cai/oauth/authorize?x=1",
		Status:        "pending",
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

	if err := st.PutBox(Box{Token: "a", InstanceID: "id-a", Status: "pending", Lifecycle: LifecycleRunning}); err != nil {
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

// TestIdentityStoreFlowRoundTrip checks an in-flight OIDC flow can be taken
// exactly once.
func TestIdentityStoreFlowRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	want := OIDCFlow{Provider: "google", ReturnToken: "TOK", Nonce: "N", PKCEVerifier: "V", ExpiresAt: time.Unix(1700000000, 0).UTC()}
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
