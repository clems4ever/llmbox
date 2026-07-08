import { defineConfig } from "vite";

// The build lands in internal/hub/webdist, which the hub embeds into the Go
// binary (see internal/hub/admin.go). The dist is committed so `go build` never
// needs Node; rebuild it with `make web` after changing anything under web/.
export default defineConfig({
  // The SPA is served at /admin, so emitted asset URLs must be absolute from
  // the site root (they are served at /admin/assets/...).
  base: "/admin/",
  build: {
    outDir: "../internal/hub/webdist",
    emptyOutDir: true,
  },
  server: {
    // `npm run dev` proxies API and sign-in routes to a locally running hub, so
    // the dev server serves the TS live while real data comes from the hub.
    proxy: {
      "/api": "http://localhost:8080",
      "/auth": "http://localhost:8080",
      "/signin": "http://localhost:8080",
    },
  },
});
