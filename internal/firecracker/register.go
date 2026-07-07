package firecracker

import (
	"context"
	"fmt"

	"github.com/clems4ever/llmbox/internal/box/backend"
)

// init registers the Firecracker backend so importing this package makes
// "firecracker" selectable through backend.New.
//
// @testcase TestFirecrackerBackendRegistered checks importing this package registers the firecracker backend.
func init() {
	backend.Register("firecracker", newBackend)
}

// newBackend builds a Firecracker Provisioner from neutral backend options,
// reading the microVM-specific fields (kernel, rootfs, payload, state dir) and the
// common limits/namespace. Any of the kernel, rootfs, or payload paths left empty
// are auto-resolved from the published OCI images in the registry, so a spoke can
// run with no image flags at all. The Docker-only fields in opts are ignored.
//
// @arg opts The neutral backend options; Firecracker reads KernelImagePath, RootfsImagePath, PayloadImagePath, StateDir, DisableEgress, PoolSize, Limits, Namespace, and RegistryAuths.
// @return backend.Provisioner A configured Firecracker provisioner with its egress pool provisioned.
// @error error if a missing image cannot be resolved, the provisioner cannot be constructed, or the egress pool cannot be provisioned.
//
// @testcase TestNewBackendConfiguresProvisioner builds a Firecracker backend and applies the options.
func newBackend(opts backend.Options) (backend.Provisioner, error) {
	kernel, rootfs, payload := opts.KernelImagePath, opts.RootfsImagePath, opts.PayloadImagePath
	if kernel == "" || rootfs == "" || payload == "" {
		r := newAssetResolver(assetCacheDir(), opts.RegistryAuths)
		var err error
		if kernel, rootfs, payload, err = r.resolveImages(context.Background(), kernel, rootfs, payload); err != nil {
			return nil, fmt.Errorf("resolving firecracker guest images from %s (set --fc-kernel/--fc-rootfs/--fc-payload to use local files): %w", r.registry, err)
		}
	}
	p, err := NewProvisioner(kernel, rootfs, opts.StateDir)
	if err != nil {
		return nil, err
	}
	p.SetPayloadImage(payload)
	p.SetPerBoxLimits(opts.Limits)
	p.SetNamespace(opts.Namespace)
	p.SetNetworking(!opts.DisableEgress)
	p.SetPoolSize(opts.PoolSize)
	// Provision the egress TAP pool now, at startup, before the HTTP server serves
	// and any same-host browser connects — so creating a box never adds a host
	// interface mid-request (which a browser aborts with ERR_NETWORK_CHANGED). This
	// also surfaces a missing CAP_NET_ADMIN as a clear startup error rather than a
	// confusing per-create failure. A no-op when egress is disabled.
	if err := p.EnsureNetwork(context.Background()); err != nil {
		return nil, fmt.Errorf("provisioning firecracker egress pool (need CAP_NET_ADMIN / root, or set disable_egress): %w", err)
	}
	return p, nil
}
