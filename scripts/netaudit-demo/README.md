# netaudit demo capture

`capture.mjs` renders the **real** built admin UI and screenshots/records the
per-box **Network activity** panel. Only the `/api/v1` responses are stubbed with
synthetic flow data — the panel itself is the shipped component, so this is a
faithful preview of the feature without needing a KVM host with live `conntrack`.

The synthetic feed grows byte counters and adds new flows on each poll, so the
recording shows the table updating live.

## Run

```sh
cd web && npm run build          # produce internal/hub/webdist
npm i -D playwright-core && npx playwright install chromium
node scripts/netaudit-demo/capture.mjs
# -> docs/assets/netaudit/network-audit-drawer.png
#    docs/assets/netaudit/network-audit-panel.png
#    docs/assets/netaudit/network-audit-live.webm  (turn into a gif with ffmpeg)
```

Overrides: `CHROME_PATH` (Chromium executable) and `PLAYWRIGHT_CORE` (module path).
