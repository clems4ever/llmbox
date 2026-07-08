package apikey

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	storepkg "github.com/clems4ever/llmbox/internal/hub/store"
)

// openStore opens a fresh SQLite store in a temp dir for one test.
func openStore(t *testing.T) storepkg.Store {
	t.Helper()
	st, err := storepkg.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestCreateStoresHashAndAuthenticates checks Create returns a prefixed secret,
// persists only its hash, and that Authenticate accepts the fresh key.
func TestCreateStoresHashAndAuthenticates(t *testing.T) {
	st := openStore(t)
	now := time.Now()

	secret, err := Create(st, "ci", time.Hour, now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(secret, "lbx_") {
		t.Errorf("secret = %q, want lbx_ prefix", secret)
	}

	// Only the hash is stored: the plaintext secret is not a valid store key.
	if _, found, _ := st.GetAPIKey(secret); found {
		t.Error("plaintext secret found in store; only the hash must be stored")
	}
	if _, found, _ := st.GetAPIKey(HashSecret(secret)); !found {
		t.Error("hash of the secret not found in store")
	}

	rec, ok, err := Authenticate(st, secret, now)
	if err != nil || !ok {
		t.Fatalf("Authenticate = (%+v,%v,%v), want ok", rec, ok, err)
	}
	if rec.Name != "ci" {
		t.Errorf("record name = %q, want ci", rec.Name)
	}
}

// TestCreateRejectsEmptyName checks a key cannot be minted without a name.
func TestCreateRejectsEmptyName(t *testing.T) {
	if _, err := Create(openStore(t), "", time.Hour, time.Now()); err == nil {
		t.Error("Create with empty name = nil, want error")
	}
}

// TestCreateRejectsTTL checks non-positive TTLs are refused so a key always
// expires.
func TestCreateRejectsTTL(t *testing.T) {
	st := openStore(t)
	for _, ttl := range []time.Duration{0, -time.Hour} {
		if _, err := Create(st, "ci", ttl, time.Now()); err == nil {
			t.Errorf("Create with ttl %v = nil, want error", ttl)
		}
	}
}

// TestAuthenticateRejectsUnknown checks an unknown (or empty) secret is
// rejected without error.
func TestAuthenticateRejectsUnknown(t *testing.T) {
	st := openStore(t)
	if _, ok, err := Authenticate(st, "lbx_deadbeef", time.Now()); ok || err != nil {
		t.Errorf("unknown secret = (%v,%v), want (false,nil)", ok, err)
	}
	if _, ok, err := Authenticate(st, "", time.Now()); ok || err != nil {
		t.Errorf("empty secret = (%v,%v), want (false,nil)", ok, err)
	}
}

// TestAuthenticateRejectsExpired checks a key past its expiry no longer
// authenticates, indistinguishable from an unknown key.
func TestAuthenticateRejectsExpired(t *testing.T) {
	st := openStore(t)
	now := time.Now()
	secret, err := Create(st, "ci", time.Hour, now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, ok, _ := Authenticate(st, secret, now.Add(2*time.Hour)); ok {
		t.Error("expired key authenticated")
	}
}
