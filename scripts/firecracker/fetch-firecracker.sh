#!/usr/bin/env bash
# Install the firecracker (and jailer) binary into ~/.local/bin. No root needed
# to install; running microVMs needs read/write on /dev/kvm (join the `kvm`
# group or `setfacl -m u:$USER:rw /dev/kvm`).
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
install -m0755 "$tmp/release-${tag}-${ARCH}/jailer-${tag}-${ARCH}"     "$DEST/jailer" || true

"$DEST/firecracker" --version | head -1
echo ">> ensure $DEST is on PATH"
