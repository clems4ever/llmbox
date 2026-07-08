package firecracker

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	fcsdk "github.com/firecracker-microvm/firecracker-go-sdk"

	"github.com/clems4ever/llmbox/internal/shared/cluster"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/boxapi"
)

// fakePortSvc is a recording boxapi.PortService asserting which box identity
// the per-VM listeners stamp onto forwarded requests.
type fakePortSvc struct {
	mu        sync.Mutex
	lastBoxID string
}

// OpenBoxPort is a test helper.
func (f *fakePortSvc) OpenBoxPort(_ context.Context, boxID string, port int, _ string) (cluster.BoxPortInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastBoxID = boxID
	return cluster.BoxPortInfo{Port: port, URL: "https://x.example.com/"}, nil
}

// CloseBoxPort is a test helper.
func (f *fakePortSvc) CloseBoxPort(_ context.Context, boxID string, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastBoxID = boxID
	return nil
}

// ListBoxPorts is a test helper.
func (f *fakePortSvc) ListBoxPorts(_ context.Context, boxID string) ([]cluster.BoxPortInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastBoxID = boxID
	return nil, nil
}

// newPortsProvisioner is newFakeProvisioner with a box-port service and a
// caller-supplied state dir, so a second provisioner can rehydrate the first
// one's state.
func newPortsProvisioner(t *testing.T, stateDir string, svc boxapi.PortService) *Provisioner {
	t.Helper()
	rootfs := filepath.Join(stateDir, "base-rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("rootfs-bytes"), 0o600); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}
	p, err := NewProvisioner("/fake/vmlinux", rootfs, stateDir, svc)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.egress = &fakeEgress{}
	p.newMachine = func(ctx context.Context, cfg fcsdk.Config) (machine, error) {
		return &fakeMachine{path: cfg.VsockDevices[0].Path}, nil
	}
	return p
}

// postOverSocket POSTs an open_port request over a unix socket and returns the
// HTTP status.
func postOverSocket(t *testing.T, sockPath string) int {
	t.Helper()
	c := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: 5 * time.Second,
	}
	resp, err := c.Post("http://box/v1/open_port", "application/json", strings.NewReader(`{"port":3000}`))
	if err != nil {
		t.Fatalf("POST over %s: %v", sockPath, err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return resp.StatusCode
}

// TestProvisionStartsBoxAPIListener checks Provision pre-listens on the
// guest-initiated vsock UDS path, bound to the box's ID.
func TestProvisionStartsBoxAPIListener(t *testing.T) {
	svc := &fakePortSvc{}
	p := newPortsProvisioner(t, shortStateDir(t), svc)

	inst, err := p.Provision(context.Background(), sandbox.CreateOptions{BoxID: "vm-box"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	fi := inst.(*fcInstance)
	if status := postOverSocket(t, boxAPISocketPath(fi.vsockUDS)); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	svc.mu.Lock()
	if svc.lastBoxID != "vm-box" {
		t.Errorf("stamped box ID = %q, want vm-box", svc.lastBoxID)
	}
	svc.mu.Unlock()
}

// TestDestroyClosesBoxAPIListener checks destroying a box also closes its
// box-port API listener.
func TestDestroyClosesBoxAPIListener(t *testing.T) {
	p := newPortsProvisioner(t, shortStateDir(t), &fakePortSvc{})

	inst, err := p.Provision(context.Background(), sandbox.CreateOptions{BoxID: "vm-box"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	fi := inst.(*fcInstance)
	sockPath := boxAPISocketPath(fi.vsockUDS)
	if err := inst.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := net.Dial("unix", sockPath); err == nil {
		t.Error("boxapi socket still accepts connections after Destroy")
	}
}

// TestRehydrateListsPriorBoxes checks a new provisioner over the same state dir
// sees boxes persisted by a previous run, reported stopped when their VMM is
// gone, with their TAP slot marked used.
func TestRehydrateListsPriorBoxes(t *testing.T) {
	stateDir := shortStateDir(t)
	p1 := newPortsProvisioner(t, stateDir, nil)
	inst, err := p1.Provision(context.Background(), sandbox.CreateOptions{BoxID: "crash-box"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	token := inst.Meta().InstanceID
	// Simulate a spoke crash: no Close (Close destroys the boxes).

	p2 := newPortsProvisioner(t, stateDir, nil)
	list, err := p2.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("rehydrated boxes = %d, want 1", len(list))
	}
	b := list[0].Meta()
	if b.InstanceID != token || b.BoxID != "crash-box" || b.State != "stopped" {
		t.Errorf("rehydrated meta = %+v, want token %s / crash-box / stopped", b, token)
	}
	if _, err := p2.Find(context.Background(), "crash-box"); err != nil {
		t.Errorf("Find by box id after rehydrate: %v", err)
	}
	p2.mu.Lock()
	if !p2.used[list[0].(*fcInstance).meta.NetIndex] {
		t.Error("rehydrated box's TAP slot is not marked used")
	}
	p2.mu.Unlock()
}

// TestRehydrateDestroysDeadBox checks a rehydrated box can be destroyed:
// its state dir is removed and its slot freed.
func TestRehydrateDestroysDeadBox(t *testing.T) {
	stateDir := shortStateDir(t)
	p1 := newPortsProvisioner(t, stateDir, nil)
	if _, err := p1.Provision(context.Background(), sandbox.CreateOptions{BoxID: "crash-box"}); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	p2 := newPortsProvisioner(t, stateDir, nil)
	inst, err := p2.Find(context.Background(), "crash-box")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	fi := inst.(*fcInstance)
	if err := inst.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Stat(boxDir(stateDir, fi.meta.Token)); !os.IsNotExist(err) {
		t.Errorf("box dir still exists after Destroy (stat err = %v)", err)
	}
	p2.mu.Lock()
	if p2.used[fi.meta.NetIndex] {
		t.Error("TAP slot still marked used after Destroy")
	}
	if len(p2.boxes) != 0 {
		t.Errorf("boxes = %d after Destroy, want 0", len(p2.boxes))
	}
	p2.mu.Unlock()
}

// TestRehydrateRestartsBoxAPIListeners checks rehydrated boxes get their
// box-port API listeners back, still bound to the right box ID.
func TestRehydrateRestartsBoxAPIListeners(t *testing.T) {
	stateDir := shortStateDir(t)
	p1 := newPortsProvisioner(t, stateDir, &fakePortSvc{})
	inst, err := p1.Provision(context.Background(), sandbox.CreateOptions{BoxID: "crash-box"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	sockPath := boxAPISocketPath(inst.(*fcInstance).vsockUDS)

	svc2 := &fakePortSvc{}
	newPortsProvisioner(t, stateDir, svc2)
	if status := postOverSocket(t, sockPath); status != http.StatusOK {
		t.Fatalf("status after rehydrate = %d, want 200", status)
	}
	svc2.mu.Lock()
	if svc2.lastBoxID != "crash-box" {
		t.Errorf("stamped box ID = %q, want crash-box", svc2.lastBoxID)
	}
	svc2.mu.Unlock()
}

// TestVMMAlive distinguishes a live API socket from a dead or missing one.
func TestVMMAlive(t *testing.T) {
	dir := shortStateDir(t)
	sock := filepath.Join(dir, "fc.sock")
	if vmmAlive(sock) {
		t.Error("vmmAlive = true for a missing socket")
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if !vmmAlive(sock) {
		t.Error("vmmAlive = false for a live socket")
	}
}

// TestHaltVMM sends the Ctrl-Alt-Del action to a fake Firecracker API socket.
func TestHaltVMM(t *testing.T) {
	dir := shortStateDir(t)
	sock := filepath.Join(dir, "fc.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	var mu sync.Mutex
	var gotMethod, gotPath, gotBody string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotMethod, gotPath, gotBody = r.Method, r.URL.Path, string(b)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	if err := haltVMM(sock); err != nil {
		t.Fatalf("haltVMM: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotMethod != http.MethodPut || gotPath != "/actions" || !strings.Contains(gotBody, "SendCtrlAltDel") {
		t.Errorf("VMM saw %s %s %s, want PUT /actions SendCtrlAltDel", gotMethod, gotPath, gotBody)
	}
}
