#!/usr/bin/env bash
# Build the guest kernel + rootfs the Firecracker conformance test boots.
#
# The rootfs is intentionally minimal: BusyBox, the statically-linked llmbox
# guest as PID 1 (init), and a mock `claude` that mimics the auth-login /
# remote-control output the conformance contract asserts on. This is the test
# fixture — NOT a production box image (that bakes in the real claude; see
# Dockerfile.box and docs/firecracker.md).
#
# Output (under $OUT, default ~/fc-assets):
#   vmlinux       — Firecracker CI guest kernel (virtio-vsock/net/blk built in)
#   rootfs.ext4   — the guest root filesystem
#
# Then run the live conformance suite:
#   export PATH="$HOME/.local/bin:$PATH"          # or wherever firecracker is
#   LLMBOX_FC_KERNEL=$OUT/vmlinux \
#   LLMBOX_FC_ROOTFS=$OUT/rootfs.ext4 \
#     go test ./internal/spoke/firecracker/ -run TestConformanceFirecracker -v
#
# Requirements on the host: docker, mke2fs (e2fsprogs), a Go toolchain, and
# either the firecracker binary on PATH or run fetch-firecracker.sh first.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT="${OUT:-$HOME/fc-assets}"
KERNEL_URL="${KERNEL_URL:-https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/x86_64/vmlinux-6.1.102}"
ROOTFS_MB="${ROOTFS_MB:-256}"
BUSYBOX_IMAGE="${BUSYBOX_IMAGE:-busybox:latest}"

mkdir -p "$OUT"
ROOTDIR="$OUT/rootfs"
rm -rf "$ROOTDIR"
mkdir -p "$ROOTDIR"

echo ">> fetching guest kernel"
[ -f "$OUT/vmlinux" ] || curl -fsSL "$KERNEL_URL" -o "$OUT/vmlinux"

echo ">> building static llmbox-guest (linux/amd64)"
( cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o "$OUT/llmbox-guest" ./cmd/llmbox-guest )

echo ">> exporting BusyBox rootfs"
cid="$(docker create "$BUSYBOX_IMAGE")"
docker export "$cid" | tar -C "$ROOTDIR" -xf -
docker rm "$cid" >/dev/null

echo ">> installing guest, mock claude, and init"
install -m0755 "$OUT/llmbox-guest" "$ROOTDIR/usr/bin/llmbox-guest"

cat > "$ROOTDIR/usr/bin/claude" <<'CLAUDE'
#!/bin/sh
# Mock claude for conformance: mirrors testutils.MockClaudeScript.
case "$1" in
auth)
  echo "To authenticate, visit https://claude.com/cai/oauth/authorize?a=1&code_challenge=chal&state=st8 then paste the code"
  IFS= read -r code
  echo "submitted code: $code"
  exit 0
  ;;
remote-control)
  echo "Remote control session ready: https://claude.ai/s/mock-session-xyz"
  while IFS= read -r _; do : ; done
  exit 0
  ;;
*)
  echo "mock claude: unexpected args: $*" >&2
  exit 2
  ;;
esac
CLAUDE
chmod 0755 "$ROOTDIR/usr/bin/claude"

# PID 1: mount the essential filesystems, set up a PTY (creack/pty needs
# /dev/ptmx + devpts), bring up loopback, then exec the guest on vsock. The guest
# eth0 (when the backend enables egress) is configured by the kernel ip= arg
# before init runs, so init only touches loopback.
cat > "$ROOTDIR/init" <<'INIT'
#!/bin/sh
/bin/busybox mount -t proc proc /proc 2>/dev/null
/bin/busybox mount -t sysfs sysfs /sys 2>/dev/null
/bin/busybox mount -t devtmpfs devtmpfs /dev 2>/dev/null
/bin/busybox mkdir -p /dev/pts /tmp /var/tmp /root /run /workspace
/bin/busybox chmod 1777 /tmp /var/tmp 2>/dev/null
/bin/busybox mount -t devpts devpts /dev/pts 2>/dev/null
[ -e /dev/ptmx ] || /bin/busybox ln -s /dev/pts/ptmx /dev/ptmx
/bin/busybox ip link set lo up 2>/dev/null || /bin/busybox ifconfig lo up 2>/dev/null
export HOME=/root PATH=/usr/bin:/bin:/sbin
echo "llmbox-init: starting guest on vsock 5000"
exec /usr/bin/llmbox-guest --vsock-port 5000 --boxapi-port 5001 --claude /usr/bin/claude
INIT
chmod 0755 "$ROOTDIR/init"

echo ">> building ext4 image ($ROOTFS_MB MiB)"
rm -f "$OUT/rootfs.ext4"
mke2fs -q -F -t ext4 -d "$ROOTDIR" "$OUT/rootfs.ext4" "${ROOTFS_MB}M"
e2fsck -fn "$OUT/rootfs.ext4" >/dev/null

echo ">> done"
echo "   kernel: $OUT/vmlinux"
echo "   rootfs: $OUT/rootfs.ext4"
