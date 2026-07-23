//go:build integration

package netfw

import (
	"context"
	"net/netip"
	"os"
	"os/exec"
	"testing"
)

// TestNFTablesRealApply applies a real baseline + allow + revoke + teardown
// against the host's nftables and checks the ruleset reflects each step. It is
// opt-in via `-tags integration` and self-skips unless run as root with nft
// present, since mutating the kernel ruleset needs CAP_NET_ADMIN. It proves the
// commands the unit tests assert on are actually accepted by nft.
//
// @arg t The test, skipped when not privileged.
func TestNFTablesRealApply(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to mutate nftables")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not installed")
	}
	n := NewNFTables(nil)
	box := "itest-box"
	t.Cleanup(func() { _ = n.Teardown(box) })

	spec := BoxSpec{Source: netip.MustParsePrefix("172.31.0.0/30"), DNS: netip.MustParseAddr("172.31.0.1")}
	if err := n.Baseline(box, spec); err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if err := n.Allow(box, netip.MustParseAddr("140.82.121.3")); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if out := nftList(t); !contains(out, "140.82.121.3") {
		t.Fatalf("ruleset missing the allowed IP:\n%s", out)
	}
	if err := n.Revoke(box, netip.MustParseAddr("140.82.121.3")); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if out := nftList(t); contains(out, "140.82.121.3") {
		t.Fatalf("ruleset still has the revoked IP:\n%s", out)
	}
	if err := n.Teardown(box); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
}

func nftList(t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "nft", "list", "table", "inet", tableName).CombinedOutput()
	if err != nil {
		t.Fatalf("nft list: %v: %s", err, out)
	}
	return string(out)
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
