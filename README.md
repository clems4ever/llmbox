# llmbox

[![CI](https://github.com/clems4ever/llmbox/actions/workflows/ci.yml/badge.svg)](https://github.com/clems4ever/llmbox/actions/workflows/ci.yml)
[![coverage](.github/badges/coverage.svg)](https://github.com/clems4ever/llmbox/actions/workflows/ci.yml)
[![ui coverage](.github/badges/ui-coverage.svg)](https://github.com/clems4ever/llmbox/actions/workflows/ci.yml)

An [MCP](https://modelcontextprotocol.io) server for spinning up **sandboxed
boxes** ("llmboxes") on demand. From a chatbot you say *"create an llmbox"* and
get back a box you can `exec` into, dial ports on, and expose over HTTP. llmbox
is pure box infrastructure: it provides the sandbox lifecycle
(create/destroy/pause/resume/exec/dial) plus an HTTP proxy for a box's ports.
Built with the official [Go SDK](https://github.com/modelcontextprotocol/go-sdk).

Each box is a container (or [Firecracker microVM](docs/firecracker.md)) on its
own isolated network. **The box's workload is installed and started by the
spoke's init script** (`--init-script`), not by llmbox — llmbox only provides the
sandbox and, optionally, exposes the box's ports to a browser via the
[proxy](docs/proxy.md) (`--publish-port` or the `*_llmbox_proxy` tools). See
[Architecture](docs/architecture.md) for the full design.

## Quick start

```bash
docker build -t llmbox .

# Copy the example config and edit it (at least set public_url).
cp llmbox.example.yaml llmbox.yaml

docker run -d --name llmbox \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD/llmbox.yaml:/etc/llmbox/llmbox.yaml:ro" \
  --group-add "$(stat -c '%g' /var/run/docker.sock)" \
  -p 8080:8080 \
  llmbox --config /etc/llmbox/llmbox.yaml
```

One port (`8080`) serves everything: the box-control JSON API (under `/api/v1/`)
and the UI (admin dashboard, sign-in, health). The MCP protocol itself is served
by a separate binary, **`llmbox-mcp`**, which forwards every tool call to that
box-control API:

```bash
docker run -d --name llmbox-mcp -p 8082:8082 \
  llmbox-mcp --upstream http://llmbox:8080 --addr :8082
```

Then add `llmbox-mcp`'s URL (streamable HTTP) — or run it with `--stdio` as a
child process — as a remote MCP server in your client. The box-control API is
authenticated with API keys (`llmbox-server apikey add`) or an admin login
session; still run llmbox
behind an authenticating proxy. Full details — Docker socket permissions,
`docker compose`, TLS — are in [Running & configuration](docs/configuration.md).

### Prebuilt binaries

Each `v*` tag publishes static, dependency-free binaries to the
[GitHub Releases](https://github.com/clems4ever/llmbox/releases) page (built by
[GoReleaser](.goreleaser.yaml)). This lets a box host run a spoke **without
Docker or a Go toolchain** — for example to host [Firecracker](docs/firecracker.md)
microVM boxes on a KVM machine:

```bash
# Download and unpack the spoke for your platform (linux amd64/arm64).
VERSION=vX.Y.Z
curl -fsSL -o llmbox-spoke.tar.gz \
  "https://github.com/clems4ever/llmbox/releases/download/${VERSION}/llmbox-spoke_${VERSION#v}_linux_amd64.tar.gz"
tar xzf llmbox-spoke.tar.gz llmbox-spoke

# Connect it to a hub and serve Firecracker boxes (see docs/firecracker.md).
./llmbox-spoke firecracker --hub wss://hub.example.com/spoke/connect --token <join-token>
```

Archives are also published for `llmbox-server`, `llmbox-mcp`, and the in-box
`llmbox-guest` init.

## MCP tools

`create_llmbox`, `get_llmbox`, `list_llmboxes`, `list_spokes`,
`destroy_llmbox`, `exec_llmbox`, plus `create_llmbox_proxy` /
`delete_llmbox_proxy` / `list_llmbox_proxies` for exposing a box's HTTP ports.
See [MCP tools](docs/mcp-tools.md) for arguments and return values.

## Documentation

| Doc | What's in it |
|-----|--------------|
| [Architecture](docs/architecture.md) | How the pieces fit together: the box-control API, the spoke init script, and the code components. |
| [MCP tools](docs/mcp-tools.md) | Full reference for every tool's arguments and results. |
| [Running & configuration](docs/configuration.md) | Running the server, connecting a chatbot, and the YAML config reference. |
| [Authentication](docs/authentication.md) | Admin OIDC sign-in gating the admin UI and the per-box HTTP proxies, plus API keys and the single-tenant trust model. |
| [Box lifecycle hooks](docs/hooks.md) | Injecting per-box secrets/files via `box.create`/`box.destroy` hooks, plus box networking and isolation. |
| [Firecracker backend](docs/firecracker.md) | Running each box as a Firecracker microVM instead of a Docker container: vsock control, TAP/NAT egress, and building a guest rootfs. |
| [Operations](docs/operations.md) | State persistence, pausing boxes, and orphan cleanup. |
| [Development](docs/development.md) | Building, CI, and the unit / integration / end-to-end test suites. |

## Status & caveats

- A box's workload is provisioned by the spoke's `--init-script`, which runs once
  at creation. A box whose init script fails is surfaced with phase **`broken`**
  and the captured script output, so a bad provisioning step is loud rather than
  silent.
- The only human sign-in is **admin OIDC** (`auth.admin.emails`). It gates the
  admin dashboard/API **and** the per-box HTTP proxies. Every box-control API call
  (creation included) requires an API key or an admin session.
- The box-control API is **single-tenant by design**: it authenticates the
  caller but does not authorize per box, so any valid API key or admin can
  `exec`/`destroy` **any** box. This is safe only when a single trusted
  tenant sits behind the API (typically an authenticating proxy in front of
  `llmbox-mcp`); do not share one hub across mutually-distrusting users. See
  [the trust model](docs/authentication.md#trust-model-the-box-control-api-is-single-tenant).
  Per-user MCP clients and binding a box to its initiator are the natural
  follow-ups for multi-tenant use.
