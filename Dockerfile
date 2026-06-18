# llmbox-mcp — MCP server + auth web server that manages sandboxed Claude boxes.
#
# One process serves two things on the same HTTP port:
#   /              MCP over streamable HTTP (a chatbot creates/lists/destroys boxes)
#   /auth/{token}  web page where a user pastes their OAuth code to activate a box
#
# It drives the Docker daemon to launch the Claude image, so it must be given
# access to a Docker socket at runtime.
#
# Build:
#   docker build -t llmbox-mcp .
#
# Run (Docker socket in, HTTP port out):
#   docker run --rm \
#     -v /var/run/docker.sock:/var/run/docker.sock \
#     -p 8080:8080 \
#     -e LLMBOX_PUBLIC_URL=https://boxes.example.com \
#     llmbox-mcp
#
# Key configuration:
#   LLMBOX_HTTP_ADDR         listen address (default ":8080")
#   LLMBOX_PUBLIC_URL        external base URL for auth links (default "http://localhost:8080")
#   LLMBOX_CLAUDE_IMAGE      base image launched per box (default "debian:bookworm-slim");
#                            any glibc image works — Claude is injected, not baked in
#   LLMBOX_CLAUDE_BIN        path to the Claude binary injected into each box (default "/opt/llmbox/claude")
#   LLMBOX_REMOTE_ARGS       args for `claude remote-control` (default "--spawn same-dir")
#   LLMBOX_AUTH_TTL_SECONDS  destroy un-authenticated boxes after this many seconds (default 300)

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
# Pin a version by setting CLAUDE_VERSION (e.g. "2.1.89"); empty installs latest.
ARG CLAUDE_VERSION=
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
    -o /out/llmbox-mcp ./cmd/llmbox

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot

ENV LLMBOX_HTTP_ADDR=":8080" \
    LLMBOX_PUBLIC_URL="http://localhost:8080" \
    LLMBOX_CLAUDE_IMAGE="debian:bookworm-slim" \
    LLMBOX_CLAUDE_BIN="/opt/llmbox/claude" \
    LLMBOX_AUTH_TTL_SECONDS="300"

EXPOSE 8080

COPY --from=build /out/llmbox-mcp /usr/local/bin/llmbox-mcp
# The standalone Claude binary the server injects into each box (see claude stage).
COPY --from=claude /opt/llmbox/claude /opt/llmbox/claude

ENTRYPOINT ["/usr/local/bin/llmbox-mcp"]
