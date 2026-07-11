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

	"github.com/clems4ever/llmbox/internal/guest"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/box"
	"github.com/clems4ever/llmbox/testutils"
)

// Fake is an in-process box.Provisioner: each box is a real guest (backed
// by the mock claude) serving a Unix socket, with a per-box HOME so concurrent
// boxes stay isolated. It exercises the whole Manager + guest-protocol stack with
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

// Provision starts a new box: a per-box HOME and socket, a guest backed by
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

	inst := &fakeInstance{
		fake:    f,
		id:      fmt.Sprintf("fakebox%06d", n),
		boxID:   opts.BoxID,
		phase:   "pending",
		created: time.Now().Unix(),
		sock:    sock,
		dir:     dir,
	}
	if err := inst.serve(); err != nil {
		return nil, err
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

// fakeInstance is one Fake box: a guest on a Unix socket plus its phase. dir is the
// box's persistent directory (home survives a pause), so serve can restart the
// guest on the same socket on resume.
type fakeInstance struct {
	fake    *Fake
	id      string
	boxID   string
	sock    string
	dir     string
	created int64

	mu     sync.Mutex
	phase  string
	paused bool
	guest  *guest.Guest
	cancel context.CancelFunc
	errc   chan error
}

// serve starts (or restarts) the box's guest listening on its socket and blocks
// until the socket appears. It is used by Provision and by Resume, reusing the
// box's persistent home so on-disk state (auth) survives a pause. Callers must not
// hold i.mu.
//
// @error error if the box home cannot be prepared or the control socket never appears.
//
// @testcase TestConformanceFake serves each provisioned box's guest via serve.
func (i *fakeInstance) serve() error {
	home := filepath.Join(i.dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		return fmt.Errorf("creating box home: %w", err)
	}
	a := guest.New(guest.Options{
		ClaudeCmd:      i.fake.claude,
		Home:           home,
		InitScriptPath: filepath.Join(i.dir, "init-script"),
	})
	actx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- a.ListenAndServe(actx, i.sock) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(i.sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			return fmt.Errorf("box control socket did not appear")
		}
		time.Sleep(5 * time.Millisecond)
	}
	i.mu.Lock()
	i.guest, i.cancel, i.errc = a, cancel, errc
	i.mu.Unlock()
	return nil
}

// stopGuest shuts the box's guest down and waits for its serve loop to return. It
// is the compute-teardown shared by Pause and Destroy. Callers must not hold i.mu.
//
// @testcase TestConformanceFake stops a box's guest via Destroy and Pause.
func (i *fakeInstance) stopGuest() {
	i.mu.Lock()
	g, cancel, errc := i.guest, i.cancel, i.errc
	i.guest, i.cancel, i.errc = nil, nil, nil
	i.mu.Unlock()
	if g != nil {
		g.Shutdown()
	}
	if cancel != nil {
		cancel()
	}
	if errc != nil {
		<-errc
	}
}

// Meta returns the box's current view.
//
// @return sandbox.Box The box's ID, box ID, phase, and creation time.
//
// @testcase TestConformanceFake reads box metadata via Meta.
func (i *fakeInstance) Meta() sandbox.Box {
	i.mu.Lock()
	phase, paused := i.phase, i.paused
	i.mu.Unlock()
	state := "running"
	if paused {
		state = sandbox.StatePaused
	}
	return sandbox.Box{
		InstanceID: i.id,
		Name:       i.id,
		BoxID:      i.boxID,
		Image:      "fake",
		State:      state,
		Phase:      phase,
		Created:    i.created,
	}
}

// Control opens a new connection to the box's guest socket.
//
// @arg ctx Context for the dial.
// @return net.Conn A control connection to the box's guest.
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

// Pause stops the box's guest to free compute while keeping its home (and the box
// registered), so Resume can restart it. Pausing an already-gone box returns a
// wrapped sandbox.ErrBoxNotFound.
//
// @arg ctx Context (unused by the fake).
// @error error Wrapped sandbox.ErrBoxNotFound when the box is already gone.
//
// @testcase TestConformanceFake pauses a box and reports it paused.
func (i *fakeInstance) Pause(ctx context.Context) error {
	i.fake.mu.Lock()
	_, ok := i.fake.boxes[i.id]
	i.fake.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, i.id)
	}
	i.stopGuest()
	i.mu.Lock()
	i.paused = true
	i.mu.Unlock()
	return nil
}

// Resume restarts a paused box's guest on its socket, reusing its home so on-disk
// state survives. Resuming an already-gone box returns a wrapped
// sandbox.ErrBoxNotFound.
//
// @arg ctx Context (unused by the fake).
// @error error Wrapped sandbox.ErrBoxNotFound when the box is already gone, or if the guest cannot be restarted.
//
// @testcase TestConformanceFake resumes a paused box and reports it running.
func (i *fakeInstance) Resume(ctx context.Context) error {
	i.fake.mu.Lock()
	_, ok := i.fake.boxes[i.id]
	i.fake.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, i.id)
	}
	if err := i.serve(); err != nil {
		return err
	}
	i.mu.Lock()
	i.paused = false
	i.mu.Unlock()
	return nil
}

// Destroy shuts the box's guest down and deregisters it. Destroying an
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
	i.stopGuest()
	return nil
}
