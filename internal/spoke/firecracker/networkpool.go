package firecracker

import (
	"context"
	"fmt"
	"io"
	"os"
)

// This file exposes the host-side egress pool (TAP devices + NAT/isolation rules)
// as a standalone, operator-run step, so the CAP_NET_ADMIN work can be provisioned
// once at boot by a privileged systemd oneshot (or a manual root invocation) and the
// long-running spoke can then attach to the pre-provisioned pool unprivileged with
// --egress-mode=external. It mirrors exactly what the managed-mode spoke would do at
// startup (EnsurePool), just decoupled into its own lifecycle.

// PoolConfig parameterises the externally provisioned egress pool. The zero value
// asks for the same pool a default managed spoke would create, so an operator can
// provision it with no flags and the spoke can attach with no matching flags.
type PoolConfig struct {
	// Size is the number of TAP slots (llmboxfc0..Size-1); 0 uses the default.
	Size int
	// TapGroupGID owns the created TAP devices so a jailed, unprivileged Firecracker
	// running under that group can open its TAP without CAP_NET_ADMIN; 0 uses the
	// default fc-net GID (matching the spoke's jailer default).
	TapGroupGID int
	// Uplink is the host interface guest traffic is masqueraded out of; empty
	// resolves the default-route interface.
	Uplink string
}

// resolve fills in the defaults so callers can pass a partial config.
//
// @return PoolConfig The config with Size and TapGroupGID defaulted.
//
// @testcase TestPoolConfigResolveDefaults fills unset size and gid with the defaults.
func (c PoolConfig) resolve() PoolConfig {
	if c.Size <= 0 {
		c.Size = defaultPoolSize
	}
	if c.TapGroupGID <= 0 {
		c.TapGroupGID = defaultFcGID
	}
	return c
}

// SetupNetworkPool provisions the egress TAP pool and shared NAT/isolation rules on
// this host, idempotently, so an external provisioner can create the plumbing the
// spoke then attaches to in --egress-mode=external. It requires root / CAP_NET_ADMIN
// (it runs sysctl, ip, and iptables) and is safe to re-run — matching EnsurePool's
// idempotence — so a systemd oneshot can run it on every boot. The TAPs are created
// owned by cfg.TapGroupGID, which must match the group the spoke's jailed VMMs run
// under (the spoke's --tap-group / default fc-net GID) so they can open their TAP.
//
// @arg ctx Context for the ip/iptables invocations.
// @arg out Destination for the human-readable progress line; nil discards it.
// @arg cfg The pool size, owning GID, and uplink (zero fields take defaults).
// @error error if not root, or a device/rule cannot be created.
//
// @testcase TestSetupNetworkPoolRequiresRoot errors (or is skipped as root) when unprivileged.
func SetupNetworkPool(ctx context.Context, out io.Writer, cfg PoolConfig) error {
	cfg = cfg.resolve()
	if os.Geteuid() != 0 {
		return fmt.Errorf("setting up the firecracker egress pool needs root / CAP_NET_ADMIN " +
			"(it runs sysctl, ip, and iptables); re-run as root")
	}
	e := newHostEgress(cfg.Uplink, cfg.TapGroupGID)
	if err := e.EnsurePool(ctx, cfg.Size); err != nil {
		return fmt.Errorf("provisioning firecracker egress pool: %w", err)
	}
	if out != nil {
		fmt.Fprintf(out, "provisioned firecracker egress pool: %d TAP device(s) llmboxfc0..llmboxfc%d owned by GID %d, NAT/isolation rules installed\n",
			cfg.Size, cfg.Size-1, cfg.TapGroupGID)
		fmt.Fprintf(out, "run the spoke with --egress-mode=external%s to attach without CAP_NET_ADMIN\n", poolFlagHint(cfg))
	}
	return nil
}

// TeardownNetworkPool removes the egress TAP pool and shared rules this host
// provisioned, best-effort. It requires root and is the inverse of SetupNetworkPool,
// for decommissioning a host or resizing a pool.
//
// @arg ctx Context for the ip/iptables invocations.
// @arg out Destination for the human-readable progress line; nil discards it.
// @arg cfg The pool size and uplink (zero fields take defaults); TapGroupGID is unused.
// @error error if not root.
//
// @testcase TestTeardownNetworkPoolRequiresRoot errors when unprivileged.
func TeardownNetworkPool(ctx context.Context, out io.Writer, cfg PoolConfig) error {
	cfg = cfg.resolve()
	if os.Geteuid() != 0 {
		return fmt.Errorf("tearing down the firecracker egress pool needs root / CAP_NET_ADMIN; re-run as root")
	}
	e := newHostEgress(cfg.Uplink, 0)
	if err := e.TeardownPool(ctx, cfg.Size); err != nil {
		return err
	}
	if out != nil {
		fmt.Fprintf(out, "removed firecracker egress pool: %d TAP device(s) and NAT/isolation rules\n", cfg.Size)
	}
	return nil
}

// poolFlagHint echoes the non-default pool knobs the spoke must repeat to match a
// pool provisioned with overrides, so the setup output points the operator at the
// exact flags to add.
//
// @arg cfg The resolved pool config.
// @return string The flag suffix (leading space) for non-default size/gid, or empty.
//
// @testcase TestSetupNetworkPoolRequiresRoot exercises the default (empty) hint path.
func poolFlagHint(cfg PoolConfig) string {
	hint := ""
	if cfg.Size != defaultPoolSize {
		hint += fmt.Sprintf(" --pool-size %d", cfg.Size)
	}
	if cfg.TapGroupGID != defaultFcGID {
		hint += fmt.Sprintf(" --tap-group %d", cfg.TapGroupGID)
	}
	return hint
}
