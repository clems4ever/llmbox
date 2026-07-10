# Operations

Persistence, box credentials across restarts, and orphan cleanup.

## Session persistence

The auth-session registry (which token maps to which box, its authorize URL, and
status) is persisted to a [SQLite](https://sqlite.org) file at `state_file`, so a
server restart doesn't invalidate in-flight auth links. On startup the server
restores the saved sessions and, as each spoke reconnects, reconciles them against
that spoke's live boxes — dropping any whose box is gone.

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

### Box credentials across a restart

Once a box is authenticated, Claude writes its OAuth token to
`~/.claude/.credentials.json` **inside** the box. A `docker restart` preserves
the container's writable layer, so that token survives. The box entrypoint runs
on every start, but `claude auth login` is guarded: it only runs when no
credentials (and no `CLAUDE_CODE_OAUTH_TOKEN`) are present, so a restart skips
straight to remote-control without asking the user to authenticate again.

> [!NOTE]
> This covers `docker restart` only. **Recreating** a box (`docker rm` + a new
> `create_llmbox`) starts from a fresh filesystem and requires re-authenticating,
> since boxes do not bind-mount a host credentials file.

## Orphan cleanup

A box's auth phase is encoded in its container name — `llmbox-pending-<id>`
before authentication, renamed `llmbox-<id>` after. A reaper runs every 30s and
destroys any `llmbox-pending-*` box older than `auth_ttl`. Because
the phase lives in Docker (not just in memory), this also cleans up boxes
orphaned by a restart of this server, while leaving authenticated boxes running.

Safety: every box created here carries the `com.llmbox.managed=true` label;
list/destroy/reap are scoped to that label, so unrelated host containers are
never touched.
