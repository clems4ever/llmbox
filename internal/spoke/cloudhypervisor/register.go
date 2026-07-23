package cloudhypervisor

import (
	"context"
	"fmt"

	"github.com/clems4ever/llmbox/internal/spoke/box/backend"
	"github.com/clems4ever/llmbox/internal/spoke/microvm/mvmnet"
)

// init registers the Cloud Hypervisor backend so importing this package makes
// "cloud-hypervisor" selectable through backend.New.
//
// @testcase TestCloudHypervisorBackendRegistered checks importing this package registers the backend.
func init() {
	backend.Register("cloud-hypervisor", newBackend)
}

// newBackend builds a Cloud Hypervisor Provisioner from neutral backend options,
// reading the microVM-specific fields (kernel, rootfs, state dir) plus the
// Cloud-Hypervisor-specific GPU passthrough list and binary path. The Docker- and
// Firecracker-only fields are ignored.
//
// @arg opts The neutral backend options.
// @return backend.Provisioner A configured Cloud Hypervisor provisioner.
// @error error if a GPU passthrough address is malformed or the provisioner cannot be constructed.
//
// @testcase TestNewBackendConfiguresProvisioner builds a Cloud Hypervisor backend and applies the options.
// @testcase TestNewBackendRejectsBadGPUAddress errors when a GPU passthrough address is malformed.
func newBackend(opts backend.Options) (backend.Provisioner, error) {
	// Preflight: validate the host can actually run microVM boxes (cloud-hypervisor
	// binary, /dev/kvm, CPU virtualization, readable kernel/rootfs, egress privileges/
	// tools, IOMMU for GPU passthrough) before doing anything, so a misconfigured host
	// fails fast at spoke startup with one actionable message instead of a confusing
	// error on the first box create.
	mode, err := resolveEgressMode(opts)
	if err != nil {
		return nil, err
	}
	if err := realProbes().validate(preflightConfigFrom(opts, mode)); err != nil {
		return nil, err
	}

	p, err := buildProvisioner(opts)
	if err != nil {
		return nil, err
	}
	// Ready the egress TAP pool now, at startup, before the HTTP server serves — so
	// creating a box never adds a host interface mid-request, and a missing
	// CAP_NET_ADMIN (managed) or an unprovisioned pool (external) surfaces as a clear
	// startup error. A no-op in disabled (control-only) mode.
	if err := p.EnsureNetwork(context.Background()); err != nil {
		return nil, fmt.Errorf("provisioning cloud-hypervisor egress pool (need CAP_NET_ADMIN / root, or set --egress-mode=external / --disable-egress): %w", err)
	}
	return p, nil
}

// buildProvisioner is the pure-wiring half of newBackend: it constructs and
// configures a Provisioner from opts without any behavioural difference, so unit
// tests can assert the option plumbing against the concrete type. GPU passthrough
// addresses are validated here so a typo fails the spoke at startup rather than
// producing a VmConfig Cloud Hypervisor rejects at boot.
//
// @arg opts The neutral backend options.
// @return *Provisioner The configured provisioner.
// @error error if a GPU passthrough address is malformed or the provisioner cannot be constructed.
//
// @testcase TestNewBackendConfiguresProvisioner builds a Cloud Hypervisor backend and applies the options.
// @testcase TestNewBackendRejectsBadGPUAddress errors when a GPU passthrough address is malformed.
func buildProvisioner(opts backend.Options) (*Provisioner, error) {
	for _, addr := range opts.GPUPassthrough {
		if !validatePCIAddress(addr) {
			return nil, fmt.Errorf("invalid GPU passthrough PCI address %q (want domain:bus:device.function, e.g. 0000:65:00.0)", addr)
		}
	}
	for _, m := range opts.GPUMediatedDevices {
		if !validateMediatedDevice(m) {
			return nil, fmt.Errorf("invalid vGPU mediated device %q (want an mdev UUID or an absolute /sys path)", m)
		}
	}
	mode, err := resolveEgressMode(opts)
	if err != nil {
		return nil, err
	}
	p, err := NewProvisioner(opts.KernelImagePath, opts.RootfsImagePath, opts.StateDir)
	if err != nil {
		return nil, err
	}
	p.SetPerBoxLimits(opts.Limits)
	p.SetNamespace(opts.Namespace)
	p.SetGPUs(opts.GPUPassthrough)
	p.SetMDEVs(opts.GPUMediatedDevices)
	p.SetEgressMode(mode)
	p.SetPoolSize(opts.PoolSize)
	p.SetTapGroup(opts.TapGroupGID)
	if opts.CloudHypervisorBinary != "" {
		p.SetCHBinary(opts.CloudHypervisorBinary)
	}
	return p, nil
}

// resolveEgressMode maps the neutral egress options to a mode: EgressMode wins when
// set, otherwise the legacy DisableEgress boolean picks disabled vs. managed. It
// rejects the contradictory combination of a non-disabled --egress-mode with
// --disable-egress so a mistake fails fast. It mirrors the Firecracker backend's
// resolution so the two behave identically.
//
// @arg opts The neutral backend options (EgressMode and DisableEgress).
// @return egressMode The resolved mode.
// @error error if EgressMode is unknown, or contradicts DisableEgress.
//
// @testcase TestResolveEgressMode maps the flags and rejects a contradiction.
func resolveEgressMode(opts backend.Options) (egressMode, error) {
	if opts.EgressMode == "" {
		if opts.DisableEgress {
			return egressDisabled, nil
		}
		return egressManaged, nil
	}
	mode, err := mvmnet.ParseEgressMode(opts.EgressMode)
	if err != nil {
		return egressManaged, err
	}
	if opts.DisableEgress && mode != egressDisabled {
		return egressManaged, fmt.Errorf("--disable-egress conflicts with --egress-mode=%s (use --egress-mode=disabled instead)", mode)
	}
	return mode, nil
}
