package docker

import (
	"testing"

	"github.com/clems4ever/llmbox/internal/box/backend"
	"github.com/clems4ever/llmbox/internal/sandbox"
)

// TestNewBackendConfiguresProvisioner builds a Docker backend through the factory
// and checks the neutral options were applied to the concrete provisioner.
func TestNewBackendConfiguresProvisioner(t *testing.T) {
	p, err := newBackend(backend.Options{
		DefaultImage: "example.com/img:tag",
		SocketDir:    "/tmp/socks",
		Peers:        []string{"peer-a"},
		Limits:       sandbox.Limits{MemoryBytes: 1 << 30, NanoCPUs: 2e9, PidsLimit: 100},
		Namespace:    "ns1",
		GPUs:         "all",
	})
	if err != nil {
		t.Fatalf("newBackend: %v", err)
	}
	dp, ok := p.(*Provisioner)
	if !ok {
		t.Fatalf("newBackend returned %T, want *Provisioner", p)
	}
	if dp.defaultImage != "example.com/img:tag" || dp.socketDir != "/tmp/socks" {
		t.Fatalf("image/socketDir not applied: %q %q", dp.defaultImage, dp.socketDir)
	}
	if dp.namespace != "ns1" || dp.limits.MemoryBytes != 1<<30 || len(dp.boxGPUs) == 0 {
		t.Fatalf("options not applied: ns=%q limits=%+v gpus=%v", dp.namespace, dp.limits, dp.boxGPUs)
	}
	if err := dp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestNewBackendRejectsBadGPUs checks a malformed GPU spec fails the factory.
func TestNewBackendRejectsBadGPUs(t *testing.T) {
	if _, err := newBackend(backend.Options{GPUs: "0"}); err == nil {
		t.Fatal("newBackend with a malformed GPU spec should fail")
	}
}

// TestDockerBackendRegistered checks importing this package makes "docker"
// selectable through the registry.
func TestDockerBackendRegistered(t *testing.T) {
	var found bool
	for _, n := range backend.Names() {
		if n == "docker" {
			found = true
		}
	}
	if !found {
		t.Fatalf("backend.Names() = %v, want it to include docker", backend.Names())
	}
}
