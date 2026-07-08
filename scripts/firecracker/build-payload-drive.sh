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
PAYLOAD_MB="${PAYLOAD_MB:-512}"
mkdir -p "$OUT"

PDIR="$OUT/payload"
rm -rf "$PDIR"
mkdir -p "$PDIR"

echo ">> building static llmbox-agent (linux/amd64)"
( cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o "$PDIR/llmbox-agent" ./cmd/llmbox-agent )

# Everything the payload carries besides the guest agent is sourced from the repo
# checkout or fetched fresh here — NOT lifted out of ghcr.io/.../llmbox-box:latest.
# That image is (re)published by a separate, slower workflow (ci.yml build-push,
# gated on tests) that finishes AFTER this one, so lifting from it would ship the
# *previous* commit's claude binary, trust seed, and skills. Sourcing everything
# locally keeps the payload in lockstep with the commit being built (and drops the
# docker dependency this script used to have).

# The standalone claude binary: fetched with the same installer the box image uses
# (Dockerfile.box), pinned to the same version — the single source of truth is the
# ARG CLAUDE_VERSION line there. This assumes an amd64 build host, matching the
# amd64 llmbox-agent built above and the amd64 rootfs the payload rides.
echo ">> fetching the standalone claude binary (version pinned in Dockerfile.box)"
CLAUDE_VERSION="$(sed -n 's/^ARG CLAUDE_VERSION=//p' "$REPO_ROOT/Dockerfile.box" | head -n1)"
CLAUDE_VERSION="${CLAUDE_VERSION:-stable}"
claude_home="$(mktemp -d)"
curl -fsSL https://claude.ai/install.sh | HOME="$claude_home" bash -s -- "$CLAUDE_VERSION"
claude_link="$claude_home/.local/bin/claude"
[ -e "$claude_link" ] || { echo "claude not found at $claude_link after install" >&2; exit 1; }
install -m0755 "$(readlink -f "$claude_link")" "$PDIR/claude"
"$PDIR/claude" --version
rm -rf "$claude_home"

# The ~/.claude.json trust seed and the Claude Code skills (box-ports etc.) are
# plain files tracked in git, so copy them straight from the checkout. The base
# rootfs is agent-agnostic, so the payload entrypoint seeds them into /root at boot.
echo ">> copying the trust seed and skills from the repo checkout"
cp "$REPO_ROOT/docker/claude.json" "$PDIR/claude.json" 2>/dev/null || echo '{}' > "$PDIR/claude.json"
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
