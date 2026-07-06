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

## Building a rootfs

The rootfs must boot with the llmbox agent as its init, listening on vsock. Its
`init` mounts `/proc`, `/sys`, `/dev` (devtmpfs) and `/dev/pts` (devpts, for the
agent's PTY), brings up `lo`, then `exec`s `llmbox-agent --vsock-port 5000`.

Two build scripts are provided:

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
