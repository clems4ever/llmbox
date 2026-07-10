# Authenticating activation

The auth-page URL is handed back from `create_llmbox` and therefore travels
**through the chatbot** (claude.ai's servers). The 256-bit token in it is the only
thing gating activation, so anyone who can see that traffic — and reaches the box
before the requester does — can activate the box with **their own** Claude
account, hijacking it and any per-box secrets your [hooks](hooks.md) inject. See
[Status & caveats](../README.md#status--caveats) for the residual gaps.

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

## API authentication

Every box-control API call (`/api/v1/*`) is authenticated — the same single API
serves `llmbox-mcp`, scripts, and the admin web app:

- **API keys** (headless callers): mint one on the hub host with
  `llmbox-server apikey add --name <label> [--ttl 8760h]`; list and delete with
  `apikey list` / `apikey delete --id <prefix>|--name <label>`. The key is shown
  once and passed as a bearer token (`Authorization: Bearer lbx_...`); only its
  SHA-256 is stored, and every key expires.
- **Admin sessions** (the web app): a signed-in administrator's login cookie
  plus the session's CSRF token echoed in the `X-CSRF-Token` header. The app
  bootstraps this from `GET /api/v1/me`.

With no sign-in provider configured, only API keys can authenticate API calls.

## Trust model: the box-control API is single-tenant

API authentication proves **who** is calling; it is not per-box
**authorization**. Every authenticated principal — any valid API key, and any
signed-in admin — can act on **every** box: `exec_llmbox` runs an arbitrary
`/bin/sh -c` command in any box, `get_llmbox_logs` reads any box's console, and
`destroy_llmbox` removes any box. There is deliberately no per-box ownership
check: a box is not bound to the caller that created it.

This is safe under one assumption, which llmbox **requires**: a single trusted
tenant sits behind the box-control API. In the intended deployment `llmbox-mcp`
runs behind an authenticating proxy (e.g. oauth2-proxy) that only lets one
operator/organization through, so there is only ever one principal and "act on
any box" means "act on your own boxes". Treat anything that can reach the
box-control API — an API key, an admin cookie, or the identity the front proxy
lets through — as **fully privileged over every box on the hub**.

Two consequences follow from this boundary:

- **`exec_llmbox` is credential-equivalent.** A command run in a box can read
  that box's Claude credentials, so a caller with box-control access can reach
  every box's secrets. Scope and guard API keys accordingly.
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
