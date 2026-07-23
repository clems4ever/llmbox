package cloudhypervisor

import (
	"fmt"
	"regexp"
)

// defaultKernelCmdline is the guest kernel command line every box boots with. It
// deliberately differs from the Firecracker backend's args in one way: it does NOT
// pass "pci=off", because Cloud Hypervisor's whole reason for existing here is the
// PCI bus that carries VFIO-passed-through GPUs — turning PCI off would hide them.
// root=/dev/vda points the kernel at the first virtio-block disk (the rootfs);
// init=/init lets the guest rootfs run its own entrypoint, and net.ifnames=0 keeps
// a predictable NIC name for the (future) egress interface.
const defaultKernelCmdline = "console=ttyS0 reboot=k panic=1 net.ifnames=0 root=/dev/vda rw init=/init"

// guestCID is the guest's AF_VSOCK context id. It is fixed per box (each box has its
// own vsock UDS) and matches the Firecracker backend's guest CID so the same guest
// rootfs works on either VMM.
const guestCID = 3

// pciAddressRE matches a PCI address in the "domain:bus:device.function" form Cloud
// Hypervisor / the kernel use in sysfs (e.g. "0000:65:00.0"). Addresses are
// validated at option-parse time so a typo fails the spoke at startup rather than
// producing a VmConfig Cloud Hypervisor rejects at boot.
var pciAddressRE = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)

// validatePCIAddress reports whether addr is a well-formed PCI address usable for
// VFIO passthrough.
//
// @arg addr The candidate PCI address, e.g. "0000:65:00.0".
// @return bool True when addr is a valid domain:bus:device.function address.
//
// @testcase TestValidatePCIAddress accepts well-formed addresses and rejects malformed ones.
func validatePCIAddress(addr string) bool { return pciAddressRE.MatchString(addr) }

// pciDeviceSysfsPath returns the sysfs directory Cloud Hypervisor expects for a VFIO
// passthrough device (its "--device path=" form) for a PCI address.
//
// @arg addr The device's PCI address, e.g. "0000:65:00.0".
// @return string The sysfs device path, e.g. "/sys/bus/pci/devices/0000:65:00.0/".
//
// @testcase TestBuildVMConfigGPUPassthrough maps GPU addresses to sysfs device paths.
func pciDeviceSysfsPath(addr string) string {
	return fmt.Sprintf("/sys/bus/pci/devices/%s/", addr)
}

// vmConfig is the subset of Cloud Hypervisor's VmConfig REST schema the backend
// sets. It is marshalled to JSON and PUT to the VMM's /api/v1/vm.create endpoint.
type vmConfig struct {
	CPUs    vmCPUs    `json:"cpus"`
	Memory  vmMemory  `json:"memory"`
	Payload vmPayload `json:"payload"`
	Disks   []vmDisk  `json:"disks"`
	Vsock   vmVsock   `json:"vsock"`
	Serial  vmConsole `json:"serial"`
	Console vmConsole `json:"console"`
	// Devices carries VFIO PCI passthrough entries — this is how a GPU reaches the
	// guest. Omitted when the box has none.
	Devices []vmDevice `json:"devices,omitempty"`
}

// vmCPUs sets the boot and maximum vCPU counts.
type vmCPUs struct {
	BootVCPUs int64 `json:"boot_vcpus"`
	MaxVCPUs  int64 `json:"max_vcpus"`
}

// vmMemory sets the guest RAM size in bytes.
type vmMemory struct {
	Size int64 `json:"size"`
}

// vmPayload names the guest kernel and its command line.
type vmPayload struct {
	Kernel  string `json:"kernel"`
	Cmdline string `json:"cmdline"`
}

// vmDisk is one virtio-block device backed by a host file.
type vmDisk struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

// vmVsock is the virtio-vsock device carrying the guest control channel, bound to a
// host Unix socket.
type vmVsock struct {
	CID    int64  `json:"cid"`
	Socket string `json:"socket"`
}

// vmDevice is a VFIO PCI passthrough device, named by its sysfs path.
type vmDevice struct {
	Path string `json:"path"`
}

// vmConsole configures a serial/console port ("Off", "Tty", "Null", ...).
type vmConsole struct {
	Mode string `json:"mode"`
}

// vmConfigParams are the resolved, backend-neutral inputs buildVMConfig turns into a
// Cloud Hypervisor VmConfig. The provisioner fills these from a box's spec and the
// spoke's limits; keeping buildVMConfig pure over this struct makes the whole
// translation — including GPU passthrough — unit-testable without a host or a VMM.
type vmConfigParams struct {
	// Kernel is the host path to the guest kernel image.
	Kernel string
	// Cmdline is the guest kernel command line; empty uses defaultKernelCmdline.
	Cmdline string
	// Rootfs is the host path to the box's writable rootfs image (becomes /dev/vda).
	Rootfs string
	// VsockUDS is the host Unix-socket path for the guest control channel.
	VsockUDS string
	// VCPUs is the guest vCPU count (boot and max).
	VCPUs int64
	// MemoryBytes is the guest RAM size in bytes.
	MemoryBytes int64
	// GPUs holds the PCI addresses to pass through by VFIO; each becomes a device.
	GPUs []string
}

// buildVMConfig translates neutral box parameters into a Cloud Hypervisor VmConfig,
// wiring the kernel/cmdline payload, the rootfs as the first virtio-block disk, the
// vsock control channel, a serial console for boot logs, and — the point of this
// backend — one VFIO passthrough device per requested GPU PCI address. It is pure:
// it reads only its input and allocates no host resources, so it is fully covered by
// unit tests.
//
// @arg p The resolved VM parameters (kernel, rootfs, vsock path, sizing, GPUs).
// @return vmConfig The VmConfig to PUT to the VMM, with a devices entry per GPU.
//
// @testcase TestBuildVMConfigBasics wires the kernel, disk, vsock, and sizing.
// @testcase TestBuildVMConfigGPUPassthrough emits one VFIO device per GPU address.
// @testcase TestBuildVMConfigNoGPUsOmitsDevices leaves devices unset when no GPU is requested.
func buildVMConfig(p vmConfigParams) vmConfig {
	cmdline := p.Cmdline
	if cmdline == "" {
		cmdline = defaultKernelCmdline
	}
	cfg := vmConfig{
		CPUs:    vmCPUs{BootVCPUs: p.VCPUs, MaxVCPUs: p.VCPUs},
		Memory:  vmMemory{Size: p.MemoryBytes},
		Payload: vmPayload{Kernel: p.Kernel, Cmdline: cmdline},
		Disks:   []vmDisk{{Path: p.Rootfs, Readonly: false}},
		Vsock:   vmVsock{CID: guestCID, Socket: p.VsockUDS},
		// The guest logs to serial (captured by the VMM); the virtio console is off.
		Serial:  vmConsole{Mode: "Tty"},
		Console: vmConsole{Mode: "Off"},
	}
	for _, addr := range p.GPUs {
		cfg.Devices = append(cfg.Devices, vmDevice{Path: pciDeviceSysfsPath(addr)})
	}
	return cfg
}
