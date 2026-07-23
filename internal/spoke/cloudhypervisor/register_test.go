package cloudhypervisor

import (
	"testing"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/box/backend"
)

// TestNewBackendConfiguresProvisioner builds a Cloud Hypervisor backend through the
// factory and checks the neutral options — kernel/rootfs/state, limits, namespace,
// and GPU passthrough — were applied to the concrete provisioner.
func TestNewBackendConfiguresProvisioner(t *testing.T) {
	p, err := buildProvisioner(backend.Options{
		KernelImagePath:       "/k/vmlinux",
		RootfsImagePath:       "/k/rootfs.ext4",
		StateDir:              t.TempDir(),
		Limits:                sandbox.Limits{MemoryBytes: 1 << 30, NanoCPUs: 2e9, DiskBytes: 2 << 30},
		Namespace:             "ns1",
		GPUPassthrough:        []string{"0000:65:00.0"},
		GPUMediatedDevices:    []string{"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"},
		EgressMode:            "external",
		PoolSize:              32,
		TapGroupGID:           4242,
		CloudHypervisorBinary: "/opt/cloud-hypervisor",
	})
	if err != nil {
		t.Fatalf("buildProvisioner: %v", err)
	}
	if p.kernelImage != "/k/vmlinux" || p.defaultRootfs != "/k/rootfs.ext4" {
		t.Fatalf("paths not applied: %+v", p)
	}
	if p.namespace != "ns1" || p.limits.MemoryBytes != 1<<30 {
		t.Fatalf("options not applied: ns=%q limits=%+v", p.namespace, p.limits)
	}
	if len(p.gpus) != 1 || p.gpus[0] != "0000:65:00.0" {
		t.Fatalf("GPU passthrough not applied: %v", p.gpus)
	}
	if len(p.mdevs) != 1 || p.mdevs[0] != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatalf("vGPU mdev passthrough not applied: %v", p.mdevs)
	}
	if p.egressMode != egressExternal || p.poolSize != 32 || p.tapGroup != 4242 {
		t.Fatalf("egress options not applied: mode=%v pool=%d tap=%d", p.egressMode, p.poolSize, p.tapGroup)
	}
	l, ok := p.launcher.(*chLauncher)
	if !ok || l.chBinary != "/opt/cloud-hypervisor" {
		t.Fatalf("cloud-hypervisor binary not applied: %+v", p.launcher)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestNewBackendRejectsBadGPUAddress checks a malformed GPU passthrough address fails
// the factory at startup rather than at box-boot time.
func TestNewBackendRejectsBadGPUAddress(t *testing.T) {
	if _, err := buildProvisioner(backend.Options{
		KernelImagePath: "/k/vmlinux",
		RootfsImagePath: "/k/rootfs.ext4",
		StateDir:        t.TempDir(),
		GPUPassthrough:  []string{"not-a-pci-address"},
	}); err == nil {
		t.Fatal("buildProvisioner with a malformed GPU passthrough address should fail")
	}
}

// TestCloudHypervisorBackendRegistered checks importing this package makes
// "cloud-hypervisor" selectable through the registry.
func TestCloudHypervisorBackendRegistered(t *testing.T) {
	var found bool
	for _, n := range backend.Names() {
		if n == "cloud-hypervisor" {
			found = true
		}
	}
	if !found {
		t.Fatalf("backend.Names() = %v, want it to include cloud-hypervisor", backend.Names())
	}
}
