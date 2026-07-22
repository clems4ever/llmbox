# Operations

State persistence, pausing boxes, and orphan cleanup.

## State persistence

The hub's registry — the box records (box ID → spoke, generation, description,
phase), API keys, login sessions, and cluster (spoke/token) records — is
persisted to a [SQLite](https://sqlite.org) file at `state_file`, so a server
restart doesn't lose them. On startup the server restores the saved records and,
as each spoke reconnects, reconciles them against that spoke's live boxes —
tombstoning any whose box is gone.

To survive **container recreation**, point `state_file` at a mounted volume:

```yaml
# in llmbox.yaml
state_file: /var/lib/llmbox/sessions.db
```
```yaml
# in docker-compose.yml
volumes:
  - ./data/llmbox:/var/lib/llmbox
```

> [!IMPORTANT]
> The `llmbox` image runs as the distroless **`nonroot`** user
> (**UID/GID 65532**). The host directory you mount must be writable by that
> UID, or the server crash-loops with `permission denied` opening the store:
>
> ```bash
> mkdir -p ./data/llmbox && sudo chown -R 65532:65532 ./data/llmbox
> ```

### Box state across a restart

A `docker restart` preserves the container's writable layer, so whatever a box's
[init script](hub-and-spoke.md#customising-boxes-with-an-init-script) wrote to
disk survives. **Recreating** a box (`docker rm` + a new `create-box`) starts
from a fresh filesystem and re-runs the init script, since boxes do not bind-mount
host state.

## Pausing a box to save compute

An idle box still holds its CPU/RAM reservation. Pause it — from the admin UI's
per-box **Pause** button — to stop its compute while keeping its disk, then
**Resume** it when you need it again:

- **Pause** stops the box's container (Docker) or halts its microVM (Firecracker),
  freeing CPU **and** RAM. The box's disk survives intact — the `/workspace` tree,
  whatever the init script installed, its identity, and (on Firecracker) its
  network slot — so nothing is rebuilt. A paused box still appears in the list,
  reported as **paused** (never reaped, never tombstoned).
- **Resume** restarts the compute from that disk. Anything written to disk is
  preserved, so it comes straight back up from where it left off on disk.

The one thing that does *not* survive a pause is any **in-memory** state: pausing
ends the box's running processes to release RAM, so a long-running service in the
box is stopped. The init script is **not** re-run on resume — if you rely on a
service the init script started, restart it yourself after resume (or run it under
a supervisor like `pm2` that a fresh boot brings back). Pausing is offered only
for a running box; `exec` against a paused box returns a "resume it first" error
rather than a raw connection failure. The model is identical on both the Docker
and Firecracker backends.

## Orphan cleanup

A reaper runs every 30s and keeps the hub's records converged with reality:

- **Expired login sessions** and OIDC flows are purged (expiry is also enforced at
  read time; this just bounds the store's growth).
- **Departed spokes**: any box record or proxy pinned to a spoke that has been
  **de-enrolled** from the cluster is purged (its destroy [hooks](hooks.md) are
  replayed). A spoke that is merely offline is kept — it may reconnect — so only a
  truly removed spoke's objects are dropped.
- **Vanished boxes**: each connected spoke's live inventory is folded back into the
  box records, tombstoning any box the spoke no longer has.

Safety: every box created here carries the `com.llmbox.managed=true` label;
list/destroy are scoped to that label, so unrelated host containers are never
touched.
