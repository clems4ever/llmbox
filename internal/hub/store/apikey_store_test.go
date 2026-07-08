package store

import (
	"path/filepath"
	"testing"
	"time"
)

// TestAPIKeyStoreRoundTrip checks API keys persist by hash, read back their
// fields, and miss cleanly for an unknown hash.
func TestAPIKeyStoreRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	created := time.Now().UTC().Round(time.Second)
	exp := created.Add(time.Hour)
	if err := st.PutAPIKey("hash1", APIKeyRecord{Name: "ci", CreatedAt: created, ExpiresAt: exp}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}

	rec, found, err := st.GetAPIKey("hash1")
	if err != nil || !found {
		t.Fatalf("GetAPIKey = (%+v,%v,%v)", rec, found, err)
	}
	if rec.Name != "ci" || !rec.CreatedAt.Equal(created) || !rec.ExpiresAt.Equal(exp) {
		t.Errorf("record = %+v", rec)
	}
	// Unlike join tokens, a get does not consume the key.
	if _, found, _ := st.GetAPIKey("hash1"); !found {
		t.Error("key was consumed by a read")
	}
	// Missing key is not found, not an error.
	if _, found, err := st.GetAPIKey("nope"); found || err != nil {
		t.Errorf("missing key = (%v,%v)", found, err)
	}

	// Re-putting the same hash replaces the record.
	if err := st.PutAPIKey("hash1", APIKeyRecord{Name: "renamed", CreatedAt: created, ExpiresAt: exp}); err != nil {
		t.Fatalf("PutAPIKey replace: %v", err)
	}
	if rec, _, _ := st.GetAPIKey("hash1"); rec.Name != "renamed" {
		t.Errorf("replaced record = %+v, want name renamed", rec)
	}
}

// TestAPIKeyStoreListAndDelete checks listing returns every stored key with its
// hash ID and that deletion removes exactly the targeted key (idempotently).
func TestAPIKeyStoreListAndDelete(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC().Round(time.Second)
	for _, h := range []string{"h-a", "h-b"} {
		if err := st.PutAPIKey(h, APIKeyRecord{Name: "k-" + h, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Fatalf("PutAPIKey(%s): %v", h, err)
		}
	}

	keys, err := st.ListAPIKeys()
	if err != nil || len(keys) != 2 {
		t.Fatalf("ListAPIKeys = %v (%v), want 2 keys", keys, err)
	}
	ids := map[string]string{}
	for _, k := range keys {
		ids[k.ID] = k.Name
	}
	if ids["h-a"] != "k-h-a" || ids["h-b"] != "k-h-b" {
		t.Errorf("listed keys = %v", ids)
	}

	if err := st.DeleteAPIKey("h-a"); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	if keys, _ := st.ListAPIKeys(); len(keys) != 1 || keys[0].ID != "h-b" {
		t.Errorf("after delete, keys = %v, want only h-b", keys)
	}
	// Deleting a missing key is a no-op.
	if err := st.DeleteAPIKey("h-a"); err != nil {
		t.Errorf("second delete = %v, want nil", err)
	}
}
