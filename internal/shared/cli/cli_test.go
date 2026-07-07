package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/clems4ever/llmbox/internal/shared/config"
)

// TestBoxLimitsConvertsUnits checks the operator-friendly box config units
// (mebibytes, fractional CPUs) are converted to the raw byte / nano-CPU counts
// the Docker API expects, and that zero stays zero (unlimited).
func TestBoxLimitsConvertsUnits(t *testing.T) {
	got := BoxLimits(config.BoxConfig{MemoryMB: 2048, CPUs: 1.5, PidsLimit: 512, MaxBoxes: 7})
	if got.MemoryBytes != 2048*1024*1024 {
		t.Errorf("MemoryBytes = %d, want %d", got.MemoryBytes, 2048*1024*1024)
	}
	if got.NanoCPUs != 1_500_000_000 {
		t.Errorf("NanoCPUs = %d, want 1500000000", got.NanoCPUs)
	}
	if got.PidsLimit != 512 {
		t.Errorf("PidsLimit = %d, want 512", got.PidsLimit)
	}
	if got.MaxBoxes != 7 {
		t.Errorf("MaxBoxes = %d, want 7", got.MaxBoxes)
	}
	if zero := BoxLimits(config.BoxConfig{}); zero.MemoryBytes != 0 || zero.NanoCPUs != 0 || zero.PidsLimit != 0 || zero.MaxBoxes != 0 {
		t.Errorf("zero config should stay unlimited, got %+v", zero)
	}
}

// TestRegistryAuthsKeyedByHost checks each configured registry maps to an auth
// config keyed by its host (with the host echoed as the server address), and that
// an empty config yields nil so pulls stay anonymous.
func TestRegistryAuthsKeyedByHost(t *testing.T) {
	got := RegistryAuths([]config.RegistryConfig{
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

// TestLoadConfigDefaultsWhenAbsent checks an implicit (non-explicit) missing
// config path yields the built-in defaults rather than an error.
func TestLoadConfigDefaultsWhenAbsent(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	cfg, err := LoadConfig(missing, false)
	if err != nil {
		t.Fatalf("LoadConfig implicit missing = %v, want nil", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("default HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
}

// TestLoadConfigErrorsWhenExplicitMissing checks an explicitly named missing
// file is a hard error.
func TestLoadConfigErrorsWhenExplicitMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	if _, err := LoadConfig(missing, true); err == nil {
		t.Error("LoadConfig explicit missing = nil, want error")
	}
}

// TestLoadConfigReadsFile checks LoadConfig parses an existing file.
func TestLoadConfigReadsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llmbox.yaml")
	if err := os.WriteFile(path, []byte("http_addr: \":9090\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path, true)
	if err != nil {
		t.Fatalf("LoadConfig = %v", err)
	}
	if cfg.HTTPAddr != ":9090" {
		t.Errorf("HTTPAddr = %q, want :9090", cfg.HTTPAddr)
	}
}
