#!/usr/bin/env bash
# Build a generic Debian *base* rootfs for Firecracker boxes: a full bookworm base
# with systemd as PID 1, Docker, Node, and net tooling. It is deliberately
# GUEST-AGNOSTIC — it contains nothing about llmbox or any particular guest.
# Everything guest-specific rides on a separate read-only "payload" drive
# (build-payload-drive.sh) that this base mounts and runs at boot.
#
# The only contract the base defines is a generic payload loader:
#
#   mount the payload block device (/dev/vdb) read-only at /payload,
#   then run /payload/entrypoint
#
# Any guest (llmbox, or something else entirely) ships a payload with an
# /entrypoint and its own binaries; swapping guests never touches this base. That
# also keeps the base slow-changing and cacheable (see firecracker-assets.yml):
# nothing a guest update changes lives here.
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

# The generic payload loader: mount the read-only payload drive (/dev/vdb) at
# /payload and run its entrypoint. This unit names no specific guest, binary, or
# protocol — the payload owns all of that (its entrypoint seeds whatever state it
# needs and execs whatever it runs). Restart=always relaunches a crashed entrypoint;
# the mount is guarded with mountpoint so it re-runs cleanly.
cat > "$OUT/payload.service" <<'UNIT'
[Unit]
Description=Payload loader (mount the payload drive and run its entrypoint)
# No network-online ordering on purpose: a payload may serve control over vsock and
# need no egress to start (egress, when enabled, is configured by the kernel ip= arg
# before userspace runs). Ordering after network-online.target would keep a
# control-only box (no eth0) waiting on networkd-wait-online to time out.
After=basic.target

[Service]
ExecStartPre=/bin/mkdir -p /payload
ExecStartPre=/bin/sh -c 'mountpoint -q /payload || mount -o ro /dev/vdb /payload'
ExecStart=/payload/entrypoint
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
    --include=systemd-sysv,systemd-timesyncd,dbus,udev,ca-certificates,iproute2,iptables,nftables,kmod,procps,less,nano,curl,gnupg,openssh-client,git,nodejs,npm,docker.io,sudo,uidmap,dbus-user-session \
    --components=main \
    ${SUITE} /rootfs http://deb.debian.org/debian

  echo '>> installing the generic payload loader and mountpoint'
  install -d -m0755 /rootfs/payload
  install -m0644 /out/payload.service /rootfs/etc/systemd/system/payload.service

  echo '>> enabling services (payload loader + docker) and console niceties'
  chroot /rootfs systemctl enable payload.service >/dev/null 2>&1 || \
    ln -sf ../payload.service /rootfs/etc/systemd/system/multi-user.target.wants/payload.service
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
  echo box > /rootfs/etc/hostname
  # Let root log in on the serial console with no password, for debugging.
  chroot /rootfs passwd -d root >/dev/null 2>&1 || true

  # A generic unprivileged account box workloads run as. The payload runs the
  # box workload (and Exec) as 'agent' via the guest's --user flag rather than as
  # root. Passwordless sudo keeps the box a
  # single-tenant, full-access environment (the real isolation boundary is the
  # microVM, not this in-guest uid); docker-group membership lets the box user
  # drive the baked-in dockerd without sudo. The account is defined here, in the
  # generic base, because it is OS-level setup; the payload only seeds its home.
  chroot /rootfs groupadd -f docker
  chroot /rootfs useradd --create-home --shell /bin/bash --groups sudo,docker agent
  install -d -m0755 /rootfs/etc/sudoers.d
  printf 'agent ALL=(ALL) NOPASSWD:ALL\n' > /rootfs/etc/sudoers.d/agent
  chmod 0440 /rootfs/etc/sudoers.d/agent

  # The provisioner boots with init=/init; point it at systemd.
  ln -sf /sbin/init /rootfs/init

  echo '>> building ext4 image (${ROOTFS_GB} GiB) as root (perms/ownership correct)'
  rm -f /out/base-rootfs.ext4
  mke2fs -q -F -t ext4 -d /rootfs /out/base-rootfs.ext4 ${ROOTFS_GB}G
  e2fsck -fn /out/base-rootfs.ext4 >/dev/null
  echo '>> done'
"
echo ">> base rootfs: $OUT/base-rootfs.ext4"
