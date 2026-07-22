# Box lifecycle hooks

llmbox knows nothing about what any particular integration needs in a box. Instead
it runs **hooks** — external executables you point it at with the `hooks` config
list — at two points in a box's life:

- **`box.create`** — fires *before* the new box starts. A hook may return **files
  to inject** into the box (secrets, config, even binaries) and an opaque **state**
  string llmbox persists with the box.
- **`box.destroy`** — fires when the box is destroyed or reaped. llmbox replays the
  state the `box.create` hook returned, so the hook can undo whatever it did.

The wire protocol is plain JSON over the hook's stdin/stdout, defined in the
importable [`hookproto`](../hookproto/hookproto.go) package. For each event llmbox
writes one `Request` to the hook's stdin and reads one `Response` from its stdout:

```jsonc
// stdin  (llmbox -> hook)
{ "event": "box.create", "box": { "box_id": "web-box", "image": "debian:bookworm-slim" } }

// stdout (hook -> llmbox)
{
  "files": [
    { "path": "/home/node/.secret/token", "content": "…", "mode": "0600", "uid": 1000, "gid": 1000 },
    { "path": "/usr/local/bin/tool", "content_base64": "…", "mode": "0755" }
  ],
  "state": "opaque-handle-for-destroy"
}
```

A non-zero exit fails the hook: on `box.create` that aborts the box (and any state
already returned is replayed to `box.destroy` for cleanup); on `box.destroy` it is
logged and ignored. Injected files are streamed into the **created-but-not-yet-
started** container via the Docker copy API, owned by the `uid`/`gid` the hook
chose — so a secret in a non-root user's home stays readable by that user, and is
never put in an env var or label where `docker inspect` would expose it. Hooks run
as subprocesses of this server, so they inherit its environment (pass a hook its
own config that way) and must be present in the `llmbox` container (bake them
into a derived image, or mount them in).

Writing a hook in Go is a few lines — implement a `hookproto.Handler` and call
`hookproto.Main`:

```go
func main() {
    hookproto.Main(func(req hookproto.Request) (hookproto.Response, error) {
        switch req.Event {
        case hookproto.EventBoxCreate:
            // mint a credential, return files to inject + state to remember
            return hookproto.Response{Files: ..., State: token}, nil
        case hookproto.EventBoxDestroy:
            // undo it, using req.State
            return hookproto.Response{}, revoke(req.State)
        }
        return hookproto.Response{}, nil
    })
}
```

**Reference hook — granular.** The
[granular-llmbox](https://github.com/clems4ever/granular-llmbox) repo implements a
hook that gives each box its own scoped identity for acting on the user's behalf
through a [granular](https://github.com/clems4ever/granular) authorization server:
on create it mints a subject token, installs the granular CLIs, config, and a
skill into the box, and on destroy it revokes the subject. It depends on llmbox
(for `hookproto`), never the other way around — which is the whole point of the
hook boundary.

## Box networking and isolation

A hook's box often needs to reach *other* containers (e.g. an integration's
resource servers) **without** being able to reach other boxes. llmbox uses a
hub-and-spoke layout instead of one shared network:

- Every box is created on its **own** dedicated Docker network (`llmboxnet-<id>`)
  and attached to nothing else, so no two boxes ever share a network — they
  cannot talk to each other.
- llmbox connects each container named by a spoke's `--box-peer` flag (repeatable)
  into that per-box network, so the box reaches those peers by name while staying
  isolated.
- The network is torn down (and the peers disconnected from it) when the box is
  destroyed or reaped.

`--box-peer` takes a **container name**. When the peers run in a separate compose
project, give them a fixed `container_name:` so the name is stable, e.g.:

```yaml
services:
  granular-github:
    container_name: granular-github   # must match a --box-peer value
```

### Auditing box egress

Because box egress is routed by host **kernel primitives** — a per-box TAP + NAT
for [Firecracker](firecracker.md#how-it-works), a per-box bridge for Docker — the
datapath never passes through llmbox, so nothing in llmbox can see what a box talks
to. To make egress observable without changing that routing, the spoke reads the
host's **connection tracker** (`conntrack`): every flow is metadata the kernel
already keeps, and each box owns a distinct source subnet, so a flow is attributed
to a box by a cheap address lookup.

- The spoke keeps a small, bounded ring of recent flows **per box** — destination,
  protocol, connection state, and per-direction byte counts. It is **metadata
  only**: no packet payload is ever read or stored, so the audit view cannot itself
  become a data-exfiltration surface.
- The hub exposes a box's flows on `POST /api/v1/box-network` (keyed by box ID,
  routed to the box's spoke), and the admin UI shows them live in the **Network
  activity** panel of a box's details drawer.
- It is **observe-only** today — a view, not a firewall. Blocking/allow-listing is
  a separate, deliberate step (kernel `nftables` for L3/L4, or a transparent proxy
  for domain-level policy).
- It works for **both backends** (conntrack sees the Docker bridge traffic too). It
  degrades cleanly to an empty view when `conntrack` is unavailable or the spoke
  cannot reach the host's netfilter tables (e.g. a Docker-backed spoke without host
  network access), and is inherently empty for a control-only box with no egress.
