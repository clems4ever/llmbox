// Capture screenshots + a video of the per-box network-audit view against the
// REAL built admin UI. Only the /api/v1 responses are stubbed (with synthetic
// flow data) — every pixel of the Network activity panel is the real component
// rendered by the shipped bundle (build it first: `cd web && npm run build`).
//
// It uses Playwright's bundled Chromium. Install once with:
//   npm i -D playwright-core && npx playwright install chromium
// then run:  node scripts/netaudit-demo/capture.mjs
//
// Paths are overridable via env: PLAYWRIGHT_CORE (module path) and CHROME_PATH
// (Chromium executable). CHROME_PATH defaults to $PLAYWRIGHT_BROWSERS_PATH or the
// standard ms-playwright cache.
import http from "node:http";
import fs from "node:fs";
import path from "node:path";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";

const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_CORE || "playwright-core");

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const repo = path.resolve(__dirname, "../..");
const dist = path.join(repo, "internal/hub/webdist");
const outDir = path.join(repo, "docs/assets/netaudit");
// Let Playwright resolve its own bundled Chromium unless CHROME_PATH is set.
const chromePath = process.env.CHROME_PATH || chromium.executablePath();

fs.mkdirSync(outDir, { recursive: true });

// --- tiny static server for the built UI ---
const types = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".ico": "image/x-icon" };
const server = http.createServer((req, res) => {
  // The admin UI is built with base "/admin/", so strip that prefix to map onto
  // the dist tree; any unknown path falls back to index.html (client routing).
  let p = decodeURIComponent(req.url.split("?")[0]).replace(/^\/admin/, "");
  if (p === "/" || p === "") p = "/index.html";
  const file = path.join(dist, p);
  if (!file.startsWith(dist) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) {
    res.writeHead(200, { "content-type": "text/html" });
    res.end(fs.readFileSync(path.join(dist, "index.html")));
    return;
  }
  res.writeHead(200, { "content-type": types[path.extname(file)] || "application/octet-stream" });
  res.end(fs.readFileSync(file));
});
await new Promise((r) => server.listen(0, r));
const base = `http://127.0.0.1:${server.address().port}/admin/`;

// --- synthetic data ---
const boxes = [
  { instance_id: "i-7f3a", name: "research-agent", box_id: "research-agent", description: "Coding agent sandbox", spoke: "edge-fc-1", image: "debian-bookworm", state: "running", status: "Up 12 min", created: 1_700_000_000, last_seen: 1_700_000_600 },
  { instance_id: "i-2b10", name: "scraper", box_id: "scraper", spoke: "edge-fc-1", image: "debian-bookworm", state: "running", status: "Up 4 min", created: 1_700_000_300, last_seen: 1_700_000_600 },
];

// A base set of flows; byte counters grow on each poll and a new flow appears
// partway through, so the video shows the table updating live.
const nowSec = () => Math.floor(Date.now() / 1000);
let polls = 0;
function flowsForResearchAgent() {
  polls += 1;
  const t = nowSec();
  const grow = polls * 1;
  const base = [
    { proto: "tcp", dst_ip: "140.82.121.4", dst_port: 443, src_port: 51000, state: "ESTABLISHED", bytes_out: 1420 + grow * 210, bytes_in: 5300 + grow * 4100, first_seen: t - 40, last_seen: t },
    { proto: "tcp", dst_ip: "151.101.0.223", dst_port: 443, src_port: 51022, state: "ESTABLISHED", bytes_out: 900 + grow * 60, bytes_in: 2_200_000 + grow * 480_000, first_seen: t - 33, last_seen: t },
    { proto: "udp", dst_ip: "8.8.8.8", dst_port: 53, src_port: 34567, state: "", bytes_out: 60 + grow * 6, bytes_in: 180 + grow * 20, first_seen: t - 30, last_seen: t - 2 },
    { proto: "tcp", dst_ip: "104.18.32.7", dst_port: 443, src_port: 51044, state: "TIME_WAIT", bytes_out: 512 + grow * 4, bytes_in: 14_800 + grow * 120, first_seen: t - 20, last_seen: t - 6 },
  ];
  if (polls >= 3) {
    base.unshift({ proto: "tcp", dst_ip: "192.30.255.117", dst_port: 22, src_port: 51090, state: "SYN_SENT", bytes_out: 0, bytes_in: 0, first_seen: t, last_seen: t });
  }
  if (polls >= 5) {
    base.splice(2, 0, { proto: "tcp", dst_ip: "34.117.59.81", dst_port: 443, src_port: 51101, state: "ESTABLISHED", bytes_out: 4200 + grow * 300, bytes_in: 98_000 + grow * 12_000, first_seen: t - 3, last_seen: t });
  }
  return base;
}

const json = (route, body) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(body) });

async function wireApi(page) {
  await page.route("**/api/v1/**", async (route) => {
    const url = route.request().url();
    if (url.endsWith("/me")) return json(route, { email: "admin@example.com", admin: true, csrf: "csrf" });
    if (url.endsWith("/spoke-statuses")) return json(route, { spokes: [{ name: "edge-fc-1", connected: true, default: true, enrolled_at: "2026-07-20T10:00:00Z" }] });
    if (url.endsWith("/list-join-tokens")) return json(route, { tokens: [] });
    if (url.endsWith("/list-boxes")) return json(route, { boxes });
    if (url.endsWith("/proxy-enabled")) return json(route, { enabled: true });
    if (url.endsWith("/list-proxies")) return json(route, { proxies: [{ box_id: "research-agent", port: 8080, url: "https://research-agent.proxy.example.com/", slug: "abc", description: "dev server" }] });
    if (url.endsWith("/box-network")) return json(route, { flows: flowsForResearchAgent() });
    return json(route, {});
  });
}

const browser = await chromium.launch({ executablePath: chromePath || undefined, args: ["--no-sandbox"] });

// --- screenshots ---
{
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 960 }, deviceScaleFactor: 2 });
  const page = await ctx.newPage();
  await wireApi(page);
  await page.goto(base, { waitUntil: "networkidle" });
  await page.click('[data-box-row="research-agent"]');
  await page.waitForSelector("[data-network-table]");
  // let a couple of polls land so byte counters look real
  await page.waitForTimeout(300);
  await page.screenshot({ path: path.join(outDir, "network-audit-drawer.png") });
  // Tighter shot of just the network panel.
  const panel = await page.$("#network-section");
  await panel.screenshot({ path: path.join(outDir, "network-audit-panel.png") });
  await ctx.close();
}

// --- video (live updates) ---
{
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 960 }, recordVideo: { dir: outDir, size: { width: 1440, height: 960 } } });
  const page = await ctx.newPage();
  await wireApi(page);
  await page.goto(base, { waitUntil: "networkidle" });
  await page.click('[data-box-row="research-agent"]');
  await page.waitForSelector("[data-network-table]");
  // ~18s: several 3s polls so the byte counters climb and new flows appear.
  await page.waitForTimeout(18000);
  await ctx.close();
  // Rename the autogenerated video file.
  const vids = fs.readdirSync(outDir).filter((f) => f.endsWith(".webm"));
  if (vids.length) fs.renameSync(path.join(outDir, vids[0]), path.join(outDir, "network-audit-live.webm"));
}

await browser.close();
server.close();
console.log("captured to", outDir);
