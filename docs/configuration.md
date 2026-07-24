# Running & configuration

How to run the server, drive the box-control API, and what every config key does.

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
(under `/api/v1/`) and the UI (admin dashboard, sign-in, health) together. The API is
authenticated: headless callers (scripts, other services) present an API key as a
bearer token, and the admin web app authenticates with the signed-in admin's
login cookie plus a CSRF header. Mint keys on the hub host with
`llmbox-server apikey add --name <label> [--ttl 8760h]` (list/delete likewise);
only the key's SHA-256 lands in the state file. See [Driving the box-control
API](#driving-the-box-control-api) for how a chatbot or automation uses it.

Or use [`docker-compose.yml`](../docker-compose.yml) (`docker compose up --build`),
which wires up the Docker socket, the docker group, and a persisted session
volume — see [State persistence](operations.md#state-persistence) for the
one-time `chown` the mounted volume needs.

Serve over TLS in production: the admin sign-in and login cookie should not
travel in clear text. Either terminate TLS at a reverse proxy in front, or set the
`tls:` block (`enabled`, `cert_file`, `key_file`) to have llmbox serve HTTPS
directly. A loud warning is logged at startup whenever it serves plaintext.

## Driving the box-control API

A chatbot or any automation drives boxes over the box-control JSON API under
`/api/v1/` (the server's `http_addr`, default `:8080`). Every call is a `POST`
carrying an API key as a bearer token. Mint one on the hub host (against the hub's
state file):

```bash
llmbox-server apikey add --name automation
```

Then create a box, run a command in it, and expose one of its ports:

```bash
KEY=lbx_...
BASE=http://llmbox:8080

# Create a box (its workload is provisioned by the spoke's --init-script).
curl -sS -H "Authorization: Bearer $KEY" \
  -d '{"opts":{"BoxID":"my-box"}}' "$BASE/api/v1/create-box"

# Run a command inside it.
curl -sS -H "Authorization: Bearer $KEY" \
  -d '{"box_id":"my-box","command":"echo hi"}' "$BASE/api/v1/box-exec"

# Expose an HTTP server the box listens on (returns a URL the user opens).
curl -sS -H "Authorization: Bearer $KEY" \
  -d '{"box_id":"my-box","port":8000}' "$BASE/api/v1/create-proxy"
```

The full set of endpoints — `create-box`, `lookup-box`, `list-boxes`,
`destroy-box`, `pause-box`, `resume-box`, `box-exec`, `spoke-statuses`, and the
proxy verbs `create-proxy` / `delete-proxy` / `list-proxies` — is defined in
[`internal/shared/api`](../internal/shared/api). The same package ships a Go
`api.Client` that speaks the API, so a Go caller can drive boxes without
hand-rolling the HTTP requests.

## Configuration

llmbox reads a single **YAML config file**, selected with `--config <path>`
(default `./llmbox.yaml`). When the default file is absent, the built-in defaults
below are used; an explicitly named missing or invalid file is a hard error.
Copy [`llmbox.example.yaml`](../llmbox.example.yaml) and edit it. Every field is
optional:

| YAML key       | Default                   | Purpose |
|----------------|---------------------------|---------|
| `http_addr`    | `:8080`                   | Single listen address for the whole server: the box-control API (`/api/v1/`, authenticated by API key or admin session) and the UI (admin dashboard, sign-in, health). |
| `public_url`   | `http://localhost:8080`   | External base URL used to build sign-in redirect links. **Set this in production.** |
| `state_file`   | `llmbox-sessions.db`      | SQLite file persisting the box registry, API keys, login sessions, and cluster records across restarts (see [State persistence](operations.md#state-persistence)). |
| `hooks`        | (empty)                   | List of [box lifecycle hook](hooks.md) executables. |
| `auth`         | (disabled)                | Admin sign-in (Google and/or GitHub) gating the admin UI and the per-box HTTP proxies (see [Authentication](authentication.md)). |
| `proxy`        | (disabled)                | Expose box HTTP ports at `<slug>.<base_domain>` (see [Proxying box HTTP ports](proxy.md)). |
| `tls`          | (disabled)                | Serve HTTPS directly (`cert_file`/`key_file`) instead of behind a TLS-terminating proxy. |

Unknown keys in the config file are rejected so typos surface as errors.

The hub runs **no box backend of its own** — every box runs on a
[spoke](hub-and-spoke.md) — so the hub config holds no box-provisioning knobs.
The box image, backend, per-box resource caps, and private-registry credentials
are all set with `llmbox-spoke` flags on the host that actually launches the
box (e.g. `--image`, `--box-memory-mb`, `--registry`). Run `llmbox-spoke --help`
for the full list.
