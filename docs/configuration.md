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
  -p 8081:8081 \
  llmbox --config /etc/llmbox/llmbox.yaml
```

llmbox listens on two ports: `8080` for the UI/API (auth pages, admin, health)
and `8081` for the box-control API, kept separate so the box-control port can sit
behind its own authenticating reverse proxy (e.g. oauth2-proxy). The MCP protocol
itself is served by the separate `llmbox-mcp` binary (see [Connecting a
chatbot](#connecting-a-chatbot)), which forwards to this box-control API.

Or use [`docker-compose.yml`](../docker-compose.yml) (`docker compose up --build`),
which wires up the Docker socket, the docker group, and a persisted session
volume — see [Session persistence](operations.md#session-persistence) for the
one-time `chown` the mounted volume needs.

Put it behind TLS in production: the auth page receives the OAuth code, and the
auth URL — though it carries a 256-bit unguessable token — should not travel in
clear text.

## Connecting a chatbot

The MCP protocol is served by a separate binary, **`llmbox-mcp`**, which forwards
every tool call to the llmbox server's box-control API (`mcp_addr`, default
`:8081`). Run it pointing at that upstream:

```bash
# Streamable HTTP on :8082, forwarding to the llmbox server's box-control API.
llmbox-mcp --upstream http://llmbox:8081 --addr :8082

# …or over stdio, for a chatbot that launches it as a child process.
llmbox-mcp --stdio --upstream http://llmbox:8081
```

Add `llmbox-mcp`'s URL (streamable HTTP) or its stdio command as a remote MCP
server in your client. It holds no state and needs no Docker socket, so it can
run anywhere that can reach the box-control API. Put it (or the box-control API)
behind an authenticating proxy before exposing it. See [MCP tools](mcp-tools.md)
for the full tool reference.

## Configuration

llmbox reads a single **YAML config file**, selected with `--config <path>`
(default `./llmbox.yaml`). When the default file is absent, the built-in defaults
below are used; an explicitly named missing or invalid file is a hard error.
Copy [`llmbox.example.yaml`](../llmbox.example.yaml) and edit it. Every field is
optional:

| YAML key       | Default                   | Purpose |
|----------------|---------------------------|---------|
| `http_addr`    | `:8080`                   | UI/API listen address (auth pages, admin, health). |
| `mcp_addr`     | `:8081`                   | Box-control API listen address (the `llmbox-mcp` binary forwards MCP tool calls here). Served on its own port so it can sit behind an authenticating proxy; never expose it directly to untrusted networks. |
| `public_url`   | `http://localhost:8080`   | External base URL used to build auth links. **Set this in production.** |
| `claude_image` | `ghcr.io/clems4ever/llmbox-box:latest` | Base image launched per box. Must bake in the standalone Claude binary, tini (PID 1), util-linux, and a CA bundle (see `Dockerfile.box`); build your own FROM it to add tooling. |
| `remote_args`  | `--spawn same-dir`        | Args passed to `claude remote-control`. |
| `auth_ttl`     | `5m`                      | Destroy un-authenticated boxes after this long (a Go duration string, e.g. `300s`, `5m`). |
| `state_file`   | `llmbox-sessions.db`      | bbolt file persisting the auth-session registry across restarts (see [Session persistence](operations.md#session-persistence)). |
| `hooks`        | (empty)                   | List of [box lifecycle hook](hooks.md) executables. |
| `box_peers`    | (empty)                   | List of container names wired into every box's network (see [Box networking](hooks.md#box-networking-and-isolation)). |
| `registries`   | (empty)                   | Per-registry pull credentials for box images on private registries (see below). |
| `auth`         | (disabled)                | Require sign-in before a box can be activated (see [Authenticating activation](authentication.md)). |

The Docker client itself is still configured the standard way (`DOCKER_HOST`,
etc.). Unknown keys in the config file are rejected so typos surface as errors.

If `claude_image` isn't present on the daemon, the server pulls it on the
first box creation and retries.

### Private registries

To pull box images from an authenticated registry, give llmbox the credentials
directly under `registries` instead of relying on the Docker daemon being logged
in. Each entry is matched against the host of the image being pulled; an image
whose registry has no entry is pulled anonymously. The password/token is read
from a file and never inlined in the YAML. On a [spoke](hub-and-spoke.md),
configure this where the box image is actually pulled.

```yaml
registries:
  - registry: "ghcr.io"          # registry host; use "docker.io" for Docker Hub
    username: "your-github-user"
    password_file: "/etc/llmbox/ghcr-token"   # a GitHub PAT with read:packages
```
