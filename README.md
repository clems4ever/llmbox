# llmbox-mcp

An [MCP](https://modelcontextprotocol.io) server that manages the lifecycle of
Docker containers running **Claude Code in remote-control mode**. It lets an MCP
client (Claude, an agent, etc.) spin up, list, and tear down disposable Claude
"boxes" on demand.

Built with the official [Go SDK](https://github.com/modelcontextprotocol/go-sdk).

## Components

| Path                 | What it is |
|----------------------|------------|
| `cmd/llmbox-mcp`     | The MCP server (Go). Speaks MCP over stdio, drives the Docker daemon. |
| `internal/docker`    | Thin wrapper over the Docker Engine API. |
| `Dockerfile.claude`  | Image for **Claude Code in remote-control mode** (`claude-remote`). |
| `Dockerfile.mcp`     | Image for **this MCP server** (`llmbox-mcp`). |
| `docker-compose.yml` | Convenience runner for the Claude remote-control container. |

The two Dockerfiles use distinct extensions (`.claude` / `.mcp`) and produce two
independent images.

## Tools

| Tool                | Arguments | Description |
|---------------------|-----------|-------------|
| `list_containers`   | –         | List the Claude containers created by this server. |
| `create_container`  | `image?`, `name?`, `env?` (`["KEY=VALUE", ...]`) | Create and start a new Claude remote-control container. |
| `destroy_container` | `container` (ID or name) | Stop and remove a managed container. |

### Safety

Every container the server creates is tagged with the `com.llmbox.managed=true`
label. `list_containers` and `destroy_container` are scoped to that label, so the
server can never list or destroy containers it did not create.

## Configuration

| Env var               | Default          | Purpose |
|-----------------------|------------------|---------|
| `LLMBOX_CLAUDE_IMAGE` | `claude-remote`  | Default image launched by `create_container`. |
| `DOCKER_HOST` etc.    | (Docker default) | Standard Docker client configuration. |

## Running

### Locally

```bash
go build -o llmbox-mcp ./cmd/llmbox-mcp
./llmbox-mcp   # speaks MCP over stdio
```

### As a container

The server needs access to a Docker socket to manage containers:

```bash
docker build -f Dockerfile.mcp -t llmbox-mcp .
docker run --rm -i \
  -v /var/run/docker.sock:/var/run/docker.sock \
  llmbox-mcp
```

### MCP client config

```json
{
  "mcpServers": {
    "llmbox": {
      "command": "docker",
      "args": [
        "run", "--rm", "-i",
        "-v", "/var/run/docker.sock:/var/run/docker.sock",
        "ghcr.io/clems4ever/llmbox-mcp:latest"
      ]
    }
  }
}
```

## Building the Claude image

```bash
docker build -f Dockerfile.claude -t claude-remote .
# or, with credential mounting / compose:
docker compose up --build
```

## CI

`.github/workflows/docker.yml` builds both images and pushes them to GitHub
Container Registry (`ghcr.io/<owner>/claude-remote` and
`ghcr.io/<owner>/llmbox-mcp`) on pushes to `main` and on version tags. Pull
requests build both images without pushing.
