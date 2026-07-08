package boxconfig

import (
	"testing"
)

// TestRegistryAuthsKeyedByHost checks each configured registry maps to an auth
// config keyed by its host (with the host echoed as the server address), and that
// an empty config yields nil so pulls stay anonymous.
func TestRegistryAuthsKeyedByHost(t *testing.T) {
	got := RegistryAuths([]RegistryConfig{
		{Registry: "ghcr.io", Username: "bob", Password: "tok"},
		{Registry: "registry.example.com:5000", Username: "alice", Password: "pw"},
	})
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %v", len(got), got)
	}
	if a := got["ghcr.io"]; a.Username != "bob" || a.Password != "tok" || a.ServerAddress != "ghcr.io" {
		t.Errorf("ghcr.io auth = %+v", a)
	}
	if a := got["registry.example.com:5000"]; a.Username != "alice" || a.Password != "pw" || a.ServerAddress != "registry.example.com:5000" {
		t.Errorf("example auth = %+v", a)
	}
	if RegistryAuths(nil) != nil {
		t.Error("no registries should yield nil (anonymous pulls)")
	}
}
