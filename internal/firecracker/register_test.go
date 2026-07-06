package firecracker

import (
	"testing"

	"github.com/clems4ever/llmbox/internal/box/backend"
	"github.com/clems4ever/llmbox/internal/sandbox"
)

// TestNewBackendConfiguresProvisioner builds a Firecracker backend through the
// factory and checks the microVM-specific options were applied.
func TestNewBackendConfiguresProvisioner(t *testing.T) {
	// DisableEgress so the factory does not provision the host TAP pool (which needs
	// root); this test only checks option wiring.
	p, err := newBackend(backend.Options{
		KernelImagePath: "/k/vmlinux",
		RootfsImagePath: "/k/rootfs.ext4",
		StateDir:        "/var/lib/fc",
		Limits:          sandbox.Limits{MemoryBytes: 1 << 30, NanoCPUs: 2e9},
		Namespace:       "ns1",
		DisableEgress:   true,
		PoolSize:        24,
	})
	if err != nil {
		t.Fatalf("newBackend: %v", err)
	}
	fp, ok := p.(*Provisioner)
	if !ok {
		t.Fatalf("newBackend returned %T, want *Provisioner", p)
	}
	if fp.kernelImage != "/k/vmlinux" || fp.defaultRootfs != "/k/rootfs.ext4" || fp.stateDir != "/var/lib/fc" {
		t.Fatalf("paths not applied: %+v", fp)
	}
	if fp.namespace != "ns1" || fp.limits.MemoryBytes != 1<<30 {
		t.Fatalf("options not applied: ns=%q limits=%+v", fp.namespace, fp.limits)
	}
	if fp.netEnabled || fp.poolSize != 24 {
		t.Fatalf("networking/pool not applied: netEnabled=%v poolSize=%d", fp.netEnabled, fp.poolSize)
	}
}

// TestFirecrackerBackendRegistered checks importing this package makes
// "firecracker" selectable through the registry.
func TestFirecrackerBackendRegistered(t *testing.T) {
	var found bool
	for _, n := range backend.Names() {
		if n == "firecracker" {
			found = true
		}
	}
	if !found {
		t.Fatalf("backend.Names() = %v, want it to include firecracker", backend.Names())
	}
}
