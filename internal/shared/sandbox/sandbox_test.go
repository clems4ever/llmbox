package sandbox

import (
	"strings"
	"testing"
)

// TestValidBoxID accepts well-formed ids and rejects malformed ones.
func TestValidBoxID(t *testing.T) {
	for _, id := range []string{"a", "my-box", "refactor-auth-service", "b1", strings.Repeat("a", 63)} {
		if !ValidBoxID(id) {
			t.Errorf("ValidBoxID(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"", "UPPER", "has space", `x"; rm -rf /`, "-lead", "trail-", "a/b", strings.Repeat("a", 64)} {
		if ValidBoxID(id) {
			t.Errorf("ValidBoxID(%q) = true, want false", id)
		}
	}
}
