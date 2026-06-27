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
# Run (Docker socket in, HTTP ports out; mount a YAML config and point at it).
# 8080 = UI/API, 8081 = MCP endpoint:
#   docker run --rm \
#     -v /var/run/docker.sock:/var/run/docker.sock \
#     -v "$PWD/llmbox.yaml:/etc/llmbox/llmbox.yaml:ro" \
#     -p 8080:8080 -p 8081:8081 \
#     llmbox --config /etc/llmbox/llmbox.yaml
#
# Configuration is a YAML file (see llmbox.example.yaml and the README); at a
# minimum set public_url. llmbox reads no environment variables of its own.
#
# The server neither runs nor ships Claude itself: each box image bakes in the
# standalone Claude binary (see Dockerfile.box), so this image only carries the
# llmbox server binary.

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

# 8080 = UI/API, 8081 = MCP endpoint (put the MCP port behind an auth proxy).
EXPOSE 8080 8081

COPY --from=build /out/llmbox /usr/local/bin/llmbox

ENTRYPOINT ["/usr/local/bin/llmbox"]
