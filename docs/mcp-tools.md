# MCP tools

The tools served at the root MCP endpoint. Add the server as a remote MCP server
in your client (see [Configuration](configuration.md#connecting-a-chatbot)).

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
