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
	if p.egressMode != egressDisabled || p.poolSize != 24 {
		t.Fatalf("networking/pool not applied: egressMode=%v poolSize=%d", p.egressMode, p.poolSize)
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

// TestResolveEgressMode checks the neutral egress options map to the right mode:
// EgressMode wins when set, the legacy DisableEgress boolean picks disabled
// otherwise, and a non-disabled EgressMode combined with DisableEgress is rejected.
func TestResolveEgressMode(t *testing.T) {
	cases := []struct {
		name    string
		opts    backend.Options
		want    egressMode
		wantErr bool
	}{
		{"default is managed", backend.Options{}, egressManaged, false},
		{"legacy disable-egress", backend.Options{DisableEgress: true}, egressDisabled, false},
		{"explicit managed", backend.Options{EgressMode: "managed"}, egressManaged, false},
		{"explicit external", backend.Options{EgressMode: "external"}, egressExternal, false},
		{"explicit disabled", backend.Options{EgressMode: "disabled"}, egressDisabled, false},
		{"disabled + disable-egress agree", backend.Options{EgressMode: "disabled", DisableEgress: true}, egressDisabled, false},
		{"unknown mode", backend.Options{EgressMode: "bogus"}, egressManaged, true},
		{"external contradicts disable-egress", backend.Options{EgressMode: "external", DisableEgress: true}, egressManaged, true},
	}
	for _, c := range cases {
		got, err := resolveEgressMode(c.opts)
		if c.wantErr {
			if err == nil {
				t.Fatalf("%s: resolveEgressMode = %v, want an error", c.name, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("%s: resolveEgressMode = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
}

// TestBuildProvisionerAppliesExternalEgress checks the external egress mode flows
// through the wiring half of the factory onto the provisioner.
func TestBuildProvisionerAppliesExternalEgress(t *testing.T) {
	p, _, err := buildProvisioner(backend.Options{
		KernelImagePath: "/k/vmlinux",
		RootfsImagePath: "/k/rootfs.ext4",
		StateDir:        "/var/lib/fc",
		EgressMode:      "external",
	})
	if err != nil {
		t.Fatalf("buildProvisioner: %v", err)
	}
	if p.egressMode != egressExternal {
		t.Fatalf("egressMode = %v, want external", p.egressMode)
	}
	if !p.guestNetEnabled() {
		t.Fatalf("external mode must still give the guest an egress NIC")
	}
}
