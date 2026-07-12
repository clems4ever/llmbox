---
name: llmbox-ports
description: >-
  Expose an HTTP port running inside this llmbox to a public URL, or list and
  unpublish those URLs. Use whenever the user wants to share, preview, open in a
  browser, or hand out a link to a server running in this box — a dev server,
  web app, API, dashboard, or notebook. The box publishes no host ports, so this
  is the only way to reach an in-box server from outside.
---

# Publishing this box's ports

This box runs inside **llmbox**, a sandbox. A server you start here (say a dev
server on `localhost:3000`) is **not reachable from the outside** until you
publish its port. Publishing hands you back a stable public `https://` URL that
the user can open in a browser. There is no host port mapping and no other way
out — this box-port API is it.

## The API

It is a tiny HTTP/JSON API served on a **unix socket** inside this box:

```
/run/llmbox/boxapi.sock
```

Talk to it with `curl --unix-socket`. The hostname in the URL is ignored (unix
sockets have no host); only the path matters. Every route is `POST` with a JSON
body and returns JSON. This box's identity is bound to the socket — you never
send a box id, and you can only ever act on **this** box's own ports.

### Publish a port → get a public URL

```bash
curl -sS --unix-socket /run/llmbox/boxapi.sock \
  -X POST http://boxapi/v1/open_port \
  -H 'Content-Type: application/json' \
  -d '{"port": 3000, "description": "Vite dev server"}'
```

Response:

```json
{"port": {"port": 3000, "url": "https://ab12cd34.proxy.example.com/", "description": "Vite dev server"}}
```

Give the `url` to the user. `description` is optional but recommended — it is
shown in the admin UI and the port list, so make it human-readable.

Re-publishing the same port is safe: it returns the existing URL rather than a
new one.

### List published ports

```bash
curl -sS --unix-socket /run/llmbox/boxapi.sock \
  -X POST http://boxapi/v1/list_ports \
  -H 'Content-Type: application/json' -d '{}'
```

```json
{"ports": [{"port": 3000, "url": "https://ab12cd34.proxy.example.com/", "description": "Vite dev server"}]}
```

### Unpublish a port

```bash
curl -sS --unix-socket /run/llmbox/boxapi.sock \
  -X POST http://boxapi/v1/close_port \
  -H 'Content-Type: application/json' \
  -d '{"port": 3000}'
```

Returns `{}` on success. The URL stops working immediately.

## How to use it

1. **Start the server first.** Publishing a port that nothing is listening on
   gives a URL that 502s until a server binds it. Bind to `0.0.0.0` (or
   `127.0.0.1`) inside the box — the proxy dials `localhost:<port>`.
2. **Publish the port**, then give the user the returned `url` verbatim.
3. The app is served at the **sub-domain root**, so absolute paths
   (`/static/app.js`, `fetch('/api')`), client-side routing, WebSockets, and SSE
   work with no path rewriting.
4. When you are done, **unpublish** ports you no longer need with
   `close_port`. Ports are also revoked automatically when this box is destroyed.

## Errors

Every non-2xx response is `{"error": "..."}`.

- **400** — bad request: malformed JSON, or a port outside `1`–`65535`.
- **502** — the hub rejected the request or is unreachable; the message explains
  which. Surface it to the user rather than retrying blindly.

If the socket itself is missing (`No such file or directory`), this box was
created without the box-port API (for example an older box, or one without a box
id) and cannot publish ports — tell the user to recreate the box.

## Quick reference

| Action        | Route             | Body                                   | Result                         |
| ------------- | ----------------- | -------------------------------------- | ------------------------------ |
| Publish port  | `/v1/open_port`   | `{"port":N,"description":"..."}`        | `{"port":{...,"url":"https…"}}` |
| List ports    | `/v1/list_ports`  | `{}`                                    | `{"ports":[...]}`              |
| Unpublish     | `/v1/close_port`  | `{"port":N}`                            | `{}`                           |

Socket: `/run/llmbox/boxapi.sock` · all routes `POST`, JSON in/out.
