# llmbox — box-control API + auth web server that manages sandboxed Claude boxes.
#
# It serves everything on one HTTP port (http_addr):
#   /auth/{token}  web page where a user pastes their OAuth code to activate a box
#   /api/v1/...    box-control JSON API (the llmbox-mcp binary forwards MCP calls here)
#
# The MCP protocol itself is served by a separate image (Dockerfile.mcp), which
# forwards to the box-control API. It drives the Docker daemon to launch the
# Claude image, so it must be given access to a Docker socket at runtime.
#
# Build:
#   docker build -t llmbox .
#
# Run (Docker socket in, HTTP port out; mount a YAML config and point at it):
#   docker run --rm \
#     -v /var/run/docker.sock:/var/run/docker.sock \
#     -v "$PWD/llmbox.yaml:/etc/llmbox/llmbox.yaml:ro" \
#     -p 8080:8080 \
#     llmbox --config /etc/llmbox/llmbox.yaml
#
# Configuration is a YAML file (see llmbox.example.yaml and the README); at a
# minimum set public_url. llmbox reads no environment variables of its own.
#
# The server neither runs nor ships Claude itself: each box image bakes in the
# standalone Claude binary (see Dockerfile.box), so this image only carries the
# llmbox server binary.

# ---- web build stage ----
# The admin SPA dist (internal/hub/webdist) is generated, not committed, and the
# hub embeds it at go build — so build it here first. Vite emits into
# /src/internal/hub/webdist (outDir ../internal/hub/webdist relative to web/).
FROM node:22-bookworm AS webbuild

WORKDIR /src/web

COPY web/package.json web/package-lock.json ./
RUN npm ci

COPY web/ ./
RUN npm run build

# ---- build stage ----
FROM golang:1.26-bookworm AS build

WORKDIR /src

# Cache dependencies first (copy just the module files so this layer stays
# cacheable across source-only changes).
COPY go.mod go.sum ./
RUN go mod download

# Build a static binary so it runs on a distroless base. Layer the built admin
# SPA over the placeholder-only webdist from the repo before compiling, so the
# binary embeds the real UI.
COPY . .
COPY --from=webbuild /src/internal/hub/webdist ./internal/hub/webdist
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags="-s -w" \
    -o /out/llmbox-server ./cmd/llmbox-server

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot

# 8080 = the whole server (UI + box-control API); put it behind an auth proxy.
EXPOSE 8080

COPY --from=build /out/llmbox-server /usr/local/bin/llmbox-server

ENTRYPOINT ["/usr/local/bin/llmbox-server"]
