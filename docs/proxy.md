# Proxying box HTTP ports

llmbox can expose an HTTP server running **inside a box** to a human's browser,
so a user can ask Claude to "start a dev server and let me see it" and get back a
working URL. Proxies are **default-deny**: nothing is reachable until the agent
explicitly enables a proxy (over MCP or the admin UI).

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

When activation auth is configured, a proxy request must carry a signed-in
session allowed to activate boxes — the same gate as box activation. The login
cookie is host-scoped by default, so to share one sign-in between the main UI and
the per-proxy sub-domains, set the parent domain both share:

```yaml
auth:
  cookie_domain: ".example.com"
```

With no auth provider configured, proxying is open (like the rest of the server,
which then relies on a front authenticating proxy) — do not expose it to
untrusted networks in that mode.

## Local vs. remote spokes

- A box on the **local** spoke is reverse-proxied with a live connection, so it
  **streams**: WebSockets, SSE, and large transfers all work.
- A box on a **remote** spoke is reached over the cluster transport via the
  `proxy_http` verb, which **buffers** each request and response into one frame.
  Ordinary HTTP and single-page apps (static assets + JSON APIs) work; live
  streaming to a remote box (WebSocket/SSE) does not, and very large bodies are
  capped. The same managed-only resolution applies, so the verb can only reach a
  port inside one of the spoke's own boxes — never an arbitrary host address.

## Other notes

- For a containerized hub (e.g. the Docker Compose deployment), the hub must be
  able to reach a **local**-spoke box's Docker network — run the hub as a host
  process, or attach it to the box networks (e.g. via `box_peers`). A host-process
  hub reaches box bridge IPs directly. Remote-spoke proxying has no such
  requirement (the spoke reaches its own boxes).
