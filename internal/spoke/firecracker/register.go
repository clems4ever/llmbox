package firecracker

import (
	"context"
	"fmt"
	"log"

	"github.com/clems4ever/llmbox/internal/spoke/box/backend"
)

// init registers the Firecracker backend so importing this package makes
// "firecracker" selectable through backend.New.
//
// @testcase TestFirecrackerBackendRegistered checks importing this package registers the firecracker backend.
func init() {
	backend.Register("firecracker", newBackend)
}

// newBackend builds a Firecracker Provisioner from neutral backend options, then
// validates the jailer prerequisites and provisions the egress pool. Every box is
// launched through the jailer (chrooted, unprivileged per-VM UID); jailing is
// mandatory, so a host missing the prerequisites fails closed here rather than
// silently running unjailed.
//
// @arg opts The neutral backend options; Firecracker reads the microVM-specific fields plus the common limits/namespace.
// @return backend.Provisioner A configured Firecracker provisioner with its jailer prerequisites checked and egress pool provisioned.
// @error error if a missing image cannot be resolved, the jailer prerequisites are not met, guest assets cannot be shared with the fc-net group, or the egress pool cannot be provisioned.
//
// @testcase TestNewBackendConfiguresProvisioner builds a Firecracker backend and applies the options.
func newBackend(opts backend.Options) (backend.Provisioner, error) {
	p, payload, err := buildProvisioner(opts)
	if err != nil {
		return nil, err
	}
	// Fail closed on missing jailer prerequisites (binaries, root, /dev/kvm, UID/GID
	// range) — there is no direct-launch fallback. This resolves the firecracker and
	// jailer binaries to absolute paths as a side effect.
	if err := p.jailer.checkJailerPrereqs(p.netEnabled); err != nil {
		return nil, err
	}
	// Share the read-only guest assets with the fc-net group so every jailed VMM can
	// read the kernel/payload once they are hard-linked into its chroot.
	if err := ensureAssetsReadable(p.jailer.gid, p.kernelImage, payload); err != nil {
		return nil, fmt.Errorf("sharing firecracker guest assets with the jailer group: %w", err)
	}
	// Provision the egress TAP pool now, at startup, before the HTTP server serves
	// and any same-host browser connects — so creating a box never adds a host
	// interface mid-request (which a browser aborts with ERR_NETWORK_CHANGED). This
	// also surfaces a missing CAP_NET_ADMIN as a clear startup error rather than a
	// confusing per-create failure. A no-op when egress is disabled.
	if !opts.DisableEgress {
		log.Printf("firecracker: provisioning egress network (needs CAP_NET_ADMIN / root; use --disable-egress for a control-only spoke)...")
	}
	if err := p.EnsureNetwork(context.Background()); err != nil {
		return nil, fmt.Errorf("provisioning firecracker egress pool (need CAP_NET_ADMIN / root, or set disable_egress): %w", err)
	}
	return p, nil
}

// buildProvisioner constructs and configures a Firecracker Provisioner from neutral
// backend options, without touching the host (no prerequisite check, no networking).
// Any of the kernel, rootfs, or payload paths left empty are auto-resolved from the
// published OCI images in the registry, so a spoke can run with no image flags at
// all. The Docker-only fields in opts are ignored. It is the pure-wiring half of
// newBackend, split out so unit tests can exercise option plumbing off a real host.
//
// @arg opts The neutral backend options; Firecracker reads KernelImagePath, RootfsImagePath, PayloadImagePath, StateDir, DisableEgress, PoolSize, Limits, Namespace, RegistryAuths, and the Jailer* fields.
// @return *Provisioner The configured provisioner (host untouched).
// @return string The resolved payload image path (shared read-only with the fc-net group by the caller), or empty.
// @error error if a missing image cannot be resolved or the provisioner cannot be constructed.
//
// @testcase TestNewBackendConfiguresProvisioner builds a Firecracker backend and applies the options.
func buildProvisioner(opts backend.Options) (*Provisioner, string, error) {
	kernel, rootfs, payload := opts.KernelImagePath, opts.RootfsImagePath, opts.PayloadImagePath
	if kernel == "" || rootfs == "" || payload == "" {
		r := newAssetResolver(assetCacheDir(opts.StateDir), opts.RegistryAuths)
		var err error
		if kernel, rootfs, payload, err = r.resolveImages(context.Background(), kernel, rootfs, payload); err != nil {
			return nil, "", fmt.Errorf("resolving firecracker guest images from %s (set --kernel/--rootfs/--payload to use local files): %w", r.registry, err)
		}
	}
	p, err := NewProvisioner(kernel, rootfs, opts.StateDir, opts.BoxPorts)
	if err != nil {
		return nil, "", err
	}
	p.SetPayloadImage(payload)
	p.SetPerBoxLimits(opts.Limits)
	p.SetNamespace(opts.Namespace)
	p.SetNetworking(!opts.DisableEgress)
	p.SetPoolSize(opts.PoolSize)
	// Jailer knobs (all optional; empty/zero keeps the safe defaults).
	p.SetJailerBinary(opts.JailerBinary)
	p.SetFirecrackerBinary(opts.FirecrackerBinary)
	p.SetChrootBase(opts.ChrootBase)
	p.SetUIDRange(opts.UIDMin, opts.UIDMax)
	p.SetTapGroup(opts.TapGroupGID)
	p.SetCgroupVersion(opts.CgroupVersion)
	return p, payload, nil
}
