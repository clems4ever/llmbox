package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/clems4ever/llmbox-mcp/internal/docker"
)

// fakeManager is a stand-in for *docker.Manager that records calls and returns
// canned results, so the MCP tool wiring can be tested without a daemon.
type fakeManager struct {
	listResult []docker.Container
	listErr    error
	createID   string
	createErr  error
	createOpts docker.CreateOptions
	destroyErr error
	destroyed  string
}

func (f *fakeManager) List(context.Context) ([]docker.Container, error) {
	return f.listResult, f.listErr
}

func (f *fakeManager) Create(_ context.Context, opts docker.CreateOptions) (string, error) {
	f.createOpts = opts
	return f.createID, f.createErr
}

func (f *fakeManager) Destroy(_ context.Context, idOrName string) error {
	f.destroyed = idOrName
	return f.destroyErr
}

// newTestSession spins up the server with the fake manager and returns a
// connected in-memory client session.
func newTestSession(t *testing.T, mgr containerManager) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "llmbox-mcp", Version: "test"}, nil)
	registerTools(server, mgr)

	serverT, clientT := mcp.NewInMemoryTransports()
	ctx := context.Background()

	if _, err := server.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestToolsListed(t *testing.T) {
	cs := newTestSession(t, &fakeManager{})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}
	for _, want := range []string{"list_containers", "create_container", "destroy_container"} {
		if !got[want] {
			t.Errorf("tool %q not registered (have %v)", want, got)
		}
	}
}

func TestListContainersTool(t *testing.T) {
	mgr := &fakeManager{listResult: []docker.Container{
		{ID: "abc123456789", Name: "box1", Image: "claude-remote", State: "running"},
	}}
	cs := newTestSession(t, mgr)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_containers",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}
	var out listOutput
	decodeStructured(t, res, &out)
	if len(out.Containers) != 1 || out.Containers[0].Name != "box1" {
		t.Errorf("unexpected output: %+v", out.Containers)
	}
}

func TestCreateContainerTool(t *testing.T) {
	mgr := &fakeManager{createID: "newcontainerid"}
	cs := newTestSession(t, mgr)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "create_container",
		Arguments: map[string]any{
			"image": "my-image",
			"name":  "mybox",
			"env":   []string{"CLAUDE_CODE_OAUTH_TOKEN=tok"},
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}
	var out createOutput
	decodeStructured(t, res, &out)
	if out.ID != "newcontainerid" {
		t.Errorf("ID = %q, want newcontainerid", out.ID)
	}
	// Arguments must reach the manager.
	if mgr.createOpts.Image != "my-image" || mgr.createOpts.Name != "mybox" {
		t.Errorf("create opts not forwarded: %+v", mgr.createOpts)
	}
	if len(mgr.createOpts.Env) != 1 || mgr.createOpts.Env[0] != "CLAUDE_CODE_OAUTH_TOKEN=tok" {
		t.Errorf("env not forwarded: %v", mgr.createOpts.Env)
	}
}

func TestDestroyContainerTool(t *testing.T) {
	mgr := &fakeManager{}
	cs := newTestSession(t, mgr)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "destroy_container",
		Arguments: map[string]any{"container": "box1"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}
	if mgr.destroyed != "box1" {
		t.Errorf("manager.Destroy got %q, want box1", mgr.destroyed)
	}
}

func TestDestroyContainerToolRequiresArg(t *testing.T) {
	cs := newTestSession(t, &fakeManager{})
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "destroy_container",
		Arguments: map[string]any{"container": ""},
	})
	if err != nil {
		t.Fatalf("CallTool transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected a tool error when container is empty")
	}
}

func TestToolErrorPropagates(t *testing.T) {
	mgr := &fakeManager{listErr: errors.New("daemon down")}
	cs := newTestSession(t, mgr)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_containers",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected a tool error when the manager fails")
	}
}

// decodeStructured unmarshals a tool result's structured content into v.
func decodeStructured(t *testing.T, res *mcp.CallToolResult, v any) {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}
}
