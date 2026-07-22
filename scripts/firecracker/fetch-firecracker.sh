#!/usr/bin/env bash
# Install the firecracker AND jailer binaries into ~/.local/bin, from the same
# release so they are version-matched. The llmbox firecracker spoke launches every
# microVM through the jailer (chrooted, unprivileged per-VM UID) — jailing is
# mandatory, there is no unjailed mode — so BOTH binaries are required.
#
# No root is needed to install; running microVMs needs the spoke to run as root
# (the jailer must chroot, create device nodes, and drop privilege) and read/write
# on /dev/kvm.
set -euo pipefail

DEST="${DEST:-$HOME/.local/bin}"
ARCH="$(uname -m)"
mkdir -p "$DEST"

rel="https://github.com/firecracker-microvm/firecracker/releases"
tag="${FC_VERSION:-$(basename "$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$rel/latest")")}"
echo ">> installing firecracker $tag ($ARCH) into $DEST"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "$rel/download/${tag}/firecracker-${tag}-${ARCH}.tgz" | tar -C "$tmp" -xz
install -m0755 "$tmp/release-${tag}-${ARCH}/firecracker-${tag}-${ARCH}" "$DEST/firecracker"
# Jailer is required (jailed launch is the only mode) and must match firecracker's
# version; installing both from this one release guarantees that. Fail loudly if the
# release lacks a jailer rather than silently leaving the spoke unable to start.
install -m0755 "$tmp/release-${tag}-${ARCH}/jailer-${tag}-${ARCH}"     "$DEST/jailer"

"$DEST/firecracker" --version | head -1
"$DEST/jailer" --version | head -1
echo ">> ensure $DEST is on PATH"
