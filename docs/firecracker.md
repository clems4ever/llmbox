# Firecracker box backend

llmbox can run each box as a [Firecracker](https://firecracker-microvm.github.io/)
microVM instead of a Docker container. The backend is selected by name and
implements the same `box.Provisioner` contract as Docker, so the manager, cluster,
server, and MCP layers are unchanged — a box is a box regardless of how it is
isolated.

## How it works

- **Compute**: each box is a microVM booting a shared guest kernel (`vmlinux`) and
  a per-box copy of a rootfs image. The rootfs `init` (PID 1) is the llmbox guest
  agent.
- **Control channel**: the host reaches the agent over the VM's **vsock** (guest
  `AF_VSOCK` port 5000), performing Firecracker's `CONNECT` handshake over the
  hypervisor's Unix socket. This is the microVM analogue of the Docker backend's
  bind-mounted control socket. All box behaviour — login, exec, logs, port proxy —
  runs through the agent over this channel, never through the provisioner.
- **Egress**: guest outbound traffic goes through a **TAP device** the host NATs
  (`iptables MASQUERADE`), with the guest's `eth0` configured statically via the
  kernel `ip=` boot arg. Each box gets its own `/30` (and an inter-guest `DROP`
  rule), so boxes cannot reach one another — only the host gateway. The agent is
  never in the egress path. The TAPs are a **pool provisioned once at startup**
  (`pool_size`, default 16), not created per box: creating or destroying a box
  assigns/frees a slot without touching the host network. That matters when a
  browser runs on the **same host** as the hypervisor — a network interface
  appearing mid-request makes Chrome abort in-flight requests with
  `ERR_NETWORK_CHANGED`, so the interface set is kept stable across a box's
  lifetime. Egress needs `CAP_NET_ADMIN`; it can be disabled for control-only /
  air-gapped boxes (`disable_egress`), which also removes the privilege
  requirement.
- **State**: Firecracker has no daemon that tracks boxes, so the provisioner
  persists each box's metadata under a state directory and holds live machine
  handles in memory; `List`/`Find`/`Destroy`/reap consult that state. Like the
  Docker backend it can be pinned to a namespace so two spokes sharing a host never
  see each other's boxes.

## Configuration

Select the backend and point it at the guest kernel and default rootfs.

**Hub (`config.yaml`):**

```yaml
backend: firecracker
firecracker:
  kernel_image: /var/lib/llmbox/vmlinux
  rootfs_image: /var/lib/llmbox/rootfs.ext4
  state_dir:    /run/llmbox/firecracker   # optional
```

**Spoke (flags):**

```
llmbox spoke --backend firecracker \
  --fc-kernel /var/lib/llmbox/vmlinux \
  --fc-rootfs /var/lib/llmbox/rootfs.ext4 \
  --fc-state-dir /run/llmbox/firecracker
```

Per-box CPU/memory caps (`box.cpus`, `box.memory_mb`) map onto the VM's vCPU count
(rounded to a Firecracker-valid 1-or-even value) and guest memory size.

## Host requirements

- Linux with KVM (`/dev/kvm` readable/writable by the running user).
- The `firecracker` binary on `PATH` (see `scripts/firecracker/fetch-firecracker.sh`).
- A Firecracker-compatible guest kernel with `virtio-vsock`, `virtio-net`,
  `virtio-blk`, `ext4`, `devtmpfs`, and `devpts` built in. The Firecracker CI
  kernels have all of these.
- `CAP_NET_ADMIN` (typically root) and `net.ipv4.ip_forward=1` **only** when egress
  networking is enabled.

## Zero-build spoke (images resolved from the registry)

You do not have to build or host any of the guest images. If `--fc-kernel`,
`--fc-rootfs`, or `--fc-payload` is left empty, the spoke pulls the published image
from the registry on startup and caches it locally:

```sh
llmbox spoke --hub wss://hub.example.com/spoke/connect --backend firecracker
#   kernel  <- ghcr.io/clems4ever/llmbox-fc-kernel:latest
#   rootfs  <- ghcr.io/clems4ever/llmbox-fc-base:latest
#   payload <- ghcr.io/clems4ever/llmbox-fc-payload:latest
```

The images are stored as opaque OCI artifacts (pulled with an embedded oras
client, not `docker pull` — they are raw files Firecracker boots, not runnable
images) and published by `.github/workflows/firecracker-assets.yml`. Overrides:

- `LLMBOX_FC_REGISTRY` — the OCI namespace to pull from (default
  `ghcr.io/clems4ever`); point it at a fork's packages.
- `LLMBOX_FC_TAG` — the tag to pull (default `latest`); pin a specific build.
- `LLMBOX_FC_ASSET_CACHE` — where pulled images are cached (default under the user
  cache dir; must survive reboots since the base rootfs is multi-GiB).

Pulls are anonymous for public packages; a registry credential configured for the
host (see the registry section) is used automatically when present. Supplying any
path flag uses that local file instead of pulling. Note the payload is pulled only
when the rootfs is also auto-resolved — if you pass a custom all-in-one
`--fc-rootfs` (agent baked in) and no `--fc-payload`, no payload is attached.

## Building a rootfs

If you would rather build the images yourself (e.g. to customise them), the
scripts below produce exactly what the registry publishes. The rootfs must boot
with the llmbox agent as its init, listening on vsock. Its `init` mounts `/proc`,
`/sys`, `/dev` (devtmpfs) and `/dev/pts` (devpts, for the agent's PTY), brings up
`lo`, then `exec`s `llmbox-agent --vsock-port 5000`.

### Full Debian server (recommended for real use)

For a box that behaves like a real machine — systemd, Docker, services, whatever
Claude wants to run — use the full-server pair:

- **`scripts/firecracker/build-kernel.sh`** builds a Firecracker guest kernel
  (`vmlinux-full`) with the container-runtime + systemd features the minimal CI
  kernel lacks: overlayfs, bridge/veth, netfilter (iptables + nftables + NAT +
  conntrack), `br_netfilter`, all cgroup controllers, autofs, tun, bpf. It compiles
  in an `ubuntu:22.04` container (~15 min).
The Debian box is split into two images by how often they change, so an agent
update never rebuilds the multi-GiB OS:

- **`scripts/firecracker/build-base-rootfs.sh`** builds a generic, **agent-agnostic**
  Debian bookworm **base** (`base-rootfs.ext4`) with **systemd as PID 1**, Docker,
  Node, and net tooling. It contains **nothing** about llmbox, `claude`, or any
  particular agent — only a generic *payload loader* that at boot mounts the payload
  drive (`/dev/vdb`) read-only at `/payload` and runs `/payload/entrypoint`. Because
  it is agent-agnostic it is slow-changing and cacheable: in CI it is built once and
  cached in GHCR keyed on its inputs (`.github/workflows/firecracker-assets.yml`);
  `make firecracker-debian-assets` pulls the cached base before building locally.
- **`scripts/firecracker/build-payload-drive.sh`** builds the tiny read-only
  **payload** (`payload.ext4`) carrying **everything llmbox-specific**: the static
  agent, the standalone `claude`, its trust seed, and an `entrypoint` that seeds a
  writable copy of the trust file and execs the agent on vsock. This half is cheap
  and rebuilt on every agent change, and is attached to every box as a shared
  read-only second drive.
- **`scripts/firecracker/build-debian-rootfs.sh`** is a convenience wrapper that
  builds both.

The base/payload contract is deliberately generic — *mount the payload drive, run
its `entrypoint`* — so a different agent (not llmbox) is just a different payload;
the base never changes. Because the payload is identical for every llmbox box it is
attached read-only and **shared** across all microVMs — one image, mounted
everywhere — while each box still gets its own writable copy of the base rootfs.
The base's systemd does all the mounts and reaps the payload's children.

```yaml
firecracker:
  kernel_image:  ~/fc-assets/vmlinux-full
  rootfs_image:  ~/fc-assets/base-rootfs.ext4   # the Debian base (no agent)
  payload_image: ~/fc-assets/payload.ext4       # agent + claude, shared read-only
  disable_egress: false          # egress on; run the server as root
```

On a spoke, the equivalents are `--fc-rootfs`, `--fc-payload`, and `--fc-kernel`.
Leave `payload_image` empty to fall back to the all-in-one layout (agent baked into
the rootfs).

Verified on boot: systemd reaches its targets, the agent unit is reachable over
vsock, `/tmp` is `1777`, and the Docker daemon starts with `overlay2` + cgroup v2
and no missing-feature warnings — so `docker run` works (image pulls need egress).

### Minimal rootfs (agent-as-init, no systemd)

For a lightweight box or the conformance test, two simpler scripts build a
busybox/container-fs rootfs with the agent as PID 1 (no systemd, no Docker):

- **`scripts/firecracker/build-box-rootfs.sh`** — a **production** rootfs from the
  `llmbox-box` container image (the same image the Docker backend runs), with the
  real `claude` baked in. It rebuilds the agent from the current source and
  overwrites the image's copy (the published image predates the vsock transport),
  and runs the agent under `tini` so Claude's child processes are reaped. Use this
  for real sessions.
- **`scripts/firecracker/build-conformance-rootfs.sh`** — a minimal BusyBox rootfs
  with a **mock** `claude` (prints fake auth/session URLs). Used only by the
  conformance test; it proves the plumbing but is not a real Claude.

The production rootfs's init must `cd /workspace` before exec'ing the agent: the
image's `~/.claude.json` trust seed marks `/workspace` trusted (the Docker backend
sets `WORKDIR=/workspace` for the same reason), so running Claude anywhere else
fails with "Workspace not trusted". `build-box-rootfs.sh` does this.

A real Claude session needs the production rootfs **and** egress enabled
(`disable_egress: false`), because the box must reach the Anthropic API and OAuth —
which means running the server/spoke with `CAP_NET_ADMIN` (root). With egress
disabled (control-only) a box boots and its agent is reachable, but `claude` cannot
authenticate.

## Running the conformance suite

The Firecracker backend passes the same behavioural contract as Docker and the
in-process fake (`internal/box/conformance`). Build the test kernel + rootfs and
run it:

```sh
scripts/firecracker/fetch-firecracker.sh          # firecracker -> ~/.local/bin
scripts/firecracker/build-conformance-rootfs.sh   # vmlinux + rootfs.ext4 -> ~/fc-assets

export PATH="$HOME/.local/bin:$PATH"
LLMBOX_FC_KERNEL=$HOME/fc-assets/vmlinux \
LLMBOX_FC_ROOTFS=$HOME/fc-assets/rootfs.ext4 \
  go test ./internal/firecracker/ -run TestConformanceFirecracker -v
```

The test skips cleanly when the env vars, the firecracker binary, or `/dev/kvm`
are absent, so a normal `go test` is unaffected. When run as a non-root user it
boots **control-only** boxes (loopback + vsock, no TAP/NAT); the mock `claude` the
conformance rootfs ships needs no network, so the full box lifecycle is exercised
either way. The real TAP/NAT egress path is covered by the root-only
`TestHostEgressSetupTeardownSkipsWithoutTools`.
