# llmbox

[![CI](https://github.com/clems4ever/llmbox/actions/workflows/ci.yml/badge.svg)](https://github.com/clems4ever/llmbox/actions/workflows/ci.yml)
[![coverage](.github/badges/coverage.svg)](https://github.com/clems4ever/llmbox/actions/workflows/ci.yml)
[![Build and push images](https://github.com/clems4ever/llmbox/actions/workflows/docker.yml/badge.svg)](https://github.com/clems4ever/llmbox/actions/workflows/docker.yml)

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
  └─ get_llmbox(box_id) ──▶ returns the session URL once ready
```

Boxes that are never authenticated are destroyed after `auth_ttl`
(default 5 minutes) — see [Orphan cleanup](#orphan-cleanup).

## MCP tools

| Tool             | Arguments | Returns |
|------------------|-----------|---------|
| `create_llmbox`  | `image?`, `box_id?`, `description?` | `box_id`, `container_id`, `auth_url`, `auth_token`, `status`, `instructions` |
| `get_llmbox`     | `box_id` | `status` (pending/ready/error), `box_id`, `description`, `session_url` when ready |
| `list_llmboxes`  | – | the managed boxes (container_id, name, box_id, description, image, state, phase, created) |
| `destroy_llmbox` | `box_id` | the destroyed box's box ID |
| `get_llmbox_logs` | `box_id`, `tail?` | `box_id`, `logs` (the box's recent, ANSI-stripped console output) |
| `exec_llmbox` | `box_id`, `command` | `box_id`, `stdout`, `stderr`, `exit_code` |

`box_id` and `description` on `create_llmbox` are optional. When set, `box_id`
is the identifier you use to reference the box afterwards and is also applied as
the box's container hostname (so it shows up as the box's name in claude.ai/code);
it **must be unique** across boxes — a duplicate is rejected with a clear error so
the caller can pick another. Both are surfaced again by `get_llmbox` and
`list_llmboxes`. `get_llmbox` is keyed by `box_id` (case-insensitive), so set one
at create time if you want to poll a box's status; boxes created without a box ID
can still be seen via `list_llmboxes`. `get_llmbox_logs` is likewise keyed by
`box_id` and returns the box's recent console output (ANSI-stripped), bounded to
the last `tail` lines (a sensible default applies when `tail` is omitted).
`exec_llmbox` is also keyed by `box_id`: it runs `command` inside the box via
`/bin/sh -c` and returns
its `stdout`, `stderr`, and `exit_code` (a non-zero exit is reported in the result,
not as a tool error; each stream is capped to keep the payload bounded). Destroying
a box stops it gracefully (SIGTERM, then SIGKILL after a timeout) before removing it.

## Components

| Path                 | What it is |
|----------------------|------------|
| `cmd/llmbox`         | Entry point: opens the session store, runs the HTTP server (MCP + auth pages) and the reaper. |
| `internal/docker`    | Box lifecycle over the Docker Engine API (create with image auto-pull + box-ID uniqueness, login-capture, code-submit, graceful destroy, reap). |
| `internal/server`    | Session registry (persisted to bbolt), MCP tools, auth web pages, reaper loop. |
| `Dockerfile`         | Image for **this server** (`llmbox`). It bakes in the standalone Claude binary, which the server injects into each box at creation. |

Boxes run on a plain base image (`claude_image`, default
`debian:bookworm-slim`): the server **injects** the standalone Claude binary and
a small `~/.claude.json` seed into each box at creation, and runs it as root with
`HOME=/root` and a `/workspace` working directory — so nothing Claude-specific
needs to be baked into the base image. Any glibc image with `/bin/sh`,
`util-linux` (for `script`), and CA certificates works.

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

llmbox reads a single **YAML config file**, selected with `--config <path>`
(default `./llmbox.yaml`). When the default file is absent, the built-in defaults
below are used; an explicitly named missing or invalid file is a hard error.
Copy [`llmbox.example.yaml`](llmbox.example.yaml) and edit it. Every field is
optional:

| YAML key       | Default                   | Purpose |
|----------------|---------------------------|---------|
| `http_addr`    | `:8080`                   | Listen address. |
| `public_url`   | `http://localhost:8080`   | External base URL used to build auth links. **Set this in production.** |
| `claude_image` | `debian:bookworm-slim`    | Base image launched per box. Any glibc image works — Claude is injected, not baked in. |
| `claude_bin`   | `/opt/llmbox/claude`      | Path (on the server) to the standalone Claude binary injected into each box. |
| `remote_args`  | `--spawn same-dir`        | Args passed to `claude remote-control`. |
| `auth_ttl`     | `5m`                      | Destroy un-authenticated boxes after this long (a Go duration string, e.g. `300s`, `5m`). |
| `state_file`   | `llmbox-sessions.db`      | bbolt file persisting the auth-session registry across restarts (see [Session persistence](#session-persistence)). |
| `hooks`        | (empty)                   | List of [box lifecycle hook](#box-lifecycle-hooks) executables. |
| `box_peers`    | (empty)                   | List of container names wired into every box's network (see [Box networking](#box-networking-and-isolation)). |
| `auth`         | (disabled)                | Require sign-in before a box can be activated (see [Authenticating activation](#authenticating-activation)). |

The Docker client itself is still configured the standard way (`DOCKER_HOST`,
etc.). Unknown keys in the config file are rejected so typos surface as errors.

If `claude_image` isn't present on the daemon, the server pulls it on the
first box creation and retries. Pulls use the daemon's existing credentials, so
for a **private** registry make sure the daemon is logged in (e.g. `docker
login`) or the image is pre-pulled.

## Authenticating activation

The auth-page URL is handed back from `create_llmbox` and therefore travels
**through the chatbot** (claude.ai's servers). The 256-bit token in it is the only
thing gating activation, so anyone who can see that traffic — and reaches the box
before the requester does — can activate the box with **their own** Claude
account, hijacking it and any per-box secrets your [hooks](#box-lifecycle-hooks)
inject. See [Status / caveats](#status--caveats) for the residual gaps.

To close this, enable a sign-in provider under `auth`. Activation then requires
the visitor to authenticate over a channel that **never** touches the chatbot
(OIDC, browser ↔ provider ↔ llmbox) and be in an allowed domain or email
allowlist; an unauthenticated visitor sees only the sign-in buttons, never the
code form or the session URL. Each provider is a dedicated config block so more
can be added later.

```yaml
auth:
  session_ttl: "1h"
  google:
    enabled: true
    client_id: "xxxxxxxx.apps.googleusercontent.com"
    client_secret_file: "/etc/llmbox/google-client-secret"  # secret read from file, never inlined
    allowed_domains: ["your-company.com"]
    allowed_emails: []
```

Setup notes:
- In the Google Cloud console, create an **OAuth 2.0 Client ID** (type *Web
  application*) and register the redirect URI `{public_url}/auth/google/callback`
  (the `redirect_url` field defaults to this).
- The client secret is **read from a file** (`client_secret_file`); it is never
  written in the YAML. Mount it read-only.
- Enabling a provider with no `allowed_domains` **and** no `allowed_emails` is a
  hard error — it would otherwise authorize every Google account.
- Login sessions are persisted server-side (in the `state_file` bbolt DB) so they
  survive restarts; `session_ttl` bounds their lifetime.

When `auth` is omitted, activation is unauthenticated (the server logs a warning
at startup) and behaves as before.

## Box lifecycle hooks

llmbox knows nothing about what any particular integration needs in a box. Instead
it runs **hooks** — external executables you point it at with the `hooks` config
list — at two points in a box's life:

- **`box.create`** — fires *before* the new box starts. A hook may return **files
  to inject** into the box (secrets, config, even binaries) and an opaque **state**
  string llmbox persists with the box.
- **`box.destroy`** — fires when the box is destroyed or reaped. llmbox replays the
  state the `box.create` hook returned, so the hook can undo whatever it did.

The wire protocol is plain JSON over the hook's stdin/stdout, defined in the
importable [`hookproto`](hookproto/hookproto.go) package. For each event llmbox
writes one `Request` to the hook's stdin and reads one `Response` from its stdout:

```jsonc
// stdin  (llmbox -> hook)
{ "event": "box.create", "box": { "box_id": "web-box", "image": "debian:bookworm-slim" } }

// stdout (hook -> llmbox)
{
  "files": [
    { "path": "/home/node/.secret/token", "content": "…", "mode": "0600", "uid": 1000, "gid": 1000 },
    { "path": "/usr/local/bin/tool", "content_base64": "…", "mode": "0755" }
  ],
  "state": "opaque-handle-for-destroy"
}
```

A non-zero exit fails the hook: on `box.create` that aborts the box (and any state
already returned is replayed to `box.destroy` for cleanup); on `box.destroy` it is
logged and ignored. Injected files are streamed into the **created-but-not-yet-
started** container via the Docker copy API, owned by the `uid`/`gid` the hook
chose — so a secret in a non-root user's home stays readable by that user, and is
never put in an env var or label where `docker inspect` would expose it. Hooks run
as subprocesses of this server, so they inherit its environment (pass a hook its
own config that way) and must be present in the `llmbox` container (bake them
into a derived image, or mount them in).

Writing a hook in Go is a few lines — implement a `hookproto.Handler` and call
`hookproto.Main`:

```go
func main() {
    hookproto.Main(func(req hookproto.Request) (hookproto.Response, error) {
        switch req.Event {
        case hookproto.EventBoxCreate:
            // mint a credential, return files to inject + state to remember
            return hookproto.Response{Files: ..., State: token}, nil
        case hookproto.EventBoxDestroy:
            // undo it, using req.State
            return hookproto.Response{}, revoke(req.State)
        }
        return hookproto.Response{}, nil
    })
}
```

**Reference hook — granular.** The
[granular-llmbox](https://github.com/clems4ever/granular-llmbox) repo implements a
hook that gives each box its own scoped identity for acting on the user's behalf
through a [granular](https://github.com/clems4ever/granular) authorization server:
on create it mints a subject token, installs the granular CLIs, config, and a
skill into the box, and on destroy it revokes the subject. It depends on llmbox
(for `hookproto`), never the other way around — which is the whole point of the
hook boundary.

### Box networking and isolation

A hook's box often needs to reach *other* containers (e.g. an integration's
resource servers) **without** being able to reach other boxes. llmbox uses a
hub-and-spoke layout instead of one shared network:

- Every box is created on its **own** dedicated Docker network (`llmboxnet-<id>`)
  and attached to nothing else, so no two boxes ever share a network — they
  cannot talk to each other.
- llmbox connects each container named in the `box_peers` config list into that
  per-box network, so the box reaches those peers by name while staying isolated.
- The network is torn down (and the peers disconnected from it) when the box is
  destroyed or reaped.

`box_peers` is a list of **container names**. When the peers run in a separate
compose project, give them a fixed `container_name:` so the name is stable, e.g.:

```yaml
services:
  granular-github:
    container_name: granular-github   # must match an entry in box_peers
```

## Session persistence

The auth-session registry (which token maps to which box, its authorize URL, and
status) is persisted to a [bbolt](https://github.com/etcd-io/bbolt) file at
`state_file`, so a server restart doesn't invalidate in-flight auth links.
On startup the server reconciles the saved sessions against Docker and drops any
whose box no longer exists.

To survive **container recreation**, point `state_file` at a mounted volume:

```yaml
# in llmbox.yaml
state_file: /var/lib/llmbox/sessions.db
```
```yaml
# in docker-compose.yml
volumes:
  - ./data/llmbox:/var/lib/llmbox
```

> [!IMPORTANT]
> The `llmbox` image runs as the distroless **`nonroot`** user
> (**UID/GID 65532**). The host directory you mount must be writable by that
> UID, or the server crash-loops with `permission denied` opening the store:
>
> ```bash
> mkdir -p ./data/llmbox && sudo chown -R 65532:65532 ./data/llmbox
> ```

### Box credentials across a restart

Once a box is authenticated, Claude writes its OAuth token to
`~/.claude/.credentials.json` **inside** the box. A `docker restart` preserves
the container's writable layer, so that token survives. The box entrypoint runs
on every start, but `claude auth login` is guarded: it only runs when no
credentials (and no `CLAUDE_CODE_OAUTH_TOKEN`) are present, so a restart skips
straight to remote-control without asking the user to authenticate again.

> [!NOTE]
> This covers `docker restart` only. **Recreating** a box (`docker rm` + a new
> `create_llmbox`) starts from a fresh filesystem and requires re-authenticating,
> since boxes do not bind-mount a host credentials file.

## Orphan cleanup

A box's auth phase is encoded in its container name — `llmbox-pending-<id>`
before authentication, renamed `llmbox-<id>` after. A reaper runs every 30s and
destroys any `llmbox-pending-*` box older than `auth_ttl`. Because
the phase lives in Docker (not just in memory), this also cleans up boxes
orphaned by a restart of this server, while leaving authenticated boxes running.

Safety: every box created here carries the `com.llmbox.managed=true` label;
list/destroy/reap are scoped to that label, so unrelated host containers are
never touched.

## Development

A [`Makefile`](Makefile) wraps the common tasks — run `make help` to list them.
The most-used:

```bash
make build              # build ./llmbox
make run CONFIG=llmbox.yaml   # run the server against a config file
make check              # gofmt-check + go vet + unit tests
make cover              # unit tests with a coverage total
make test-integration   # integration tests (needs Docker + a Claude binary)
make docker-build       # build the Docker image
```

## CI

`.github/workflows/ci.yml` runs `go vet` and the unit-test suite with coverage on
every push and pull request, publishing the coverage badge (see [Configuration](#configuration)).
`.github/workflows/docker.yml` builds the server image and pushes it to GitHub
Container Registry (`ghcr.io/<owner>/llmbox`) on pushes to `main` and version
tags. Pull requests build without pushing.

The Claude Code binary baked into the image is **pinned** to a specific stable
release — the `ARG CLAUDE_VERSION` line in the [`Dockerfile`](Dockerfile) is the
single source of truth. `.github/workflows/bump-claude.yml` runs daily, resolves
the latest stable release from `downloads.claude.ai`, and opens a PR bumping that
line when a newer version is available. Override per build with
`docker build --build-arg CLAUDE_VERSION=<x.y.z|stable|latest> .`.

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
- [Activation auth](#authenticating-activation) gates *activation* (closing the
  leaked-token hijack), but box **creation** over MCP is still unauthenticated, so
  a caller can create boxes (a DoS bounded by the un-authenticated reaper TTL).
  Authenticating MCP clients per-user, and binding a box to the specific
  initiator, are the natural follow-ups.
- The box wrapper pre-accepts the workspace-trust dialog (writes
  `projects[cwd].hasTrustDialogAccepted` to `~/.claude.json` after login), since
  `claude remote-control` otherwise aborts with "Workspace not trusted" in a
  fresh box. If a `SubmitCode` fails, the box's actual message (invalid code,
  trust, eligibility, …) is surfaced on the auth page instead of a bare EOF.
