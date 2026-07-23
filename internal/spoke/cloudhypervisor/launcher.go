package cloudhypervisor

import (
	"context"
	"net"
)

// vmSpec is everything a launcher needs to boot one box's microVM. The provisioner
// builds it from a box's persisted meta plus the spoke's kernel/rootfs and per-box
// limits, so the launcher owns only the VMM mechanics (copy rootfs, write VmConfig,
// start cloud-hypervisor, boot, wait for the guest) and nothing about box identity
// or bookkeeping.
type vmSpec struct {
	// Token is the box's generation token, used for VMM/log identification.
	Token string
	// BoxDir is the box's state directory; the launcher writes the per-box rootfs
	// and its sockets under it.
	BoxDir string
	// Kernel is the host path to the guest kernel image.
	Kernel string
	// RootfsSrc is the host path to the base rootfs image to copy for this box.
	RootfsSrc string
	// APISock is the Cloud Hypervisor REST API Unix-socket path to launch the VMM on.
	APISock string
	// VsockUDS is the vsock Unix-socket path for the guest control channel.
	VsockUDS string
	// DiskBytes is the resolved size to grow the per-box rootfs copy to.
	DiskBytes int64
	// VCPUs is the guest vCPU count.
	VCPUs int64
	// MemoryBytes is the guest RAM size in bytes.
	MemoryBytes int64
	// GPUs holds the PCI addresses to pass through to the box by VFIO.
	GPUs []string
}

// vmHandle is a live handle to one running box VMM. It is deliberately tiny: once a
// VM is booted the provisioner reaches its guest over the control channel (Dial), so
// the only thing left to do with a handle is stop it.
type vmHandle interface {
	// Stop stops the box's VMM (best-effort), releasing its compute. The box's
	// rootfs and state directory are the provisioner's to keep or remove.
	Stop() error
}

// launcher is the VM launch/control seam that decouples the provisioner's box
// lifecycle bookkeeping from the concrete VMM. The real implementation
// (chLauncher) drives Cloud Hypervisor over its REST API; tests substitute a fake
// that runs a real in-process guest, so the whole Provision/Pause/Resume/Destroy
// contract is exercised in CI without KVM. It mirrors the Firecracker backend's
// machineFactory seam.
type launcher interface {
	// Launch boots a box's VM from spec and returns a handle once the guest is
	// reachable over its control channel. On any failure it must leave no VMM
	// running.
	Launch(ctx context.Context, spec vmSpec) (vmHandle, error)
	// Dial opens a control connection to the box's guest over its vsock UDS. It works
	// for a box with a live handle and for a rehydrated box whose orphaned VMM
	// survived a spoke restart.
	Dial(ctx context.Context, vsockUDS string) (net.Conn, error)
	// Alive reports whether an orphaned VMM (no live handle) still answers on its API
	// socket, so rehydrate can tell a running box from a dead one.
	Alive(apiSock string) bool
	// Halt best-effort stops an orphaned VMM by its API socket, so a box's rootfs is
	// never removed under a still-running VM.
	Halt(apiSock string) error
}
