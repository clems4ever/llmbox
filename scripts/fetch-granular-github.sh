#!/usr/bin/env bash
# Fetch the granular-github CLI from the (private) granular GitHub releases into
# ./dist, so Dockerfile.claude can COPY it in without a token in the build.
#
# The release repo is private, so this needs a token with `contents: read` on it
# (a fine-grained PAT, or classic PAT with `repo`). A git deploy key does NOT work
# here — release downloads go through the REST API, which deploy keys can't use.
#
# Usage:
#   GH_TOKEN=github_pat_xxx ./scripts/fetch-granular-github.sh [version]
#
# version defaults to "latest"; pass a release tag (e.g. v0.0.11) to pin one.
set -euo pipefail

: "${GH_TOKEN:?set GH_TOKEN (a token with contents:read on $REPO)}"
REPO="${GRANULAR_REPO:-clems4ever/granular}"
VERSION="${1:-latest}"

# Target arch of the image being built. Defaults to the host arch; set
# GRANULAR_ARCH (amd64|arm64) when cross-building (e.g. an amd64 image on an
# Apple-Silicon host).
arch="${GRANULAR_ARCH:-}"
if [ -z "$arch" ]; then
  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) echo "unsupported arch: $(uname -m); set GRANULAR_ARCH" >&2; exit 1 ;;
  esac
fi

api="https://api.github.com/repos/$REPO/releases"
if [ "$VERSION" = latest ]; then rel="$api/latest"; else rel="$api/tags/$VERSION"; fi

# Match the goreleaser asset for this platform regardless of the version string
# embedded in the name: granular-github_<ver>_linux_<arch>.tar.gz.
asset_id="$(
  curl -fsSL -H "Authorization: Bearer $GH_TOKEN" -H "Accept: application/vnd.github+json" "$rel" \
    | python3 -c "import sys,json
r=json.load(sys.stdin)
m=[a for a in r['assets'] if a['name'].startswith('granular-github_') and a['name'].endswith('_linux_${arch}.tar.gz')]
if not m: sys.exit('no granular-github_*_linux_${arch}.tar.gz asset in '+r.get('tag_name','?'))
print(m[0]['id'])"
)"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -H "Authorization: Bearer $GH_TOKEN" -H "Accept: application/octet-stream" \
  "$api/assets/$asset_id" -o "$tmp/granular-github.tar.gz"
tar -xzf "$tmp/granular-github.tar.gz" -C "$tmp"

bin="$(find "$tmp" -type f -name granular-github | head -n1)"
[ -n "$bin" ] || { echo "granular-github binary not found in archive" >&2; exit 1; }

mkdir -p dist
install -m 0755 "$bin" dist/granular-github
echo "fetched dist/granular-github ($VERSION, $arch)"
