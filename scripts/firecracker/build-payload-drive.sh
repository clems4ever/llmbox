#!/usr/bin/env bash
# Build the llmbox agent "payload" drive for Firecracker: a small read-only ext4
# carrying the static guest agent, the standalone claude binary, and claude's trust
# seed. The base rootfs (build-base-rootfs.sh) mounts this at /opt/llmbox via its
# loader unit and runs the agent from it.
#
# This is the CHEAP, fast-changing half of the split: the agent changes on every
# commit, so this image is rebuilt often — but it is a few hundred MiB, needs no
# privileged mmdebstrap, and takes seconds. The multi-GiB base rootfs stays
# untouched (and cached) across agent changes. Attach both at boot:
#   llmbox spoke --backend firecracker \
#     --fc-rootfs  $OUT/base-rootfs.ext4 \
#     --fc-payload $OUT/payload.ext4
#
# mke2fs runs unprivileged: the payload is mounted read-only and only ever exec'd by
# root in the guest, so the files being owned by the building user is fine.
#
# Output (under $OUT, default ~/fc-assets):
#   payload.ext4
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT="${OUT:-$HOME/fc-assets}"
BOX_IMAGE="${BOX_IMAGE:-ghcr.io/clems4ever/llmbox-box:latest}"
PAYLOAD_MB="${PAYLOAD_MB:-512}"
mkdir -p "$OUT"

PDIR="$OUT/payload"
rm -rf "$PDIR"
mkdir -p "$PDIR"

echo ">> building static llmbox-agent (linux/amd64)"
( cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o "$PDIR/llmbox-agent" ./cmd/llmbox-agent )

echo ">> lifting the standalone claude binary and trust seed from $BOX_IMAGE"
cid="$(docker create "$BOX_IMAGE")"
docker cp "$cid":/usr/local/bin/claude "$PDIR/claude"
docker cp "$cid":/root/.claude.json "$PDIR/claude.json" 2>/dev/null || echo '{}' > "$PDIR/claude.json"
docker rm "$cid" >/dev/null
chmod 0755 "$PDIR/llmbox-agent" "$PDIR/claude"

echo ">> building ext4 payload ($PAYLOAD_MB MiB)"
rm -f "$OUT/payload.ext4"
mke2fs -q -F -t ext4 -d "$PDIR" "$OUT/payload.ext4" "${PAYLOAD_MB}M"
e2fsck -fn "$OUT/payload.ext4" >/dev/null

echo ">> payload: $OUT/payload.ext4"
