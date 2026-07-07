#!/usr/bin/env bash
# Convenience wrapper that builds BOTH halves of a Firecracker Debian box locally:
#
#   1. build-base-rootfs.sh   -> base-rootfs.ext4   (multi-GiB Debian OS, cached; no agent)
#   2. build-payload-drive.sh -> payload.ext4       (tiny; the agent + claude, rebuilt often)
#
# The base is the slow, rarely-changing half — in CI it is built once and cached in
# GHCR keyed on its inputs (see .github/workflows/firecracker-assets.yml), not
# rebuilt per commit. The payload is the cheap, fast-changing half carrying the
# agent. Splitting them means an agent change rebuilds only the small payload.
#
# Output (under $OUT, default ~/fc-assets): base-rootfs.ext4 and payload.ext4.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="${OUT:-$HOME/fc-assets}"

OUT="$OUT" "$DIR/build-base-rootfs.sh"
OUT="$OUT" "$DIR/build-payload-drive.sh"

echo
echo ">> Debian box assets ready:"
echo "   base:    $OUT/base-rootfs.ext4"
echo "   payload: $OUT/payload.ext4"
echo ">> boot with: --fc-rootfs $OUT/base-rootfs.ext4 --fc-payload $OUT/payload.ext4"
