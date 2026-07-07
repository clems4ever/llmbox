package store

import (
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
	if err := st.PutJoinToken("hash1", cluster.JoinTokenRecord{Name: "edge", ExpiresAt: exp}); err != nil {
		t.Fatalf("PutJoinToken: %v", err)
	}

	rec, found, err := st.TakeJoinToken("hash1")
	if err != nil || !found {
		t.Fatalf("TakeJoinToken = (%+v,%v,%v)", rec, found, err)
	}
	if rec.Name != "edge" || !rec.ExpiresAt.Equal(exp) {
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
	_ = st.PutJoinToken("hashA", cluster.JoinTokenRecord{Name: "edge-1", ExpiresAt: exp})
	_ = st.PutJoinToken("hashB", cluster.JoinTokenRecord{Name: "edge-2", ExpiresAt: exp})

	tokens, err := st.ListJoinTokens()
	if err != nil || len(tokens) != 2 {
		t.Fatalf("ListJoinTokens = (%v,%v), want 2", tokens, err)
	}
	byID := map[string]cluster.JoinTokenInfo{}
	for _, tk := range tokens {
		byID[tk.ID] = tk
	}
	if byID["hashA"].Name != "edge-1" || !byID["hashA"].ExpiresAt.Equal(exp) {
		t.Errorf("listing for hashA = %+v", byID["hashA"])
	}

	if err := st.DeleteJoinToken("hashA"); err != nil {
		t.Fatalf("DeleteJoinToken: %v", err)
	}
	tokens, _ = st.ListJoinTokens()
	if len(tokens) != 1 || tokens[0].ID != "hashB" {
		t.Errorf("after delete, listing = %v, want only hashB", tokens)
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
