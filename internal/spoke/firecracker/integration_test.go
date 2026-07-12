package firecracker

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/box"
	"github.com/clems4ever/llmbox/internal/spoke/box/conformance"
)

// closeAndDestroy tears down every box the provisioner still holds, then closes it.
// The live tests use it instead of a bare Close because the production Close is
// release-only — it deliberately leaves box VMs running so a respawned spoke
// rehydrates them — so a test must destroy its boxes explicitly or leak real
// firecracker processes on the host.
//
// @arg t The test the cleanup is registered on.
// @arg p The provisioner whose boxes to destroy and then close.
//
// @testcase TestConformanceFirecracker reaps its live boxes through closeAndDestroy.
func closeAndDestroy(t testing.TB, p *Provisioner) {
	t.Helper()
	boxes, _ := p.List(context.Background())
	for _, b := range boxes {
		_ = b.Destroy(context.Background())
	}
	_ = p.Close()
}

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
// created it, and checks the guest is still reachable. The firecracker process and
// the SDK's stop-on-context-done goroutine must both run on a background context,
// not the request's, or a later operation (submit code, exec, logs) hits a dead
// vsock with "connection refused".
//
// @testcase TestVMSurvivesRequestContextCancel boots a box, cancels the create context, and checks the VM survives.
func TestVMSurvivesRequestContextCancel(t *testing.T) {
	kernel, rootfs := fcArtifacts(t)
	stateDir, err := os.MkdirTemp("/tmp", "fc-ctx-")
	if err != nil {
		t.Fatalf("state dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })

	p, err := NewProvisioner(kernel, rootfs, stateDir, nil)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.SetNetworking(false) // control-only: no CAP_NET_ADMIN needed
	t.Cleanup(func() { closeAndDestroy(t, p) })

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
// llmbox guest on vsock) pointed at by env vars. It skips when any is missing, so a
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
	// boxes (loopback + vsock): the conformance flow drives the guest over vsock
	// (Init/Exec/Dial) and needs no network, so it exercises the full box lifecycle
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
		p, err := NewProvisioner(kernel, rootfs, stateDir, nil)
		if err != nil {
			t.Fatalf("NewProvisioner: %v", err)
		}
		p.SetNetworking(networking)
		// Ensure every VM this subtest booted is torn down even if the contract
		// leaves some alive.
		t.Cleanup(func() { closeAndDestroy(t, p) })
		return p
	})
}

// TestBoxAPIOverVsock proves Firecracker's guest-initiated vsock convention end
// to end on a real microVM: the provisioner pre-listens on <vsock_uds>_5001,
// the guest bridges /run/llmbox/boxapi.sock to host port 5001, and a
// curl inside the guest reaches the host-side box-port API — with the box's
// identity stamped by the host listener, not by anything the guest sent. It
// needs a rootfs whose guest runs with --boxapi-port 5001 and which ships curl
// (the production box rootfs; the busybox conformance rootfs skips).
//
// @testcase TestBoxAPIOverVsock curls the in-guest box API socket on a live microVM.
func TestBoxAPIOverVsock(t *testing.T) {
	kernel, rootfs := fcArtifacts(t)
	stateDir, err := os.MkdirTemp("/tmp", "fc-boxapi-")
	if err != nil {
		t.Fatalf("state dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })

	svc := &fakePortSvc{}
	p, err := NewProvisioner(kernel, rootfs, stateDir, svc)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.SetNetworking(false) // control-only: vsock needs no egress
	t.Cleanup(func() { closeAndDestroy(t, p) })

	inst, err := p.Provision(context.Background(), sandbox.CreateOptions{BoxID: "vsock-box"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Drive the in-guest side over the guest's Exec verb, exactly as a workload
	// would from a shell inside the box.
	mgr := box.NewManager(p, box.Config{})
	res, err := mgr.Exec(context.Background(), inst.Meta().InstanceID, []string{
		"sh", "-c", "command -v curl >/dev/null 2>&1 || { echo NO-CURL; exit 0; }; " +
			"curl -s --unix-socket /run/llmbox/boxapi.sock -X POST http://localhost/v1/open_port -d '{\"port\":3000,\"description\":\"it\"}'",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	out := res.Stdout + res.Stderr
	if strings.Contains(out, "NO-CURL") {
		t.Skip("guest rootfs has no curl; run against the production box rootfs to exercise the box API")
	}
	if !strings.Contains(out, `"url":"https://x.example.com/"`) {
		t.Fatalf("in-guest curl output = %q, want the fake service's URL", out)
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if svc.lastBoxID != "vsock-box" {
		t.Fatalf("stamped box ID = %q, want vsock-box (identity must come from the host listener)", svc.lastBoxID)
	}
}

// TestBoxRunsAsUnprivilegedUserWithSudo proves the production base+payload boot
// runs box commands as the unprivileged 'agent' user — the payload passes
// --user agent to the guest so the workload never runs as root — while
// passwordless sudo still escalates to root. It needs the base rootfs plus the
// guest payload; point LLMBOX_FC_ROOTFS at the base rootfs and set
// LLMBOX_FC_PAYLOAD to run it, else it skips.
//
// @testcase TestBoxRunsAsUnprivilegedUserWithSudo runs Exec as agent and escalates via sudo to root on a live microVM.
func TestBoxRunsAsUnprivilegedUserWithSudo(t *testing.T) {
	kernel, rootfs := fcArtifacts(t)
	payload := os.Getenv("LLMBOX_FC_PAYLOAD")
	if payload == "" {
		t.Skip("set LLMBOX_FC_PAYLOAD (with LLMBOX_FC_ROOTFS pointing at the base rootfs) to run the box-user test")
	}
	stateDir, err := os.MkdirTemp("/tmp", "fc-agent-")
	if err != nil {
		t.Fatalf("state dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })

	p, err := NewProvisioner(kernel, rootfs, stateDir, nil)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.SetNetworking(false) // control-only: vsock needs no egress
	p.SetPayloadImage(payload)
	t.Cleanup(func() { closeAndDestroy(t, p) })

	inst, err := p.Provision(context.Background(), sandbox.CreateOptions{BoxID: "agent-box"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	mgr := box.NewManager(p, box.Config{})
	who, err := mgr.Exec(context.Background(), inst.Meta().InstanceID, []string{"id", "-un"})
	if err != nil {
		t.Fatalf("Exec id -un: %v", err)
	}
	if got := strings.TrimSpace(who.Stdout); got != "agent" {
		t.Fatalf("box user = %q, want agent (stderr: %q)", got, who.Stderr)
	}

	root, err := mgr.Exec(context.Background(), inst.Meta().InstanceID, []string{"sudo", "-n", "id", "-un"})
	if err != nil {
		t.Fatalf("Exec sudo -n id -un: %v", err)
	}
	if got := strings.TrimSpace(root.Stdout); got != "root" {
		t.Fatalf("sudo id -un = %q, want root (stderr: %q)", got, root.Stderr)
	}
}

// compile-time check that *Provisioner satisfies the provisioner contract.
var _ box.Provisioner = (*Provisioner)(nil)
