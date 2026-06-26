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
and `8081` for the MCP endpoint, kept separate so the MCP port can sit behind its
own authenticating reverse proxy (e.g. oauth2-proxy).

Or use [`docker-compose.yml`](../docker-compose.yml) (`docker compose up --build`),
which wires up the Docker socket, the docker group, and a persisted session
volume — see [Session persistence](operations.md#session-persistence) for the
one-time `chown` the mounted volume needs.

Put it behind TLS in production: the auth page receives the OAuth code, and the
auth URL — though it carries a 256-bit unguessable token — should not travel in
clear text.

## Connecting a chatbot

`create_llmbox` etc. are served at the root of the **MCP port** (`mcp_addr`,
default `:8081`), `https://boxes.example.com/` (streamable HTTP). Add that as a
remote MCP server in your client. See [MCP tools](mcp-tools.md) for the full tool
reference.

## Configuration

llmbox reads a single **YAML config file**, selected with `--config <path>`
(default `./llmbox.yaml`). When the default file is absent, the built-in defaults
below are used; an explicitly named missing or invalid file is a hard error.
Copy [`llmbox.example.yaml`](../llmbox.example.yaml) and edit it. Every field is
optional:

| YAML key       | Default                   | Purpose |
|----------------|---------------------------|---------|
| `http_addr`    | `:8080`                   | UI/API listen address (auth pages, admin, health). |
| `mcp_addr`     | `:8081`                   | MCP endpoint listen address. Served on its own port so it can sit behind an authenticating proxy; never expose it directly to untrusted networks. |
| `public_url`   | `http://localhost:8080`   | External base URL used to build auth links. **Set this in production.** |
| `claude_image` | `ghcr.io/clems4ever/llmbox-box:latest` | Base image launched per box. Any glibc image with a CA bundle works — Claude is injected, not 
baked in. |
| `claude_bin`   | `/opt/llmbox/claude`      | Path (on the server) to the standalone Claude binary injected into each box. |
| `remote_args`  | `--spawn same-dir`        | Args passed to `claude remote-control`. |
| `auth_ttl`     | `5m`                      | Destroy un-authenticated boxes after this long (a Go duration string, e.g. `300s`, `5m`). |
| `state_file`   | `llmbox-sessions.db`      | bbolt file persisting the auth-session registry across restarts (see [Session persistence](operations.md#session-persistence)). |
| `hooks`        | (empty)                   | List of [box lifecycle hook](hooks.md) executables. |
| `box_peers`    | (empty)                   | List of container names wired into every box's network (see [Box networking](hooks.md#box-networking-and-isolation)). |
| `auth`         | (disabled)                | Require sign-in before a box can be activated (see [Authenticating activation](authentication.md)). |

The Docker client itself is still configured the standard way (`DOCKER_HOST`,
etc.). Unknown keys in the config file are rejected so typos surface as errors.

If `claude_image` isn't present on the daemon, the server pulls it on the
first box creation and retries. Pulls use the daemon's existing credentials, so
for a **private** registry make sure the daemon is logged in (e.g. `docker
login`) or the image is pre-pulled.
