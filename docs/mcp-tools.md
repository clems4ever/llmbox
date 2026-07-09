# MCP tools

The tools served by the `llmbox-mcp` binary (which forwards to the llmbox
server's box-control API). Add `llmbox-mcp` as a remote MCP server in your client
(see [Configuration](configuration.md#connecting-a-chatbot)).

| Tool             | Arguments | Returns |
|------------------|-----------|---------|
| `create_llmbox`  | `image?`, `box_id?`, `description?` | `box_id`, `instance_id`, `auth_url`, `auth_token`, `status`, `instructions` |
| `get_llmbox`     | `box_id` | `status` (pending/ready/error), `box_id`, `description`, `session_url` when ready |
| `list_llmboxes`  | – | the managed boxes (instance_id, name, box_id, description, image, state, phase, created) |
| `destroy_llmbox` | `box_id` | the destroyed box's box ID |
| `get_llmbox_logs` | `box_id`, `tail?` | `box_id`, `logs` (the box's recent, ANSI-stripped console output) |
| `exec_llmbox` | `box_id`, `command` | `box_id`, `stdout`, `stderr`, `exit_code` |
| `create_llmbox_proxy` | `box_id`, `port`, `description?` | `box_id`, `port`, `url` (open it in a browser), `description`, `instructions` |
| `delete_llmbox_proxy` | `box_id`, `port` | `box_id`, `port` |
| `list_llmbox_proxies` | `box_id?` | the enabled proxies (`box_id`, `port`, `url`, `slug`, `spoke`, `description`) |

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
is disabled. Requests to a proxy are gated by the same sign-in that gates box
activation. See [Proxying box HTTP ports](proxy.md).
