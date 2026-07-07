# Hub-and-spoke clustering

> Status: in progress. This document is the design and the implementation plan.

## Goal

Boxes run on **remote hosts** ("spokes") while a single **hub** (the llmbox
server the chatbot talks to over MCP) stays the only front-end. An operator
generates a **join token**, starts a spoke with it, and the spoke joins the
cluster; the hub then places boxes on that spoke. The **hub runs no box backend
of its own** — it is a pure router/registry, so every box runs on an
independently started spoke (`llmbox-spoke`). A box created with no explicit spoke
runs on the **default spoke**, which an admin chooses in the admin UI (a DB
setting); until one is set, an unqualified create is refused. Even a single-host
deployment therefore runs one spoke alongside the hub.

## Where the network boundary goes

The split is the **box-operations interface**, not the raw Docker API. The
`server` package already needs exactly seven verbs from the Docker layer
(`internal/server/server.go`, the `boxManager` interface):

    CreateLLMBox, SubmitCode, List, Destroy, Logs, Exec, ReapOrphans

This same set becomes the spoke RPC surface — promoted to the exported
`cluster.BoxManager`. A spoke exposes **only** these seven verbs over the wire;
it is never a generic Docker proxy. This is the security boundary: a spoke holds
a Docker socket (root-equivalent on its host), so the protocol is a fixed
allowlist of box verbs with validated arguments, and the spoke re-validates
inputs rather than trusting the hub.

Every box **verb** is bounded request/response (Exec/Logs already buffer and cap
their output; SubmitCode and CreateLLMBox capture a single URL with a timeout).
The one exception is **HTTP proxying** to a box's port: to carry WebSocket and
SSE, the hub opens a raw byte **tunnel** to the box over the same connection
(`stream_open`/`stream_data`/`stream_close` frames, multiplexed by the frame ID),
and the spoke splices it to the box port via `DialBox`. So the protocol is framed
request/response for verbs, plus a lightweight stream multiplex for the proxy —
no need for a heavyweight session layer (yamux/gRPC bidi).

## Transport: WebSocket, spoke-dials-hub

Spokes live on edge hosts, often behind NAT, so the **spoke dials the hub** and
the hub pushes commands down that persistent, full-duplex connection (inverted
call direction). WebSocket rides the hub's existing `net/http` server and TLS on
the same port (new route `/spoke/connect`), is reverse-proxy friendly, and lets
us reuse the existing auth machinery shape for enrollment. We use
`github.com/coder/websocket` (context-aware, minimal).

A tiny JSON `frame` carries everything, correlated by an incrementing request
ID so many in-flight verb calls (and proxy tunnels) share one socket:

```
frame { Type: enroll|welcome|req|resp|err|stream_open|stream_data|stream_close, ID, Method, Payload, Data }
```

The `Data` field carries raw tunnel bytes for the stream frames; the ID doubles
as the stream ID so many concurrent tunnels interleave over the one connection.

The transport is abstracted behind a `transport` interface (Send/Recv/Close) so
the hub-side `remoteSpoke` and the spoke-side dispatcher are unit-tested over an
in-memory pipe, with the real `wsTransport` exercised by a loopback test.

## Enrollment: one-time join token → per-spoke bearer credential

Two-phase, so the long-lived credential is never the thing humans copy-paste:

1. **Join token** — generated on the hub by an operator with shell access:

       llmbox-spoke token create --name worker-1 [--ttl 1h]

   High-entropy random secret with the **spoke name baked in** and a TTL.
   Stored **hashed** (SHA-256) in a new bbolt bucket; the plaintext is printed
   once and never again. One-time use.

2. **Enrollment** — the spoke dials `/spoke/connect` and sends an `enroll` frame
   with the join token. The hub validates+consumes the token (one-time: deleted
   on success), mints a **long-lived per-spoke bearer credential**, records the
   spoke, and replies `welcome` with the credential and the spoke's name. The
   spoke persists the credential to its local state file and reconnects with it
   thereafter (`enroll` with `reconnect` flag), never needing the join token
   again.

Bearer credentials are stored hashed too. (mTLS is a future upgrade; we ship
bearer first per the agreed scope.)

### Threat model notes

A spoke identity is powerful: whoever the hub treats as a spoke receives boxes
and exec traffic (i.e. user code/sessions). So: the hub authenticates the spoke
(join token, then bearer); spoke verbs re-validate inputs; the verb allowlist is
the guarantee the spoke never becomes a generic Docker proxy. A leaked join
token is bounded to enrolling one spoke (one-time, TTL'd, operator-visible). See
the activation-auth threat model for the analogous leaked-token reasoning.

**Managed-only enforcement.** Every verb that targets an existing container —
`Destroy`, `Logs`, `Exec`, `SubmitCode` — resolves through `docker.Manager`'s
`findManaged`, which only matches containers carrying `ManagedLabel`. So a hub
that sends an ID/name for a container the spoke didn't create (whether by bug,
compromise, or MITM) gets a `no managed box matches` error and **no action** —
it cannot destroy, exec into, read logs from, or write stdin to an arbitrary
host container. `CreateLLMBox` only ever makes new (labelled) containers, and
`ReapOrphans` lists managed boxes only. This makes "the spoke can only ever act
on its own boxes" an enforced property of the box layer, not an implicit one.

## Hub routing across spokes

The hub keeps a **spoke registry**: name → `BoxManager`, populated only by remote
spokes as they connect/disconnect. The hub has no box manager of its own.

Box→spoke affinity lives on the **session**: `create_llmbox` takes an optional
`spoke` argument; when omitted the box runs on the **default spoke** the admin
picked (persisted as a store setting), and the resolved spoke name is stored on
the session. Per-box verbs (get/logs/exec/destroy, submit-code) route to the
session's spoke. Cluster-wide verbs fan out:

- `List` queries every registered spoke and aggregates (each `Box` is tagged
  with its spoke).
- `ReapOrphans` reaps each spoke; `Restore` reconciles each session against its
  spoke's list (a session whose spoke is currently disconnected is kept, not
  dropped, since the box may still be alive).

## Package layout

```
internal/cluster/
  proto.go        frame + verb request/response payloads, (de)serialization
  transport.go    transport interface, wsTransport, in-memory pipe (test)
  boxmanager.go   BoxManager interface (the 7 verbs)
  proxy.go        BoxDialer interface (the spoke's box-port dial capability)
  stream.go       proxy tunnel: hub-side net.Conn + spoke-side pump over stream frames
  remote.go       remoteSpoke: BoxManager + DialBox that round-trip over a transport (hub side)
  dispatch.go     spoke-side loop: receive verb/stream frames, call local BoxManager/DialBox, reply
  hub.go          /spoke/connect handler, enrollment, registry of connected spokes
  spoke.go        Spoke: dial hub, enroll/reconnect, run dispatch loop
  tokens.go       join-token + spoke-credential lifecycle (hashing, one-time consume)
```

Token/credential persistence extends the existing bolt store
(`internal/server/store.go`) with `spoke_join_tokens` and `spokes` buckets, via
new `ClusterStore` methods.

## CLI / config

- `llmbox-spoke token create --name <name> [--ttl 1h] [--state-file …]` —
  hub-side; prints the token once. Writes to the hub's state file (default
  `llmbox-sessions.db`; point `--state-file` at the running hub's `state_file`).
- `llmbox-spoke docker --hub wss://hub/spoke/connect --token <join-token>` — runs a
  spoke: connects to a local Docker daemon via `docker.NewManager`, enrolls (or
  reconnects with its saved credential), and serves verbs.
- config: **the spoke reads no config file** — every setting is a flag, so a host
  runs the single command the admin UI generates. The hub always exposes the
  `/spoke/connect` route. Because the hub runs no box backend, the per-box Docker
  knobs live entirely on the spoke: `--box-memory-mb`, `--box-cpus`,
  `--box-pids-limit`, `--box-socket-dir`, `--box-peer`, `--remote-args`,
  `--registry[-username|-password-file]`, and `--allowed-image`. The hub only
  resolves the per-box **image** it stamps onto an imageless create.

### Sharing one Docker daemon: namespaces

Each spoke normally owns its own Docker daemon, so scoping box operations to the
`com.llmbox.managed` label is unambiguous. If you want to run **two spokes (or a
hub plus a spoke) against the same daemon**, give each a distinct **namespace** so
they do not list, reap, or destroy each other's boxes:

- on a spoke, `llmbox-spoke docker --hub … --namespace spoke-a` (a flag — the spoke has
  no config file). The hub's local provisioner reads `box.namespace` from the
  hub's config file.
- A namespaced provisioner stamps every box and its network with
  `com.llmbox.namespace=<ns>` and scopes list/find/destroy — and therefore the
  orphan reaper — to that label. An empty namespace (the default) is unscoped and
  preserves the one-spoke-per-daemon behaviour.

## Testing

- Unit tests per layer (token lifecycle, proto round-trip, remoteSpoke +
  dispatch over the in-memory pipe, hub enrollment over a loopback WS, server
  routing with fake managers). Target parity with the existing suite's coverage.
- A dedicated **e2e** test in `e2e/cluster/` (separate directory, own build tag)
  inspired by `e2e/`: stand up a real hub HTTP server, run an in-process spoke
  backed by a fake box manager, generate a join token, enroll, create a box
  routed to the spoke, exec/list/destroy through it, and assert the join token
  is one-time.
