# Firecracker box backend

llmbox can run each box as a [Firecracker](https://firecracker-microvm.github.io/)
microVM instead of a Docker container. The backend is selected by name and
implements the same `box.Provisioner` contract as Docker, so the manager, cluster,
and server layers are unchanged — a box is a box regardless of how it is
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
  lifetime. Who owns this host-side plumbing is chosen with `--egress-mode`
  (see [Egress modes](#egress-modes)): the spoke can provision it itself
  (`managed`, the default, needs `CAP_NET_ADMIN`), attach to a pool an
  administrator provisioned out of band (`external`, no `CAP_NET_ADMIN` in the
  long-running spoke), or run control-only with no egress at all (`disabled`,
  the former `--disable-egress`).
- **State**: Firecracker has no daemon that tracks boxes, so the provisioner
  persists each box's metadata under a state directory and holds live machine
  handles in memory; `List`/`Find`/`Destroy`/reap consult that state. Like the
  Docker backend it can be pinned to a namespace so two spokes sharing a host never
  see each other's boxes.

## Security: every box is jailed

llmbox never launches `firecracker` directly. Every microVM is started through the
official **[jailer](https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md)**,
which wraps the VMM in a defense-in-depth boundary around an untrusted guest:

- **Per-VM chroot.** Each box gets its own chroot (`<chroot-base>/firecracker/<token>/root`).
  The VMM only sees the kernel, its own writable rootfs, and the shared read-only
  payload — hard-linked in — plus its own API/vsock sockets. One box cannot reach
  another box's disk, sockets, or the shared asset cache through host paths.
- **Unprivileged per-VM UID.** Each live box runs under a **unique UID** drawn from a
  configurable range (default `100000–165535`); its chroot, rootfs, and sockets are
  owned by that UID (mode `0700`/`0600`), so no other box's identity can read them.
  The UID is persisted and freed only when the box is destroyed, so restart recovery
  never reuses a UID while its box exists.
- **Shared network group, not shared files.** All jailed VMMs run under one shared
  **fc-net GID** (default `100000`) that owns the pooled TAP devices, so a jailed,
  unprivileged Firecracker can open its assigned TAP **without `CAP_NET_ADMIN`**.
  Filesystem isolation rides on the per-VM UID (files are owner-only), so sharing the
  group leaks no box state and a TAP slot can be reassigned to a different box without
  churning the interface.
- **cgroup + device isolation.** The jailer places each VMM in its own cgroup and
  builds its `/dev/{kvm,net/tun,urandom}` nodes inside the chroot. The VM's CPU/memory
  envelope is bounded by the Firecracker machine config (`--box-cpus`, `--box-memory-mb`).

**Jailing is mandatory — there is no flag to run unjailed.** If the prerequisites
are missing the spoke **fails closed at startup** with an actionable error; it never
falls back to a direct launch.

### Jailer prerequisites

- A **`jailer`** binary on `PATH`, version-matched to `firecracker`
  (`scripts/firecracker/fetch-firecracker.sh` installs both from one release).
- The spoke must run **as root** — the jailer must `chroot`, create device nodes,
  `chown` staged files, and drop to the per-VM UID.
- `/dev/kvm` present and usable.
- The **chroot base** must be on the **same filesystem** as the state directory (so
  the jailer can hard-link each box's rootfs into the chroot). The default
  `<state-dir>/chroot` satisfies this automatically.

### Jailer configuration

All optional — the defaults above are safe. Tune the jail with:

```
--jailer <path>          # jailer binary; empty resolves "jailer" from PATH
--firecracker <path>     # firecracker binary the jailer exec-s
--chroot-base <dir>      # chroot base; empty uses <state-dir>/chroot
--uid-min / --uid-max    # per-VM UID range; 0 uses the default
--tap-group <gid>        # shared fc-net GID that owns the pooled TAPs
--cgroup-version 1|2     # empty auto-detects
```

### Migration from a pre-jailer spoke

Boxes created by an older, direct-launch spoke carry no chroot in their metadata.
After upgrade they remain **discoverable and destroyable** at their old flat socket
paths (`vm list`/`vm destroy` and rehydration still find them) until they drain;
**every newly created box is jailed**. There is no in-place conversion — drain the
old boxes and new ones come up jailed.

## Boxes survive a spoke restart

The microVMs **outlive the spoke process**, exactly like Docker containers outlive
the spoke — restarting (or crashing) the spoke does **not** kill running boxes. On
restart the spoke rehydrates them from the state directory, probes each VMM, and
re-attaches; `List`/`Find`/`Control`/`Destroy` work again against the still-running
VMs. A microVM is stopped only by an explicit `Destroy` (box deletion), a `Pause`,
or the operator `vm destroy`/`destroy-all` commands below — never by shutdown.

Making this hold takes a few deliberate choices, since a Firecracker VMM is a direct
child of the spoke (Docker gets it for free — dockerd owns the containers):

- The VMM runs on a background context, and the jailer is told to **daemonize**
  (`setsid`), detaching the VMM into its own session so neither a request/lifetime
  context cancel nor a terminal `Ctrl-C` (SIGINT to the spoke's group) reaps it; the
  SDK's default signal forwarding is disabled so the spoke's shutdown signal is not
  relayed to it either.
- `Close` (spoke shutdown) is **release-only**: it closes in-memory listeners but
  leaves every VM running and the egress TAP pool up (re-adopted idempotently on the
  next start).
- The generated systemd unit sets **`KillMode=process`** for a firecracker spoke, so
  `systemctl restart` signals only the spoke process, not the whole cgroup — mirroring
  how Docker's own daemon unit keeps containers alive across a daemon restart. (The
  setup script the admin UI generates does this for you.)

> If you deploy your own unit, set `KillMode=process`. The systemd default
> (`control-group`) SIGKILLs the entire cgroup on stop, taking every microVM with it.

### Operator commands

Box lifecycle is normally driven by the hub. These are an escape hatch for
inspecting or reaping boxes a crashed or detached spoke left running on a host —
they read the on-disk state directly and need no hub connection or running spoke
(pass the same `--state-dir` the spoke runs with; empty uses the backend default):

```
llmbox-spoke firecracker vm list                  # id, phase, and running state of every box
llmbox-spoke firecracker vm destroy <box-id|token> # stop and remove one box (halts a live VMM first)
llmbox-spoke firecracker vm destroy-all --yes      # stop and remove every box on this host
```

The `network` subcommands provision the host-side egress pool out of band, so the
long-running spoke can run in `--egress-mode=external` without holding
`CAP_NET_ADMIN` itself (they need root — they run `sysctl`, `ip`, and `iptables` —
and are idempotent, so a boot-time systemd oneshot can run `setup` on every boot):

```
llmbox-spoke firecracker network setup     # create the TAP pool + NAT/isolation rules (root)
llmbox-spoke firecracker network teardown  # remove them (root; for decommissioning/resizing)
```

Pass the same `--pool-size` and `--tap-group` the spoke uses so the provisioned pool
and the slots the spoke attaches to line up.

## Egress modes

`--egress-mode` selects **who owns the host-side TAP/NAT plumbing**, decoupling
"does the guest get an egress NIC" from "does the long-running spoke mutate host
networking". `--disable-egress` is a backwards-compatible alias for
`--egress-mode=disabled`.

| Mode | Guest egress NIC | Spoke mutates host networking | Long-running spoke needs `CAP_NET_ADMIN` |
| --- | --- | --- | --- |
| `managed` (default) | yes | yes — provisions the pool at startup, re-adopts it on restart | yes |
| `external` | yes | **no** — only validates (read-only) that a pre-provisioned pool exists, then attaches | **no** |
| `disabled` | no | no | no |

In **`external`** mode an administrator provisions the TAP pool and NAT rules once
(via `firecracker network setup` or an equivalent root oneshot) and the spoke
attaches to it: it configures the guest NIC and `ip=` arg normally, allocates pool
slots normally, and **never** runs `sysctl`/`ip`/`iptables`, so it holds no
`CAP_NET_ADMIN`. Startup fails closed with an actionable error if a required TAP is
missing or down, rather than silently attaching to an unexpected interface.

> **Note.** Jailed launch is still mandatory and the jailer itself requires root
> (it must `chroot`, create device nodes, and `setuid` to the per-VM UID), so the
> spoke unit today still runs as `root`. `external` mode removes the **network**
> mutation (`CAP_NET_ADMIN`) from the long-running process and isolates it in the
> small boot-time provisioning unit — the security benefit described in
> [issue #124](https://github.com/clems4ever/llmbox/issues/124). The TAP devices
> must be owned by the same group the spoke's jailed VMMs run under (`--tap-group`,
> default fc-net GID `100000`) so those VMMs can open them.

### Durable systemd provisioning

The setup script the admin UI generates (systemd tab) installs this two-unit layout
for a networked Firecracker spoke automatically. Deploying it by hand:

```ini
# /etc/systemd/system/llmbox-firecracker-network.service
[Unit]
Description=llmbox firecracker egress network (TAP/NAT pool)
Wants=network-online.target
After=network-online.target
Before=llmbox-spoke.service

[Service]
Type=oneshot
ExecStart=/usr/local/bin/llmbox-spoke firecracker network setup
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
```

```ini
# /etc/systemd/system/llmbox-spoke.service
[Unit]
Description=llmbox spoke runner
Wants=network-online.target
After=network-online.target
Requires=llmbox-firecracker-network.service
After=llmbox-firecracker-network.service

[Service]
ExecStart=/usr/local/bin/llmbox-spoke firecracker \
  --hub wss://hub.example.com/spoke/connect --token <join-token> \
  --state /var/lib/llmbox/llmbox-spoke.json \
  --state-dir /var/lib/llmbox/firecracker \
  --egress-mode external
Restart=on-failure
RestartSec=5
StateDirectory=llmbox
KillMode=process

[Install]
WantedBy=multi-user.target
```

`RemainAfterExit=yes` keeps the oneshot "active" after it exits so the ordering
holds across a spoke restart, and `Requires=` re-runs it (idempotently) if it was
reset. Give the two units matching `--pool-size`/`--tap-group` if you override the
defaults.

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

A caller sets the per-box size via the `opts.DiskBytes` field of the `create-box`
API request; it is clamped to `[base image size, --box-max-disk-gb]`. The disk cannot
be shrunk below the base image. (The Docker backend has no per-box block device, so
these knobs are Firecracker-only.)

## Host requirements

- Linux with KVM (`/dev/kvm` present and usable).
- **Run the spoke as root.** Every box is launched through the jailer, which must
  `chroot`, create device nodes, and drop to a per-VM UID — see
  [Security: every box is jailed](#security-every-box-is-jailed).
- The `firecracker` **and** `jailer` binaries on `PATH`, version-matched (see
  `scripts/firecracker/fetch-firecracker.sh`). Both are required — jailing is
  mandatory.
- A Firecracker-compatible guest kernel with `virtio-vsock`, `virtio-net`,
  `virtio-blk`, `ext4`, `devtmpfs`, and `devpts` built in. The Firecracker CI
  kernels have all of these.
- The chroot base on the same filesystem as the state directory (the default
  `<state-dir>/chroot` satisfies this).
- `net.ipv4.ip_forward=1` is set for you **only** when egress networking is enabled.

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
images) and published by `.github/workflows/firecracker-assets.yml`. Every image is
published **zstd-compressed**; the spoke verifies the compressed layer's digest and
decompresses it to the raw file it boots on pull. The base rootfs compresses roughly
30× (it is a small, mostly-empty ext4 — see "Disk size" above — e.g. ~190 MiB pushed
vs a ~1.4 GiB image); the payload (a tiny, content-sized ext4 around the static guest
binary) and the kernel (an uncompressed `vmlinux`) compress more modestly (~2–3×) but
still cut what a spoke downloads. Overrides:

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
default `--egress-mode=managed`, which provisions the pool from the spoke and so
needs `CAP_NET_ADMIN`; use `--egress-mode=external` to attach to a pool an
administrator provisioned out of band — see [Egress modes](#egress-modes)). With
`--egress-mode=disabled` (control-only) a box boots and its guest is reachable, but
the box has no outbound network.

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
