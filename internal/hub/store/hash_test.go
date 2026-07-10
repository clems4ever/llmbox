package store

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestHashTokenIsDeterministicHex checks HashToken maps a token to a stable
// 64-character lowercase hex digest (SHA-256), never returning the token itself.
func TestHashTokenIsDeterministicHex(t *testing.T) {
	const token = "lbx-plaintext-token-value"
	h := HashToken(token)
	if h == token {
		t.Fatal("HashToken returned the token in the clear")
	}
	if HashToken(token) != h {
		t.Error("HashToken is not deterministic")
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(h) {
		t.Errorf("hash %q is not 64-char lowercase hex", h)
	}
}

// TestHashTokenDistinguishesInputs checks different tokens hash to different
// values (so a lookup by hash cannot collide two distinct secrets).
func TestHashTokenDistinguishesInputs(t *testing.T) {
	if HashToken("a") == HashToken("b") {
		t.Error("distinct tokens hashed to the same value")
	}
	if HashToken("") == HashToken("x") {
		t.Error("empty and non-empty tokens hashed alike")
	}
}

// TestOpenRestrictsFilePermissions checks Open leaves the state file and its WAL
// sidecars readable only by the owner (0600), so a co-located user cannot read
// the session store.
func TestOpenRestrictsFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		fi, err := os.Stat(path + suffix)
		if err != nil {
			// -wal/-shm may be checkpointed away; only the main file is guaranteed.
			continue
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("%q mode = %o, want 600", path+suffix, perm)
		}
	}
}
