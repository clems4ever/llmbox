package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// write writes content to a temp file and returns its path.
func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "llmbox.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestDefault checks Default returns the documented default values.
func TestDefault(t *testing.T) {
	c := Default()
	if c.HTTPAddr != DefaultHTTPAddr {
		t.Errorf("HTTPAddr = %q, want %q", c.HTTPAddr, DefaultHTTPAddr)
	}
	if c.PublicURL != DefaultPublicURL {
		t.Errorf("PublicURL = %q, want %q", c.PublicURL, DefaultPublicURL)
	}
	if time.Duration(c.AuthTTL) != DefaultAuthTTL {
		t.Errorf("AuthTTL = %v, want %v", time.Duration(c.AuthTTL), DefaultAuthTTL)
	}
	if c.StateFile != DefaultStateFile {
		t.Errorf("StateFile = %q, want %q", c.StateFile, DefaultStateFile)
	}
}

// TestLoad checks a full config file parses into every field.
func TestLoad(t *testing.T) {
	path := write(t, `
http_addr: ":9090"
public_url: "https://boxes.example.com"
claude_image: "ubuntu:24.04"
claude_bin: "/usr/local/bin/claude"
remote_args: "--spawn new-dir"
auth_ttl: "10m"
state_file: "/var/lib/llmbox/sessions.db"
hooks:
  - /opt/granular-llmbox/hook
box_peers:
  - granular-github
  - granular-as
`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if c.HTTPAddr != ":9090" || c.PublicURL != "https://boxes.example.com" {
		t.Errorf("addr/url = %q / %q", c.HTTPAddr, c.PublicURL)
	}
	if c.ClaudeImage != "ubuntu:24.04" || c.ClaudeBin != "/usr/local/bin/claude" || c.RemoteArgs != "--spawn new-dir" {
		t.Errorf("docker fields = %q / %q / %q", c.ClaudeImage, c.ClaudeBin, c.RemoteArgs)
	}
	if time.Duration(c.AuthTTL) != 10*time.Minute {
		t.Errorf("AuthTTL = %v, want 10m", time.Duration(c.AuthTTL))
	}
	if len(c.Hooks) != 1 || c.Hooks[0] != "/opt/granular-llmbox/hook" {
		t.Errorf("Hooks = %v", c.Hooks)
	}
	if len(c.BoxPeers) != 2 || c.BoxPeers[0] != "granular-github" || c.BoxPeers[1] != "granular-as" {
		t.Errorf("BoxPeers = %v", c.BoxPeers)
	}
}

// TestLoadAppliesDefaults checks unset fields fall back to defaults.
func TestLoadAppliesDefaults(t *testing.T) {
	c, err := Load(write(t, "public_url: \"https://x\"\n"))
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if c.HTTPAddr != DefaultHTTPAddr {
		t.Errorf("HTTPAddr = %q, want default", c.HTTPAddr)
	}
	if time.Duration(c.AuthTTL) != DefaultAuthTTL {
		t.Errorf("AuthTTL = %v, want default", time.Duration(c.AuthTTL))
	}
	if c.StateFile != DefaultStateFile {
		t.Errorf("StateFile = %q, want default", c.StateFile)
	}
}

// TestLoadMissingFile checks Load errors when the file does not exist.
func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("Load missing = nil, want error")
	}
}

// TestLoadRejectsUnknownKey checks an unknown field surfaces as an error.
func TestLoadRejectsUnknownKey(t *testing.T) {
	if _, err := Load(write(t, "bogus_key: 1\n")); err == nil {
		t.Error("Load unknown key = nil, want error")
	}
}

// TestLoadParsesDuration checks a duration string is parsed.
func TestLoadParsesDuration(t *testing.T) {
	c, err := Load(write(t, "auth_ttl: \"90s\"\n"))
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if time.Duration(c.AuthTTL) != 90*time.Second {
		t.Errorf("AuthTTL = %v, want 90s", time.Duration(c.AuthTTL))
	}
}

// TestLoadRejectsBadDuration checks an unparseable duration is an error.
func TestLoadRejectsBadDuration(t *testing.T) {
	if _, err := Load(write(t, "auth_ttl: \"not-a-duration\"\n")); err == nil {
		t.Error("Load bad duration = nil, want error")
	}
}

// TestLoadEmptyFile checks an empty file yields all defaults without error.
func TestLoadEmptyFile(t *testing.T) {
	c, err := Load(write(t, ""))
	if err != nil {
		t.Fatalf("Load empty = %v", err)
	}
	if c.HTTPAddr != DefaultHTTPAddr {
		t.Errorf("HTTPAddr = %q, want default", c.HTTPAddr)
	}
}
