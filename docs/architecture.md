# Architecture

How llmbox is put together. llmbox is **pure box infrastructure**: it provides the
sandbox lifecycle (create/destroy/pause/resume/exec/dial) and an HTTP proxy for a
box's ports. A box's actual workload is installed and started by the **spoke's
init script** — llmbox itself never runs the workload.

## Two surfaces on one port

Everything is served on the single server port, split across two paths:

| Path             | Audience | Carries |
|------------------|----------|---------|
| `/api/v1/...`    | the chatbot, via the `llmbox-mcp` binary (which serves MCP and forwards here) | box-control verbs (create/get/list/exec/destroy + proxy) |
| the UI           | a human, in a browser | the admin dashboard, the OIDC sign-in page, and health |

The `/api/v1/*` API is authenticated by an **API key** (headless callers) or an
**admin login session** (the web app). The UI's admin dashboard and the per-box
[HTTP proxies](proxy.md) are gated by **admin OIDC sign-in** (see
[Authentication](authentication.md)).

## Flow

```
chat: "create an llmbox"
  └─ create_llmbox ──▶ hub places the box on a spoke
                       spoke provisions it by running its --init-script once
                       returns  box_id + instance_id

exec_llmbox / *_llmbox_proxy ──▶ run commands in the box, or expose its ports

get_llmbox / list_llmboxes ──▶ inspect boxes (phase "ready", or "broken"
                               with the failed init script's output)
```

A box whose init script succeeds is phase **`ready`** immediately. A box whose
init script **fails** is kept for inspection as phase **`broken`**, with the
captured script output surfaced on the box (`last_error`).

## Components

| Path                 | What it is |
|----------------------|------------|
| `cmd/llmbox-server`  | Entry point (the hub): opens the state store and runs the HTTP server (box-control API + admin/sign-in UI). |
| `cmd/llmbox-mcp`     | The MCP binary: serves the MCP protocol and forwards every tool call to the hub's box-control API. |
| `cmd/llmbox-spoke`   | The spoke: connects to the hub and runs the box backend (Docker or Firecracker). Provisions each box with its `--init-script` and can publish box ports with `--publish-port`. |
| `internal/spoke/docker` / `internal/spoke/firecracker` | Box lifecycle over the Docker Engine API or as a Firecracker microVM. Exec/dial and init-script provisioning run in the box's `internal/guest`. |
| `internal/hub`       | Box registry (persisted to SQLite), MCP tools, admin UI, OIDC sign-in, spoke routing. |
| `internal/guest`     | The `llmbox-guest` init that runs **inside** each box. It serves exactly three verbs to the spoke — `Init` (runs the host-provided init script once), `Exec` (run a command), and `Dial` (open a byte stream to a box port for the proxy). |
| `Dockerfile`         | Image for **this server** (`llmbox`). Carries only the llmbox server binary. |
| `Dockerfile.box`     | Default box image (the spoke's `--image`). A generic sandbox base with `tini` as PID 1 (so short-lived processes are reaped) plus Node.js + pm2 for running daemons. The workload itself is provisioned by the spoke's init script. |

Boxes run on the box image (the spoke's `--image` flag). Each box runs on its own
isolated network and gets a local box-port control socket for
[box-initiated port publishing](proxy.md#box-initiated-port-publishing). The box's
workload — whatever the integration needs — is installed and started by the
spoke's `--init-script`; see [Customising boxes with an init
script](hub-and-spoke.md#customising-boxes-with-an-init-script).
