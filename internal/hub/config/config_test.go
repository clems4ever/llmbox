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
	if time.Duration(c.Auth.SessionTTL) != DefaultSessionTTL {
		t.Errorf("SessionTTL = %v, want %v", time.Duration(c.Auth.SessionTTL), DefaultSessionTTL)
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
state_file: "/var/lib/llmbox/sessions.db"
hooks:
  - /opt/granular-llmbox/hook
`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if c.HTTPAddr != ":9090" || c.PublicURL != "https://boxes.example.com" {
		t.Errorf("addr/url = %q / %q", c.HTTPAddr, c.PublicURL)
	}
	if c.StateFile != "/var/lib/llmbox/sessions.db" {
		t.Errorf("StateFile = %q", c.StateFile)
	}
	if len(c.Hooks) != 1 || c.Hooks[0] != "/opt/granular-llmbox/hook" {
		t.Errorf("Hooks = %v", c.Hooks)
	}
}

// TestLoadRejectsBoxRunningKey checks that box-running knobs moved to the spoke
// (image, backend, resource caps, registries) are rejected as unknown keys, so a
// stale hub config surfaces the move rather than being silently ignored.
func TestLoadRejectsBoxRunningKey(t *testing.T) {
	for _, key := range []string{"box_image", "backend", "remote_args", "box_peers", "box", "firecracker", "registries"} {
		if _, err := Load(write(t, key+": x\n")); err == nil {
			t.Errorf("Load with %q = nil, want unknown-key error", key)
		}
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
	if time.Duration(c.Auth.SessionTTL) != DefaultSessionTTL {
		t.Errorf("SessionTTL = %v, want default", time.Duration(c.Auth.SessionTTL))
	}
	if c.StateFile != DefaultStateFile {
		t.Errorf("StateFile = %q, want default", c.StateFile)
	}
}

// TestExampleConfigParses checks the shipped llmbox.example.yaml parses under the
// hub's strict decoder, so the example never drifts to a key the hub rejects
// (e.g. a box-provisioning knob that has since moved to the spoke).
func TestExampleConfigParses(t *testing.T) {
	if _, err := Load(filepath.Join("..", "..", "..", "llmbox.example.yaml")); err != nil {
		t.Fatalf("llmbox.example.yaml does not parse: %v", err)
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
	c, err := Load(write(t, "auth:\n  session_ttl: \"90s\"\n"))
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if time.Duration(c.Auth.SessionTTL) != 90*time.Second {
		t.Errorf("SessionTTL = %v, want 90s", time.Duration(c.Auth.SessionTTL))
	}
}

// TestLoadRejectsBadDuration checks an unparseable duration is an error.
func TestLoadRejectsBadDuration(t *testing.T) {
	if _, err := Load(write(t, "auth:\n  session_ttl: \"not-a-duration\"\n")); err == nil {
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
`)
	if _, err := Load(cfgPath); err == nil {
		t.Error("Load with missing secret file = nil, want error")
	}
}

// TestLoadTLS checks an enabled TLS block parses its cert and key file paths.
func TestLoadTLS(t *testing.T) {
	c, err := Load(write(t, `
tls:
  enabled: true
  cert_file: "/etc/llmbox/tls-cert.pem"
  key_file: "/etc/llmbox/tls-key.pem"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.TLS.Enabled {
		t.Error("TLS.Enabled = false, want true")
	}
	if c.TLS.CertFile != "/etc/llmbox/tls-cert.pem" || c.TLS.KeyFile != "/etc/llmbox/tls-key.pem" {
		t.Errorf("cert/key = %q / %q", c.TLS.CertFile, c.TLS.KeyFile)
	}
}

// TestLoadTLSRequiresCertAndKey checks enabling TLS without both a cert and a key
// file is a hard error (there would be nothing to serve the connection with).
func TestLoadTLSRequiresCertAndKey(t *testing.T) {
	if _, err := Load(write(t, "tls:\n  enabled: true\n  cert_file: \"/etc/llmbox/tls-cert.pem\"\n")); err == nil {
		t.Error("Load TLS with no key_file = nil, want error")
	}
	if _, err := Load(write(t, "tls:\n  enabled: true\n  key_file: \"/etc/llmbox/tls-key.pem\"\n")); err == nil {
		t.Error("Load TLS with no cert_file = nil, want error")
	}
}

// TestLoadRejectsBaseDomainWithPort checks a proxy.base_domain carrying a port
// is rejected: incoming proxy Host matching strips the port, so a port here
// silently kills routing.
func TestLoadRejectsBaseDomainWithPort(t *testing.T) {
	if _, err := Load(write(t, "proxy:\n  base_domain: \"llmbox.dev:8443\"\n")); err == nil {
		t.Error("Load base_domain with port = nil, want error")
	}
	// A scheme, path, and wildcard are equally rejected.
	for _, bad := range []string{"https://llmbox.dev", "llmbox.dev/x", "*.llmbox.dev", ".llmbox.dev"} {
		if _, err := Load(write(t, "proxy:\n  base_domain: \""+bad+"\"\n")); err == nil {
			t.Errorf("Load base_domain %q = nil, want error", bad)
		}
	}
}

// TestLoadRejectsCookieDomainLeadingDot checks an auth.cookie_domain with a
// leading dot is rejected: SafeReturnURL prefixes a dot when matching, so a
// leading dot never matches any host.
func TestLoadRejectsCookieDomainLeadingDot(t *testing.T) {
	cfg := "public_url: \"https://llmbox.dev\"\nauth:\n  cookie_domain: \".llmbox.dev\"\n"
	if _, err := Load(write(t, cfg)); err == nil {
		t.Error("Load cookie_domain with leading dot = nil, want error")
	}
	withPort := "public_url: \"https://llmbox.dev:8443\"\nauth:\n  cookie_domain: \"llmbox.dev:8443\"\n"
	if _, err := Load(write(t, withPort)); err == nil {
		t.Error("Load cookie_domain with port = nil, want error")
	}
}

// TestLoadRejectsCookieDomainNotCoveringBase checks a cookie domain that is not a
// parent of the proxy base domain is rejected, since the shared login cookie
// would not reach the proxy sub-domains.
func TestLoadRejectsCookieDomainNotCoveringBase(t *testing.T) {
	cfg := "public_url: \"https://other.com\"\nproxy:\n  base_domain: \"llmbox.dev\"\nauth:\n  cookie_domain: \"other.com\"\n"
	if _, err := Load(write(t, cfg)); err == nil {
		t.Error("Load cookie_domain not covering base_domain = nil, want error")
	}
}

// TestLoadRejectsCookieDomainNotCoveringPublicURL checks a cookie domain the
// public_url host is outside of is rejected: the browser would reject the
// Set-Cookie the public_url host emits.
func TestLoadRejectsCookieDomainNotCoveringPublicURL(t *testing.T) {
	cfg := "public_url: \"https://llmbox.dev:8443\"\nauth:\n  cookie_domain: \"elsewhere.com\"\n"
	if _, err := Load(write(t, cfg)); err == nil {
		t.Error("Load cookie_domain not covering public_url = nil, want error")
	}
}

// TestLoadAcceptsConsistentProxyAndCookieDomains checks a bare cookie domain that
// spans both the public_url host and the proxy base domain loads cleanly.
func TestLoadAcceptsConsistentProxyAndCookieDomains(t *testing.T) {
	cfg := "public_url: \"https://llmbox.dev:8443\"\nproxy:\n  base_domain: \"llmbox.dev\"\nauth:\n  cookie_domain: \"llmbox.dev\"\n"
	c, err := Load(write(t, cfg))
	if err != nil {
		t.Fatalf("Load consistent proxy/cookie domains = %v, want nil", err)
	}
	if c.Proxy.BaseDomain != "llmbox.dev" || c.Auth.CookieDomain != "llmbox.dev" {
		t.Errorf("parsed domains = %q / %q", c.Proxy.BaseDomain, c.Auth.CookieDomain)
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
