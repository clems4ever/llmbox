// Command llmbox-mcp is an MCP server that manages the lifecycle of Docker
// containers running Claude Code in remote-control mode.
//
// It exposes three tools over stdio:
//
//	list_containers    - list the Claude containers this server created
//	create_container   - launch a new Claude remote-control container
//	destroy_container  - stop and remove a managed container
//
// The server talks to the Docker daemon via the standard Docker environment
// variables (DOCKER_HOST, etc.), so it can run on the host or inside a
// container with /var/run/docker.sock mounted in.
//
// Configuration (environment variables):
//
//	LLMBOX_CLAUDE_IMAGE  default image launched by create_container
//	                     (defaults to "claude-remote")
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/clems4ever/llmbox-mcp/internal/docker"
)

const version = "v0.1.0"

func main() {
	if err := run(); err != nil {
		log.Fatalf("llmbox-mcp: %v", err)
	}
}

func run() error {
	mgr, err := docker.NewManager(os.Getenv("LLMBOX_CLAUDE_IMAGE"))
	if err != nil {
		return err
	}
	defer mgr.Close()

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "llmbox-mcp",
		Version: version,
	}, nil)

	registerTools(server, mgr)

	// Run until the client disconnects.
	return server.Run(context.Background(), &mcp.StdioTransport{})
}

// ---- Tool input/output schemas ----

type listInput struct{}

type listOutput struct {
	Containers []docker.Container `json:"containers" jsonschema:"the Claude containers managed by this server"`
}

type createInput struct {
	Image string   `json:"image,omitempty" jsonschema:"image to launch; defaults to the configured Claude image"`
	Name  string   `json:"name,omitempty" jsonschema:"optional name for the new container"`
	Env   []string `json:"env,omitempty" jsonschema:"extra environment variables in KEY=VALUE form, e.g. CLAUDE_CODE_OAUTH_TOKEN=... or CLAUDE_REMOTE_ARGS=..."`
}

type createOutput struct {
	ID string `json:"id" jsonschema:"the ID of the newly created container"`
}

type destroyInput struct {
	Container string `json:"container" jsonschema:"the ID or name of the managed container to destroy"`
}

type destroyOutput struct {
	Destroyed string `json:"destroyed" jsonschema:"the ID or name of the container that was destroyed"`
}

// containerManager is the behaviour registerTools depends on. *docker.Manager
// implements it; tests supply a fake.
type containerManager interface {
	List(ctx context.Context) ([]docker.Container, error)
	Create(ctx context.Context, opts docker.CreateOptions) (string, error)
	Destroy(ctx context.Context, idOrName string) error
}

// registerTools wires the Docker manager into MCP tools.
func registerTools(server *mcp.Server, mgr containerManager) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_containers",
		Description: "List the Claude remote-control containers created by this server.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ listInput) (*mcp.CallToolResult, listOutput, error) {
		cs, err := mgr.List(ctx)
		if err != nil {
			return nil, listOutput{}, err
		}
		return nil, listOutput{Containers: cs}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "create_container",
		Description: "Create and start a new Claude remote-control container from the Claude image.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in createInput) (*mcp.CallToolResult, createOutput, error) {
		id, err := mgr.Create(ctx, docker.CreateOptions{
			Image: in.Image,
			Name:  in.Name,
			Env:   in.Env,
		})
		if err != nil {
			return nil, createOutput{}, err
		}
		return nil, createOutput{ID: id}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "destroy_container",
		Description: "Stop and remove a managed Claude container by ID or name.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in destroyInput) (*mcp.CallToolResult, destroyOutput, error) {
		if in.Container == "" {
			return nil, destroyOutput{}, fmt.Errorf("container ID or name is required")
		}
		if err := mgr.Destroy(ctx, in.Container); err != nil {
			return nil, destroyOutput{}, err
		}
		return nil, destroyOutput{Destroyed: in.Container}, nil
	})
}
