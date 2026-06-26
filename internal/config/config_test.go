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
	if c.MCPAddr != DefaultMCPAddr {
		t.Errorf("MCPAddr = %q, want %q", c.MCPAddr, DefaultMCPAddr)
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
mcp_addr: ":9091"
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
	if c.HTTPAddr != ":9090" || c.MCPAddr != ":9091" || c.PublicURL != "https://boxes.example.com" {
		t.Errorf("addr/url = %q / %q / %q", c.HTTPAddr, c.MCPAddr, c.PublicURL)
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

// TestLoadAdminEmails checks the auth.admin.emails allow-list is parsed.
func TestLoadAdminEmails(t *testing.T) {
	c, err := Load(write(t, `public_url: "https://x"
auth:
  admin:
    emails:
      - "you@corp.com"
      - "boss@corp.com"
`))
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	got := c.Auth.Admin.Emails
	if len(got) != 2 || got[0] != "you@corp.com" || got[1] != "boss@corp.com" {
		t.Errorf("admin emails = %v", got)
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
	if c.MCPAddr != DefaultMCPAddr {
		t.Errorf("MCPAddr = %q, want default", c.MCPAddr)
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

// writeFile writes content to dir/name and returns its absolute path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLoadGoogleAuth checks an enabled Google provider loads, resolves its secret
// from the referenced file, and defaults its redirect URL from the public URL.
func TestLoadGoogleAuth(t *testing.T) {
	dir := t.TempDir()
	secret := writeFile(t, dir, "secret", "topsecret\n")
	cfgPath := writeFile(t, dir, "llmbox.yaml", `
public_url: "https://boxes.example.com"
auth:
  session_ttl: "30m"
  google:
    enabled: true
    client_id: "cid"
    client_secret_file: "`+secret+`"
    allowed_domains: ["corp.com"]
`)
	c, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	g := c.Auth.Google
	if g.ClientSecret != "topsecret" {
		t.Errorf("ClientSecret = %q, want topsecret (trimmed from file)", g.ClientSecret)
	}
	if g.RedirectURL != "https://boxes.example.com/auth/google/callback" {
		t.Errorf("RedirectURL = %q, want defaulted callback", g.RedirectURL)
	}
	if time.Duration(c.Auth.SessionTTL) != 30*time.Minute {
		t.Errorf("SessionTTL = %v, want 30m", time.Duration(c.Auth.SessionTTL))
	}
	if len(g.AllowedDomains) != 1 || g.AllowedDomains[0] != "corp.com" {
		t.Errorf("AllowedDomains = %v", g.AllowedDomains)
	}
}

// TestLoadGoogleRequiresAllowlist checks enabling Google with no allow rule is a
// hard error (it would otherwise authorize every Google account).
func TestLoadGoogleRequiresAllowlist(t *testing.T) {
	dir := t.TempDir()
	secret := writeFile(t, dir, "secret", "s")
	cfgPath := writeFile(t, dir, "llmbox.yaml", `
auth:
  google:
    enabled: true
    client_id: "cid"
    client_secret_file: "`+secret+`"
`)
	if _, err := Load(cfgPath); err == nil {
		t.Error("Load with no allow rule = nil, want error")
	}
}

// TestLoadGoogleMissingSecretFile checks an unreadable client secret file errors.
func TestLoadGoogleMissingSecretFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeFile(t, dir, "llmbox.yaml", `
auth:
  google:
    enabled: true
    client_id: "cid"
    client_secret_file: "`+filepath.Join(dir, "nope")+`"
    allowed_domains: ["corp.com"]
`)
	if _, err := Load(cfgPath); err == nil {
		t.Error("Load with missing secret file = nil, want error")
	}
}

// TestLoadRegistry checks a registry entry parses and resolves its password from
// the referenced file, leaving the password out of the YAML.
func TestLoadRegistry(t *testing.T) {
	dir := t.TempDir()
	pw := writeFile(t, dir, "ghcr-token", "ghp_secrettoken\n")
	cfgPath := writeFile(t, dir, "llmbox.yaml", `
registries:
  - registry: "ghcr.io"
    username: "bob"
    password_file: "`+pw+`"
`)
	c, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Registries) != 1 {
		t.Fatalf("Registries = %v, want one entry", c.Registries)
	}
	r := c.Registries[0]
	if r.Registry != "ghcr.io" || r.Username != "bob" {
		t.Errorf("registry/username = %q / %q", r.Registry, r.Username)
	}
	if r.Password != "ghp_secrettoken" {
		t.Errorf("Password = %q, want token trimmed from file", r.Password)
	}
}

// TestLoadRegistryMissingPasswordFile checks an unreadable password file errors.
func TestLoadRegistryMissingPasswordFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeFile(t, dir, "llmbox.yaml", `
registries:
  - registry: "ghcr.io"
    username: "bob"
    password_file: "`+filepath.Join(dir, "nope")+`"
`)
	if _, err := Load(cfgPath); err == nil {
		t.Error("Load with missing password file = nil, want error")
	}
}

// TestLoadRegistryRequiresHost checks a registry entry with no host is rejected.
func TestLoadRegistryRequiresHost(t *testing.T) {
	dir := t.TempDir()
	pw := writeFile(t, dir, "token", "t")
	cfgPath := writeFile(t, dir, "llmbox.yaml", `
registries:
  - username: "bob"
    password_file: "`+pw+`"
`)
	if _, err := Load(cfgPath); err == nil {
		t.Error("Load with no registry host = nil, want error")
	}
}
