// Package cloudhypervisor runs each box as a Cloud Hypervisor microVM, a sibling
// backend to internal/spoke/firecracker built on the same rust-vmm foundation. It
// exists for one capability Firecracker deliberately lacks: a PCI bus with VFIO
// passthrough, so a box can be handed a real GPU (or a MIG slice) for hardware-
// isolated GPU compute — see backend.Options.GPUPassthrough.
//
// Like the Firecracker backend it self-registers through internal/spoke/box/backend
// (as "cloud-hypervisor"), persists per-box state under a state dir (Cloud
// Hypervisor has no daemon that remembers boxes), and reaches each box's guest over
// the same hybrid-vsock control channel (the "CONNECT <port>" handshake in vsock.go).
// The one backend-specific piece is how a VM is launched and controlled: Cloud
// Hypervisor is driven through its REST API on a per-VM Unix API socket (client.go),
// configured from a VmConfig translated out of a neutral box spec (vmconfig.go).
//
// The VM launch/control is hidden behind the launcher seam (launcher.go) so the
// whole provisioner lifecycle — Provision/List/Find/Pause/Resume/Destroy — is
// exercised in CI against a fake launcher (a real in-process guest, no KVM), the
// same backend-neutral conformance contract Docker and Firecracker pass. The real
// launcher (chlauncher.go) is covered by a KVM-gated integration test.
//
// Phase 1 is control-only networking (loopback + vsock, with the box's ports
// reachable through the HTTP proxy over vsock); spoke-managed TAP/NAT egress is a
// follow-up that will share the Firecracker backend's network pool.
package cloudhypervisor
