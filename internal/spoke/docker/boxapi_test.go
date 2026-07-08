package docker

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/clems4ever/llmbox/internal/shared/cluster"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/boxapi"
)

// fakePortSvc is a recording boxapi.PortService asserting which box identity
// the per-box listeners stamp onto forwarded requests.
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

// newPortTestProvisioner builds a Provisioner over the fake docker with a
// box-port service wired in.
func newPortTestProvisioner(t *testing.T, f *fakeDocker, svc boxapi.PortService) *Provisioner {
	t.Helper()
	t.Cleanup(f.closeListeners)
	p := &Provisioner{cli: f, defaultImage: "test-image", socketDir: t.TempDir(), ports: svc, apiSrvs: map[string]*boxapi.Server{}}
	t.Cleanup(p.stopAllBoxAPIs)
	return p
}

// openPortViaSocket POSTs an open_port request over the box's boxapi socket the
// way curl --unix-socket does inside the box, returning the HTTP status.
func openPortViaSocket(t *testing.T, sockPath string) (int, []byte) {
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
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// TestProvisionStartsBoxAPIListener checks Provision serves the box-port API in
// the box's socket dir, bound to the box's ID.
func TestProvisionStartsBoxAPIListener(t *testing.T) {
	f := &fakeDocker{}
	svc := &fakePortSvc{}
	p := newPortTestProvisioner(t, f, svc)

	inst, err := p.Provision(context.Background(), sandbox.CreateOptions{BoxID: "my-box"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	sockPath := filepath.Join(f.mountSource, boxapi.SocketName)
	status, body := openPortViaSocket(t, sockPath)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body %s", status, body)
	}
	var out struct {
		Port cluster.BoxPortInfo `json:"port"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Port.URL == "" {
		t.Errorf("body = %s", body)
	}
	svc.mu.Lock()
	if svc.lastBoxID != "my-box" {
		t.Errorf("stamped box ID = %q, want my-box", svc.lastBoxID)
	}
	svc.mu.Unlock()
	_ = inst
}

// TestDestroyStopsBoxAPIListener checks destroying a box also stops its
// box-port API listener.
func TestDestroyStopsBoxAPIListener(t *testing.T) {
	f := &fakeDocker{}
	p := newPortTestProvisioner(t, f, &fakePortSvc{})

	inst, err := p.Provision(context.Background(), sandbox.CreateOptions{BoxID: "my-box"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	sockPath := filepath.Join(f.mountSource, boxapi.SocketName)
	if err := inst.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := net.Dial("unix", sockPath); err == nil {
		t.Error("boxapi socket still accepts connections after Destroy")
	}
	p.apiMu.Lock()
	if len(p.apiSrvs) != 0 {
		t.Errorf("apiSrvs = %d entries after Destroy, want 0", len(p.apiSrvs))
	}
	p.apiMu.Unlock()
}

// TestCloseStopsBoxAPIListeners checks provisioner Close stops every live
// box-port listener.
func TestCloseStopsBoxAPIListeners(t *testing.T) {
	f := &fakeDocker{}
	p := newPortTestProvisioner(t, f, &fakePortSvc{})

	if _, err := p.Provision(context.Background(), sandbox.CreateOptions{BoxID: "my-box"}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	sockPath := filepath.Join(f.mountSource, boxapi.SocketName)
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := net.Dial("unix", sockPath); err == nil {
		t.Error("boxapi socket still accepts connections after Close")
	}
}

// TestProvisionCleansUpBoxAPIOnStartFailure checks a failed start leaves no
// live box-port listener behind.
func TestProvisionCleansUpBoxAPIOnStartFailure(t *testing.T) {
	f := &fakeDocker{startErr: context.DeadlineExceeded}
	p := newPortTestProvisioner(t, f, &fakePortSvc{})

	if _, err := p.Provision(context.Background(), sandbox.CreateOptions{BoxID: "my-box"}); err == nil {
		t.Fatal("expected Provision to fail")
	}
	p.apiMu.Lock()
	if len(p.apiSrvs) != 0 {
		t.Errorf("apiSrvs = %d entries after failed start, want 0", len(p.apiSrvs))
	}
	p.apiMu.Unlock()
}

// TestRecoverBoxAPIsRestartsListeners checks listeners are restarted for boxes
// recovered from container labels after a spoke restart.
func TestRecoverBoxAPIsRestartsListeners(t *testing.T) {
	f := &fakeDocker{}
	svc := &fakePortSvc{}
	p := newPortTestProvisioner(t, f, svc)

	// A pre-existing box: its socket dir survives on disk, its identity lives in
	// the container labels, but no listener is running (the old process died).
	const token = "cafebabe12345678"
	boxDir := filepath.Join(p.socketDir, token)
	if err := os.MkdirAll(boxDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f.listResult = []container.Summary{{
		ID:    "aabbccddeeff00112233",
		Names: []string{"/llmbox-aabbccddeeff"},
		Labels: map[string]string{
			ManagedLabel: "true",
			socketLabel:  token,
			BoxIDLabel:   "survivor-box",
		},
		State: "running",
	}}

	if err := p.RecoverBoxAPIs(context.Background()); err != nil {
		t.Fatalf("RecoverBoxAPIs: %v", err)
	}
	status, _ := openPortViaSocket(t, filepath.Join(boxDir, boxapi.SocketName))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	svc.mu.Lock()
	if svc.lastBoxID != "survivor-box" {
		t.Errorf("stamped box ID = %q, want survivor-box", svc.lastBoxID)
	}
	svc.mu.Unlock()
}
