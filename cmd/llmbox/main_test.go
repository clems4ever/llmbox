package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/clems4ever/llmbox/internal/config"
)

// TestBoxLimitsConvertsUnits checks the operator-friendly box config units
// (mebibytes, fractional CPUs) are converted to the raw byte / nano-CPU counts
// the Docker API expects, and that zero stays zero (unlimited).
func TestBoxLimitsConvertsUnits(t *testing.T) {
	got := boxLimits(config.BoxConfig{MemoryMB: 2048, CPUs: 1.5, PidsLimit: 512, MaxBoxes: 7})
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
	if zero := boxLimits(config.BoxConfig{}); zero.MemoryBytes != 0 || zero.NanoCPUs != 0 || zero.PidsLimit != 0 || zero.MaxBoxes != 0 {
		t.Errorf("zero config should stay unlimited, got %+v", zero)
	}
}

// TestNewRootCmd checks the root command is wired up with the expected name,
// version, the --config flag, and the "version" subcommand which prints the
// build version.
func TestNewRootCmd(t *testing.T) {
	cmd := newRootCmd()
	if cmd.Use != name {
		t.Errorf("root Use = %q, want %q", cmd.Use, name)
	}
	if cmd.Version != version {
		t.Errorf("root Version = %q, want %q", cmd.Version, version)
	}
	if cmd.Flags().Lookup("config") == nil {
		t.Error("root command missing --config flag")
	}

	var found bool
	for _, c := range cmd.Commands() {
		if c.Name() == "version" {
			found = true
			var buf bytes.Buffer
			c.SetOut(&buf)
			c.Run(c, nil)
			if got := buf.String(); got != name+" "+version+"\n" {
				t.Errorf("version output = %q, want %q", got, name+" "+version+"\n")
			}
		}
	}
	if !found {
		t.Error("version subcommand not registered")
	}
}

// TestLoadConfigDefaultsWhenAbsent checks an implicit (non-explicit) missing
// config path yields the built-in defaults rather than an error.
func TestLoadConfigDefaultsWhenAbsent(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	cfg, err := loadConfig(missing, false)
	if err != nil {
		t.Fatalf("loadConfig implicit missing = %v, want nil", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("default HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
}

// TestLoadConfigErrorsWhenExplicitMissing checks an explicitly named missing
// file is a hard error.
func TestLoadConfigErrorsWhenExplicitMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	if _, err := loadConfig(missing, true); err == nil {
		t.Error("loadConfig explicit missing = nil, want error")
	}
}

// TestLoadConfigReadsFile checks loadConfig parses an existing file.
func TestLoadConfigReadsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llmbox.yaml")
	if err := os.WriteFile(path, []byte("http_addr: \":9090\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path, true)
	if err != nil {
		t.Fatalf("loadConfig = %v", err)
	}
	if cfg.HTTPAddr != ":9090" {
		t.Errorf("HTTPAddr = %q, want :9090", cfg.HTTPAddr)
	}
}
