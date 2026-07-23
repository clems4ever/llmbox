package cloudhypervisor

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/guest"
)

// fakeLauncher is a launcher that runs a real in-process guest for each box instead
// of a Cloud Hypervisor VM, so the whole Provisioner lifecycle can be exercised in CI
// without KVM. It mirrors the Firecracker backend's fake machine and the conformance
// Fake: the guest serves the box's real control protocol over a plain Unix socket at
// the box's vsock path, and its HOME lives under the box directory so on-disk state
// survives a pause/resume. This is what lets conformance.Run — the same contract
// Docker and Firecracker pass — run against the Cloud Hypervisor provisioner here.
type fakeLauncher struct{}

// fakeVM is a running fake box: cancelling its context stops the guest.
type fakeVM struct {
	cancel context.CancelFunc
	errc   chan error
}

// Stop shuts the box's guest down and waits for its serve loop to return.
//
// @error error Always nil.
//
// @testcase TestConformanceCloudHypervisorFake stops boxes through this handle.
func (v *fakeVM) Stop() error {
	if v.cancel != nil {
		v.cancel()
	}
	if v.errc != nil {
		<-v.errc
	}
	return nil
}

// Launch starts a real guest for the box on its vsock path, reusing a HOME under the
// box directory so a resumed box keeps its on-disk state, and blocks until the
// control socket appears.
//
// @arg ctx Unused; the guest runs on its own lifetime.
// @arg spec The box's VM parameters (only BoxDir and VsockUDS are used).
// @return vmHandle A handle whose Stop shuts the guest down.
// @error error if the box HOME cannot be created or the control socket never appears.
//
// @testcase TestConformanceCloudHypervisorFake launches every box through this method.
func (l *fakeLauncher) Launch(ctx context.Context, spec vmSpec) (vmHandle, error) {
	home := filepath.Join(spec.BoxDir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, fmt.Errorf("creating box home: %w", err)
	}
	// A resumed box reuses the same socket path; clear any stale socket file so the
	// guest can bind it again.
	_ = os.Remove(spec.VsockUDS)
	g := guest.New(guest.Options{
		Home:           home,
		InitScriptPath: filepath.Join(spec.BoxDir, "init-script"),
	})
	gctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- g.ListenAndServe(gctx, spec.VsockUDS) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(spec.VsockUDS); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			return nil, fmt.Errorf("fake box control socket did not appear")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return &fakeVM{cancel: cancel, errc: errc}, nil
}

// Dial connects to the box's guest over its plain Unix socket (the fake has no vsock
// handshake).
//
// @arg ctx Context for the dial.
// @arg vsockUDS The box's control socket path.
// @return net.Conn A connection to the guest.
// @error error if the socket cannot be dialled.
//
// @testcase TestConformanceCloudHypervisorFake talks to boxes through Dial.
func (l *fakeLauncher) Dial(ctx context.Context, vsockUDS string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", vsockUDS)
}

// Alive always reports false: the fake never leaves an orphaned VMM, so rehydrate
// treats any reloaded box as stopped.
//
// @arg apiSock Unused.
// @return bool Always false.
//
// @testcase TestConformanceCloudHypervisorFake never reports a fake orphan alive.
func (l *fakeLauncher) Alive(apiSock string) bool { return false }

// Halt is a no-op: the fake has no orphaned VMM to stop.
//
// @arg apiSock Unused.
// @error error Always nil.
//
// @testcase TestConformanceCloudHypervisorFake halts nothing through this method.
func (l *fakeLauncher) Halt(apiSock string) error { return nil }

// newFakeProvisioner builds a Provisioner backed by the fake launcher, with a temp
// state dir and dummy (unused) kernel/rootfs paths, and tears its boxes down when the
// test ends. It is the NewProvisioner conformance backends pass to conformance.Run.
//
// @arg t The test the provisioner's boxes and temp files are scoped to.
// @return *Provisioner A provisioner whose boxes run in-process.
//
// @testcase TestConformanceCloudHypervisorFake builds the provisioner under test here.
func newFakeProvisioner(t testing.TB) *Provisioner {
	t.Helper()
	p, err := NewProvisioner("fake-kernel", "fake-rootfs", t.TempDir())
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.launcher = &fakeLauncher{}
	t.Cleanup(func() {
		boxes, _ := p.List(context.Background())
		for _, b := range boxes {
			_ = b.Destroy(context.Background())
		}
	})
	return p
}
