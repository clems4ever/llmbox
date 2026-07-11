# MCP tools

The tools served by the `llmbox-mcp` binary (which forwards to the llmbox
server's box-control API). Add `llmbox-mcp` as a remote MCP server in your client
(see [Configuration](configuration.md#connecting-a-chatbot)).

| Tool             | Arguments | Returns |
|------------------|-----------|---------|
| `create_llmbox`  | `box_id`, `image?`, `description?`, `spoke?` | `box_id`, `instance_id` |
| `get_llmbox`     | `box_id` | `box_id`, `instance_id`, `description` |
| `list_llmboxes`  | – | the managed boxes (instance_id, name, box_id, description, image, state, phase, created) |
| `list_spokes`    | – | the connected spokes and the default spoke |
| `destroy_llmbox` | `box_id` | the destroyed box's box ID |
| `exec_llmbox` | `box_id`, `command` | `box_id`, `stdout`, `stderr`, `exit_code` |
| `create_llmbox_proxy` | `box_id`, `port`, `description?` | `box_id`, `port`, `url` (open it in a browser), `description`, `instructions` |
| `delete_llmbox_proxy` | `box_id`, `port` | `box_id`, `port` |
| `list_llmbox_proxies` | `box_id?` | the enabled proxies (`box_id`, `port`, `url`, `slug`, `spoke`, `description`) |

`box_id` on `create_llmbox` is **required** (`description` and `spoke` are
optional); it is the identifier you use to reference the box for every later verb
(get/exec/destroy/proxy) and is also applied as the box's hostname. The hub
addresses boxes only by their box ID — there is no other handle — so it **must be
unique** across boxes; a duplicate is rejected with a clear error so the caller
can pick another. The box's workload is provisioned by the spoke's init script;
`create_llmbox` returns as soon as the box is created (its `box_id` and an opaque
`instance_id`). The returned `instance_id` is an opaque backend generation token
for the box's current incarnation, surfaced for information only — never address a
box by it. A box whose init script failed is reported with phase **`broken`** by
`list_llmboxes`. `box_id` and `description` are surfaced again by `get_llmbox`
and `list_llmboxes`. `get_llmbox` is keyed by `box_id` (case-insensitive).
`exec_llmbox` is also keyed by `box_id`: it runs `command` inside the box via
`/bin/sh -c` and returns
its `stdout`, `stderr`, and `exit_code` (a non-zero exit is reported in the result,
not as a tool error; each stream is capped to keep the payload bounded). Destroying
a box stops it gracefully (SIGTERM, then SIGKILL after a timeout) before removing it.

## Exposing a box's HTTP server

`create_llmbox_proxy` lets the user reach an HTTP server running **inside** a box
from their browser. The guest starts a server in the box (e.g. via `exec_llmbox`
or `pm2`), calls `create_llmbox_proxy` with the box's `box_id` and the `port` the
server listens on, and hands the returned `url` to the user. An optional
`description` can be attached at creation to record what the proxy is for; it is
echoed back by `create_llmbox_proxy` and surfaced by `list_llmbox_proxies` (and
in the admin UI). The description is set only on first creation — a repeated
`create_llmbox_proxy` for the same box and port returns the existing proxy
unchanged and ignores a newly supplied description. The proxy is
**default-deny**: a box port is unreachable until a proxy is enabled for it, and
the proxy is removed automatically when the box is destroyed. `delete_llmbox_proxy`
disables it; `list_llmbox_proxies` lists the enabled proxies, their URLs, and any
descriptions.

Each proxy is served at its own sub-domain (`https://<slug>.<base_domain>/`) so
single-page apps and servers that emit absolute paths work without rewriting. The
feature is only available when the operator has configured `proxy.base_domain`
(with the matching wildcard DNS and TLS); the tools report a clear error when it
is disabled. Requests to a proxy are gated by the same admin sign-in that gates
the admin UI. See [Proxying box HTTP ports](proxy.md).
