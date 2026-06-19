# Architecture

How llmbox is put together and why the auth secret stays out of the chatbot.

## The auth secret never touches the chatbot

The OAuth code exchanges for a full-scope account token, so it must never enter
the model's context. This server is split accordingly — one process, two
front-ends on the same HTTP port:

| Path            | Audience | Carries |
|-----------------|----------|---------|
| `/` (root)      | the chatbot (MCP over streamable HTTP) | box IDs + the **auth page URL** only |
| `/auth/{token}` | the human, in a browser | the **OAuth code** (browser → this server → container stdin) |

The code travels from the user's browser to the box's `claude auth login`
process; it is never an MCP input or output and is never logged.

## Flow

```
chat: "create an llmbox"
  └─ create_llmbox ──▶ starts a box parked at `claude auth login`,
                       captures its OAuth authorize URL,
                       returns  https://YOUR_HOST/auth/<token>   (+ auth_token)

user opens that URL ──▶ "Sign in with Claude" (their account) ──▶ copies the code
                   ──▶ pastes it into the page ──▶ server feeds it to the box

box finishes login ──▶ `claude remote-control` starts ──▶ session URL
  └─ get_llmbox(box_id) ──▶ returns the session URL once ready
```

Boxes that are never authenticated are destroyed after `auth_ttl`
(default 5 minutes) — see [Orphan cleanup](operations.md#orphan-cleanup).

## The activation page

This is what the user sees at the auth-page URL — paste the code to activate, and
the box reports ready with its session URL. The page is responsive, so on a phone
it drops the card framing and fills the screen. These images are **captured by the
end-to-end test** (headless Chrome via WebDriver) and refreshed by CI **on the
pull request** that changes the UI — committed straight into the PR's diff — so
they always reflect the current UI and stay reviewable; see
[Testing](development.md#testing).

| Activate | Ready | On mobile |
|----------|-------|-----------|
| ![The llmbox activation page](../.github/screenshots/auth-page.png) | ![The activated llmbox page showing the session URL](../.github/screenshots/auth-ready.png) | ![The llmbox activation page on a phone-sized screen](../.github/screenshots/auth-page-mobile.png) |

## Components

| Path                 | What it is |
|----------------------|------------|
| `cmd/llmbox`         | Entry point: opens the session store, runs the HTTP server (MCP + auth pages) and the reaper. |
| `internal/docker`    | Box lifecycle over the Docker Engine API (create with image auto-pull + box-ID uniqueness, login-capture, code-submit, graceful destroy, reap). |
| `internal/server`    | Session registry (persisted to bbolt), MCP tools, auth web pages, reaper loop. |
| `Dockerfile`         | Image for **this server** (`llmbox`). It bakes in the standalone Claude binary, which the server injects into each box at creation. |

Boxes run on a plain base image (`claude_image`, default
`debian:bookworm-slim`): the server **injects** the standalone Claude binary and
a small `~/.claude.json` seed into each box at creation, and runs it as root with
`HOME=/root` and a `/workspace` working directory — so nothing Claude-specific
needs to be baked into the base image. Any glibc image with `/bin/sh`,
`util-linux` (for `script`), and CA certificates works.
