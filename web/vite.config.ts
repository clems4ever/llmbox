/// <reference types="vitest/config" />
import { resolve } from "node:path";
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";

// The build lands in internal/hub/webdist, which the hub embeds into the Go
// binary (see internal/hub/admin.go). The dist is generated, not committed —
// `make web` builds it, and the test/build Makefile targets, CI, and the
// Dockerfile all build it on demand before compiling Go.
//
// Two page shells come out of one build (a multi-page app): the admin
// dashboard (index.html at /admin) and the sign-in page (signin.html at
// /signin). The hub serves each shell on its route; both load hashed assets
// from the shared /admin/assets/ base.

/** pageRewrite maps the hub's page routes onto their HTML entries in dev, so
 * `npm run dev` serves the sign-in page live too (its JSON endpoints are
 * proxied to the hub below).
 *
 * @return Plugin The dev-server middleware plugin.
 */
function pageRewrite(): Plugin {
  return {
    name: "llmbox-page-rewrite",
    configureServer(server) {
      server.middlewares.use((req, _res, next) => {
        const url = (req.url ?? "").split("?")[0];
        if (url === "/signin") req.url = "/admin/signin.html";
        next();
      });
    },
  };
}

export default defineConfig({
  plugins: [react(), pageRewrite()],
  // The SPA is served at /admin, so emitted asset URLs must be absolute from
  // the site root (they are served at /admin/assets/...).
  base: "/admin/",
  build: {
    outDir: "../internal/hub/webdist",
    emptyOutDir: true,
    rollupOptions: {
      input: {
        admin: resolve(__dirname, "index.html"),
        signin: resolve(__dirname, "signin.html"),
      },
    },
  },
  server: {
    // `npm run dev` proxies the JSON/OIDC routes to a locally running hub, so
    // the dev server serves the pages live while real data comes from the hub.
    // The sign-in page shell (/signin) is served by the dev server via
    // pageRewrite above, so only deeper paths are proxied.
    proxy: {
      "/api": "http://localhost:8080",
      "^/auth/[^/]+/(login|callback)$": "http://localhost:8080",
      "^/signin/state": "http://localhost:8080",
      "/favicon.svg": "http://localhost:8080",
    },
  },
  test: {
    // Component tests run in jsdom with Testing Library; globals lets specs use
    // describe/it/expect without importing them. setup.ts wires jest-dom matchers
    // and a matchMedia shim Mantine needs under jsdom.
    globals: true,
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
    css: true,
    coverage: {
      provider: "v8",
      // Report on the app source only; config, entry, and test scaffolding carry
      // no branching logic worth measuring.
      include: ["src/**/*.{ts,tsx}"],
      exclude: ["src/**/*.test.{ts,tsx}", "src/test/**", "src/**/main.tsx"],
      reporter: ["text", "json-summary", "html"],
      reportsDirectory: "./coverage",
    },
  },
});
