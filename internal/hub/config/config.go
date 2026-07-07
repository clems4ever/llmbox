// Package config loads llmbox's YAML configuration file for the hub (the
// llmbox-server). It replaces the older LLMBOX_* environment variables: every
// setting now lives in one YAML document, and any field left unset falls back to
// a built-in default. This config is hub-only — the spoke reads no config file;
// the box-provisioning knobs both sides share live in internal/shared/boxconfig.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/clems4ever/llmbox/internal/shared/boxconfig"
)

// Default values applied to config fields left unset. The Docker manager applies
// its own defaults for the image and remote args, so those are deliberately
// absent here (an empty value flows through to the manager).
const (
	DefaultHTTPAddr  = ":8080"
	DefaultPublicURL = "http://localhost:8080"
	DefaultAuthTTL   = 300 * time.Second
	DefaultStateFile = "llmbox-sessions.db"

	// DefaultSessionTTL is how long an activation login session stays valid when
	// auth.session_ttl is unset.
	DefaultSessionTTL = time.Hour
)

// Duration is a time.Duration that unmarshals from a Go duration string (e.g.
// "5m", "300s") in YAML, so the config reads naturally rather than forcing a
// raw-seconds integer.
type Duration time.Duration

// UnmarshalYAML decodes a Go duration string (e.g. "5m") into the Duration.
//
// @arg value The YAML node holding the duration string.
// @error error if the node is not a string or not a valid Go duration.
//
// @testcase TestLoadParsesDuration parses a duration string from the config.
// @testcase TestLoadRejectsBadDuration fails on an unparseable duration.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Config is the parsed llmbox configuration. Field semantics mirror the former
// LLMBOX_* environment variables; see the README's Configuration section.
type Config struct {
	// HTTPAddr is the single listen address for the whole server: the box-control
	// JSON API (under /api/v1/) plus the UI (auth pages, admin, spoke connect,
	// health, favicon). The box-control API is unauthenticated by the server itself
	// (see internal/server/http.go), so run behind an authenticating reverse proxy.
	HTTPAddr  string `yaml:"http_addr"`
	PublicURL string `yaml:"public_url"`
	// Backend selects the box isolation backend by name ("docker" or
	// "firecracker"); empty defaults to Docker, preserving prior behaviour.
	Backend     string              `yaml:"backend"`
	ClaudeImage string              `yaml:"claude_image"`
	RemoteArgs  string              `yaml:"remote_args"`
	AuthTTL     Duration            `yaml:"auth_ttl"`
	StateFile   string              `yaml:"state_file"`
	Hooks       []string            `yaml:"hooks"`
	BoxPeers    []string            `yaml:"box_peers"`
	Auth        AuthConfig          `yaml:"auth"`
	Box         boxconfig.BoxConfig `yaml:"box"`
	Firecracker FirecrackerConfig   `yaml:"firecracker"`
	Proxy       ProxyConfig         `yaml:"proxy"`
	TLS         TLSConfig           `yaml:"tls"`
	// Registries holds credentials for pulling box images from authenticated
	// container registries. The manager selects the entry whose host matches the
	// image being pulled; an image whose registry has no entry is pulled
	// anonymously, as before.
	Registries []boxconfig.RegistryConfig `yaml:"registries"`
}

// FirecrackerConfig holds the microVM-backend settings, used only when backend is
// "firecracker". A Docker deployment leaves it empty.
type FirecrackerConfig struct {
	// KernelImage is the host path to the guest kernel (vmlinux) every box boots.
	KernelImage string `yaml:"kernel_image"`
	// RootfsImage is the host path to the default guest root filesystem image
	// booted when a create supplies no image of its own.
	RootfsImage string `yaml:"rootfs_image"`
	// PayloadImage is an optional host path to a small read-only ext4 carrying the
	// guest agent (plus claude and its trust seed). When set, every box attaches it
	// as a shared read-only second drive that the base rootfs mounts, so the agent
	// can be updated by swapping this tiny image without rebuilding the multi-GiB
	// base rootfs. Empty bakes the agent into the rootfs (the all-in-one layout).
	PayloadImage string `yaml:"payload_image"`
	// StateDir is where the backend persists per-box metadata (Firecracker has no
	// daemon registry) so List/Find/reap survive a restart. Empty uses the
	// backend default.
	StateDir string `yaml:"state_dir"`
	// DisableEgress boots control-only boxes (loopback + vsock, no TAP/NAT), which
	// removes the CAP_NET_ADMIN requirement so the server can run unprivileged.
	// The guest then has no outbound network, so a box cannot reach the Claude API
	// — use it for air-gapped boxes or local plumbing tests, not real sessions.
	// Default false (egress enabled).
	DisableEgress bool `yaml:"disable_egress"`
	// PoolSize is the number of egress TAP devices provisioned once at startup and
	// reused across boxes; it caps concurrent networked boxes. Provisioning them at
	// startup (not per box) keeps a same-host browser from aborting requests with
	// ERR_NETWORK_CHANGED when a box is created. Empty uses the backend default.
	PoolSize int `yaml:"pool_size"`
}

// ProxyConfig enables exposing box HTTP ports through the hub. When base_domain
// is set, the hub serves a reverse proxy at https://<slug>.<base_domain>/ for
// every enabled proxy (created over MCP or the admin UI), forwarding requests to
// the box's port on its spoke. Each proxy gets its own subdomain so single-page
// apps and servers that emit absolute paths work unchanged (no path rewriting).
// A wildcard DNS record and TLS certificate for *.<base_domain> are required.
// Empty base_domain disables the feature: no proxy is served and the MCP/admin
// proxy tools report it as disabled.
type ProxyConfig struct {
	// BaseDomain is the parent domain proxy subdomains hang off, e.g.
	// "proxy.example.com" (a proxy is then reached at <slug>.proxy.example.com).
	BaseDomain string `yaml:"base_domain"`
}

// TLSConfig makes the server terminate TLS itself instead of serving plaintext
// HTTP. When enabled, the single HTTP server (UI + box-control API) is served
// over HTTPS using the PEM certificate and private key at the configured paths,
// and the startup insecure-transport warning is suppressed. Leave it disabled
// (the default) only when a TLS-terminating reverse proxy sits in front, since
// the box-control API and relayed OAuth codes must never cross the wire in the
// clear.
type TLSConfig struct {
	// Enabled turns on in-process TLS termination. When true, cert_file and
	// key_file are required.
	Enabled bool `yaml:"enabled"`
	// CertFile is the path to the PEM-encoded server certificate (a full chain,
	// leaf first, when intermediates are needed).
	CertFile string `yaml:"cert_file"`
	// KeyFile is the path to the PEM-encoded private key matching CertFile.
	KeyFile string `yaml:"key_file"`
}

// AuthConfig configures who may activate a box. When a provider is enabled, the
// activation page requires the visitor to sign in with that provider (over a
// channel that never transits the chatbot) and be authorized before the box's
// OAuth code is accepted. Each provider is a dedicated sub-block so more can be
// added later without changing the existing ones.
type AuthConfig struct {
	SessionTTL Duration     `yaml:"session_ttl"`
	Google     GoogleConfig `yaml:"google"`
	Admin      AdminConfig  `yaml:"admin"`
	// CookieDomain, when set, is the Domain attribute placed on the login-session
	// cookie so one sign-in is shared across sub-domains (e.g. ".example.com" lets
	// the session reach both the main UI and the per-proxy <slug>.proxy.example.com
	// hosts). Empty keeps the cookie host-only (the default), which is correct
	// unless the proxy feature is used with sub-domain hosts.
	CookieDomain string `yaml:"cookie_domain"`
}

// AdminConfig lists the signed-in identities allowed to use the admin web UI
// (/admin) for managing spokes and boxes. It is independent of the box-activation
// allow rule: an admin email need not be allowed to activate boxes, and vice
// versa. When Emails is empty the admin UI is disabled entirely.
type AdminConfig struct {
	Emails []string `yaml:"emails"`
}

// GoogleConfig configures Sign in with Google (OIDC) for box activation. A
// non-empty allow rule (allowed_domains or allowed_emails) is required when
// enabled, so enabling it can never authorize every Google account.
type GoogleConfig struct {
	Enabled          bool     `yaml:"enabled"`
	ClientID         string   `yaml:"client_id"`
	ClientSecretFile string   `yaml:"client_secret_file"`
	RedirectURL      string   `yaml:"redirect_url"`
	AllowedDomains   []string `yaml:"allowed_domains"`
	AllowedEmails    []string `yaml:"allowed_emails"`

	// ClientSecret is read from ClientSecretFile at load time; it is never set in
	// the YAML itself (secrets live in files, not in the config document).
	ClientSecret string `yaml:"-"`
}

// Default returns a Config with every field set to its built-in default. It is
// used when no config file is present so llmbox runs out of the box.
//
// @return *Config A config populated with defaults.
//
// @testcase TestDefault returns the documented default values.
func Default() *Config {
	c := &Config{}
	c.applyDefaults()
	return c
}

// Load reads and parses the YAML config file at path, applying defaults for any
// field left unset. Unknown keys are rejected so typos surface as errors.
//
// @arg path Filesystem path to the YAML config file.
// @return *Config The parsed configuration with defaults applied.
// @error error if the file cannot be read or the YAML cannot be parsed.
//
// @testcase TestLoad parses a full config file.
// @testcase TestLoadAppliesDefaults fills unset fields with defaults.
// @testcase TestLoadMissingFile errors when the file does not exist.
// @testcase TestLoadRejectsUnknownKey errors on an unknown field.
// @testcase TestLoadParsesDuration parses a duration string from the config.
// @testcase TestLoadRejectsBadDuration fails on an unparseable duration.
// @testcase TestLoadGoogleAuth loads an enabled Google provider and resolves its secret file.
// @testcase TestLoadGoogleRequiresAllowlist rejects an enabled Google provider with no allow rule.
// @testcase TestLoadGoogleMissingSecretFile errors when the client secret file is absent.
// @testcase TestLoadRegistry loads a registry entry and resolves its password file.
// @testcase TestLoadRegistryMissingPasswordFile errors when the password file is absent.
// @testcase TestLoadRegistryRequiresHost rejects a registry entry with no registry host.
// @testcase TestLoadTLS loads an enabled TLS block with a cert and key file.
// @testcase TestLoadTLSRequiresCertAndKey rejects enabled TLS with no cert_file or key_file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	cfg := &Config{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	// An empty file yields io.EOF (no document); that's a valid "all defaults".
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.resolveSecrets(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// LoadConfig loads the YAML config at path. When the path was not given
// explicitly on the command line and the default file is absent, it prints a
// warning to stderr and returns the built-in defaults so the command runs without
// a config file; an explicitly named missing or invalid file is an error. The
// warning exists because a silent default state_file is a common cause of a
// command (e.g. `token create`) writing to a different store than the running hub
// reads.
//
// @arg path The config file path.
// @arg explicit Whether --config was set on the command line.
// @return *Config The loaded (or default) configuration.
// @error error if an explicitly named file is missing, or any named file is invalid.
//
// @testcase TestLoadConfigDefaultsWhenAbsent returns defaults for a missing implicit file.
// @testcase TestLoadConfigErrorsWhenExplicitMissing errors for a missing explicit file.
// @testcase TestLoadConfigReadsFile parses an existing config file.
func LoadConfig(path string, explicit bool) (*Config, error) {
	if !explicit {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			// Falling back to built-in defaults silently has bitten operators:
			// a command run without the hub's config (e.g. `token create` via
			// docker exec) ends up using DefaultStateFile, a different store than
			// the running hub, so its tokens are never seen. Make the fallback
			// visible.
			fmt.Fprintf(os.Stderr, "warning: no config file at %q; using built-in defaults (state_file %q)\n", path, DefaultStateFile)
			return Default(), nil
		}
	}
	return Load(path)
}

// resolveSecrets reads every file-referenced secret into its in-memory field, so
// the rest of the program never touches the secret files. Secrets are always
// referenced by path and never inlined in the YAML.
//
// @error error if an enabled provider's secret file cannot be read.
//
// @testcase TestLoadGoogleAuth resolves the Google client secret from its file.
// @testcase TestLoadGoogleMissingSecretFile errors when the secret file is absent.
// @testcase TestLoadRegistry resolves a registry password from its file.
// @testcase TestLoadRegistryMissingPasswordFile errors when the password file is absent.
func (c *Config) resolveSecrets() error {
	if c.Auth.Google.Enabled {
		secret, err := secretFromFile(c.Auth.Google.ClientSecretFile)
		if err != nil {
			return fmt.Errorf("auth.google.client_secret_file: %w", err)
		}
		c.Auth.Google.ClientSecret = secret
	}
	for i := range c.Registries {
		secret, err := secretFromFile(c.Registries[i].PasswordFile)
		if err != nil {
			return fmt.Errorf("registries[%d].password_file: %w", i, err)
		}
		c.Registries[i].Password = secret
	}
	return nil
}

// secretFromFile reads path and returns its trimmed contents as a secret value.
//
// @arg path The file holding the secret; empty is an error.
// @return string The trimmed file contents.
// @error error if no path is given or the file cannot be read.
//
// @testcase TestLoadGoogleAuth reads a secret from a file via this helper.
// @testcase TestLoadGoogleMissingSecretFile errors for a missing secret file.
func secretFromFile(path string) (string, error) {
	if path == "" {
		return "", errors.New("no file path configured")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// validate rejects internally-inconsistent configuration. In particular an
// enabled auth provider must carry credentials and a non-empty allow rule, so
// enabling it can never authorize every account.
//
// @error error if an enabled provider is missing credentials or an allow rule.
//
// @testcase TestLoadGoogleRequiresAllowlist rejects an enabled provider with no allow rule.
// @testcase TestLoadRegistryRequiresHost rejects a registry entry with no registry host.
// @testcase TestLoadTLSRequiresCertAndKey rejects enabled TLS with no cert_file or key_file.
func (c *Config) validate() error {
	if c.TLS.Enabled && (c.TLS.CertFile == "" || c.TLS.KeyFile == "") {
		return errors.New("tls.enabled requires cert_file and key_file")
	}
	if g := c.Auth.Google; g.Enabled {
		if g.ClientID == "" {
			return errors.New("auth.google.enabled requires client_id")
		}
		if g.ClientSecret == "" {
			return errors.New("auth.google.enabled requires a non-empty client_secret_file")
		}
		if len(g.AllowedDomains) == 0 && len(g.AllowedEmails) == 0 {
			return errors.New("auth.google.enabled requires allowed_domains or allowed_emails (refusing to authorize every Google account)")
		}
	}
	for i, r := range c.Registries {
		if r.Registry == "" {
			return fmt.Errorf("registries[%d] requires a registry host", i)
		}
	}
	return nil
}

// applyDefaults fills any field left at its zero value with its built-in
// default. Fields whose defaults are owned by the Docker manager are left as-is.
//
// @testcase TestLoadAppliesDefaults fills unset fields with defaults.
// @testcase TestDefault returns the documented default values.
func (c *Config) applyDefaults() {
	if c.HTTPAddr == "" {
		c.HTTPAddr = DefaultHTTPAddr
	}
	if c.PublicURL == "" {
		c.PublicURL = DefaultPublicURL
	}
	if c.AuthTTL == 0 {
		c.AuthTTL = Duration(DefaultAuthTTL)
	}
	if c.StateFile == "" {
		c.StateFile = DefaultStateFile
	}
	if c.Auth.SessionTTL == 0 {
		c.Auth.SessionTTL = Duration(DefaultSessionTTL)
	}
	// Default each enabled provider's redirect URL from the public URL.
	if c.Auth.Google.Enabled && c.Auth.Google.RedirectURL == "" {
		c.Auth.Google.RedirectURL = strings.TrimRight(c.PublicURL, "/") + "/auth/google/callback"
	}
	// Apply finite per-box resource caps when unset so boxes are bounded out of
	// the box (max_boxes stays unlimited unless explicitly set).
	if c.Box.MemoryMB == 0 {
		c.Box.MemoryMB = boxconfig.DefaultBoxMemoryMB
	}
	if c.Box.CPUs == 0 {
		c.Box.CPUs = boxconfig.DefaultBoxCPUs
	}
	if c.Box.PidsLimit == 0 {
		c.Box.PidsLimit = boxconfig.DefaultBoxPidsLimit
	}
}
