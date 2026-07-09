#!/usr/bin/env bash
# Print a short content hash that identifies the Firecracker base rootfs by its
# INPUTS — the base build script and the suite/size knobs — not by its bytes.
#
# mke2fs output is nondeterministic (UUIDs, timestamps), so two builds of the same
# inputs produce different image bytes; keying the cache on the output would never
# hit. Keying on the inputs means the base is rebuilt only when something that
# actually changes it changes, and an unrelated commit reuses the cached image.
#
# Used as the GHCR tag by both the publish workflow (firecracker-assets.yml) and the
# Makefile's pull-before-build, so both agree on which cached image to look for.
#
# Usage: asset-key.sh [base|kernel]   (default: base)
#
# The base carries no guest or claude (those live on the payload drive), so the box
# image is deliberately NOT part of its key. The payload is not keyed here — it
# tracks the guest, which changes every commit, so it is republished per build
# rather than content-addressed.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUITE="${SUITE:-bookworm}"
ROOTFS_GB="${ROOTFS_GB:-6}"

kind="${1:-base}"
case "$kind" in
  base)
    { printf 'suite=%s rootfs_gb=%s\n' "$SUITE" "$ROOTFS_GB"; cat "$DIR/build-base-rootfs.sh"; } \
      | sha256sum | cut -c1-16
    ;;
  kernel)
    { printf 'kernel\n'; cat "$DIR/build-kernel.sh"; } | sha256sum | cut -c1-16
    ;;
  *)
    echo "usage: asset-key.sh [base|kernel]" >&2
    exit 2
    ;;
esac
