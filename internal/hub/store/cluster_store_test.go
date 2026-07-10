package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/cluster"
)

// TestClusterStoreJoinTokenRoundTrip checks join tokens persist, take once (one-time), and miss cleanly.
func TestClusterStoreJoinTokenRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	exp := time.Now().Add(time.Hour).UTC().Round(time.Second)
	if err := st.PutJoinToken("hash1", cluster.JoinTokenRecord{Name: "edge", Backend: "firecracker", ExpiresAt: exp}); err != nil {
		t.Fatalf("PutJoinToken: %v", err)
	}

	rec, found, err := st.TakeJoinToken("hash1")
	if err != nil || !found {
		t.Fatalf("TakeJoinToken = (%+v,%v,%v)", rec, found, err)
	}
	if rec.Name != "edge" || rec.Backend != "firecracker" || !rec.ExpiresAt.Equal(exp) {
		t.Errorf("record = %+v", rec)
	}
	// One-time: a second take finds nothing.
	if _, found, _ := st.TakeJoinToken("hash1"); found {
		t.Error("join token was not consumed")
	}
	// Missing token is not found, not an error.
	if _, found, err := st.TakeJoinToken("nope"); found || err != nil {
		t.Errorf("missing token = (%v,%v)", found, err)
	}
}

// TestClusterStoreJoinTokenListAndDelete checks join tokens can be listed (with
// their hash ID, name, and expiry) and revoked by ID.
func TestClusterStoreJoinTokenListAndDelete(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	exp := time.Now().Add(time.Hour).UTC().Round(time.Second)
	_ = st.PutJoinToken("hashA", cluster.JoinTokenRecord{Name: "edge-1", Backend: "docker", ExpiresAt: exp})
	_ = st.PutJoinToken("hashB", cluster.JoinTokenRecord{Name: "edge-2", Backend: "firecracker", ExpiresAt: exp})

	tokens, err := st.ListJoinTokens()
	if err != nil || len(tokens) != 2 {
		t.Fatalf("ListJoinTokens = (%v,%v), want 2", tokens, err)
	}
	byID := map[string]cluster.JoinTokenInfo{}
	for _, tk := range tokens {
		byID[tk.ID] = tk
	}
	if byID["hashA"].Name != "edge-1" || byID["hashA"].Backend != "docker" || !byID["hashA"].ExpiresAt.Equal(exp) {
		t.Errorf("listing for hashA = %+v", byID["hashA"])
	}
	if byID["hashB"].Backend != "firecracker" {
		t.Errorf("listing for hashB = %+v", byID["hashB"])
	}

	if err := st.DeleteJoinToken("hashA"); err != nil {
		t.Fatalf("DeleteJoinToken: %v", err)
	}
	tokens, _ = st.ListJoinTokens()
	if len(tokens) != 1 || tokens[0].ID != "hashB" {
		t.Errorf("after delete, listing = %v, want only hashB", tokens)
	}
}

// TestOpenMigratesJoinTokenBackend checks Open retrofits the backend column onto
// a spoke_join_tokens table created before it existed, keeping the old row
// readable (empty backend) and the migration idempotent across reopens.
func TestOpenMigratesJoinTokenBackend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")

	// Lay down the pre-backend-column table with one token, as an old build did.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	exp := time.Now().Add(time.Hour).UTC().Round(time.Second)
	if _, err := db.Exec(`CREATE TABLE spoke_join_tokens (
		secret_hash TEXT PRIMARY KEY,
		name        TEXT NOT NULL,
		expires_at  TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("creating old table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO spoke_join_tokens VALUES ('oldhash', 'edge', ?)`, encodeTime(exp)); err != nil {
		t.Fatalf("seeding old row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing seed db: %v", err)
	}

	// Opening twice proves the ALTER TABLE is guarded, not blindly reapplied.
	for i := 0; i < 2; i++ {
		st, err := Open(path)
		if err != nil {
			t.Fatalf("Open (pass %d): %v", i+1, err)
		}
		tokens, err := st.ListJoinTokens()
		if err != nil || len(tokens) != 1 {
			t.Fatalf("ListJoinTokens (pass %d) = (%v,%v), want the migrated row", i+1, tokens, err)
		}
		if tokens[0].Name != "edge" || tokens[0].Backend != "" || !tokens[0].ExpiresAt.Equal(exp) {
			t.Errorf("migrated row = %+v, want empty backend", tokens[0])
		}
		_ = st.Close()
	}
}

// TestClusterStoreSpokeRoundTrip checks enrolled spokes persist, list, and delete.
func TestClusterStoreSpokeRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	rec := cluster.SpokeRecord{Name: "edge", CredentialHash: "abc", EnrolledAt: time.Now().UTC().Round(time.Second)}
	if err := st.PutSpoke("edge", rec); err != nil {
		t.Fatalf("PutSpoke: %v", err)
	}

	got, found, err := st.GetSpoke("edge")
	if err != nil || !found || got.CredentialHash != "abc" {
		t.Fatalf("GetSpoke = (%+v,%v,%v)", got, found, err)
	}

	spokes, err := st.ListSpokes()
	if err != nil || len(spokes) != 1 || spokes[0].Name != "edge" {
		t.Fatalf("ListSpokes = (%v,%v)", spokes, err)
	}

	if err := st.DeleteSpoke("edge"); err != nil {
		t.Fatalf("DeleteSpoke: %v", err)
	}
	if _, found, _ := st.GetSpoke("edge"); found {
		t.Error("spoke not deleted")
	}
}
