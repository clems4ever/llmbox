package firecracker

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// haveCmds reports whether every named executable is resolvable on PATH.
func haveCmds(names ...string) bool {
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			return false
		}
	}
	return true
}

// TestPoolConfigResolveDefaults checks the zero config resolves to the same pool a
// default managed spoke uses, and that explicit values are left untouched.
func TestPoolConfigResolveDefaults(t *testing.T) {
	got := PoolConfig{}.resolve()
	if got.Size != defaultPoolSize || got.TapGroupGID != defaultFcGID {
		t.Fatalf("resolve() = %+v, want size=%d gid=%d", got, defaultPoolSize, defaultFcGID)
	}
	custom := PoolConfig{Size: 3, TapGroupGID: 4242, Uplink: "eth9"}.resolve()
	if custom.Size != 3 || custom.TapGroupGID != 4242 || custom.Uplink != "eth9" {
		t.Fatalf("resolve() overwrote explicit values: %+v", custom)
	}
}

// TestSetupNetworkPoolRequiresRoot checks the setup path refuses to run without root
// (where it cannot create TAPs / rules) with an actionable error. As root it is
// skipped rather than mutating the host from a unit test.
func TestSetupNetworkPoolRequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; skipping the non-root guard assertion to avoid mutating the host")
	}
	err := SetupNetworkPool(context.Background(), nil, PoolConfig{})
	if err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("SetupNetworkPool(non-root) = %v, want a root-required error", err)
	}
}

// TestTeardownNetworkPoolRequiresRoot mirrors the setup guard for teardown.
func TestTeardownNetworkPoolRequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; skipping the non-root guard assertion")
	}
	err := TeardownNetworkPool(context.Background(), nil, PoolConfig{})
	if err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("TeardownNetworkPool(non-root) = %v, want a root-required error", err)
	}
}

// TestPoolFlagHint checks the setup output only names knobs that differ from the
// defaults, so a default provisioning prints the bare --egress-mode=external hint.
func TestPoolFlagHint(t *testing.T) {
	if h := poolFlagHint(PoolConfig{Size: defaultPoolSize, TapGroupGID: defaultFcGID}); h != "" {
		t.Fatalf("poolFlagHint(defaults) = %q, want empty", h)
	}
	h := poolFlagHint(PoolConfig{Size: 32, TapGroupGID: 4242})
	if !strings.Contains(h, "--pool-size 32") || !strings.Contains(h, "--tap-group 4242") {
		t.Fatalf("poolFlagHint = %q, want it to name both overrides", h)
	}
}

// TestSetupNetworkPoolProvisionsAsRoot exercises the real provisioning + teardown
// round-trip, but only when running as root with ip/iptables present (otherwise the
// non-root guard already returns before any host mutation). It leaves no devices
// behind. The human-readable output is checked to include the attach hint.
func TestSetupNetworkPoolProvisionsAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("real pool provisioning needs root (CAP_NET_ADMIN)")
	}
	if !haveCmds("ip", "iptables") {
		t.Skip("ip/iptables not available")
	}
	var out bytes.Buffer
	cfg := PoolConfig{Size: 2}
	if err := SetupNetworkPool(context.Background(), &out, cfg); err != nil {
		t.Fatalf("SetupNetworkPool: %v", err)
	}
	t.Cleanup(func() { _ = TeardownNetworkPool(context.Background(), nil, cfg) })
	if !strings.Contains(out.String(), "--egress-mode=external") {
		t.Fatalf("setup output = %q, want the external attach hint", out.String())
	}
	// Idempotent: a second setup must not error.
	if err := SetupNetworkPool(context.Background(), nil, cfg); err != nil {
		t.Fatalf("SetupNetworkPool (second): %v", err)
	}
}

// The ip-link UP-flag parsing is covered by internal/spoke/microvm/mvmnet
// (TestLinkFlagsUp), the shared package this backend's egress now lives in.
