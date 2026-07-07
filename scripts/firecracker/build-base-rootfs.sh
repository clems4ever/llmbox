#!/usr/bin/env bash
# Build the REAL Debian *base* rootfs for Firecracker: a full bookworm base with
# systemd as PID 1, Docker, Node, and net tooling — but WITHOUT the llmbox agent or
# claude baked in. Those ride on a separate, tiny read-only "payload" drive
# (build-payload-drive.sh) that a generic loader unit in this base mounts at boot.
#
# Splitting them this way is the whole point: this base is multi-GiB and changes
# only when the package set changes (rare), so it is built once and cached (in CI,
# in GHCR keyed on the inputs — see .github/workflows/firecracker-assets.yml). The
# agent, which changes on every commit, lives on the cheap payload drive instead, so
# an agent change never rebuilds this image.
#
# Everything is assembled as root inside a throwaway Debian container (via
# mmdebstrap), so file ownership and modes are correct (root-owned, /tmp 1777) —
# no non-root tar hacks. mke2fs runs there too, as root, so the image is right.
#
# Pair it with the full guest kernel (build-kernel.sh -> vmlinux-full); the minimal
# CI kernel lacks the overlay/netfilter/cgroup features Docker needs.
#
# Output (under $OUT, default ~/fc-assets):
#   base-rootfs.ext4
set -euo pipefail

OUT="${OUT:-$HOME/fc-assets}"
SUITE="${SUITE:-bookworm}"
ROOTFS_GB="${ROOTFS_GB:-6}"
mkdir -p "$OUT"

# The generic loader: mount the shared read-only payload drive (/dev/vdb) at
# /opt/llmbox, seed a writable copy of the claude trust file, then run the agent
# FROM the payload. This unit is stable — it never mentions a specific agent build —
# so a new agent means a new payload drive, never a rebuild of this base rootfs.
# The mount is guarded with mountpoint so Restart=always re-runs cleanly.
cat > "$OUT/llmbox-agent.service" <<'UNIT'
[Unit]
Description=llmbox guest agent (claude remote-control over vsock)
# No network-online ordering on purpose: the agent serves control over vsock and
# needs no egress to start (egress, when enabled, is configured by the kernel ip=
# arg before userspace runs). Ordering it after network-online.target would keep a
# control-only box (no eth0) unreachable until networkd-wait-online times out.
After=basic.target

[Service]
# Mount the read-only payload (idempotent, so Restart=always re-runs cleanly) and
# seed a writable copy of the trust file (claude rewrites ~/.claude.json at runtime,
# so it cannot live on the read-only payload). -n keeps a box-local copy across
# restarts.
ExecStartPre=/bin/mkdir -p /opt/llmbox
ExecStartPre=/bin/sh -c 'mountpoint -q /opt/llmbox || mount -o ro /dev/vdb /opt/llmbox'
ExecStartPre=/bin/sh -c 'cp -n /opt/llmbox/claude.json /root/.claude.json 2>/dev/null || true'
ExecStart=/opt/llmbox/llmbox-agent --vsock-port 5000 --claude /opt/llmbox/claude
WorkingDirectory=/workspace
Environment=HOME=/root
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
UNIT

# The build runs privileged (mmdebstrap --mode=root needs mount) in a throwaway
# container; nothing privileged persists.
docker run --rm --privileged -v "$OUT":/out debian:bookworm bash -euc "
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq mmdebstrap e2fsprogs uidmap >/dev/null

  echo '>> mmdebstrap ${SUITE} base (systemd, docker, node, net tooling)'
  mmdebstrap --mode=root --variant=important \
    --include=systemd-sysv,systemd-timesyncd,dbus,udev,ca-certificates,iproute2,iptables,nftables,kmod,procps,less,nano,curl,gnupg,openssh-client,git,nodejs,npm,docker.io,uidmap,dbus-user-session \
    --components=main \
    ${SUITE} /rootfs http://deb.debian.org/debian

  echo '>> installing loader unit and workspace (agent + claude arrive on the payload drive)'
  install -d -m0700 /rootfs/root
  install -d -m0755 /rootfs/workspace
  install -d -m0755 /rootfs/opt/llmbox
  install -m0644 /out/llmbox-agent.service /rootfs/etc/systemd/system/llmbox-agent.service

  echo '>> enabling services (agent + docker) and console niceties'
  chroot /rootfs systemctl enable llmbox-agent.service >/dev/null 2>&1 || \
    ln -sf ../llmbox-agent.service /rootfs/etc/systemd/system/multi-user.target.wants/llmbox-agent.service
  chroot /rootfs systemctl enable docker.service   >/dev/null 2>&1 || true
  chroot /rootfs systemctl enable systemd-networkd  >/dev/null 2>&1 || true
  # eth0 is configured by the kernel ip= arg at boot; ask systemd-networkd to leave
  # it alone (keep the kernel's static config) and just not flush it.
  mkdir -p /rootfs/etc/systemd/network
  cat > /rootfs/etc/systemd/network/10-eth0-keep.network <<NET
[Match]
Name=eth0
[Network]
KeepConfiguration=yes
NET
  # Don't let networkd-wait-online block boot on a control-only box (no eth0):
  # wait for any interface, cap at 15s, so egress boxes still gate on eth0 but
  # air-gapped boxes come up promptly.
  mkdir -p /rootfs/etc/systemd/system/systemd-networkd-wait-online.service.d
  cat > /rootfs/etc/systemd/system/systemd-networkd-wait-online.service.d/timeout.conf <<WAIT
[Service]
ExecStart=
ExecStart=/usr/lib/systemd/systemd-networkd-wait-online --any --timeout=15
WAIT
  # Static resolver (no systemd-resolved); NAT egress can reach these.
  printf 'nameserver 1.1.1.1\nnameserver 8.8.8.8\n' > /rootfs/etc/resolv.conf
  echo llmbox > /rootfs/etc/hostname
  # Let root log in on the serial console with no password, for debugging.
  chroot /rootfs passwd -d root >/dev/null 2>&1 || true

  # The provisioner boots with init=/init; point it at systemd.
  ln -sf /sbin/init /rootfs/init

  echo '>> building ext4 image (${ROOTFS_GB} GiB) as root (perms/ownership correct)'
  rm -f /out/base-rootfs.ext4
  mke2fs -q -F -t ext4 -d /rootfs /out/base-rootfs.ext4 ${ROOTFS_GB}G
  e2fsck -fn /out/base-rootfs.ext4 >/dev/null
  echo '>> done'
"
echo ">> base rootfs: $OUT/base-rootfs.ext4"
