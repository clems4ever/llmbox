# Hub-and-spoke clustering

Boxes run on **remote hosts** ("spokes") while a single **hub** (the llmbox
server the chatbot talks to over MCP) stays the only front-end. The **hub runs no
box backend of its own** — it is a pure router/registry, so every box runs on a
spoke started with `llmbox-spoke`. Even a single-host deployment runs one spoke
alongside the hub.

An operator mints a **join token** on the hub, starts a spoke with it, and the
spoke joins the cluster; the hub then places boxes on that spoke. A box created
with no explicit `spoke` runs on the **default spoke**, which an admin picks in
the admin UI (a stored setting); until one is set, an unqualified create is
refused.

## The wire boundary is a fixed set of box verbs

A spoke exposes **only** these verbs over the wire — it is never a generic Docker
proxy:

    CreateLLMBox, List, Destroy, Pause, Resume, Exec

This is the security boundary: a spoke holds a Docker socket (root-equivalent on
its host), so the protocol is a fixed allowlist of box verbs with validated
arguments, and the spoke re-validates every input rather than trusting the hub.
Each verb is bounded request/response (Exec buffers and caps its output;
CreateLLMBox runs the init script under a timeout).

The one exception is **HTTP proxying** to a box's port: to carry WebSocket and
SSE, the hub opens a raw byte **tunnel** to the box over the same connection
(`stream_open`/`stream_data`/`stream_close` frames, multiplexed by frame ID), and
the spoke splices it to the box port. So the protocol is framed request/response
for verbs plus a lightweight stream multiplex for the proxy — no heavyweight
session layer.

## Transport: WebSocket, spoke-dials-hub

Spokes live on edge hosts, often behind NAT, so the **spoke dials the hub** and
the hub pushes commands down that persistent, full-duplex connection. WebSocket
rides the hub's existing `net/http` server and TLS on the same port (route
`/spoke/connect`) and is reverse-proxy friendly. A small JSON `frame` carries
everything, correlated by an incrementing request ID so many in-flight verb calls
and proxy tunnels share one socket:

```
frame { Type: enroll|welcome|req|resp|err|stream_open|stream_data|stream_close, ID, Method, Payload, Data }
```

## Enrollment: join token → per-spoke bearer credential

Two-phase, so the long-lived credential is never the thing humans copy-paste:

1. **Join token** — minted on the hub by an operator with shell access:

       llmbox-server token create --name worker-1 [--ttl 1h]

   A high-entropy secret with the **spoke name baked in** and a TTL, stored
   **hashed** (SHA-256) in the hub's state file. Printed once, one-time use.
   List and revoke with `llmbox-server token list` / `token revoke`.

2. **Enrollment** — the spoke dials `/spoke/connect` and sends an `enroll` frame
   with the join token. The hub validates and consumes it (deleted on success),
   mints a **long-lived per-spoke bearer credential**, records the spoke, and
   replies `welcome`. The spoke persists the credential and reconnects with it
   thereafter, never needing the join token again. Bearer credentials are stored
   hashed too.

### Threat model

A spoke identity is powerful: whoever the hub treats as a spoke receives boxes
and exec traffic (user code/sessions). So the hub authenticates the spoke (join
token, then bearer), spoke verbs re-validate inputs, and the verb allowlist
guarantees the spoke never becomes a generic Docker proxy. A leaked join token is
bounded to enrolling one spoke (one-time, TTL'd, operator-visible).

**Managed-only enforcement.** Every verb targeting an existing box — `Destroy`,
`Pause`, `Resume`, `Exec` — resolves only to containers carrying the
`com.llmbox.managed` label. A hub that sends an ID for a container the spoke
didn't create gets a `no managed box matches` error and **no action**, so it can
never touch an arbitrary host container. This makes "the spoke can only ever act
on its own boxes" an enforced property of the box layer.

## Hub routing across spokes

The hub keeps a **spoke registry** (name → connection), populated by remote
spokes as they connect and disconnect. Box→spoke affinity lives on the session:
`create_llmbox` takes an optional `spoke`; when omitted the box runs on the
default spoke, and the resolved name is stored on the session. Per-box verbs
(get/exec/destroy/pause/resume) route to that spoke. Cluster-wide verbs fan out:

- `List` queries every registered spoke and aggregates (each box is tagged with
  its spoke).
- Restore reconciles each session against its spoke's list (a session whose spoke
  is currently disconnected is kept, not dropped, since the box may still be
  alive; only a de-enrolled spoke's sessions are purged).

Token and credential records live in the hub's SQLite state file
(`internal/hub/store`), alongside the box registry and API keys.

## CLI / config

- `llmbox-server token create --name <name> [--ttl 1h] [--state-file …]` —
  hub-side; prints the token once. Writes to the hub's state file (default
  `llmbox-sessions.db`; point `--state-file` at the running hub's `state_file`).
- `llmbox-spoke docker --hub wss://hub/spoke/connect --token <join-token>` — runs
  a spoke on the Docker backend; `llmbox-spoke firecracker …` runs the
  [Firecracker](firecracker.md) backend. The credential issued at first
  enrollment is saved to `~/.llmbox/llmbox-spoke.json` by default (override with
  `--state`), and the spoke reconnects from it afterwards without the token.
- The **spoke reads no config file** — every setting is a flag, so a host runs
  the single command the admin UI generates. Because the hub runs no box backend,
  the per-box knobs live entirely on the spoke: `--image`, `--box-memory-mb`,
  `--box-cpus`, `--box-pids-limit`, `--box-socket-dir`, `--box-peer`,
  `--init-script`, `--publish-port`, `--registry[-username|-password-file]`. Run
  `llmbox-spoke --help` for the full list.

#### Customising boxes with an init script

`--init-script <path>` points at a script **on the spoke host** that runs inside
every box this spoke spawns, once at creation. **This is how a box gets its
workload**: the init script installs and starts whatever the box should run
(packages, dotfiles, a service, a seeded workspace) — llmbox itself runs no
workload. It applies to both the Docker and Firecracker backends and never
crosses the hub/spoke boundary.

- The script is read once when the spoke starts (a missing or empty file fails the
  spoke immediately), so editing it takes effect on the next spoke restart.
- It runs as the **box user** — root on the Docker backend, the unprivileged
  `agent` user (with passwordless `sudo`) on Firecracker — from that user's home
  directory, so use `sudo` for system-level changes on Firecracker.
- Give it a shebang (e.g. `#!/bin/sh`); it is executed directly.
- A **non-zero exit marks the box `broken`** rather than tearing it down: the box
  is kept, and the script's captured output is surfaced on the box (as
  `last_error`) so an operator can inspect the failure instead of having the box
  vanish. `list_llmboxes` reports it with phase `broken`.
- `--init-script-timeout` (default `5m`) bounds each run; a script that exceeds it
  is treated as a failed run (a broken box).

### Sharing one Docker daemon: namespaces

Each spoke normally owns its own Docker daemon, so scoping box operations to the
`com.llmbox.managed` label is unambiguous. To run **two spokes against the same
daemon**, give each a distinct **namespace** so they never list, reap, or destroy
each other's boxes:

- `llmbox-spoke docker --hub … --namespace spoke-a` (a flag — the spoke has no
  config file).
- A namespaced spoke stamps every box and its network with
  `com.llmbox.namespace=<ns>` and scopes list/find/destroy — and the orphan
  reaper — to that label. An empty namespace (the default) is unscoped and keeps
  the one-spoke-per-daemon behaviour.
