package cloudhypervisor

import (
	"fmt"

	"github.com/clems4ever/llmbox/internal/spoke/box/backend"
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
	return buildProvisioner(opts)
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
	p, err := NewProvisioner(opts.KernelImagePath, opts.RootfsImagePath, opts.StateDir)
	if err != nil {
		return nil, err
	}
	p.SetPerBoxLimits(opts.Limits)
	p.SetNamespace(opts.Namespace)
	p.SetGPUs(opts.GPUPassthrough)
	if opts.CloudHypervisorBinary != "" {
		p.SetCHBinary(opts.CloudHypervisorBinary)
	}
	return p, nil
}
