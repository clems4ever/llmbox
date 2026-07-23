# Cloud Hypervisor backend (GPU passthrough)

Run each box as a [Cloud Hypervisor](https://www.cloudhypervisor.org/) microVM
instead of a Docker container or a Firecracker microVM. Cloud Hypervisor is a
sibling of Firecracker — same `rust-vmm` foundation, fast boot, small footprint —
with one capability Firecracker deliberately drops: a **PCI bus with VFIO
passthrough**, so a box can be handed a **real GPU** (or a MIG slice) for
hardware-isolated GPU compute.

Use this backend when you want GPU compute inside a microVM-isolated box. If you do
not need a GPU, the [Firecracker backend](firecracker.md) is the leaner microVM
option and Docker is the simplest.

## Why not Firecracker or Docker for GPUs?

- **Firecracker** has no PCI bus and no VFIO passthrough by design (density + minimal
  attack surface), so GPU passthrough is impossible on stock Firecracker. Its
  `--box-gpus` flag does not exist; `backend.Options.GPUs` is Docker-only.
- **Docker `--gpus`** shares host devices into a container — it needs the full NVIDIA
  driver/toolkit stack on the host and is **not a VM**, so it gives none of the
  microVM isolation.
- **Cloud Hypervisor** keeps the PCI bus and supports VFIO, so a physical GPU is
  handed to the guest the same way it is under QEMU/KVM, but with a Firecracker-class
  footprint.

## Running a spoke on Cloud Hypervisor

The `cloud-hypervisor` spoke subcommand runs a spoke whose boxes are Cloud
Hypervisor microVMs. It needs the `cloud-hypervisor` binary on the host, `/dev/kvm`,
and (for GPU passthrough) the target GPUs bound to the `vfio-pci` driver with the
IOMMU enabled (`intel_iommu=on` / `amd_iommu=on` on the host kernel command line).

```bash
llmbox-spoke cloud-hypervisor \
  --hub wss://hub.example.com/spoke/connect \
  --token <join-token> \
  --kernel /var/lib/llmbox/vmlinux \
  --rootfs /var/lib/llmbox/rootfs.ext4 \
  --gpu-passthrough 0000:65:00.0 \
  --gpu-passthrough 0000:b3:00.0
```

### Flags

| Flag | Meaning |
|------|---------|
| `--kernel` | Host path to the guest kernel (vmlinux) every box boots. |
| `--rootfs` | Host path to the default guest rootfs every box boots. |
| `--state-dir` | Directory for per-box state (rootfs copy, sockets, metadata); empty uses `/run/llmbox/cloud-hypervisor`. |
| `--gpu-passthrough` | Host PCI address of a GPU (or MIG slice) to hand **every** box by VFIO, e.g. `0000:65:00.0`. Repeat or comma-separate for several. Malformed addresses fail the spoke at startup. Empty attaches none. |
| `--cloud-hypervisor` | Path to the `cloud-hypervisor` binary; empty resolves it from `PATH`. |

GPUs are machine-local, so `--gpu-passthrough` attaches the listed devices to every
box this spoke runs. To pack several isolated workloads onto one physical GPU, slice
it with **NVIDIA MIG** and pass one MIG instance per box.

## How it works

- **Launch/control.** Each box is a `cloud-hypervisor` process started with
  `--api-socket`; the backend drives it over the REST API (`vm.create` → `vm.boot`,
  and `vm.pause`/`vm.resume`/`vmm.shutdown` for the box lifecycle). The VMM runs in
  its own process group so a signal to the spoke does not stop running boxes — a box
  survives a spoke restart and is re-adopted (rehydrated) from persisted state.
- **Control channel.** The guest is reached over the same hybrid-vsock `CONNECT`
  handshake the Firecracker backend uses, so the **same guest rootfs boots on either
  VMM**.
- **GPU.** Each `--gpu-passthrough` address becomes a VFIO device
  (`/sys/bus/pci/devices/<addr>/`) in the box's `VmConfig`. The guest kernel command
  line keeps PCI **on** (unlike Firecracker's `pci=off`) so the device is visible.
- **State.** Cloud Hypervisor has no daemon that remembers boxes, so the backend
  persists per-box metadata under the state dir and reloads it on startup, exactly
  like the Firecracker backend.

## Status & limitations

- **Phase 1 is control-only networking**: boxes have loopback + the vsock control
  channel (and their ports are reachable through the [HTTP proxy](proxy.md)), which is
  enough for GPU inference/compute boxes. Spoke-managed TAP/NAT egress is a follow-up
  that will share the Firecracker backend's network pool.
- The live boot path is validated by a KVM-gated conformance test
  (`LLMBOX_CH_KERNEL` / `LLMBOX_CH_ROOTFS`, `cloud-hypervisor` on `PATH`, `/dev/kvm`,
  root). In CI the whole provisioner lifecycle is covered against a fake launcher
  running a real guest — the same backend-neutral contract Docker and Firecracker
  pass — so behaviour is proven without a GPU host.
