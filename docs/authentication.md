# Authentication

llmbox has two authentication surfaces:

- **API authentication** for the box-control API (`/api/v1/*`) — API keys or an
  admin login session.
- **Admin sign-in** for humans — one or more sign-in providers (Google and/or
  GitHub) that gate both the **admin dashboard** and the per-box
  [HTTP proxies](proxy.md).

## Admin sign-in (Google / GitHub)

Enable one or more sign-in providers under `auth`. A visitor authenticates over a
channel that **never** touches the chatbot (browser ↔ provider ↔ llmbox), and is
authorized **only** if their verified email is in the **admin allow-list**
(`auth.admin.emails`). There is no per-provider domain/email allow-list — the
admin list is the single source of authorization.

```yaml
auth:
  session_ttl: "1h"
  cookie_domain: "example.com"   # share the login cookie across proxy sub-domains
  admin:
    emails: ["you@example.com", "teammate@example.com"]
  google:
    enabled: true
    client_id: "xxxxxxxx.apps.googleusercontent.com"
    client_secret_file: "/etc/llmbox/google-client-secret"  # secret read from file, never inlined
  github:
    enabled: true
    client_id: "Iv1.xxxxxxxxxxxxxxxx"
    client_secret_file: "/etc/llmbox/github-client-secret"  # secret read from file, never inlined
```

Each enabled provider renders its own button on the sign-in page; enable just one
or offer both. A signed-in admin can reach the admin dashboard and any box proxy;
an unauthenticated visitor sees only the sign-in page.

Setup notes:
- **Google** (OIDC): in the Google Cloud console, create an **OAuth 2.0 Client
  ID** (type *Web application*) and register the redirect URI
  `{public_url}/auth/google/callback` (the `redirect_url` field defaults to this).
- **GitHub** (OAuth2): register an **OAuth App** under *Settings → Developer
  settings → OAuth Apps* with **Authorization callback URL**
  `{public_url}/auth/github/callback` (the `redirect_url` field defaults to this).
  GitHub is not an OIDC provider, so after sign-in llmbox reads the account's
  **primary verified email** from the GitHub API (the `user:email` scope) and
  matches it against `auth.admin.emails`; an unverified email is never authorized.
- The client secret is **read from a file** (`client_secret_file`); it is never
  written in the YAML. Mount it read-only.
- Login sessions are persisted server-side (in the `state_file` SQLite DB) so they
  survive restarts; `session_ttl` bounds their lifetime.
- `cookie_domain` sets the bare parent domain the login cookie is scoped to (no
  leading dot, no port), so one sign-in is shared between the main UI and the
  per-proxy sub-domains. See [Proxy authentication](proxy.md#authentication).

When `auth` is omitted, the admin UI and the box proxies are unauthenticated (the
server logs a warning at startup) — rely on a front authenticating proxy in that
mode.

## API authentication

Every box-control API call (`/api/v1/*`) is authenticated — the same single API
serves scripts, other services, and the admin web app:

- **API keys** (headless callers): mint one on the hub host with
  `llmbox-server apikey add --name <label> [--ttl 8760h]`; list and delete with
  `apikey list` / `apikey delete --id <prefix>|--name <label>`. The key is shown
  once and passed as a bearer token (`Authorization: Bearer lbx_...`); only its
  SHA-256 is stored, and every key expires.

  > **Mint into the store the hub actually reads.** These subcommands (and the
  > `token` subcommands) operate directly on the hub's `state_file`. If the hub
  > was started with a customized `state_file` (e.g. via `--config`) but you run
  > `apikey add` without pointing at it, the key lands in the default
  > `llmbox-sessions.db` and the hub rejects it as `invalid or expired API key`.
  > Each subcommand prints the store it used to stderr and warns when it falls
  > back to the default; pass the hub's config with `--config <file>` (it reads
  > `state_file` from it) or name the store directly with `--state-file <path>`.
- **Admin sessions** (the web app): a signed-in administrator's login cookie
  plus the session's CSRF token echoed in the `X-CSRF-Token` header. The app
  bootstraps this from `GET /api/v1/me`.

With no sign-in provider configured, only API keys can authenticate API calls.

## Trust model: the box-control API is single-tenant

API authentication proves **who** is calling; it is not per-box
**authorization**. Every authenticated principal — any valid API key, and any
signed-in admin — can act on **every** box: `box-exec` runs an arbitrary
`/bin/sh -c` command in any box, and `destroy-box` removes any box. There is
deliberately no per-box ownership check: a box is not bound to the caller that
created it.

This is safe under one assumption, which llmbox **requires**: a single trusted
tenant sits behind the box-control API. In the intended deployment the
box-control API runs behind an authenticating proxy (e.g. oauth2-proxy) that only lets one
operator/organization through, so there is only ever one principal and "act on
any box" means "act on your own boxes". Treat anything that can reach the
box-control API — an API key, an admin cookie, or the identity the front proxy
lets through — as **fully privileged over every box on the hub**.

Two consequences follow from this boundary:

- **`box-exec` is credential-equivalent.** A command run in a box can read
  that box's secrets (whatever the init script or [hooks](hooks.md) injected), so
  a caller with box-control access can reach every box's secrets. Scope and guard
  API keys accordingly.
- **Do not put mutually-distrusting users behind one hub.** Because there is no
  per-box isolation, sharing a single hub (or a single API key) across several
  users lets any of them exec into the others' boxes. For a multi-tenant setup,
  run **one hub per tenant** — or add per-box owner-scoping first (binding each
  box to the identity, e.g. the proxy's forwarded user, that created it).

The box itself cannot reach the box-control API: each box runs on its own
isolated network with only the reduced [box-port socket](proxy.md#box-initiated-port-publishing)
mounted in, so a prompt-injected guest cannot call these verbs against any box,
its own or another's. The trust boundary above is strictly about **external**
callers of the box-control API, not the sandboxed guest.
