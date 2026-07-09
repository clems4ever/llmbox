/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The build lands in internal/hub/webdist, which the hub embeds into the Go
// binary (see internal/hub/admin.go). The dist is generated, not committed —
// `make web` builds it, and the test/build Makefile targets, CI, and the
// Dockerfile all build it on demand before compiling Go.
export default defineConfig({
  plugins: [react()],
  // The SPA is served at /admin, so emitted asset URLs must be absolute from
  // the site root (they are served at /admin/assets/...).
  base: "/admin/",
  build: {
    outDir: "../internal/hub/webdist",
    emptyOutDir: true,
  },
  server: {
    // `npm run dev` proxies API and sign-in routes to a locally running hub, so
    // the dev server serves the app live while real data comes from the hub.
    proxy: {
      "/api": "http://localhost:8080",
      "/auth": "http://localhost:8080",
      "/signin": "http://localhost:8080",
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
      exclude: ["src/**/*.test.{ts,tsx}", "src/test/**", "src/main.tsx"],
      reporter: ["text", "json-summary", "html"],
      reportsDirectory: "./coverage",
    },
  },
});
