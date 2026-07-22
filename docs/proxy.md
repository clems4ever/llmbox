# Proxying box HTTP ports

llmbox can expose an HTTP server running **inside a box** to a human's browser,
so a service running in the box (say a dev server) can be reached at a working
URL. Proxies are **default-deny**: nothing is reachable until a proxy is
explicitly enabled (over the box-control API or the admin UI).

## How it works

```
browser ──▶ https://<slug>.proxy.example.com/...
              │  (wildcard DNS + TLS at your reverse proxy)
              ▼
            hub (APIHandler)  ── host is a proxy sub-domain?
              │  authorize the signed-in user
              │  resolve <slug> ─▶ (box, port, spoke)
              ▼
            box dialer ──▶ the box's port on its own Docker network
```

- Each enabled proxy gets an unguessable **slug** and is reached at its own
  **sub-domain** `https://<slug>.<base_domain>/`. Because the box's app is served
  at the sub-domain root, single-page apps and servers that emit absolute paths
  (`/static/app.js`, `fetch('/api')`, client-side routing, WebSockets, SSE) work
  **without any path rewriting**.
- The hub reaches the box over the spoke's box dialer, which connects to the
  box's address on its **own dedicated Docker network** — the box publishes no
  host ports, and the managed-only resolution means a proxy can never be pointed
  at an arbitrary container.
- A proxy is removed automatically when its box is destroyed or reaped.

## Enabling it

Set a base domain in the config:

```yaml
proxy:
  base_domain: "proxy.example.com"
```

and provide, at your TLS-terminating reverse proxy in front of the hub:

- a **wildcard DNS** record `*.proxy.example.com` pointing at the hub, and
- a **wildcard TLS certificate** for `*.proxy.example.com`.

## Authentication

When admin sign-in is configured, a proxy request must carry a signed-in **admin**
session — the same gate as the admin UI (see [Authentication](authentication.md)).
The login cookie is host-scoped by default, so to share one sign-in between the
main UI and the per-proxy sub-domains, set the bare parent domain both share (no
leading dot, no port):

```yaml
auth:
  cookie_domain: "example.com"
```

A signed-out **browser** that opens a proxy URL is redirected to a sign-in page
on the main host, carrying the proxy URL as the return target; once signed in,
the shared cookie lets the same URL through and the user lands back where they
started. (Non-browser requests — XHR, WebSocket, anything that isn't a top-level
navigation — get a plain `401` instead, so a redirect to HTML can't corrupt
them.) The sign-in page is responsive, dropping the card framing to fill a phone
screen. These images are **captured by the end-to-end test** and refreshed by CI
on the pull request that changes the UI; see [Testing](development.md#testing).

**When a session expires while a proxied app is open**, that same navigation
redirect only fires on the *next* full page load — which a single-page app never
makes, so its background requests would just start failing and the app would
appear to have silently disconnected. To close that gap the hub injects a tiny
**session watcher** into proxied HTML documents: it polls a reserved same-origin
endpoint (`/.llmbox/proxy-auth-check`, answered by the hub and never forwarded to
the box) on an interval, and the moment that poll returns `401` it navigates the
tab to the sign-in page — carrying the current page as the return target, so the
user is sent to log in rather than left on a dead app. The watcher only polls
that one endpoint (never the app's own requests), so a legitimate `401` from the
app can't trigger a spurious redirect, and it is injected only into uncompressed
HTML documents — XHR/JSON, sub-resources, and WebSocket traffic are untouched.

| Sign in | On mobile |
|---------|-----------|
| ![The proxy sign-in page](../.github/screenshots/signin-page.png) | ![The proxy sign-in page on a phone-sized screen](../.github/screenshots/signin-page-mobile.png) |

With no auth provider configured, proxying is open (like the admin UI, which then
relies on a front authenticating proxy) — do not expose it to untrusted networks
in that mode.

## How the box is reached

Every box runs on a spoke, and the hub reaches its port by opening a live **byte
tunnel** to it over the cluster transport (`stream_open`/`stream_data`/`stream_close`
frames): the spoke dials the box with `DialBox` and splices the two together. The
reverse proxy runs over that live connection, so it **streams** — WebSockets, SSE,
and large transfers all work, to a box on any spoke. The same managed-only
resolution applies, so a tunnel can only reach a port inside one of the spoke's
own boxes — never an arbitrary host address.

## Box-initiated port publishing

The workload running **inside** a box can publish, list, and unpublish its own
box's ports without any credential: every box gets a local control socket at
`/run/llmbox/boxapi.sock` (served from **outside** the sandbox — by the spoke
through the Docker bind mount, or via a per-VM vsock listener on Firecracker) that
it `curl`s (`/v1/open_port`, `/v1/close_port`, `/v1/list_ports`).

So an **agent** inside the box discovers this API without being told about it,
the guest installs a Claude Code skill describing it into the box at startup —
under `/home/agent/.claude/skills/llmbox-ports/` by default (override with the
guest's `--skills-dir`; empty disables it). The skill is embedded in the
`llmbox-guest` binary (`internal/guest/skills/`), so refreshing the guest
refreshes the skill in place, and it is chowned to the box's unprivileged user so
the agent can read it.

The request body carries only a port and description — never a box or spoke
identity. Scoping is enforced twice, both outside the sandbox:

1. the **spoke** stamps the box ID from its own record of which per-box channel
   the request arrived on, so nothing inside a box can address another box;
2. the **hub** takes the spoke name from the authenticated cluster connection
   and verifies that box actually lives on that spoke before touching proxy
   state (the same `create-proxy` path the admin API uses, recorded as
   `box:<box id>`).

The control socket is deliberately a unix socket, not a TCP port: the proxy
data path only dials TCP ports inside the box, so the box can never publish its
own control API. A box created **without a box ID** cannot publish ports
(proxies are keyed by box ID); its calls fail with a clear explanation. When
proxying is disabled hub-wide, opening a port fails with the disabled message,
but closing still works so a box can always clean up after itself.

## Spoke-configured port publishing (`--publish-port`)

A spoke can expose a port on **every** box it creates, without anything running
inside the box, with the `--publish-port` flag (repeatable):

```
llmbox-spoke docker ... --publish-port 8080:web-app --publish-port 3000
```

Each value is `PORT[:DESCRIPTION]`. On a successful create the spoke returns
these ports to the hub, which — right after it registers the box, when the box
ID, spoke, and generation are all known — creates a proxy for each (recorded as
`spoke:<spoke name>`). The service does not need to be listening yet: the proxy
is just a slug→port mapping the reverse proxy dials on demand, so the URL is live
in the UI the moment the box appears and starts working as soon as something
listens on the port.

This is the deterministic counterpart to box-initiated publishing: use it to
expose a service a spoke's `--init-script` installs into every box (the init
script only needs to install and start the service — it should **not** try to
call `open_port` itself, because the box is not yet registered on the hub while
the init script runs). Publishing is best-effort: if proxying is disabled
hub-wide, or the box has no box ID, the ports are skipped with a log line rather
than failing box creation. Note that, like `--init-script`, this runs at box
**creation**; a paused box that is resumed keeps its proxy record but nothing
re-runs the init script, so restart the in-box service yourself if you rely on
resume.

## Other notes

- The hub never touches a box's network directly: the spoke reaches its own boxes
  and the hub only sees the tunnel over the cluster transport. So a containerized
  hub needs no access to any box's Docker network.
