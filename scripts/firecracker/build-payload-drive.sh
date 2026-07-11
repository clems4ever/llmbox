#!/usr/bin/env bash
# Build the llmbox guest "payload" drive for Firecracker: a small read-only ext4
# carrying the llmbox-specific bits — the static guest binary and an /entrypoint
# that launches it. The box's actual workload is installed at boot by the spoke's
# init script, so the payload no longer carries any workload of its own. The
# generic base rootfs (build-base-rootfs.sh) knows none of this: it just mounts
# this drive at /payload and runs /payload/entrypoint. Swapping in a different
# guest means shipping a different payload, never rebuilding the base.
#
# This is the CHEAP, fast-changing half of the split: the guest changes on every
# commit, so this image is rebuilt often — but it is tiny, needs no privileged
# mmdebstrap, and takes seconds. The multi-GiB base rootfs stays untouched (and
# cached) across guest changes. Attach both at boot:
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

echo ">> building static llmbox-guest (linux/amd64)"
( cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o "$PDIR/llmbox-guest" ./cmd/llmbox-guest )

# The guest is the only thing the payload carries, and it is built fresh from the
# repo checkout here — NOT lifted out of ghcr.io/.../llmbox-box:latest. That image
# is (re)published by a separate, slower workflow (ci.yml build-push, gated on
# tests) that finishes AFTER this one, so lifting from it would ship the *previous*
# commit's guest. Building locally keeps the payload in lockstep with the commit
# being built (and drops the docker dependency this script used to have).

# The entrypoint the base's generic loader runs after mounting this payload at
# /payload. It owns the guest-specific wiring the base deliberately does not know
# about: prepare the workspace and exec the guest on vsock. The guest serves the
# Init/Exec/Dial control protocol and runs the spoke's init script, which installs
# and starts the box's workload — the payload launches no workload itself.
#
# Box workloads run as the unprivileged 'agent' user the base rootfs provides
# (agent has passwordless sudo to escalate). This entrypoint runs as root under
# systemd — it seeds agent's home and workspace and hands them to agent, then the
# guest drops to agent via --user. HOME is left to the guest, which sets it to
# agent's home for the processes it launches.
cat > "$PDIR/entrypoint" <<'ENTRY'
#!/bin/sh
set -e
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
BOX_USER=agent
BOX_HOME=/home/$BOX_USER
mkdir -p /workspace
# Hand the home and workspace to the box user so the workload (running as agent)
# can write the workspace and its own state.
chown -R "$BOX_USER:$BOX_USER" "$BOX_HOME" /workspace
cd /workspace
exec /payload/llmbox-guest --vsock-port 5000 --boxapi-port 5001 --user "$BOX_USER"
ENTRY

chmod 0755 "$PDIR/llmbox-guest" "$PDIR/entrypoint"

echo ">> building ext4 payload ($PAYLOAD_MB MiB)"
rm -f "$OUT/payload.ext4"
mke2fs -q -F -t ext4 -d "$PDIR" "$OUT/payload.ext4" "${PAYLOAD_MB}M"
e2fsck -fn "$OUT/payload.ext4" >/dev/null

echo ">> payload: $OUT/payload.ext4"
