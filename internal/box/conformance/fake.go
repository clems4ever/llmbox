// Package conformance provides the backend-neutral behavioural contract for box
// provisioners (Run) and an in-process Fake provisioner that satisfies it without
// Docker. Run is exercised against the Fake here, and is reused by the Docker
// backend's integration test, so both backends are validated by exactly the same
// assertions.
package conformance

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/agent"
	"github.com/clems4ever/llmbox/internal/box"
	"github.com/clems4ever/llmbox/internal/sandbox"
	"github.com/clems4ever/llmbox/testutils"
)

// Fake is an in-process box.Provisioner: each box is a real guest agent (backed
// by the mock claude) serving a Unix socket, with a per-box HOME so concurrent
// boxes stay isolated. It exercises the whole Manager + agent-protocol stack with
// no Docker, and its boxes are cleaned up when the test ends.
type Fake struct {
	t       testing.TB
	baseDir string
	claude  string

	mu    sync.Mutex
	n     int
	boxes map[string]*fakeInstance
}

// NewFake returns a Fake provisioner whose boxes are torn down on t.Cleanup.
//
// @arg t The test the provisioner's boxes and temp files are scoped to.
// @return *Fake A ready in-process provisioner.
//
// @testcase TestConformanceFake runs the contract against a Fake built here.
func NewFake(t testing.TB) *Fake {
	t.Helper()
	base := t.TempDir()
	claude := filepath.Join(base, "claude")
	if err := os.WriteFile(claude, []byte(testutils.MockClaudeScript), 0o755); err != nil {
		t.Fatalf("writing mock claude: %v", err)
	}
	f := &Fake{t: t, baseDir: base, claude: claude, boxes: map[string]*fakeInstance{}}
	t.Cleanup(f.shutdown)
	return f
}

// shutdown tears down every still-running box.
//
// @testcase TestConformanceFake relies on shutdown to clean up boxes.
func (f *Fake) shutdown() {
	f.mu.Lock()
	insts := make([]*fakeInstance, 0, len(f.boxes))
	for _, inst := range f.boxes {
		insts = append(insts, inst)
	}
	f.mu.Unlock()
	for _, inst := range insts {
		_ = inst.Destroy(context.Background())
	}
}

// Provision starts a new box: a per-box HOME and socket, a guest agent backed by
// the mock claude, registered in the pending phase.
//
// @arg ctx Context for waiting on the box's control socket.
// @arg opts The caller-controlled inputs for the box.
// @return box.Instance A handle to the new box.
// @error error if the box's control socket does not appear.
//
// @testcase TestConformanceFake provisions boxes through this method.
func (f *Fake) Provision(ctx context.Context, opts sandbox.CreateOptions) (box.Instance, error) {
	f.mu.Lock()
	f.n++
	n := f.n
	f.mu.Unlock()

	dir := filepath.Join(f.baseDir, fmt.Sprintf("box%d", n))
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, fmt.Errorf("creating box home: %w", err)
	}
	sock := filepath.Join(dir, "control.sock")

	a := agent.New(agent.Options{ClaudeCmd: f.claude, Home: home})
	actx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- a.ListenAndServe(actx, sock) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			return nil, fmt.Errorf("box control socket did not appear")
		}
		time.Sleep(5 * time.Millisecond)
	}

	inst := &fakeInstance{
		fake:    f,
		id:      fmt.Sprintf("fakebox%06d", n),
		boxID:   opts.BoxID,
		phase:   "pending",
		created: time.Now().Unix(),
		sock:    sock,
		agent:   a,
		cancel:  cancel,
		errc:    errc,
	}
	f.mu.Lock()
	f.boxes[inst.id] = inst
	f.mu.Unlock()
	return inst, nil
}

// List returns a handle to every still-running box.
//
// @arg ctx Context (unused by the fake).
// @return []box.Instance One handle per managed box.
// @error error Always nil for the fake.
//
// @testcase TestConformanceFake lists boxes through this method.
func (f *Fake) List(ctx context.Context) ([]box.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	insts := make([]box.Instance, 0, len(f.boxes))
	for _, inst := range f.boxes {
		insts = append(insts, inst)
	}
	return insts, nil
}

// Find resolves an ID or box ID to its instance, returning a wrapped
// sandbox.ErrBoxNotFound when none matches.
//
// @arg ctx Context (unused by the fake).
// @arg idOrName The instance ID or caller box ID to resolve.
// @return box.Instance The matched box.
// @error error Wrapped sandbox.ErrBoxNotFound when no box matches.
//
// @testcase TestConformanceFake resolves boxes and rejects unknown ones through Find.
func (f *Fake) Find(ctx context.Context, idOrName string) (box.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if inst, ok := f.boxes[idOrName]; ok {
		return inst, nil
	}
	for _, inst := range f.boxes {
		if inst.boxID != "" && inst.boxID == idOrName {
			return inst, nil
		}
	}
	return nil, fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, idOrName)
}

// fakeInstance is one Fake box: a guest agent on a Unix socket plus its phase.
type fakeInstance struct {
	fake    *Fake
	id      string
	boxID   string
	sock    string
	created int64
	agent   *agent.Agent
	cancel  context.CancelFunc
	errc    chan error

	mu    sync.Mutex
	phase string
}

// Meta returns the box's current view.
//
// @return sandbox.Box The box's ID, box ID, phase, and creation time.
//
// @testcase TestConformanceFake reads box metadata via Meta.
func (i *fakeInstance) Meta() sandbox.Box {
	i.mu.Lock()
	phase := i.phase
	i.mu.Unlock()
	return sandbox.Box{
		InstanceID: i.id,
		Name:       i.id,
		BoxID:      i.boxID,
		Image:      "fake",
		State:      "running",
		Phase:      phase,
		Created:    i.created,
	}
}

// Control opens a new connection to the box's agent socket.
//
// @arg ctx Context for the dial.
// @return net.Conn A control connection to the box's agent.
// @error error if the socket cannot be dialled.
//
// @testcase TestConformanceFake talks to boxes over Control.
func (i *fakeInstance) Control(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", i.sock)
}

// MarkReady moves the box to the ready phase.
//
// @arg ctx Context (unused by the fake).
// @error error Always nil for the fake.
//
// @testcase TestConformanceFake marks a box ready after login.
func (i *fakeInstance) MarkReady(ctx context.Context) error {
	i.mu.Lock()
	i.phase = "ready"
	i.mu.Unlock()
	return nil
}

// Destroy shuts the box's agent down and deregisters it. Destroying an
// already-gone box returns a wrapped sandbox.ErrBoxNotFound.
//
// @arg ctx Context (unused by the fake).
// @error error Wrapped sandbox.ErrBoxNotFound when the box is already gone.
//
// @testcase TestConformanceFake destroys boxes and is idempotent through Destroy.
func (i *fakeInstance) Destroy(ctx context.Context) error {
	i.fake.mu.Lock()
	_, ok := i.fake.boxes[i.id]
	delete(i.fake.boxes, i.id)
	i.fake.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, i.id)
	}
	i.agent.Shutdown()
	i.cancel()
	<-i.errc
	return nil
}
