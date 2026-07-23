package cloudhypervisor

import (
	"testing"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// TestMachineSizing checks the vCPU and memory derivation from per-box limits,
// including the defaults and the memory floor.
func TestMachineSizing(t *testing.T) {
	cases := []struct {
		name      string
		limits    sandbox.Limits
		wantVCPUs int64
		wantMiB   int64
	}{
		{"defaults", sandbox.Limits{}, defaultVCPUs, defaultMemSizeMib},
		{"round-up-cpu", sandbox.Limits{NanoCPUs: 2500000000}, 3, defaultMemSizeMib},
		{"one-cpu-floor", sandbox.Limits{NanoCPUs: 100000000}, 1, defaultMemSizeMib},
		{"mem-mib", sandbox.Limits{MemoryBytes: 1 << 30}, defaultVCPUs, 1024},
		{"mem-floor", sandbox.Limits{MemoryBytes: 8 << 20}, defaultVCPUs, minMemSizeMib},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &Provisioner{limits: c.limits}
			if got := p.vcpuCount(); got != c.wantVCPUs {
				t.Errorf("vcpuCount = %d, want %d", got, c.wantVCPUs)
			}
			if got := p.memSizeMib(); got != c.wantMiB {
				t.Errorf("memSizeMib = %d, want %d", got, c.wantMiB)
			}
		})
	}
}

// TestDiskBytesFor resolves the writable-disk size from the request and the limits,
// applying the default and clamping to the max.
func TestDiskBytesFor(t *testing.T) {
	p := &Provisioner{limits: sandbox.Limits{DiskBytes: 2 << 30, MaxDiskBytes: 8 << 30}}
	if got := p.diskBytesFor(0); got != 2<<30 {
		t.Errorf("diskBytesFor(0) = %d, want the default 2GiB", got)
	}
	if got := p.diskBytesFor(4 << 30); got != 4<<30 {
		t.Errorf("diskBytesFor(4GiB) = %d, want 4GiB", got)
	}
	if got := p.diskBytesFor(16 << 30); got != 8<<30 {
		t.Errorf("diskBytesFor(16GiB) = %d, want clamped to the 8GiB max", got)
	}
}

// TestProvisionRequiresKernelAndRootfs checks Provision fails fast when the spoke
// configured no kernel or rootfs, rather than launching an unbootable VM.
func TestProvisionRequiresKernelAndRootfs(t *testing.T) {
	p, err := NewProvisioner("", "", t.TempDir())
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.launcher = &fakeLauncher{}
	if _, err := p.Provision(t.Context(), sandbox.CreateOptions{}); err == nil {
		t.Fatal("Provision without a kernel/rootfs should fail")
	}
}
