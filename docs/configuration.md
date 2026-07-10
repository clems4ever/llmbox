# Running & configuration

How to run the server, connect a chatbot, and what every config key does.

## Running

The server drives the Docker daemon, so it needs the Docker socket. The image
runs as a non-root user, which must be allowed to use the socket via
`--group-add` (the socket's group, e.g. `docker`):

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

llmbox listens on a single port (`8080`): it serves the box-control JSON API
(under `/api/v1/`) and the UI (auth pages, admin, health) together. The API is
authenticated: headless callers (llmbox-mcp, scripts) present an API key as a
bearer token, and the admin web app authenticates with the signed-in admin's
login cookie plus a CSRF header. Mint keys on the hub host with
`llmbox-server apikey add --name <label> [--ttl 8760h]` (list/delete likewise);
only the key's SHA-256 lands in the state file. The MCP protocol
itself is served by the separate `llmbox-mcp` binary (see [Connecting a
chatbot](#connecting-a-chatbot)), which forwards to this box-control API.

Or use [`docker-compose.yml`](../docker-compose.yml) (`docker compose up --build`),
which wires up the Docker socket, the docker group, and a persisted session
volume — see [Session persistence](operations.md#session-persistence) for the
one-time `chown` the mounted volume needs.

Serve over TLS in production: the auth page receives the OAuth code, and the
auth URL — though it carries a 256-bit unguessable token — should not travel in
clear text. Either terminate TLS at a reverse proxy in front, or set the `tls:`
block (`enabled`, `cert_file`, `key_file`) to have llmbox serve HTTPS directly.
A loud warning is logged at startup whenever it serves plaintext.

## Connecting a chatbot

The MCP protocol is served by a separate binary, **`llmbox-mcp`**, which forwards
every tool call to the llmbox server's box-control API (the server's `http_addr`,
default `:8080`). Run it pointing at that upstream:

```bash
# Mint an API key once, on the hub host (against the hub's state file):
llmbox-server apikey add --name mcp

# Streamable HTTP on :8082, forwarding to the llmbox server's box-control API.
LLMBOX_API_KEY=lbx_... llmbox-mcp --upstream http://llmbox:8080 --addr :8082

# …or over stdio, for a chatbot that launches it as a child process.
LLMBOX_API_KEY=lbx_... llmbox-mcp --stdio --upstream http://llmbox:8080
```

Add `llmbox-mcp`'s URL (streamable HTTP) or its stdio command as a remote MCP
server in your client. It holds no state and needs no Docker socket, so it can
run anywhere that can reach the box-control API. Give it an API key minted with
`llmbox-server apikey add` via `--api-key` or `$LLMBOX_API_KEY`. See
[MCP tools](mcp-tools.md) for the full tool reference.

## Configuration

llmbox reads a single **YAML config file**, selected with `--config <path>`
(default `./llmbox.yaml`). When the default file is absent, the built-in defaults
below are used; an explicitly named missing or invalid file is a hard error.
Copy [`llmbox.example.yaml`](../llmbox.example.yaml) and edit it. Every field is
optional:

| YAML key       | Default                   | Purpose |
|----------------|---------------------------|---------|
| `http_addr`    | `:8080`                   | Single listen address for the whole server: the box-control API (`/api/v1/`, authenticated by API key or admin session) and the UI (auth pages, admin app, health). |
| `public_url`   | `http://localhost:8080`   | External base URL used to build auth links. **Set this in production.** |
| `auth_ttl`     | `5m`                      | Destroy un-authenticated boxes after this long (a Go duration string, e.g. `300s`, `5m`). |
| `state_file`   | `llmbox-sessions.db`      | SQLite file persisting the box/session registry, API keys, and cluster records across restarts (see [Session persistence](operations.md#session-persistence)). |
| `hooks`        | (empty)                   | List of [box lifecycle hook](hooks.md) executables. |
| `auth`         | (disabled)                | Require sign-in before a box can be activated (see [Authenticating activation](authentication.md)). |
| `proxy`        | (disabled)                | Expose box HTTP ports at `<slug>.<base_domain>` (see [Proxying box HTTP ports](proxy.md)). |
| `tls`          | (disabled)                | Serve HTTPS directly (`cert_file`/`key_file`) instead of behind a TLS-terminating proxy. |

Unknown keys in the config file are rejected so typos surface as errors.

The hub runs **no box backend of its own** — every box runs on a
[spoke](hub-and-spoke.md) — so the hub config holds no box-provisioning knobs.
The box image, backend, per-box resource caps, and private-registry credentials
are all set with `llmbox-spoke` flags on the host that actually launches the
box (e.g. `--image`, `--box-memory-mb`, `--registry`). Run `llmbox-spoke --help`
for the full list.
