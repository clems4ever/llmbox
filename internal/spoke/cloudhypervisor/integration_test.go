package cloudhypervisor

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/clems4ever/llmbox/internal/spoke/box"
	"github.com/clems4ever/llmbox/internal/spoke/box/conformance"
)

// TestConformanceCloudHypervisor runs the backend-neutral contract against a REAL
// Cloud Hypervisor VMM. It is the live counterpart to
// TestConformanceCloudHypervisorFake: the fake-launcher run proves the provisioner's
// bookkeeping in CI, and this proves the same contract against actual booting microVMs
// on a KVM host. It is skipped unless the host is set up to boot Cloud Hypervisor
// microVMs (kernel + rootfs env vars, the cloud-hypervisor binary on PATH, /dev/kvm,
// and root), mirroring the Firecracker backend's live test, so it never runs in a
// plain CI container.
func TestConformanceCloudHypervisor(t *testing.T) {
	kernel := os.Getenv("LLMBOX_CH_KERNEL")
	rootfs := os.Getenv("LLMBOX_CH_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("set LLMBOX_CH_KERNEL and LLMBOX_CH_ROOTFS to run the live Cloud Hypervisor tests")
	}
	if _, err := exec.LookPath(defaultCHBinary); err != nil {
		t.Skipf("cloud-hypervisor binary not on PATH: %v", err)
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm not available: %v", err)
	}
	if os.Geteuid() != 0 {
		t.Skip("booting Cloud Hypervisor microVMs needs root; run the live tests as root")
	}

	conformance.Run(t, func(t testing.TB) box.Provisioner {
		p, err := NewProvisioner(kernel, rootfs, t.TempDir())
		if err != nil {
			t.Fatalf("NewProvisioner: %v", err)
		}
		// The production Close is release-only (it leaves box VMMs running so a
		// respawned spoke rehydrates them), so a live test must destroy its boxes
		// explicitly or leak real cloud-hypervisor processes on the host.
		t.Cleanup(func() { closeAndDestroy(t, p) })
		return p
	})
}

// closeAndDestroy tears down every box the provisioner still holds, then closes it.
//
// @arg t The test the cleanup is registered on.
// @arg p The provisioner whose boxes to destroy and then close.
//
// @testcase TestConformanceCloudHypervisor reaps its live boxes through closeAndDestroy.
func closeAndDestroy(t testing.TB, p *Provisioner) {
	t.Helper()
	boxes, _ := p.List(context.Background())
	for _, b := range boxes {
		_ = b.Destroy(context.Background())
	}
	_ = p.Close()
}
