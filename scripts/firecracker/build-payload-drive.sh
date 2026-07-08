#!/usr/bin/env bash
# Build the llmbox agent "payload" drive for Firecracker: a small read-only ext4
# carrying EVERYTHING llmbox-specific — the static guest agent, the standalone
# claude binary, its trust seed, and an /entrypoint that wires them together. The
# generic base rootfs (build-base-rootfs.sh) knows none of this: it just mounts this
# drive at /payload and runs /payload/entrypoint. Swapping in a different agent means
# shipping a different payload, never rebuilding the base.
#
# This is the CHEAP, fast-changing half of the split: the agent changes on every
# commit, so this image is rebuilt often — but it is a few hundred MiB, needs no
# privileged mmdebstrap, and takes seconds. The multi-GiB base rootfs stays
# untouched (and cached) across agent changes. Attach both at boot:
#   llmbox-spoke firecracker \
#     --rootfs  $OUT/base-rootfs.ext4 \
#     --payload $OUT/payload.ext4
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

echo ">> copying Claude Code skills from the repo checkout ($REPO_ROOT/docker/skills)"
# Skills (e.g. box-ports) ship straight from the source tree, NOT lifted out of
# the box image. They are plain text tracked in git, so taking them from the
# checkout keeps them in lockstep with the commit being built. Lifting them from
# ghcr.io/.../llmbox-box:latest instead would lag by a commit: that image is
# (re)published by a separate, slower workflow (ci.yml build-push, gated on
# tests), while this payload workflow finishes first — so it would ship the
# *previous* commit's skills. The base rootfs is agent-agnostic, so the payload
# entrypoint seeds these into /root/.claude/skills at boot.
mkdir -p "$PDIR/skills"
if [ -d "$REPO_ROOT/docker/skills" ]; then
  cp -a "$REPO_ROOT/docker/skills/." "$PDIR/skills/"
fi

# The entrypoint the base's generic loader runs after mounting this payload at
# /payload. It owns all the agent-specific wiring the base deliberately does not
# know about: seed a WRITABLE copy of the trust file (claude rewrites ~/.claude.json
# at runtime, so it cannot live on the read-only payload; -n keeps a box-local copy
# across restarts), then exec the agent on vsock pointing at the payload's claude.
cat > "$PDIR/entrypoint" <<'ENTRY'
#!/bin/sh
set -e
export HOME=/root
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
mkdir -p /workspace
cp -n /payload/claude.json /root/.claude.json 2>/dev/null || true
# Seed the payload's Claude Code skills (box-ports etc.); -n keeps box-local edits.
mkdir -p /root/.claude/skills
cp -rn /payload/skills/. /root/.claude/skills/ 2>/dev/null || true
cd /workspace
exec /payload/llmbox-agent --vsock-port 5000 --boxapi-port 5001 --claude /payload/claude
ENTRY

chmod 0755 "$PDIR/llmbox-agent" "$PDIR/claude" "$PDIR/entrypoint"

echo ">> building ext4 payload ($PAYLOAD_MB MiB)"
rm -f "$OUT/payload.ext4"
mke2fs -q -F -t ext4 -d "$PDIR" "$OUT/payload.ext4" "${PAYLOAD_MB}M"
e2fsck -fn "$OUT/payload.ext4" >/dev/null

echo ">> payload: $OUT/payload.ext4"
