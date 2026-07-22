package firecracker

import (
	"testing"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/box/backend"
)

// TestNewBackendConfiguresProvisioner builds a Firecracker backend through the
// wiring half of the factory and checks the microVM-specific options — including the
// jailer knobs — were applied. It uses buildProvisioner rather than newBackend so it
// exercises option plumbing without the host prerequisite check (jailer binary /
// root / /dev/kvm), which newBackend enforces and which no unit host can satisfy.
func TestNewBackendConfiguresProvisioner(t *testing.T) {
	p, _, err := buildProvisioner(backend.Options{
		KernelImagePath: "/k/vmlinux",
		RootfsImagePath: "/k/rootfs.ext4",
		StateDir:        "/var/lib/fc",
		Limits:          sandbox.Limits{MemoryBytes: 1 << 30, NanoCPUs: 2e9},
		Namespace:       "ns1",
		DisableEgress:   true,
		PoolSize:        24,
		JailerBinary:    "/opt/jailer",
		ChrootBase:      "/srv/jail",
		UIDMin:          200000,
		UIDMax:          210000,
		TapGroupGID:     4242,
		CgroupVersion:   "2",
	})
	if err != nil {
		t.Fatalf("buildProvisioner: %v", err)
	}
	if p.kernelImage != "/k/vmlinux" || p.defaultRootfs != "/k/rootfs.ext4" || p.stateDir != "/var/lib/fc" {
		t.Fatalf("paths not applied: %+v", p)
	}
	if p.namespace != "ns1" || p.limits.MemoryBytes != 1<<30 {
		t.Fatalf("options not applied: ns=%q limits=%+v", p.namespace, p.limits)
	}
	if p.netEnabled || p.poolSize != 24 {
		t.Fatalf("networking/pool not applied: netEnabled=%v poolSize=%d", p.netEnabled, p.poolSize)
	}
	if p.jailer.jailerBin != "/opt/jailer" || p.jailer.chrootBase != "/srv/jail" {
		t.Fatalf("jailer paths not applied: %+v", p.jailer)
	}
	if p.jailer.uidMin != 200000 || p.jailer.uidMax != 210000 || p.jailer.gid != 4242 || p.jailer.cgroupVersion != "2" {
		t.Fatalf("jailer identity/cgroup not applied: %+v", p.jailer)
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
