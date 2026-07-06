package firecracker

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/box"
	"github.com/clems4ever/llmbox/internal/box/conformance"
	"github.com/clems4ever/llmbox/internal/sandbox"
)

// fcArtifacts returns the kernel/rootfs paths for the live Firecracker tests, or
// skips when the host is not set up to boot microVMs.
//
// @arg t The test to skip when artifacts are missing.
// @return string The guest kernel path.
// @return string The guest rootfs path.
//
// @testcase TestConformanceFirecracker skips via fcArtifacts when unset.
func fcArtifacts(t *testing.T) (string, string) {
	t.Helper()
	kernel := os.Getenv("LLMBOX_FC_KERNEL")
	rootfs := os.Getenv("LLMBOX_FC_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("set LLMBOX_FC_KERNEL and LLMBOX_FC_ROOTFS to run the live Firecracker tests")
	}
	if _, err := exec.LookPath(defaultFirecrackerBin); err != nil {
		t.Skipf("firecracker binary not on PATH: %v", err)
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm not available: %v", err)
	}
	return kernel, rootfs
}

// TestVMSurvivesRequestContextCancel is a regression test for boxes dying when the
// create request ends: it boots a real control-only box, cancels the context that
// created it, and checks the agent is still reachable. The firecracker process and
// the SDK's stop-on-context-done goroutine must both run on the provisioner's
// lifetime context, not the request's, or a later operation (submit code, exec,
// logs) hits a dead vsock with "connection refused".
//
// @testcase TestVMSurvivesRequestContextCancel boots a box, cancels the create context, and checks the VM survives.
func TestVMSurvivesRequestContextCancel(t *testing.T) {
	kernel, rootfs := fcArtifacts(t)
	stateDir, err := os.MkdirTemp("/tmp", "fc-ctx-")
	if err != nil {
		t.Fatalf("state dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })

	p, err := NewProvisioner(kernel, rootfs, stateDir)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.SetNetworking(false) // control-only: no CAP_NET_ADMIN needed
	t.Cleanup(func() { _ = p.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	inst, err := p.Provision(ctx, sandbox.CreateOptions{BoxID: "ctx-box"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	cancel()                    // the create request has returned
	time.Sleep(1 * time.Second) // a killed VM would be gone by now

	conn, err := inst.Control(context.Background())
	if err != nil {
		t.Fatalf("VM did not survive request-context cancellation: %v", err)
	}
	_ = conn.Close()
}

// TestConformanceFirecracker runs the backend-neutral behavioural contract against
// real Firecracker microVMs, proving the microVM backend behaves identically to the
// Docker backend and the in-process Fake. It requires a Firecracker host: /dev/kvm,
// the firecracker binary on PATH, and a guest kernel + rootfs (whose init is the
// llmbox agent on vsock) pointed at by env vars. It skips when any is missing, so a
// normal `go test` on a machine without a Firecracker host is unaffected.
//
// Set to run it:
//
//	LLMBOX_FC_KERNEL=/path/to/vmlinux
//	LLMBOX_FC_ROOTFS=/path/to/rootfs.ext4
//	LLMBOX_FC_STATE_DIR=/path/to/state   # optional; a short-path temp dir by default
//
// @testcase TestConformanceFirecracker runs the conformance suite against Firecracker.
func TestConformanceFirecracker(t *testing.T) {
	kernel, rootfs := fcArtifacts(t)
	// TAP/NAT egress setup needs CAP_NET_ADMIN. When not root, boot control-only
	// boxes (loopback + vsock): the conformance flow drives the agent over vsock
	// and its mock claude needs no network, so it exercises the full box lifecycle
	// either way. The real egress path is covered by the root-only egress test.
	networking := os.Geteuid() == 0
	stateRoot := os.Getenv("LLMBOX_FC_STATE_DIR")

	conformance.Run(t, func(t testing.TB) box.Provisioner {
		stateDir := stateRoot
		if stateDir == "" {
			var err error
			if stateDir, err = os.MkdirTemp("/tmp", "fc-conf-"); err != nil {
				t.Fatalf("state dir: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
		}
		p, err := NewProvisioner(kernel, rootfs, stateDir)
		if err != nil {
			t.Fatalf("NewProvisioner: %v", err)
		}
		p.SetNetworking(networking)
		// Ensure every VM this subtest booted is torn down even if the contract
		// leaves some alive.
		t.Cleanup(func() { _ = p.Close() })
		return p
	})
}

// compile-time check that *Provisioner satisfies the provisioner contract.
var _ box.Provisioner = (*Provisioner)(nil)
