# Firecracker box backend

llmbox can run each box as a [Firecracker](https://firecracker-microvm.github.io/)
microVM instead of a Docker container. The backend is selected by name and
implements the same `box.Provisioner` contract as Docker, so the manager, cluster,
server, and MCP layers are unchanged — a box is a box regardless of how it is
isolated.

## How it works

- **Compute**: each box is a microVM booting a shared guest kernel (`vmlinux`) and
  a per-box copy of a rootfs image. The rootfs `init` (PID 1) is the llmbox guest
  guest.
- **Control channel**: the host reaches the guest over the VM's **vsock** (guest
  `AF_VSOCK` port 5000), performing Firecracker's `CONNECT` handshake over the
  hypervisor's Unix socket. This is the microVM analogue of the Docker backend's
  bind-mounted control socket. All box behaviour — init-script provisioning, exec,
  and the port-proxy byte stream (dial) — runs through the guest over this
  channel, never through the provisioner.
- **Egress**: guest outbound traffic goes through a **TAP device** the host NATs
  (`iptables MASQUERADE`), with the guest's `eth0` configured statically via the
  kernel `ip=` boot arg. Each box gets its own `/30` (and an inter-guest `DROP`
  rule), so boxes cannot reach one another — only the host gateway. The guest is
  never in the egress path. The TAPs are a **pool provisioned once at startup**
  (`--pool-size`, default 16), not created per box: creating or destroying a box
  assigns/frees a slot without touching the host network. That matters when a
  browser runs on the **same host** as the hypervisor — a network interface
  appearing mid-request makes Chrome abort in-flight requests with
  `ERR_NETWORK_CHANGED`, so the interface set is kept stable across a box's
  lifetime. Egress needs `CAP_NET_ADMIN`; it can be disabled for control-only /
  air-gapped boxes (`--disable-egress`), which also removes the privilege
  requirement.
- **State**: Firecracker has no daemon that tracks boxes, so the provisioner
  persists each box's metadata under a state directory and holds live machine
  handles in memory; `List`/`Find`/`Destroy`/reap consult that state. Like the
  Docker backend it can be pinned to a namespace so two spokes sharing a host never
  see each other's boxes.

## Configuration

The backend is chosen by the spoke subcommand — the hub holds no box-provisioning
config, so everything here is a `llmbox-spoke firecracker` flag. Point it at the
guest kernel and default rootfs (leave them empty to pull the published images —
see below):

```
llmbox-spoke firecracker \
  --hub wss://hub.example.com/spoke/connect --token <join-token> \
  --kernel /var/lib/llmbox/vmlinux \
  --rootfs /var/lib/llmbox/rootfs.ext4 \
  --state-dir /run/llmbox/firecracker
```

Per-box CPU/memory caps (`--box-cpus`, `--box-memory-mb`) map onto the VM's vCPU
count (rounded to a Firecracker-valid 1-or-even value) and guest memory size.

## Disk size (resizable rootfs)

The base rootfs is shipped small — sized to its content, not to a fixed capacity —
and each box's writable disk is grown from it at create time. The provisioner
copies the base, `ftruncate`s the copy up to the requested size (a sparse grow, so
the empty space costs no host blocks until written), and boots the VM; a one-shot
`resize2fs` unit in the guest then grows the ext4 online to fill the larger
`/dev/vda`. This avoids downloading and storing multi-GiB images that are mostly
empty space.

The size is chosen per box, bounded by the spoke:

- `--box-disk-gb` (default 10) — the writable-disk size in GiB a box gets when the
  create request names none. `0` keeps the base image size (no grow).
- `--box-max-disk-gb` (default 100) — the hard ceiling on a per-create disk
  request, bounding what the by-design-unauthenticated create path can ask for.

A caller sets the per-box size via the `disk_gb` argument to the `create_llmbox`
MCP tool; it is clamped to `[base image size, --box-max-disk-gb]`. The disk cannot
be shrunk below the base image. (The Docker backend has no per-box block device, so
these knobs are Firecracker-only.)

## Host requirements

- Linux with KVM (`/dev/kvm` readable/writable by the running user).
- The `firecracker` binary on `PATH` (see `scripts/firecracker/fetch-firecracker.sh`).
- A Firecracker-compatible guest kernel with `virtio-vsock`, `virtio-net`,
  `virtio-blk`, `ext4`, `devtmpfs`, and `devpts` built in. The Firecracker CI
  kernels have all of these.
- `CAP_NET_ADMIN` (typically root) and `net.ipv4.ip_forward=1` **only** when egress
  networking is enabled.

## Zero-build spoke (images resolved from the registry)

You do not have to build or host any of the guest images. If `--kernel`,
`--rootfs`, or `--payload` is left empty, the spoke pulls the published image
from the registry on startup and caches it locally:

```sh
llmbox-spoke firecracker --hub wss://hub.example.com/spoke/connect --token <join-token>
#   kernel  <- ghcr.io/clems4ever/llmbox-fc-kernel:latest
#   rootfs  <- ghcr.io/clems4ever/llmbox-fc-base:latest
#   payload <- ghcr.io/clems4ever/llmbox-fc-payload:latest
```

The images are stored as opaque OCI artifacts (pulled with an embedded oras
client, not `docker pull` — they are raw files Firecracker boots, not runnable
images) and published by `.github/workflows/firecracker-assets.yml`. The base
rootfs is published **zstd-compressed** (it is a small, mostly-empty ext4 — see
"Disk size" above — so it compresses roughly 30×, e.g. ~190 MiB pushed vs a
~1.4 GiB image); the spoke verifies the compressed layer's digest and decompresses
it to the ext4 it boots on pull. The kernel and payload are small and pushed raw.
Overrides:

- `LLMBOX_FC_REGISTRY` — the OCI namespace to pull from (default
  `ghcr.io/clems4ever`); point it at a fork's packages.
- `LLMBOX_FC_TAG` — the tag to pull (default `latest`); pin a specific build.
- `LLMBOX_FC_ASSET_CACHE` — where pulled images are cached. The base rootfs is
  multi-GiB and the cache must survive reboots, so it never uses the tmpfs run-dir.
  The default is the user cache dir; a `--state-dir` you set is used instead (its
  `assets/` subdir), so pointing `--state-dir` at a disk moves the images too; and
  a root service with no `$HOME` falls back to `/var/lib/llmbox/firecracker/assets`.
  Running as a **systemd service, set `--state-dir /var/lib/llmbox/firecracker`** so
  both the images and per-box state stay on disk (the generated setup script does
  this for you).

Pulls are anonymous for public packages; a registry credential configured for the
host (see the registry section) is used automatically when present. Supplying any
path flag uses that local file instead of pulling. Note the payload is pulled only
when the rootfs is also auto-resolved — if you pass a custom all-in-one
`--rootfs` (guest baked in) and no `--payload`, no payload is attached.

## Building a rootfs

If you would rather build the images yourself (e.g. to customise them), the
scripts below produce exactly what the registry publishes. The rootfs must boot
with the llmbox guest as its init, listening on vsock. Its `init` mounts `/proc`,
`/sys`, `/dev` (devtmpfs) and `/dev/pts` (devpts, for the guest's PTY), brings up
`lo`, then `exec`s `llmbox-guest --vsock-port 5000`.

### Full Debian server (recommended for real use)

For a box that behaves like a real machine — systemd, Docker, services, whatever
the box's workload needs — use the full-server pair:

- **`scripts/firecracker/build-kernel.sh`** builds a Firecracker guest kernel
  (`vmlinux-full`) with the container-runtime + systemd features the minimal CI
  kernel lacks: overlayfs, bridge/veth, netfilter (iptables + nftables + NAT +
  conntrack), `br_netfilter`, all cgroup controllers, autofs, tun, bpf. It compiles
  in an `ubuntu:22.04` container (~15 min).
The Debian box is split into two images by how often they change, so a guest
update never rebuilds the multi-GiB OS:

- **`scripts/firecracker/build-base-rootfs.sh`** builds a generic, **guest-agnostic**
  Debian bookworm **base** (`base-rootfs.ext4`) with **systemd as PID 1**, Docker,
  Node, and net tooling. It contains **nothing** about llmbox or any
  particular guest — only a generic *payload loader* that at boot mounts the payload
  drive (`/dev/vdb`) read-only at `/payload` and runs `/payload/entrypoint`. Because
  it is guest-agnostic it is slow-changing and cacheable: in CI it is built once and
  cached in GHCR keyed on its inputs (`.github/workflows/firecracker-assets.yml`);
  `make firecracker-debian-assets` pulls the cached base before building locally.
  It also provisions a generic unprivileged **`agent`** account (home `/home/agent`,
  passwordless `sudo`, member of the `docker` group) that box workloads run as.
- **`scripts/firecracker/build-payload-drive.sh`** builds the tiny read-only
  **payload** (`payload.ext4`) carrying **everything llmbox-specific**: the static
  guest and an `entrypoint` that execs the guest on vsock. (llmbox itself runs no
  workload — the box's workload is installed and started by the spoke's
  [init script](hub-and-spoke.md#customising-boxes-with-an-init-script).) This half
  is cheap and rebuilt on every guest change, and is attached to every box as a
  shared read-only second drive. The guest runs the init script (and `Exec`) as
  the base's unprivileged **`agent`** user — while the guest itself stays root to
  serve the control channel; `agent` escalates with passwordless `sudo` when a
  command genuinely needs it.
- **`scripts/firecracker/build-debian-rootfs.sh`** is a convenience wrapper that
  builds both.

The base/payload contract is deliberately generic — *mount the payload drive, run
its `entrypoint`* — so a different guest (not llmbox) is just a different payload;
the base never changes. Because the payload is identical for every llmbox box it is
attached read-only and **shared** across all microVMs — one image, mounted
everywhere — while each box still gets its own writable copy of the base rootfs.
The base's systemd does all the mounts and reaps the payload's children.

```
llmbox-spoke firecracker --hub … --token … \
  --kernel  ~/fc-assets/vmlinux-full \
  --rootfs  ~/fc-assets/base-rootfs.ext4 \  # the Debian base (no guest)
  --payload ~/fc-assets/payload.ext4 \      # the guest, shared read-only
  --disable-egress=false                    # egress on; run the spoke as root
```

Leave `--payload` empty to fall back to the all-in-one layout (guest baked into
the rootfs).

Verified on boot: systemd reaches its targets, the guest unit is reachable over
vsock, `/tmp` is `1777`, and the Docker daemon starts with `overlay2` + cgroup v2
and no missing-feature warnings — so `docker run` works (image pulls need egress).

### Minimal rootfs (guest-as-init, no systemd)

For the conformance test, a simpler script builds a busybox/container-fs rootfs
with the guest as PID 1 (no systemd, no Docker):

- **`scripts/firecracker/build-conformance-rootfs.sh`** — a minimal BusyBox rootfs
  with the guest as init. Used only by the conformance test; it proves the box
  plumbing (init, exec, dial, lifecycle) but is not a real workload host.

For real workloads use the full Debian server pair above. A workload that reaches
the internet (an API, a package registry, an image pull) needs egress enabled (the
default, i.e. no `--disable-egress`), which means running the spoke with
`CAP_NET_ADMIN` (root). With egress disabled (control-only) a box boots and its
guest is reachable, but the box has no outbound network.

## Running the conformance suite

The Firecracker backend passes the same behavioural contract as Docker and the
in-process fake (`internal/spoke/box/conformance`). Build the test kernel + rootfs and
run it:

```sh
scripts/firecracker/fetch-firecracker.sh          # firecracker -> ~/.local/bin
scripts/firecracker/build-conformance-rootfs.sh   # vmlinux + rootfs.ext4 -> ~/fc-assets

export PATH="$HOME/.local/bin:$PATH"
LLMBOX_FC_KERNEL=$HOME/fc-assets/vmlinux \
LLMBOX_FC_ROOTFS=$HOME/fc-assets/rootfs.ext4 \
  go test ./internal/spoke/firecracker/ -run TestConformanceFirecracker -v
```

The test skips cleanly when the env vars, the firecracker binary, or `/dev/kvm`
are absent, so a normal `go test` is unaffected. When run as a non-root user it
boots **control-only** boxes (loopback + vsock, no TAP/NAT); the conformance
contract needs no network, so the full box lifecycle is exercised either way. The
real TAP/NAT egress path is covered by the root-only
`TestHostEgressSetupTeardownSkipsWithoutTools`.
