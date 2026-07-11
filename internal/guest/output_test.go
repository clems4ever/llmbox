package guest

import (
	"strings"
	"testing"
)

// TestExecCapsOutput checks capOutput returns short output verbatim and truncates
// output past the cap, appending the truncation marker.
func TestExecCapsOutput(t *testing.T) {
	// At or below the cap, the output is returned unchanged.
	if got := capOutput([]byte("hello")); got != "hello" {
		t.Fatalf("small output altered: %q", got)
	}

	// Past the cap, the output is truncated to maxExecOutput and marked.
	big := []byte(strings.Repeat("x", maxExecOutput+100))
	got := capOutput(big)
	if !strings.HasSuffix(got, "[output truncated]") {
		t.Fatalf("missing truncation marker in %q...", got[len(got)-40:])
	}
	if !strings.HasPrefix(got, strings.Repeat("x", maxExecOutput)) {
		t.Fatalf("kept prefix altered")
	}
	if len(got) <= maxExecOutput {
		t.Fatalf("expected marker appended past the cap, got len %d", len(got))
	}
}
