# llmbox

[![CI](https://github.com/clems4ever/llmbox/actions/workflows/ci.yml/badge.svg)](https://github.com/clems4ever/llmbox/actions/workflows/ci.yml)
[![coverage](.github/badges/coverage.svg)](https://github.com/clems4ever/llmbox/actions/workflows/ci.yml)
[![ui coverage](.github/badges/ui-coverage.svg)](https://github.com/clems4ever/llmbox/actions/workflows/ci.yml)

An [MCP](https://modelcontextprotocol.io) server for spinning up **sandboxed
Claude instances** ("llmboxes") on demand. From a chatbot you say *"create an
llmbox"*; you get back a URL; you open it, sign in with **your own** Claude
account, and the sandbox activates — driveable from claude.ai/code or the mobile
app via Claude's Remote Control.

Each box is a container running Claude Code in remote-control mode, authenticated
by the end user. Built with the official
[Go SDK](https://github.com/modelcontextprotocol/go-sdk).

```
"create an llmbox"  ──▶  auth URL  ──▶  you sign in with Claude  ──▶  session URL
```

The OAuth code exchanges for a full-scope account token, so it **never** enters
the model's context: the chatbot only ever sees the box ID and the auth-page URL,
while the code travels browser → server → container out of band. See
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
and the UI (auth pages, admin, health). The MCP protocol itself is served by a
separate binary, **`llmbox-mcp`**, which forwards every tool call to that
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

## MCP tools

`create_llmbox`, `get_llmbox`, `list_llmboxes`, `destroy_llmbox`,
`get_llmbox_logs`, `exec_llmbox`. See [MCP tools](docs/mcp-tools.md) for
arguments and return values.

## Documentation

| Doc | What's in it |
|-----|--------------|
| [Architecture](docs/architecture.md) | The auth-secret split, the activation flow, the activation page, and the code components. |
| [MCP tools](docs/mcp-tools.md) | Full reference for every tool's arguments and results. |
| [Running & configuration](docs/configuration.md) | Running the server, connecting a chatbot, and the YAML config reference. |
| [Authenticating activation](docs/authentication.md) | Gating activation behind a sign-in provider (OIDC) so a leaked token can't hijack a box. |
| [Box lifecycle hooks](docs/hooks.md) | Injecting per-box secrets/files via `box.create`/`box.destroy` hooks, plus box networking and isolation. |
| [Firecracker backend](docs/firecracker.md) | Running each box as a Firecracker microVM instead of a Docker container: vsock control, TAP/NAT egress, and building a guest rootfs. |
| [Operations](docs/operations.md) | Session persistence, box credentials across restarts, and orphan cleanup. |
| [Development](docs/development.md) | Building, CI, and the unit / integration / end-to-end test suites. |

## Status & caveats

- The create → authorize-URL → auth-page path is verified end-to-end (including a
  real container and the live HTTP/MCP stack). The final **code → session URL**
  exchange needs a human to authorize in a browser; the wrapper that runs
  `claude auth login` then `claude remote-control` is in
  [`internal/spoke/docker/manager.go`](internal/spoke/docker/manager.go) and is easy to tweak
  if your Claude version's prompts differ.
- Each box consumes a session on the **end user's** Claude subscription. That is
  the intended model; be deliberate about who you let create boxes.
- [Activation auth](docs/authentication.md) gates *activation* (closing the
  leaked-token hijack), and every box-control API call (creation included)
  requires an API key or an admin session, so
  a caller can create boxes (a DoS bounded by the un-authenticated reaper TTL).
  Authenticating MCP clients per-user, and binding a box to the specific
  initiator, are the natural follow-ups.
- The box wrapper pre-accepts the workspace-trust dialog (writes
  `projects[cwd].hasTrustDialogAccepted` to `~/.claude.json` after login), since
  `claude remote-control` otherwise aborts with "Workspace not trusted" in a
  fresh box. If a `SubmitCode` fails, the box's actual message (invalid code,
  trust, eligibility, …) is surfaced on the auth page instead of a bare EOF.
