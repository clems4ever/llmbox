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
| `--gpu-passthrough` | Host PCI address of a **full GPU** to hand **every** box by VFIO, e.g. `0000:65:00.0`. Repeat or comma-separate for several. Malformed addresses fail the spoke at startup. Empty attaches none. |
| `--vgpu-mdev` | Mediated device (**vGPU / MIG-backed vGPU**) to hand every box by VFIO-mdev, each an mdev UUID or an absolute `/sys` path. Repeat or comma-separate. Empty attaches none. |
| `--cloud-hypervisor` | Path to the `cloud-hypervisor` binary; empty resolves it from `PATH`. |
| `--egress-mode` | Who owns the host TAP/NAT egress plumbing: `managed` (default; the spoke provisions it, needs CAP_NET_ADMIN/root), `external` (attach to a pre-provisioned pool), or `disabled` (control-only, no egress). |
| `--disable-egress` | Boot control-only boxes (no TAP/NAT egress); alias for `--egress-mode=disabled`. |
| `--pool-size` | Number of egress TAP devices provisioned at startup (caps concurrent networked boxes); 0 uses the default (16). |
| `--tap-group` | GID that owns the pooled TAP devices; 0 uses the default. |

## GPUs: full passthrough, MIG, and vGPU

GPU devices are machine-local, so whatever you pass is attached to **every** box this
spoke runs. Three sharing models:

- **Full GPU per box** — `--gpu-passthrough 0000:65:00.0`. One physical GPU → one box.
- **MIG (hardware partitions, A100/H100/H200)** — partition the GPU into MIG instances
  on the host, expose each as an NVIDIA vGPU mediated device, and pass one per box with
  `--vgpu-mdev <mdev-uuid>`. This is the clean way to get Firecracker-style density
  *with* hardware-isolated GPU slices.
- **vGPU (time-sliced)** — `--vgpu-mdev <mdev-uuid>` for a non-MIG vGPU profile (carries
  NVIDIA vGPU licensing).

Both `--gpu-passthrough` and `--vgpu-mdev` become VFIO devices in the guest's
`VmConfig`; the difference is only the sysfs path (`/sys/bus/pci/devices/<addr>/` vs
`/sys/bus/mdev/devices/<uuid>`).

## Egress networking

By default (`--egress-mode=managed`) each box gets a TAP-backed egress NIC with NAT to
the host uplink and inter-box isolation, so boxes reach the internet but not each
other. This reuses the same shared microVM network layer
(`internal/spoke/microvm/mvmnet`) as the Firecracker backend — same pooled-TAP design,
same NAT/isolation rules, same static guest `ip=` configuration — but on its own
`llmboxch` / `172.17.0.0/16` addressing so the two backends never collide on one host.

- **`managed`** (default): the spoke provisions the TAP pool + NAT rules at startup
  (needs CAP_NET_ADMIN / root).
- **`external`**: an out-of-band provisioner owns the pool; the spoke only validates it
  read-only and never mutates host networking.
- **`disabled`** (`--disable-egress`): control-only boxes — loopback + the vsock control
  channel only. Box ports are still reachable through the [HTTP proxy](proxy.md), which
  is enough for GPU inference/compute boxes that need no outbound network.

## How it works

- **Launch/control.** Each box is a `cloud-hypervisor` process started with
  `--api-socket`; the backend drives it over the REST API (`vm.create` → `vm.boot`,
  and `vm.pause`/`vm.resume`/`vmm.shutdown` for the box lifecycle). The VMM runs in
  its own process group so a signal to the spoke does not stop running boxes — a box
  survives a spoke restart and is re-adopted (rehydrated) from persisted state.
- **Control channel.** The guest is reached over the same hybrid-vsock `CONNECT`
  handshake the Firecracker backend uses, so the **same guest rootfs boots on either
  VMM**.
- **GPU.** Each `--gpu-passthrough` address and `--vgpu-mdev` ref becomes a VFIO device
  in the box's `VmConfig`. The guest kernel command line keeps PCI **on** (unlike
  Firecracker's `pci=off`) so the device is visible.
- **Egress.** A networked box gets a virtio-net device on a pooled host TAP with a
  deterministic MAC and a static guest IP via the kernel `ip=` arg, backed by the
  shared `mvmnet` NAT/isolation pool (see above).
- **State.** Cloud Hypervisor has no daemon that remembers boxes, so the backend
  persists per-box metadata under the state dir and reloads it on startup, exactly
  like the Firecracker backend.

## Status & testing

- The live boot path is validated by a KVM-gated conformance test
  (`LLMBOX_CH_KERNEL` / `LLMBOX_CH_ROOTFS`, `cloud-hypervisor` on `PATH`, `/dev/kvm`,
  root). In CI the whole provisioner lifecycle is covered against a fake launcher
  running a real guest — the same backend-neutral contract Docker and Firecracker
  pass — so behaviour is proven without a GPU host.
- Managed-mode egress provisioning (TAP/NAT via `ip`/`iptables`) needs root and is
  exercised by the shared `mvmnet` package's root-gated tests; the mode selection,
  slot allocation, VM-config translation (GPU/mdev/net), and option plumbing are all
  covered by unit tests that run unprivileged in CI.
