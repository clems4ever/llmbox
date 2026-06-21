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
// its own defaults for the image, Claude binary, and remote args, so those are
// deliberately absent here (an empty value flows through to the manager).
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
	HTTPAddr    string        `yaml:"http_addr"`
	PublicURL   string        `yaml:"public_url"`
	ClaudeImage string        `yaml:"claude_image"`
	ClaudeBin   string        `yaml:"claude_bin"`
	RemoteArgs  string        `yaml:"remote_args"`
	AuthTTL     Duration      `yaml:"auth_ttl"`
	StateFile   string        `yaml:"state_file"`
	Hooks       []string      `yaml:"hooks"`
	BoxPeers    []string      `yaml:"box_peers"`
	Auth        AuthConfig    `yaml:"auth"`
	Cluster     ClusterConfig `yaml:"cluster"`
	Spoke       SpokeConfig   `yaml:"spoke"`
}

// ClusterConfig enables hub-and-spoke clustering on the hub. When enabled, the
// server exposes the /spoke/connect endpoint so remote spokes (started with
// `llmbox spoke`) can join and run boxes; boxes still default to the in-process
// "local" spoke. The `llmbox spoke` command has its own flags and does not read
// this block.
type ClusterConfig struct {
	Enabled bool `yaml:"enabled"`
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
func (c *Config) resolveSecrets() error {
	if c.Auth.Google.Enabled {
		secret, err := secretFromFile(c.Auth.Google.ClientSecretFile)
		if err != nil {
			return fmt.Errorf("auth.google.client_secret_file: %w", err)
		}
		c.Auth.Google.ClientSecret = secret
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
}
