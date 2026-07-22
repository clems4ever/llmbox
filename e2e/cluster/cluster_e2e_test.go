//go:build e2e

// Package clustere2e holds the end-to-end test for hub-and-spoke clustering. It
// wires the real llmbox server (box-control API + the /spoke/connect endpoint)
// with clustering enabled on a real HTTP listener, runs a real spoke that dials
// in over a real WebSocket and enrolls with a one-time join token, then drives
// the chatbot side over the real box-control HTTP API to create a box ON THE
// SPOKE and operate on it. Only the Docker box layer is simulated (per spoke);
// the cluster transport, enrollment, and routing are exercised for real.
//
// Unlike the main e2e workflow it needs no browser, so it runs standalone:
//
//	go test -tags e2e ./e2e/cluster/...
package clustere2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/hub"
	"github.com/clems4ever/llmbox/internal/hub/apikey"
	"github.com/clems4ever/llmbox/internal/shared/api"
	"github.com/clems4ever/llmbox/internal/shared/cluster"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// TestClusterEndToEnd exercises the full clustering path:
//
//  1. the operator mints a one-time join token for spoke "edge";
//  2. a spoke dials the hub over a WebSocket and enrolls with that token;
//  3. the chatbot creates a box over the API with spoke="edge"; the box lands on
//     the spoke's (simulated) Docker layer;
//  4. list/exec/destroy over the API all route to that spoke;
//  5. the join token is one-time: a second enrollment with it is rejected.
func TestClusterEndToEnd(t *testing.T) {
	ctx := t.Context()

	store, err := hub.OpenStore(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The operator mints a one-time join token for the spoke named "edge".
	joinToken, err := cluster.CreateJoinToken(store, "edge", "docker", time.Hour, time.Now())
	if err != nil {
		t.Fatalf("create join token: %v", err)
	}

	// The hub: real server with clustering enabled, on a real listener. It runs no
	// box backend of its own — every box runs on a remote spoke.
	clusterHub := cluster.NewHub(ctx, store, nil, nil, nil)
	srv := hub.New(nil, "http://placeholder", store, nil)
	srv.SetHub(clusterHub)

	// A single listener carries everything: /spoke/connect, /healthz, and the
	// box-control API under /api/v1/.
	uiLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	uiAddr := uiLn.Addr().String()
	httpSrv := &http.Server{Handler: srv.APIHandler()}
	go func() { _ = httpSrv.Serve(uiLn) }()
	t.Cleanup(func() { _ = httpSrv.Close() })
	waitHealthy(t, "http://"+uiAddr)

	// The spoke: a real spoke process (goroutine) dialing the hub over WebSocket,
	// backed by its own simulated Docker layer.
	edgeMgr := newFakeSpokeMgr("edge", "box:e2e")
	wsURL := "ws://" + uiAddr + "/spoke/connect"
	go func() {
		_ = cluster.Run(ctx, cluster.WebSocketDialer(wsURL), edgeMgr, joinToken, nil, func(cluster.Credentials) error { return nil })
	}()

	// The chatbot side, over the box-control API on the single server. The API is
	// authenticated, so mint the key a deployed programmatic caller would be given.
	apiKey, err := apikey.Create(store, "e2e", time.Hour, time.Now())
	if err != nil {
		t.Fatalf("mint api key: %v", err)
	}
	c := api.NewClient("http://"+uiAddr, nil)
	c.SetAPIKey(apiKey)

	// Create a box on the spoke. Retry until the spoke has finished enrolling.
	var createSess api.BoxSession
	deadline := time.Now().Add(10 * time.Second)
	for {
		sess, err := c.CreateBox(ctx, sandbox.CreateOptions{BoxID: "b1", SpokeName: "edge"})
		if err == nil {
			createSess = sess
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("create on spoke never succeeded (spoke did not enroll): %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if createSess.Generation == "" {
		t.Fatalf("CreateBox returned no instance_id: %+v", createSess)
	}
	// The box must have been created on the spoke.
	if edgeMgr.creates() != 1 {
		t.Errorf("edge spoke creates = %d, want 1", edgeMgr.creates())
	}
	// The spoke launched its OWN configured image; the hub named none.
	if got := edgeMgr.image(); got != "box:e2e" {
		t.Errorf("edge spoke create image = %q, want box:e2e (the spoke's own)", got)
	}

	// ListBoxes shows the box tagged with its spoke.
	boxes, err := c.ListBoxes(ctx)
	if err != nil {
		t.Fatalf("ListBoxes: %v", err)
	}
	if spoke := spokeOfBox(boxes, "b1"); spoke != "edge" {
		t.Fatalf("list shows box b1 on spoke %q, want edge", spoke)
	}

	// SpokeStatuses reports the edge spoke as connected.
	spokes, err := c.SpokeStatuses(ctx)
	if err != nil {
		t.Fatalf("SpokeStatuses: %v", err)
	}
	if !spokeConnected(spokes, "edge") {
		t.Fatalf("SpokeStatuses does not show edge connected: %+v", spokes)
	}

	// exec routes to the spoke.
	execRes, err := c.BoxExec(ctx, "b1", "echo hi")
	if err != nil {
		t.Fatalf("BoxExec: %v", err)
	}
	if execRes.Stdout != "hello-from-edge\n" {
		t.Fatalf("exec stdout = %q, want hello-from-edge", execRes.Stdout)
	}
	if edgeMgr.execs() != 1 {
		t.Errorf("edge spoke execs = %d, want 1", edgeMgr.execs())
	}

	// destroy routes to the spoke and removes the box there.
	if err := c.DestroyBox(ctx, "b1"); err != nil {
		t.Fatalf("DestroyBox: %v", err)
	}
	if edgeMgr.live() != 0 {
		t.Errorf("edge spoke still has %d live box(es) after destroy", edgeMgr.live())
	}

	// The join token is one-time: a second enrollment with it must be rejected.
	enrollCtx, enrollCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer enrollCancel()
	err = cluster.Run(enrollCtx, cluster.WebSocketDialer(wsURL), newFakeSpokeMgr("edge2", "box:e2e"), joinToken, nil, nil)
	if err == nil {
		t.Fatal("second enrollment with the same join token should have been rejected")
	}
}

// fakeSpokeMgr is a per-spoke simulated Docker box layer implementing
// cluster.BoxManager. It keeps boxes in memory and records call counts so the
// test can assert which spoke handled each operation.
type fakeSpokeMgr struct {
	name string
	img  string // the spoke's own configured image, launched for every box

	mu          sync.Mutex
	boxes       map[string]string // containerID -> boxID
	createCount int
	execCount   int
	launched    string // image launched for the most recent create (the spoke's own)
	dialTarget  string // address DialBox connects to (a real upstream "box" server)
}

// newFakeSpokeMgr builds an empty simulated spoke box manager that launches img
// for every box, mirroring a real spoke owning its box image.
func newFakeSpokeMgr(name, img string) *fakeSpokeMgr {
	return &fakeSpokeMgr{name: name, img: img, boxes: map[string]string{}}
}

// Create simulates launching a box, recording the call and returning a fake container ID.
func (m *fakeSpokeMgr) Create(_ context.Context, opts sandbox.CreateOptions) (sandbox.CreateResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := randHex(20)
	m.boxes[id] = opts.BoxID
	m.createCount++
	// The spoke launches its own configured image; the create request names none.
	m.launched = m.img
	return sandbox.CreateResult{InstanceID: id}, nil
}

// image returns the image launched for the most recent create call, under the lock.
func (m *fakeSpokeMgr) image() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.launched
}

// List returns the spoke's in-memory boxes.
func (m *fakeSpokeMgr) List(_ context.Context) ([]sandbox.Box, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []sandbox.Box
	for id, boxID := range m.boxes {
		out = append(out, sandbox.Box{InstanceID: id, BoxID: boxID, State: "running", Phase: "ready"})
	}
	return out, nil
}

// Destroy removes a matching in-memory box, by box ID or container-ID prefix.
// Like the real docker manager, destroying a box that is not here fails with a
// not-found error — the case a human-removed container produces.
func (m *fakeSpokeMgr) Destroy(_ context.Context, idOrName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, boxID := range m.boxes {
		if boxID == idOrName || id == idOrName || hasPrefix(id, idOrName) || hasPrefix(idOrName, id) {
			delete(m.boxes, id)
			return nil
		}
	}
	return fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, idOrName)
}

// Pause is a no-op in the simulation and always succeeds.
func (m *fakeSpokeMgr) Pause(_ context.Context, _ string) error {
	return nil
}

// Resume is a no-op in the simulation and always succeeds.
func (m *fakeSpokeMgr) Resume(_ context.Context, _ string) error {
	return nil
}

// humanDestroy simulates an operator removing a box's container directly on the
// host, out of band: the box vanishes from the spoke without going through the
// cluster Destroy path, so a later Destroy sees no such box.
func (m *fakeSpokeMgr) humanDestroy(boxID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, bid := range m.boxes {
		if bid == boxID {
			delete(m.boxes, id)
		}
	}
}

// hasBox reports whether the spoke currently holds a box with the given box ID.
func (m *fakeSpokeMgr) hasBox(boxID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, bid := range m.boxes {
		if bid == boxID {
			return true
		}
	}
	return false
}

// Exec records the call and returns canned output identifying this spoke.
func (m *fakeSpokeMgr) Exec(_ context.Context, _ string, _ []string) (sandbox.ExecResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.execCount++
	return sandbox.ExecResult{Stdout: "hello-from-" + m.name + "\n", ExitCode: 0}, nil
}

// setDialTarget points DialBox at addr (a real loopback server standing in for a
// box's HTTP server), under the lock so the spoke goroutine reads it safely.
func (m *fakeSpokeMgr) setDialTarget(addr string) {
	m.mu.Lock()
	m.dialTarget = addr
	m.mu.Unlock()
}

// DialBox resolves the identifier the way the real docker manager does — by box
// ID or container ID (or prefix), mirroring the manager's Find — then dials the
// configured target, standing in for a connection to a port inside a box. It
// makes the spoke satisfy cluster.BoxDialer, so the spoke can service proxy_http
// requests forwarded from the hub over the real WebSocket. Since the hub now
// addresses boxes by box ID (it forwards rec.BoxID), a request that names the box
// by its box-id label resolves here, exactly as the real manager would.
func (m *fakeSpokeMgr) DialBox(ctx context.Context, idOrName string, _ int) (net.Conn, error) {
	m.mu.Lock()
	target := m.dialTarget
	ok := false
	for id, boxID := range m.boxes {
		if boxID == idOrName || id == idOrName || hasPrefix(id, idOrName) || hasPrefix(idOrName, id) {
			ok = true
			break
		}
	}
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, idOrName)
	}
	if target == "" {
		return nil, fmt.Errorf("no dial target configured for spoke %q", m.name)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", target)
}

// creates returns how many boxes were created on this spoke.
func (m *fakeSpokeMgr) creates() int { m.mu.Lock(); defer m.mu.Unlock(); return m.createCount }

// execs returns how many commands were exec'd on this spoke.
func (m *fakeSpokeMgr) execs() int { m.mu.Lock(); defer m.mu.Unlock(); return m.execCount }

// live returns the number of boxes currently on this spoke.
func (m *fakeSpokeMgr) live() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.boxes) }

// hasPrefix reports whether s starts with prefix.
func hasPrefix(s, prefix string) bool { return len(s) >= len(prefix) && s[:len(prefix)] == prefix }

// randHex returns n random bytes hex-encoded, for fake container IDs.
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// spokeOfBox returns the spoke tag of the box with the given box ID in a
// ListBoxes result, or "" when absent.
func spokeOfBox(boxes []api.BoxView, boxID string) string {
	for _, b := range boxes {
		if b.BoxID == boxID {
			return b.Spoke
		}
	}
	return ""
}

// spokeConnected reports whether the SpokeStatuses result marks the named spoke connected.
func spokeConnected(spokes []api.SpokeStatus, name string) bool {
	for _, s := range spokes {
		if s.Name == name {
			return s.Connected
		}
	}
	return false
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
