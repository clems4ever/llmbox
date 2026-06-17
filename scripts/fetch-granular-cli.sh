#!/usr/bin/env bash
# Fetch a granular CLI binary from the (private) granular GitHub releases into
# ./dist, so Dockerfile.claude can COPY it in without a token in the build.
#
# The release repo is private, so this needs a token with `contents: read` on it
# (a fine-grained PAT, or classic PAT with `repo`). A git deploy key does NOT work
# here — release downloads go through the REST API, which deploy keys can't use.
#
# Usage:
#   GH_TOKEN=github_pat_xxx ./scripts/fetch-granular-cli.sh <cli> [version]
#
#   <cli>      granular | granular-github   (the binary / asset prefix)
#   version    a release tag (e.g. v0.0.11); defaults to "latest"
set -euo pipefail

CLI="${1:?usage: fetch-granular-cli.sh <granular|granular-github> [version]}"
VERSION="${2:-latest}"
: "${GH_TOKEN:?set GH_TOKEN (a token with contents:read on $REPO)}"
REPO="${GRANULAR_REPO:-clems4ever/granular}"

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

# Match the goreleaser asset for this CLI and platform regardless of the version
# string in the name: <cli>_<ver>_linux_<arch>.tar.gz. The trailing underscore in
# the prefix keeps "granular_" from matching "granular-github_".
asset_id="$(
  curl -fsSL -H "Authorization: Bearer $GH_TOKEN" -H "Accept: application/vnd.github+json" "$rel" \
    | CLI="$CLI" ARCH="$arch" python3 -c "import sys,json,os
cli,arch=os.environ['CLI'],os.environ['ARCH']
r=json.load(sys.stdin)
m=[a for a in r['assets'] if a['name'].startswith(cli+'_') and a['name'].endswith('_linux_%s.tar.gz'%arch)]
if not m: sys.exit('no %s_*_linux_%s.tar.gz asset in %s'%(cli,arch,r.get('tag_name','?')))
print(m[0]['id'])"
)"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -H "Authorization: Bearer $GH_TOKEN" -H "Accept: application/octet-stream" \
  "$api/assets/$asset_id" -o "$tmp/$CLI.tar.gz"
tar -xzf "$tmp/$CLI.tar.gz" -C "$tmp"

bin="$(find "$tmp" -type f -name "$CLI" | head -n1)"
[ -n "$bin" ] || { echo "$CLI binary not found in archive" >&2; exit 1; }

mkdir -p dist
install -m 0755 "$bin" "dist/$CLI"
echo "fetched dist/$CLI ($VERSION, $arch)"
