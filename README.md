# llmbox-mcp

An [MCP](https://modelcontextprotocol.io) server for spinning up **sandboxed
Claude instances** ("llmboxes") on demand. From a chatbot you say *"create an
llmbox"*; you get back a URL; you open it, sign in with **your own** Claude
account, and the sandbox activates — driveable from claude.ai/code or the mobile
app via Claude's Remote Control.

Each box is a container running Claude Code in remote-control mode, authenticated
by the end user. Built with the official
[Go SDK](https://github.com/modelcontextprotocol/go-sdk).

## The auth secret never touches the chatbot

The OAuth code exchanges for a full-scope account token, so it must never enter
the model's context. This server is split accordingly — one process, two
front-ends on the same HTTP port:

| Path            | Audience | Carries |
|-----------------|----------|---------|
| `/` (root)      | the chatbot (MCP over streamable HTTP) | box IDs + the **auth page URL** only |
| `/auth/{token}` | the human, in a browser | the **OAuth code** (browser → this server → container stdin) |

The code travels from the user's browser to the box's `claude auth login`
process; it is never an MCP input or output and is never logged.

## Flow

```
chat: "create an llmbox"
  └─ create_llmbox ──▶ starts a box parked at `claude auth login`,
                       captures its OAuth authorize URL,
                       returns  https://YOUR_HOST/auth/<token>   (+ auth_token)

user opens that URL ──▶ "Sign in with Claude" (their account) ──▶ copies the code
                   ──▶ pastes it into the page ──▶ server feeds it to the box

box finishes login ──▶ `claude remote-control` starts ──▶ session URL
  └─ get_llmbox(hostname) ──▶ returns the session URL once ready
```

Boxes that are never authenticated are destroyed after `LLMBOX_AUTH_TTL_SECONDS`
(default 5 min) — see [Orphan cleanup](#orphan-cleanup).

## MCP tools

| Tool             | Arguments | Returns |
|------------------|-----------|---------|
| `create_llmbox`  | `image?`, `hostname?`, `description?` | `box_id`, `auth_url`, `auth_token`, `status`, `instructions` |
| `get_llmbox`     | `hostname` | `status` (pending/ready/error), `hostname`, `description`, `session_url` when ready |
| `list_llmboxes`  | – | the managed boxes (id, name, hostname, description, image, state, phase, created) |
| `destroy_llmbox` | `box` (ID or name) | the destroyed box |

`hostname` and `description` on `create_llmbox` are optional. When set, `hostname`
becomes the box's container hostname and **must be unique** across boxes — a
duplicate is rejected with a clear error so the caller can pick another. Both are
surfaced again by `get_llmbox` and `list_llmboxes`. `get_llmbox` is keyed by
`hostname` (case-insensitive), so set one at create time if you want to poll a
box's status; boxes created without a hostname can still be seen via
`list_llmboxes`. Destroying a box stops it gracefully (SIGTERM, then SIGKILL
after a timeout) before removing it.

## Components

| Path                 | What it is |
|----------------------|------------|
| `cmd/llmbox-mcp`     | Entry point: opens the session store, runs the HTTP server (MCP + auth pages) and the reaper. |
| `internal/docker`    | Box lifecycle over the Docker Engine API (create with image auto-pull + hostname uniqueness, login-capture, code-submit, graceful destroy, reap). |
| `internal/server`    | Session registry (persisted to bbolt), MCP tools, auth web pages, reaper loop. |
| `Dockerfile.claude`  | Image for **Claude Code remote-control** (`claude-remote`). |
| `Dockerfile.mcp`     | Image for **this server** (`llmbox-mcp`). |

The two Dockerfiles use distinct extensions (`.claude` / `.mcp`) and build two
independent images.

## Running

The server drives the Docker daemon, so it needs the Docker socket. The image
runs as a non-root user, which must be allowed to use the socket via
`--group-add` (the socket's group, e.g. `docker`):

```bash
docker build -f Dockerfile.claude -t claude-remote .
docker build -f Dockerfile.mcp    -t llmbox-mcp .

docker run -d --name llmbox-mcp \
  -v /var/run/docker.sock:/var/run/docker.sock \
  --group-add "$(stat -c '%g' /var/run/docker.sock)" \
  -p 8080:8080 \
  -e LLMBOX_PUBLIC_URL=https://boxes.example.com \
  llmbox-mcp
```

Or use [`docker-compose.yml`](docker-compose.yml) (`docker compose up --build`),
which wires up the Docker socket, the docker group, and a persisted session
volume — see [Session persistence](#session-persistence) for the one-time
`chown` the mounted volume needs.

Put it behind TLS in production: the auth page receives the OAuth code, and the
auth URL — though it carries a 256-bit unguessable token — should not travel in
clear text.

### Connecting a chatbot

`create_llmbox` etc. are served at the root, `https://boxes.example.com/`
(streamable HTTP). Add that as a remote MCP server in your client.

## Configuration

| Env var                   | Default                   | Purpose |
|---------------------------|---------------------------|---------|
| `LLMBOX_HTTP_ADDR`        | `:8080`                   | Listen address. |
| `LLMBOX_PUBLIC_URL`       | `http://localhost:8080`   | External base URL used to build auth links. **Set this in production.** |
| `LLMBOX_CLAUDE_IMAGE`     | `claude-remote`           | Image launched per box. |
| `LLMBOX_REMOTE_ARGS`      | `--spawn same-dir`        | Args passed to `claude remote-control`. |
| `LLMBOX_AUTH_TTL_SECONDS` | `300`                     | Destroy un-authenticated boxes after this long. |
| `LLMBOX_STATE_FILE`       | `llmbox-sessions.db`      | bbolt file persisting the auth-session registry across restarts (see [Session persistence](#session-persistence)). |
| `LLMBOX_CAPTURE_DIR`      | (unset → disabled)        | **Host** directory for per-box network captures; when set, each box gets a tcpdump sidecar (see [Traffic capture](#traffic-capture)). |
| `LLMBOX_CAPTURE_IMAGE`    | `nicolaka/netshoot`       | Image used for the capture sidecar (any image with `tcpdump`). |
| `DOCKER_HOST`, etc.       | (Docker default)          | Standard Docker client configuration. |

If `LLMBOX_CLAUDE_IMAGE` isn't present on the daemon, the server pulls it on the
first box creation and retries. Pulls use the daemon's existing credentials, so
for a **private** registry make sure the daemon is logged in (e.g. `docker
login`) or the image is pre-pulled.

## Session persistence

The auth-session registry (which token maps to which box, its authorize URL, and
status) is persisted to a [bbolt](https://github.com/etcd-io/bbolt) file at
`LLMBOX_STATE_FILE`, so a server restart doesn't invalidate in-flight auth links.
On startup the server reconciles the saved sessions against Docker and drops any
whose box no longer exists.

To survive **container recreation**, put that file on a mounted volume:

```yaml
environment:
  LLMBOX_STATE_FILE: /var/lib/llmbox/sessions.db
volumes:
  - ./data/llmbox:/var/lib/llmbox
```

> [!IMPORTANT]
> The `llmbox-mcp` image runs as the distroless **`nonroot`** user
> (**UID/GID 65532**). The host directory you mount must be writable by that
> UID, or the server crash-loops with `permission denied` opening the store:
>
> ```bash
> mkdir -p ./data/llmbox && sudo chown -R 65532:65532 ./data/llmbox
> ```

## Traffic capture

Set `LLMBOX_CAPTURE_DIR` to a **host** directory and every box gets a `tcpdump`
**sidecar** that shares the box's network namespace and writes a rotating
`.pcap` (named `<box-id>.pcap`, ~500 MB max per box) into that directory. The
sidecar starts before the box's first connection (so the OAuth login is captured
too) and is removed when the box is destroyed or reaped. The box can't tamper
with it — it runs in a separate container with only `NET_RAW`.

```yaml
environment:
  LLMBOX_CAPTURE_DIR: /var/lib/llmbox/captures   # a path on the Docker host
  # LLMBOX_CAPTURE_IMAGE: nicolaka/netshoot       # optional override
volumes:
  - ./data/captures:/var/lib/llmbox/captures
```

Open the `.pcap` files in Wireshark/`tshark` to explore them.

> [!NOTE]
> `LLMBOX_CAPTURE_DIR` is a **host** path (it's bind-mounted into the sidecar by
> the Docker daemon), not a path inside the `llmbox-mcp` container. It must exist
> on the host. The sidecar runs `tcpdump` as root, so the `.pcap` files are
> root-owned.

> [!IMPORTANT]
> Box traffic is almost entirely TLS, so the capture shows **metadata** —
> destination IPs, TLS SNI hostnames, timing, and byte volumes — **not**
> decrypted request/response bodies. Seeing payloads would require terminating
> TLS through an MITM proxy with its CA trusted inside the box.

## Orphan cleanup

A box's auth phase is encoded in its container name — `llmbox-pending-<id>`
before authentication, renamed `llmbox-<id>` after. A reaper runs every 30s and
destroys any `llmbox-pending-*` box older than `LLMBOX_AUTH_TTL_SECONDS`. Because
the phase lives in Docker (not just in memory), this also cleans up boxes
orphaned by a restart of this server, while leaving authenticated boxes running.

Safety: every box created here carries the `com.llmbox.managed=true` label;
list/destroy/reap are scoped to that label, so unrelated host containers are
never touched.

## CI

`.github/workflows/docker.yml` builds both images and pushes them to GitHub
Container Registry (`ghcr.io/<owner>/claude-remote` and
`ghcr.io/<owner>/llmbox-mcp`) on pushes to `main` and version tags. Pull requests
build without pushing.

## Tested

`go test ./...` covers the Docker layer (a faked Docker client; the attach
stream is driven over an in-memory pipe) and the server (MCP tools over an
in-memory transport + the auth web handlers). An integration test against a real
container is gated behind a build tag:

```bash
go test -tags=integration -run Integration -v ./internal/docker/
```

It confirms a live `claude auth login` emits a real OAuth authorize URL (PKCE +
out-of-band code callback) that the manager captures.

## Status / caveats

- The create → authorize-URL → auth-page path is verified end-to-end (including a
  real container and the live HTTP/MCP stack). The final **code → session URL**
  exchange needs a human to authorize in a browser; the wrapper that runs
  `claude auth login` then `claude remote-control` is in
  [`internal/docker/manager.go`](internal/docker/manager.go) and is easy to tweak
  if your Claude version's prompts differ.
- Each box consumes a session on the **end user's** Claude subscription. That is
  the intended model; be deliberate about who you let create boxes.
- The box wrapper pre-accepts the workspace-trust dialog (writes
  `projects[cwd].hasTrustDialogAccepted` to `~/.claude.json` after login), since
  `claude remote-control` otherwise aborts with "Workspace not trusted" in a
  fresh box. If a `SubmitCode` fails, the box's actual message (invalid code,
  trust, eligibility, …) is surfaced on the auth page instead of a bare EOF.
