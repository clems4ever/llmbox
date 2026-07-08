---
name: box-ports
description: Publish, unpublish, or list public HTTPS URLs for TCP ports served inside this sandbox. Use when the user wants to expose a web server, API, or dev server running in this box to the internet, asks for a public/shareable/preview URL, or wants to know which ports of this box are already published.
---

# Publishing this box's ports

This box has a local control API on a unix socket that publishes public HTTPS
URLs for ports listening inside the box. You only name the **port** (and an
optional description); the platform decides the URL and enforces that you can
only ever manage **this** box's ports — you cannot name another box, and no
other box can touch yours.

All calls are `POST` with a JSON body to `http://localhost/v1/...` over the
socket `/run/llmbox/boxapi.sock`.

## Publish (open) a port

```sh
curl -s --unix-socket /run/llmbox/boxapi.sock -X POST http://localhost/v1/open_port \
  -d '{"port": 3000, "description": "vite dev server"}'
```

Response:

```json
{"port":{"port":3000,"url":"https://ab12cd34ef56ab12cd34ef56.proxy.example.com/","description":"vite dev server"}}
```

Give the returned `url` to the user — that is the public address. Notes:

- The server must be listening inside the box (on `127.0.0.1` or `0.0.0.0`)
  for the URL to serve anything; you can publish first and start it after.
- Publishing the same port again is idempotent and returns the same URL.
- The URL serves HTTP(S) traffic, including WebSocket and SSE.

## List published ports

```sh
curl -s --unix-socket /run/llmbox/boxapi.sock -X POST http://localhost/v1/list_ports -d '{}'
```

Response: `{"ports":[{"port":3000,"url":"https://...","description":"..."}]}` —
only this box's ports, never another box's.

## Unpublish (close) a port

```sh
curl -s --unix-socket /run/llmbox/boxapi.sock -X POST http://localhost/v1/close_port -d '{"port": 3000}'
```

Response on success: `{}`. Close ports the user no longer needs — a published
URL stays reachable until it is closed or the box is destroyed.

## Errors

Failures come back as `{"error":"..."}` with a non-2xx status. Report the
message to the user rather than retrying — common cases:

- port publishing is disabled on this deployment (no proxy domain configured),
- this box was created without a box ID, so it cannot publish ports,
- the spoke is momentarily disconnected from the hub (retrying once after a
  few seconds is reasonable for this one).
