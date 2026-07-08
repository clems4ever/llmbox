#!/usr/bin/env bash
# Build a PRODUCTION Firecracker rootfs from the llmbox-box container image — the
# same image the Docker backend runs, with the real `claude` baked in. The result
# boots a microVM that runs a real Claude session (unlike build-conformance-rootfs.sh,
# whose rootfs ships a mock claude for testing).
#
# What it does:
#   1. docker export the box image's filesystem (real claude, tini, CA bundle,
#      the ~/.claude.json trust seed — see Dockerfile.box).
#   2. Overwrite its llmbox-agent with a freshly-built one from THIS source tree —
#      the published image predates the vsock transport, so its baked-in agent has
#      no --vsock-port. This is required.
#   3. Drop in an init (PID 1) that mounts the essentials, sets up a PTY and DNS,
#      brings up loopback, then execs the agent under tini (so the many children
#      Claude spawns are reaped) listening on vsock.
#   4. mke2fs a right-sized ext4 image.
#
# Output (under $OUT, default ~/fc-assets):
#   box-rootfs.ext4
#
# Point the server/spoke at it and ENABLE egress (a real session needs the
# Anthropic API + OAuth), which requires running with CAP_NET_ADMIN (root):
#   firecracker:
#     kernel_image: ~/fc-assets/vmlinux            # from build-conformance-rootfs.sh / fetch
#     rootfs_image: ~/fc-assets/box-rootfs.ext4
#     disable_egress: false                        # egress ON
#
# Requirements: docker, mke2fs (e2fsprogs), a Go toolchain.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT="${OUT:-$HOME/fc-assets}"
BOX_IMAGE="${BOX_IMAGE:-ghcr.io/clems4ever/llmbox-box:latest}"
AGENT_VSOCK_PORT="${AGENT_VSOCK_PORT:-5000}"
# Host vsock port the agent bridges /run/llmbox/boxapi.sock to (the box-port
# API); must match the spoke's boxAPIVsockPort. 0 disables the bridge.
AGENT_BOXAPI_PORT="${AGENT_BOXAPI_PORT:-5001}"

mkdir -p "$OUT"
ROOTDIR="$OUT/box-rootfs"
rm -rf "$ROOTDIR"
mkdir -p "$ROOTDIR"

echo ">> building static llmbox-agent (linux/amd64) from this source tree"
( cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o "$OUT/llmbox-agent" ./cmd/llmbox-agent )

echo ">> exporting $BOX_IMAGE filesystem"
# `docker create` pulls the image if absent. If your registry needs auth or the
# image is private, `docker pull` (or `docker build -f Dockerfile.box -t $BOX_IMAGE .`)
# it first.
cid="$(docker create "$BOX_IMAGE")"
docker export "$cid" | tar -C "$ROOTDIR" -xf -
docker rm "$cid" >/dev/null

echo ">> installing the vsock-capable agent (overwriting the image's copy)"
install -m0755 "$OUT/llmbox-agent" "$ROOTDIR/usr/local/bin/llmbox-agent"
# Remove any stale copy elsewhere on PATH so the init's absolute path is the only one.
rm -f "$ROOTDIR/usr/bin/llmbox-agent"

echo ">> installing init"
cat > "$ROOTDIR/init" <<INIT
#!/bin/sh
# PID 1 for the production box: prepare the environment, then hand off to the
# agent (under tini, which reaps Claude's many short-lived children) on vsock.
mount -t proc proc /proc 2>/dev/null
mount -t sysfs sysfs /sys 2>/dev/null
mount -t devtmpfs devtmpfs /dev 2>/dev/null
mkdir -p /dev/pts /tmp /var/tmp /run /root /workspace
mount -t devpts devpts /dev/pts 2>/dev/null
[ -e /dev/ptmx ] || ln -s /dev/pts/ptmx /dev/ptmx
# Restore /tmp's sticky world-writable mode: building the rootfs as a non-root
# user drops the special bits, leaving /tmp 755, which breaks apt (its _apt
# sandbox user cannot write there → "Couldn't create temporary file /tmp/...").
chmod 1777 /tmp /var/tmp 2>/dev/null
ip link set lo up 2>/dev/null || ifconfig lo up 2>/dev/null
# eth0 is configured by the kernel ip= arg (when egress is enabled) before init
# runs; DNS is not, so seed a resolver for outbound HTTPS to the Claude API.
[ -s /etc/resolv.conf ] || printf 'nameserver 1.1.1.1\nnameserver 8.8.8.8\n' > /etc/resolv.conf
export HOME=/root PATH=/usr/local/bin:/usr/bin:/bin:/sbin
# Run in /workspace, which the image's ~/.claude.json trust seed marks trusted
# (the Docker backend sets WORKDIR=/workspace for the same reason); otherwise
# claude reports "Workspace not trusted" for the untrusted / directory.
cd /workspace 2>/dev/null || true
AGENT=/usr/local/bin/llmbox-agent
if command -v tini >/dev/null 2>&1; then
  exec tini -g -- "\$AGENT" --vsock-port ${AGENT_VSOCK_PORT} --boxapi-port ${AGENT_BOXAPI_PORT} --claude claude
else
  exec "\$AGENT" --vsock-port ${AGENT_VSOCK_PORT} --boxapi-port ${AGENT_BOXAPI_PORT} --claude claude
fi
INIT
chmod 0755 "$ROOTDIR/init"

echo ">> building ext4 image"
# Size to the contents plus headroom (Claude writes credentials/caches at runtime).
used_mb="$(du -sm "$ROOTDIR" | cut -f1)"
size_mb=$(( used_mb * 14 / 10 + 512 ))
[ "$size_mb" -lt 1024 ] && size_mb=1024
rm -f "$OUT/box-rootfs.ext4"
mke2fs -q -F -t ext4 -d "$ROOTDIR" "$OUT/box-rootfs.ext4" "${size_mb}M"
e2fsck -fn "$OUT/box-rootfs.ext4" >/dev/null

echo ">> done"
echo "   rootfs: $OUT/box-rootfs.ext4 (${size_mb} MiB)"
echo "   set firecracker.rootfs_image to it, disable_egress: false, and run the server as root."
