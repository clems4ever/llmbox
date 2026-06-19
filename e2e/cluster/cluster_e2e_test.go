//go:build e2e

// Package clustere2e holds the end-to-end test for hub-and-spoke clustering. It
// wires the real llmbox server (MCP tools + the /spoke/connect endpoint) with
// clustering enabled on a real HTTP listener, runs a real spoke that dials in
// over a real WebSocket and enrolls with a one-time join token, then drives the
// chatbot side over a real MCP client to create a box ON THE SPOKE and operate
// on it. Only the Docker box layer is simulated (per spoke); the cluster
// transport, enrollment, and routing are exercised for real.
//
// Unlike the main e2e workflow it needs no browser, so it runs standalone:
//
//	go test -tags e2e ./e2e/cluster/...
package clustere2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/clems4ever/llmbox/internal/cluster"
	"github.com/clems4ever/llmbox/internal/docker"
	"github.com/clems4ever/llmbox/internal/server"
)

// TestClusterEndToEnd exercises the full clustering path:
//
//  1. the operator mints a one-time join token for spoke "edge";
//  2. a spoke dials the hub over a WebSocket and enrolls with that token;
//  3. the chatbot creates a box over MCP with spoke="edge"; the box lands on the
//     spoke's (simulated) Docker layer, not the hub's local one;
//  4. list/exec/destroy over MCP all route to that spoke;
//  5. the join token is one-time: a second enrollment with it is rejected.
func TestClusterEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := server.OpenStore(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The operator mints a one-time join token for the spoke named "edge".
	joinToken, err := cluster.CreateJoinToken(store, "edge", time.Hour, time.Now())
	if err != nil {
		t.Fatalf("create join token: %v", err)
	}

	// The hub: real server with clustering enabled, on a real listener.
	localMgr := newFakeSpokeMgr("local")
	hub := cluster.NewHub(ctx, store, nil, nil)
	srv := server.New(localMgr, nil, "http://placeholder", 5*time.Minute, store, nil)
	srv.SetHub(hub)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	httpSrv := &http.Server{Handler: srv.Handler(srv.MCPServer("llmbox", "cluster-e2e"))}
	go func() { _ = httpSrv.Serve(ln) }()
	t.Cleanup(func() { _ = httpSrv.Close() })
	waitHealthy(t, "http://"+addr)

	// The spoke: a real spoke process (goroutine) dialing the hub over WebSocket,
	// backed by its own simulated Docker layer.
	edgeMgr := newFakeSpokeMgr("edge")
	wsURL := "ws://" + addr + "/spoke/connect"
	go func() {
		_ = cluster.Run(ctx, cluster.WebSocketDialer(wsURL), edgeMgr, joinToken, nil, func(cluster.Credentials) error { return nil })
	}()

	// The chatbot side, over a real MCP client.
	cs := connectMCP(t, "http://"+addr)

	// Create a box on the spoke. Retry until the spoke has finished enrolling.
	var createOut map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := callToolRaw(t, cs, "create_llmbox", map[string]any{"box_id": "b1", "spoke": "edge"})
		if err == nil {
			createOut = out
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("create on spoke never succeeded (spoke did not enroll): %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	containerID, _ := createOut["container_id"].(string)
	if containerID == "" {
		t.Fatalf("create_llmbox returned no container_id: %v", createOut)
	}
	// The box must have been created on the spoke, not the hub's local manager.
	if edgeMgr.creates() != 1 {
		t.Errorf("edge spoke creates = %d, want 1", edgeMgr.creates())
	}
	if localMgr.creates() != 0 {
		t.Errorf("local spoke creates = %d, want 0 (box should not run locally)", localMgr.creates())
	}

	// list_llmboxes shows the box tagged with its spoke.
	listOut := callTool(t, cs, "list_llmboxes", map[string]any{})
	if spoke := spokeOfBox(listOut, "b1"); spoke != "edge" {
		t.Fatalf("list shows box b1 on spoke %q, want edge", spoke)
	}

	// exec routes to the spoke.
	execOut := callTool(t, cs, "exec_llmbox", map[string]any{"box_id": "b1", "command": "echo hi"})
	if execOut["stdout"] != "hello-from-edge\n" {
		t.Fatalf("exec stdout = %q, want hello-from-edge", execOut["stdout"])
	}
	if edgeMgr.execs() != 1 {
		t.Errorf("edge spoke execs = %d, want 1", edgeMgr.execs())
	}

	// destroy routes to the spoke and removes the box there.
	if got := callTool(t, cs, "destroy_llmbox", map[string]any{"box_id": "b1"})["destroyed"]; got != "b1" {
		t.Fatalf("destroyed = %v, want b1", got)
	}
	if edgeMgr.live() != 0 {
		t.Errorf("edge spoke still has %d live box(es) after destroy", edgeMgr.live())
	}

	// The join token is one-time: a second enrollment with it must be rejected.
	enrollCtx, enrollCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer enrollCancel()
	err = cluster.Run(enrollCtx, cluster.WebSocketDialer(wsURL), newFakeSpokeMgr("edge2"), joinToken, nil, nil)
	if err == nil {
		t.Fatal("second enrollment with the same join token should have been rejected")
	}
}

// fakeSpokeMgr is a per-spoke simulated Docker box layer implementing
// cluster.BoxManager. It keeps boxes in memory and records call counts so the
// test can assert which spoke handled each operation.
type fakeSpokeMgr struct {
	name string

	mu          sync.Mutex
	boxes       map[string]string // containerID -> boxID
	createCount int
	execCount   int
}

func newFakeSpokeMgr(name string) *fakeSpokeMgr {
	return &fakeSpokeMgr{name: name, boxes: map[string]string{}}
}

func (m *fakeSpokeMgr) CreateLLMBox(_ context.Context, opts docker.CreateOptions) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := randHex(20)
	m.boxes[id] = opts.BoxID
	m.createCount++
	return id, "https://auth.example/", nil
}

func (m *fakeSpokeMgr) SubmitCode(_ context.Context, _, _ string) (string, error) {
	return "https://claude.ai/code/session", nil
}

func (m *fakeSpokeMgr) List(_ context.Context) ([]docker.Box, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []docker.Box
	for id, boxID := range m.boxes {
		out = append(out, docker.Box{ContainerID: id, BoxID: boxID, State: "running", Phase: "ready"})
	}
	return out, nil
}

func (m *fakeSpokeMgr) Destroy(_ context.Context, idOrName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.boxes {
		if id == idOrName || hasPrefix(id, idOrName) || hasPrefix(idOrName, id) {
			delete(m.boxes, id)
		}
	}
	return nil
}

func (m *fakeSpokeMgr) Logs(_ context.Context, _ string, _ int) (string, error) {
	return "log from " + m.name, nil
}

func (m *fakeSpokeMgr) Exec(_ context.Context, _ string, _ []string) (docker.ExecResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.execCount++
	return docker.ExecResult{Stdout: "hello-from-" + m.name + "\n", ExitCode: 0}, nil
}

func (m *fakeSpokeMgr) ReapOrphans(_ context.Context, _ time.Duration) ([]string, error) {
	return nil, nil
}

func (m *fakeSpokeMgr) creates() int { m.mu.Lock(); defer m.mu.Unlock(); return m.createCount }
func (m *fakeSpokeMgr) execs() int   { m.mu.Lock(); defer m.mu.Unlock(); return m.execCount }
func (m *fakeSpokeMgr) live() int    { m.mu.Lock(); defer m.mu.Unlock(); return len(m.boxes) }

func hasPrefix(s, prefix string) bool { return len(s) >= len(prefix) && s[:len(prefix)] == prefix }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// spokeOfBox returns the spoke tag of the box with the given box ID in a
// list_llmboxes output, or "" when absent.
func spokeOfBox(listOut map[string]any, boxID string) string {
	boxes, _ := listOut["boxes"].([]any)
	for _, b := range boxes {
		m, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if m["box_id"] == boxID {
			s, _ := m["spoke"].(string)
			return s
		}
	}
	return ""
}

// waitHealthy blocks until the server answers /healthz.
func waitHealthy(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never became healthy at %s: %v", base, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// connectMCP opens a streamable-HTTP MCP client session against the server.
func connectMCP(t *testing.T, base string) *mcp.ClientSession {
	t.Helper()
	transport := &mcp.StreamableClientTransport{Endpoint: base}
	client := mcp.NewClient(&mcp.Implementation{Name: "cluster-e2e-chatbot", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connecting MCP client: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// callTool calls an MCP tool and returns its structured output, failing on error.
func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	out, err := callToolRaw(t, cs, name, args)
	if err != nil {
		t.Fatalf("tool %s: %v", name, err)
	}
	return out
}

// callToolRaw calls an MCP tool, returning its output and any tool-level error.
func callToolRaw(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (map[string]any, error) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("calling %s: %v", name, err)
	}
	if res.IsError {
		return nil, &toolError{name: name}
	}
	out, _ := res.StructuredContent.(map[string]any)
	return out, nil
}

type toolError struct{ name string }

func (e *toolError) Error() string { return e.name + " returned a tool error" }
