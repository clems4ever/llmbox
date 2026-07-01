// Package config loads llmbox's YAML configuration file. It replaces the older
// LLMBOX_* environment variables: every setting now lives in one YAML document,
// and any field left unset falls back to a built-in default.
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
)

// Default values applied to config fields left unset. The Docker manager applies
// its own defaults for the image and remote args, so those are deliberately
// absent here (an empty value flows through to the manager).
const (
	DefaultHTTPAddr  = ":8080"
	DefaultPublicURL = "http://localhost:8080"
	DefaultAuthTTL   = 300 * time.Second
	DefaultStateFile = "llmbox-sessions.db"

	// DefaultSpokeImage is the llmbox-spoke container image the admin UI shows in
	// the ready-to-run `docker run …` command when cluster.spoke_image is unset.
	// Operators on a fork or a pinned tag override it in config.
	DefaultSpokeImage = "ghcr.io/clems4ever/granular-llmbox:latest"

	// DefaultSessionTTL is how long an activation login session stays valid when
	// auth.session_ttl is unset.
	DefaultSessionTTL = time.Hour

	// Default per-box resource caps applied when the box block is unset. They are
	// generous (a box runs Claude Code and its builds) but finite, so a single box
	// cannot exhaust the host's memory/CPU/PIDs. Set box.* to 0 to lift a cap.
	DefaultBoxMemoryMB  = 4096
	DefaultBoxCPUs      = 2.0
	DefaultBoxPidsLimit = 4096
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
	HTTPAddr    string        `yaml:"http_addr"`
	PublicURL   string        `yaml:"public_url"`
	ClaudeImage string        `yaml:"claude_image"`
	RemoteArgs  string        `yaml:"remote_args"`
	AuthTTL     Duration      `yaml:"auth_ttl"`
	StateFile   string        `yaml:"state_file"`
	Hooks       []string      `yaml:"hooks"`
	BoxPeers    []string      `yaml:"box_peers"`
	Auth        AuthConfig    `yaml:"auth"`
	Cluster     ClusterConfig `yaml:"cluster"`
	Spoke       SpokeConfig   `yaml:"spoke"`
	Box         BoxConfig     `yaml:"box"`
	Proxy       ProxyConfig   `yaml:"proxy"`
	// Registries holds credentials for pulling box images from authenticated
	// container registries. The manager selects the entry whose host matches the
	// image being pulled; an image whose registry has no entry is pulled
	// anonymously, as before.
	Registries []RegistryConfig `yaml:"registries"`
}

// RegistryConfig holds the credentials used to pull box images from one
// authenticated container registry. As with other secrets, the password is
// referenced by file and never inlined in the YAML document.
type RegistryConfig struct {
	// Registry is the registry host the credentials apply to, e.g. "ghcr.io" or
	// "registry.example.com:5000". It is matched against the host of the image
	// being pulled; use "docker.io" for Docker Hub images (which carry no host
	// prefix in their reference).
	Registry string `yaml:"registry"`
	// Username is the registry account name (for token-based registries such as
	// ghcr.io this is the account that owns the token).
	Username string `yaml:"username"`
	// PasswordFile is the path to the file holding the password or access token;
	// its contents are read at load time. The secret is never inlined in the YAML.
	PasswordFile string `yaml:"password_file"`

	// Password is read from PasswordFile at load time; it is never set in the YAML
	// itself (secrets live in files, not in the config document).
	Password string `yaml:"-"`
}

// BoxConfig caps the resources each box may consume and how many boxes may run
// at once. The limits bound resource-exhaustion (CPU/memory/PID fork-bombs,
// unbounded box counts) by a caller that can reach the by-design-unauthenticated
// create/exec path (see the MCP endpoint comment in internal/server/http.go).
// memory_mb/cpus/pids_limit default to finite values when left unset (set a high
// value to effectively lift one); max_boxes is unlimited (0) unless set.
type BoxConfig struct {
	// MemoryMB is the hard memory limit per box in mebibytes (0 = unlimited).
	MemoryMB int `yaml:"memory_mb"`
	// CPUs is the fractional CPU quota per box, e.g. 1.5 (0 = unlimited).
	CPUs float64 `yaml:"cpus"`
	// PidsLimit caps processes/threads per box, blunting fork bombs (0 = unlimited).
	PidsLimit int64 `yaml:"pids_limit"`
	// MaxBoxes caps how many boxes may run at once (0 = unlimited).
	MaxBoxes int `yaml:"max_boxes"`
	// SocketDir is the host directory holding each box's control socket (in a 0700
	// per-box subdirectory bind-mounted into the box). It must be reachable by
	// this process and bind-mountable into containers. Empty uses the provisioner
	// default (/run/llmbox/boxsockets).
	SocketDir string `yaml:"socket_dir"`
	// Namespace scopes this process's boxes to a subset of the shared Docker
	// daemon's managed containers: boxes are labelled with it and list/reap/destroy
	// only ever see boxes carrying the same namespace. Set it (to a distinct value
	// per process) only when running two spokes against one daemon, so they do not
	// collapse each other's containers. Empty is unscoped (the default: one spoke
	// per daemon sees every box). On a spoke, the --namespace flag overrides it.
	Namespace string `yaml:"namespace"`
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

// ClusterConfig enables hub-and-spoke clustering on the hub. When enabled, the
// server exposes the /spoke/connect endpoint so remote spokes (started with
// `llmbox spoke`) can join and run boxes; boxes still default to the in-process
// "local" spoke. The `llmbox spoke` command has its own flags and does not read
// this block.
type ClusterConfig struct {
	Enabled bool `yaml:"enabled"`
	// SpokeImage is the llmbox container image shown in the admin UI's
	// ready-to-run spoke command (defaults to DefaultSpokeImage). It does not
	// affect how spokes run — it is purely the image named in that copy-paste
	// command — so set it to the image/tag your spokes actually use.
	SpokeImage string `yaml:"spoke_image"`
}

// SpokeConfig is read by the `llmbox spoke` command. It carries the spoke's
// admission policy for box-creation requests arriving from the hub (defense in
// depth on the edge). allowed_images, when set, restricts which explicit images
// the spoke will launch; an empty list places no image restriction (a request
// with no image uses the spoke's own configured default).
type SpokeConfig struct {
	AllowedImages []string `yaml:"allowed_images"`
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
func (c *Config) validate() error {
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
	if c.Cluster.SpokeImage == "" {
		c.Cluster.SpokeImage = DefaultSpokeImage
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
		c.Box.MemoryMB = DefaultBoxMemoryMB
	}
	if c.Box.CPUs == 0 {
		c.Box.CPUs = DefaultBoxCPUs
	}
	if c.Box.PidsLimit == 0 {
		c.Box.PidsLimit = DefaultBoxPidsLimit
	}
}
