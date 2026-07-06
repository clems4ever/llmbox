#!/usr/bin/env bash
# Build a REAL Debian server rootfs for Firecracker: a full bookworm base with
# systemd as PID 1, Docker, Node, and the standalone claude — so the box behaves
# like a normal machine and Claude can do whatever it likes (run services, build,
# `docker run`, …). The llmbox guest agent runs as a systemd unit on vsock.
#
# Everything is assembled as root inside a throwaway Debian container (via
# mmdebstrap), so file ownership and modes are correct (root-owned, /tmp 1777) —
# no non-root tar hacks. mke2fs runs there too, as root, so the image is right.
#
# Pair it with the full guest kernel (build-kernel.sh -> vmlinux-full); the minimal
# CI kernel lacks the overlay/netfilter/cgroup features Docker needs.
#
# Output (under $OUT, default ~/fc-assets):
#   debian-rootfs.ext4
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT="${OUT:-$HOME/fc-assets}"
BOX_IMAGE="${BOX_IMAGE:-ghcr.io/clems4ever/llmbox-box:latest}"
SUITE="${SUITE:-bookworm}"
ROOTFS_GB="${ROOTFS_GB:-6}"
mkdir -p "$OUT"

echo ">> building static llmbox-agent (linux/amd64)"
( cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o "$OUT/llmbox-agent" ./cmd/llmbox-agent )

echo ">> lifting the standalone claude binary and trust seed from $BOX_IMAGE"
cid="$(docker create "$BOX_IMAGE")"
docker cp "$cid":/usr/local/bin/claude "$OUT/claude"
docker cp "$cid":/root/.claude.json "$OUT/claude.json" 2>/dev/null || echo '{}' > "$OUT/claude.json"
docker rm "$cid" >/dev/null

# systemd unit that runs the agent on vsock, in the trusted /workspace, as the box
# entrypoint. systemd (PID 1) reaps children and restarts it on crash.
cat > "$OUT/llmbox-agent.service" <<'UNIT'
[Unit]
Description=llmbox guest agent (claude remote-control over vsock)
# No network-online ordering on purpose: the agent serves control over vsock and
# needs no egress to start (egress, when enabled, is configured by the kernel ip=
# arg before userspace runs). Ordering it after network-online.target would keep a
# control-only box (no eth0) unreachable until networkd-wait-online times out.
After=basic.target

[Service]
ExecStart=/usr/local/bin/llmbox-agent --vsock-port 5000 --claude claude
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

  echo '>> installing agent, claude, trust seed, unit'
  install -m0755 /out/llmbox-agent /rootfs/usr/local/bin/llmbox-agent
  install -m0755 /out/claude       /rootfs/usr/local/bin/claude
  install -d -m0700 /rootfs/root
  install -m0600 /out/claude.json  /rootfs/root/.claude.json
  install -d -m0755 /rootfs/workspace
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
  rm -f /out/debian-rootfs.ext4
  mke2fs -q -F -t ext4 -d /rootfs /out/debian-rootfs.ext4 ${ROOTFS_GB}G
  e2fsck -fn /out/debian-rootfs.ext4 >/dev/null
  echo '>> done'
"
echo ">> rootfs: $OUT/debian-rootfs.ext4"
