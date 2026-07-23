# Network isolation with domain allowlists

llmbox can restrict a box's outbound network to an explicit set of domains
instead of the open egress it gets by default. The model is **deny-by-default**:
on an isolation-enabled runner a box can reach nothing except the runner's DNS
resolver, and a DNS lookup only opens the resolved IPs — for a short, per-group
TTL — when the looked-up domain is on the box's effective allowlist. Everything
else is dropped at the packet layer.

This page describes the **configuration plane** that ships today (allowlist
groups + assignments, managed from the admin UI and the box-control API) and the
enforcement that builds on it.

## Concepts

- **Allowlist group** — a named set of egress domains a box may reach, plus a
  **resolved-IP TTL** (default `30s`): how long a DNS-resolved IP stays pinned
  in the box's firewall after a lookup before it must be re-resolved. A short TTL
  bounds the window in which an IP reallocated to an unrelated (rogue) service
  could still be reached. Domains are exact hosts (`api.github.com`) or a single
  leading wildcard (`*.github.com`).
- **Global group** — applied to **every** box on an isolation-enabled runner.
- **Per-box groups** — extra groups attached to one box, chosen at box creation
  and editable any time afterwards.
- **Effective allowlist** — what a box may actually reach:
  `escape-hatch ∪ global groups ∪ that box's own groups`.

## Managing it from the UI

The admin dashboard's **Network** section has two tabs:

- **Allowlist groups** — create, edit, delete groups; **import/export** a
  portable JSON bundle of groups.
- **Assignments** — toggle which groups are global, and review each box's extra
  groups and effective domain count.

Per-box groups are also chosen in the **New workspace** dialog (global groups are
shown and always apply) and editable later from a box's **Network** panel in its
details drawer.

## Managing it from the box-control API

Every route is a `POST` under `/api/v1/`, authenticated like the rest of the
box-control API (an API key or an admin login session):

| Route | Purpose |
|-------|---------|
| `list-allowlist-groups` | every group, with its explicit-assignment count |
| `save-allowlist-group` | create (empty `id`) or update a group |
| `delete-allowlist-group` | remove a group and its assignments |
| `get-box-allowlist` | a box's assigned groups + flattened effective domains |
| `set-box-groups` | replace a box's non-global groups |
| `export-allowlist-groups` / `import-allowlist-groups` | portable JSON bundle |

A bundle is `{"version":1,"groups":[{"name","description","domains","ttl_seconds","is_global"}]}`.
On import, a name conflict is resolved by `mode`: `merge` (union the domains into
the existing group, the default) or `replace`.

## Enabling enforcement on a spoke

Network isolation is a per-spoke opt-in. Run the spoke with:

```bash
llmbox-spoke docker --hub wss://<hub>/spoke/connect --token <token> \
  --network-isolation \
  --dns-listen 127.0.0.1:53 \
  --dns-upstream 1.1.1.1:53      # point at a Pi-hole to forward through it
```

With `--network-isolation` set, the spoke runs `llmbox-dnsd` and applies the
per-box allowlist the hub pushes (globally, on every group change, and when a
box's own groups change). A spoke without the flag keeps open egress and ignores
pushed policy. The hub computes each box's effective allowlist (`global ∪
per-box`) and pushes it over the cluster transport as a `set_policy` verb; the
resolver blocks a non-allowlisted lookup with NXDOMAIN and opens an allowed
lookup's resolved IPs in the box's firewall for the group's TTL.

### Forwarding through Pi-hole (or any resolver)

`llmbox-dnsd` never resolves upstream itself — it forwards an *allowed* lookup to
the resolver named by `--dns-upstream` and does the allowlist check, IP pinning,
and audit around the answer. That upstream can be any DNS server, so pointing it
at a [Pi-hole](https://pi-hole.net/) forwards allowed lookups through Pi-hole's
own blocklists/logging while llmbox still enforces the per-box domain allowlist:

```bash
llmbox-spoke docker --hub … --token … \
  --network-isolation \
  --dns-upstream 10.0.0.53      # the Pi-hole's address; ":53" is the default
```

A bare host or IP defaults to port 53; give `host:port` for a non-standard port.
Internally this is the `dnsd.Resolver` interface (`ForwardResolver` is the
default) — the extension point for any richer forwarder later, without touching
the enforcement flow.

## Enforcement internals & roadmap

The configuration plane above is deliberately backend-agnostic so enforcement can
be added per runner without changing the hub. The design:

- **`llmbox-dnsd`** — a small per-runner DNS resolver a box is configured to use
  by default. It records every lookup (for audit), checks the box's effective
  allowlist, and for an allowed domain resolves upstream and pins each returned
  IP into the box's firewall `allow` set for the group's TTL. It is written
  against a `Resolver` interface, so the upstream can later be an external
  resolver (e.g. Pi-hole) while llmbox keeps doing the pin/audit around it.
- **Packet-layer firewall** — a per-box chain that DROPs all egress except to
  `llmbox-dnsd` and to the currently-pinned IPs. This is what makes the default
  deny real and blocks raw-IP egress that skips DNS.
- **DNS audit** — the recorded lookups surface per box in the UI, where a blocked
  domain can be added to a group in one click.

These land behind a runner opt-in (network isolation is off unless enabled), with
an **escape-hatch** group so an operator always has a release valve.
