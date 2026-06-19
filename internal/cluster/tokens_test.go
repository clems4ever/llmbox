package cluster

import (
	"errors"
	"testing"
	"time"
)

func TestCreateJoinTokenStoresHash(t *testing.T) {
	store := newMemStore()
	now := time.Unix(1_000, 0)
	tok, err := CreateJoinToken(store, "edge", time.Hour, now)
	if err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	if len(tok) != 64 {
		t.Errorf("token length = %d, want 64", len(tok))
	}
	rec, ok := store.join[hashSecret(tok)]
	if !ok {
		t.Fatal("token not stored under its hash")
	}
	if _, plain := store.join[tok]; plain {
		t.Error("token stored in plaintext form")
	}
	if rec.Name != "edge" || !rec.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Errorf("stored record = %+v", rec)
	}
}

func TestCreateJoinTokenRejectsEmptyName(t *testing.T) {
	if _, err := CreateJoinToken(newMemStore(), "", time.Hour, time.Now()); err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestCreateJoinTokenRejectsTTL(t *testing.T) {
	if _, err := CreateJoinToken(newMemStore(), "edge", 0, time.Now()); err == nil {
		t.Fatal("expected error for non-positive ttl")
	}
}

func TestEnrollWithJoinTokenMintsCredential(t *testing.T) {
	store := newMemStore()
	now := time.Unix(2_000, 0)
	tok, err := CreateJoinToken(store, "edge", time.Hour, now)
	if err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	name, cred, err := authenticateEnroll(store, enrollReq{JoinToken: tok}, now)
	if err != nil {
		t.Fatalf("authenticateEnroll: %v", err)
	}
	if name != "edge" {
		t.Errorf("name = %q, want edge", name)
	}
	if len(cred) != 64 {
		t.Errorf("credential length = %d, want 64", len(cred))
	}
	rec, ok := store.spokes["edge"]
	if !ok {
		t.Fatal("spoke not stored")
	}
	if !sameHash(cred, rec.CredentialHash) {
		t.Error("stored credential hash does not match minted credential")
	}
	if _, present, _ := store.TakeJoinToken(hashSecret(tok)); present {
		t.Error("join token was not consumed (one-time use)")
	}
}

func TestEnrollRejectsExpiredToken(t *testing.T) {
	store := newMemStore()
	now := time.Unix(3_000, 0)
	tok, _ := CreateJoinToken(store, "edge", time.Minute, now)
	_, _, err := authenticateEnroll(store, enrollReq{JoinToken: tok}, now.Add(2*time.Minute))
	if !errors.Is(err, errEnrollRejected) {
		t.Fatalf("err = %v, want errEnrollRejected", err)
	}
	// Even though expired, the token must have been consumed.
	if _, present, _ := store.TakeJoinToken(hashSecret(tok)); present {
		t.Error("expired token should still be consumed")
	}
}

func TestEnrollRejectsUnknownToken(t *testing.T) {
	_, _, err := authenticateEnroll(newMemStore(), enrollReq{JoinToken: "nope"}, time.Now())
	if !errors.Is(err, errEnrollRejected) {
		t.Fatalf("err = %v, want errEnrollRejected", err)
	}
}

func TestEnrollReusedTokenRejected(t *testing.T) {
	store := newMemStore()
	now := time.Unix(4_000, 0)
	tok, _ := CreateJoinToken(store, "edge", time.Hour, now)
	if _, _, err := authenticateEnroll(store, enrollReq{JoinToken: tok}, now); err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	if _, _, err := authenticateEnroll(store, enrollReq{JoinToken: tok}, now); !errors.Is(err, errEnrollRejected) {
		t.Fatalf("second enroll err = %v, want errEnrollRejected", err)
	}
}

func TestReconnectChecksCredential(t *testing.T) {
	store := newMemStore()
	now := time.Unix(5_000, 0)
	tok, _ := CreateJoinToken(store, "edge", time.Hour, now)
	_, cred, err := authenticateEnroll(store, enrollReq{JoinToken: tok}, now)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}

	// Correct credential reconnects.
	name, newCred, err := authenticateEnroll(store, enrollReq{Name: "edge", Credential: cred}, now)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if name != "edge" || newCred != "" {
		t.Errorf("reconnect returned name=%q cred=%q, want edge and empty credential", name, newCred)
	}

	// Wrong credential is rejected.
	if _, _, err := authenticateEnroll(store, enrollReq{Name: "edge", Credential: "wrong"}, now); !errors.Is(err, errEnrollRejected) {
		t.Errorf("wrong credential err = %v, want errEnrollRejected", err)
	}
	// Unknown spoke is rejected.
	if _, _, err := authenticateEnroll(store, enrollReq{Name: "ghost", Credential: cred}, now); !errors.Is(err, errEnrollRejected) {
		t.Errorf("unknown spoke err = %v, want errEnrollRejected", err)
	}
	// Missing fields are rejected.
	if _, _, err := authenticateEnroll(store, enrollReq{}, now); !errors.Is(err, errEnrollRejected) {
		t.Errorf("empty enroll err = %v, want errEnrollRejected", err)
	}
}
