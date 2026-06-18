# llmbox — MCP server + auth web server that manages sandboxed Claude boxes.
#
# One process serves two things on the same HTTP port:
#   /              MCP over streamable HTTP (a chatbot creates/lists/destroys boxes)
#   /auth/{token}  web page where a user pastes their OAuth code to activate a box
#
# It drives the Docker daemon to launch the Claude image, so it must be given
# access to a Docker socket at runtime.
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

# ---- claude stage ----
# Fetch the standalone Claude native binary (no Node runtime) once, so the server
# can inject it into every box at creation. This is a glibc stage because the
# binary is glibc-linked; we validate it is a single self-contained file by
# running `--version` here (it never runs in the distroless runtime below — it is
# only stored there as bytes to copy into boxes).
FROM debian:bookworm-slim AS claude
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
# Pinned Claude Code version (stable channel). This is the single source of
# truth for the version baked into the image; the bump-claude workflow opens a
# PR editing this line when a newer stable release appears. Override at build
# time with --build-arg CLAUDE_VERSION=<x.y.z|stable|latest>.
ARG CLAUDE_VERSION=2.1.170
RUN set -eux; \
    curl -fsSL https://claude.ai/install.sh | bash -s -- ${CLAUDE_VERSION}; \
    bin="$(command -v claude || echo "$HOME/.local/bin/claude")"; \
    real="$(readlink -f "$bin")"; \
    mkdir -p /opt/llmbox; \
    cp "$real" /opt/llmbox/claude; \
    chmod 0755 /opt/llmbox/claude; \
    /opt/llmbox/claude --version

# ---- build stage ----
FROM golang:1.26-bookworm AS build

WORKDIR /src

# Cache dependencies first (copy just the module files so this layer stays
# cacheable across source-only changes).
COPY go.mod go.sum ./
RUN go mod download

# Build a static binary so it runs on a distroless base.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags="-s -w" \
    -o /out/llmbox ./cmd/llmbox

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot

EXPOSE 8080

COPY --from=build /out/llmbox /usr/local/bin/llmbox
# The standalone Claude binary the server injects into each box (see claude stage).
COPY --from=claude /opt/llmbox/claude /opt/llmbox/claude

ENTRYPOINT ["/usr/local/bin/llmbox"]
