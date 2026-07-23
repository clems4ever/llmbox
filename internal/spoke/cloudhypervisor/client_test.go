package cloudhypervisor

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeVMM is an in-test Cloud Hypervisor VMM: an HTTP server on a Unix socket that
// records the control calls it receives and answers the way the real VMM does (204
// for actions, 200 for vmm.ping). It lets the REST client be tested end to end
// without a real cloud-hypervisor process.
type fakeVMM struct {
	mu         sync.Mutex
	calls      []string
	createBody []byte
	srv        *http.Server
}

// startFakeVMM serves a fakeVMM on a Unix socket under a temp dir and returns it with
// the socket path; the server is shut down when the test ends.
//
// @arg t The test the server is scoped to.
// @return *fakeVMM The recording server.
// @return string The API socket path to point a client at.
//
// @testcase TestClientLifecycleCalls drives a client against a server started here.
func startFakeVMM(t *testing.T) (*fakeVMM, string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "ch-api.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeVMM{}
	f.srv = &http.Server{Handler: http.HandlerFunc(f.handle)}
	go func() { _ = f.srv.Serve(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = f.srv.Shutdown(ctx)
	})
	return f, sock
}

// handle records the request and replies as the real VMM would.
func (f *fakeVMM) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if r.URL.Path == "/api/v1/vm.create" {
		f.createBody = body
	}
	f.mu.Unlock()
	switch r.URL.Path {
	case "/api/v1/vmm.ping":
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"version":"test"}`))
	case "/api/v1/vm.boom":
		http.Error(w, "kaboom", http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// TestClientLifecycleCalls drives the client's lifecycle verbs against the fake VMM
// and checks each hits the right endpoint, and that createVM sends the VmConfig body.
func TestClientLifecycleCalls(t *testing.T) {
	f, sock := startFakeVMM(t)
	c := newAPIClient(sock)
	ctx := context.Background()

	cfg := buildVMConfig(vmConfigParams{Kernel: "/k", Rootfs: "/r", VsockUDS: "/v", VCPUs: 1, GPUs: []string{"0000:65:00.0"}})
	if err := c.createVM(ctx, cfg); err != nil {
		t.Fatalf("createVM: %v", err)
	}
	if err := c.bootVM(ctx); err != nil {
		t.Fatalf("bootVM: %v", err)
	}
	if err := c.pauseVM(ctx); err != nil {
		t.Fatalf("pauseVM: %v", err)
	}
	if err := c.resumeVM(ctx); err != nil {
		t.Fatalf("resumeVM: %v", err)
	}
	if err := c.shutdownVMM(ctx); err != nil {
		t.Fatalf("shutdownVMM: %v", err)
	}

	f.mu.Lock()
	calls := append([]string(nil), f.calls...)
	body := f.createBody
	f.mu.Unlock()

	want := []string{
		"PUT /api/v1/vm.create",
		"PUT /api/v1/vm.boot",
		"PUT /api/v1/vm.pause",
		"PUT /api/v1/vm.resume",
		"PUT /api/v1/vmm.shutdown",
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call[%d] = %q, want %q", i, calls[i], want[i])
		}
	}
	// The create body must be the VmConfig, GPU devices included.
	var sent vmConfig
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("create body not a VmConfig: %v (%s)", err, body)
	}
	if len(sent.Devices) != 1 || sent.Devices[0].Path != "/sys/bus/pci/devices/0000:65:00.0/" {
		t.Errorf("create body lost GPU devices: %+v", sent.Devices)
	}
}

// TestClientPing distinguishes a live VMM socket from a dead one.
func TestClientPing(t *testing.T) {
	_, sock := startFakeVMM(t)
	if !newAPIClient(sock).ping(context.Background()) {
		t.Error("ping of a live VMM should be true")
	}
	// A socket path with no server behind it is not alive.
	if newAPIClient(filepath.Join(t.TempDir(), "absent.sock")).ping(context.Background()) {
		t.Error("ping of an absent VMM should be false")
	}
}

// TestClientCallError surfaces the VMM's status and body when a call fails, so a
// caller sees why a box operation was rejected.
func TestClientCallError(t *testing.T) {
	_, sock := startFakeVMM(t)
	err := newAPIClient(sock).call(context.Background(), http.MethodPut, "vm.boom", nil, nil)
	if err == nil {
		t.Fatal("call to a failing endpoint should error")
	}
	if !contains(err.Error(), "kaboom") || !contains(err.Error(), "500") {
		t.Errorf("error should carry the VMM status and body: %v", err)
	}
}

// contains reports whether s contains sub.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
