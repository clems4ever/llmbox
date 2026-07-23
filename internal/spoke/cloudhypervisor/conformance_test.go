package cloudhypervisor

import (
	"testing"

	"github.com/clems4ever/llmbox/internal/spoke/box"
	"github.com/clems4ever/llmbox/internal/spoke/box/conformance"
)

// TestConformanceCloudHypervisorFake runs the backend-neutral behavioural contract —
// the exact same assertions Docker and Firecracker pass — against the Cloud
// Hypervisor provisioner, backed by the in-process fake launcher so it needs no KVM.
// This is the non-regression guard for the whole provisioner lifecycle
// (Provision/List/Find/Exec/init-script/copy/pause-resume/destroy): a change that
// breaks the Cloud Hypervisor backend's behaviour fails here in CI, and a change to
// the shared Manager contract is proven against this backend too.
func TestConformanceCloudHypervisorFake(t *testing.T) {
	conformance.Run(t, func(t testing.TB) box.Provisioner {
		return newFakeProvisioner(t)
	})
}
