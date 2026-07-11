// Package config loads llmbox's YAML configuration file for the hub (the
// llmbox-server). It replaces the older LLMBOX_* environment variables: every
// setting now lives in one YAML document, and any field left unset falls back to
// a built-in default. This config is hub-only and carries only hub concerns
// (HTTP, auth, hooks, proxy, TLS, state): the spoke reads no config file and
// owns every box-provisioning knob (image, resource caps, backend) via its own
// llmbox-spoke flags.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Default values applied to config fields left unset. Box-provisioning knobs
// (image, resource caps, backend) are the spoke's concern and are configured
// with llmbox-spoke flags, so they are deliberately absent from the hub config.
const (
	DefaultHTTPAddr  = ":8080"
	DefaultPublicURL = "http://localhost:8080"
	DefaultStateFile = "llmbox-sessions.db"

	// DefaultSessionTTL is how long an admin login session stays valid when
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
	// JSON API (under /api/v1/) plus the UI (sign-in, admin, spoke connect,
	// health, favicon). The box-control API is unauthenticated by the server itself
	// (see internal/server/http.go), so run behind an authenticating reverse proxy.
	HTTPAddr  string      `yaml:"http_addr"`
	PublicURL string      `yaml:"public_url"`
	StateFile string      `yaml:"state_file"`
	Hooks     []string    `yaml:"hooks"`
	Auth      AuthConfig  `yaml:"auth"`
	Proxy     ProxyConfig `yaml:"proxy"`
	TLS       TLSConfig   `yaml:"tls"`
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
	// BaseDomain is the bare parent domain proxy subdomains hang off, e.g.
	// "proxy.example.com" (a proxy is then reached at <slug>.proxy.example.com). It
	// must be host-only — no scheme, path, wildcard, or port (the listen port is
	// taken from public_url and appended to the advertised URL; incoming Host
	// matching strips the port). A port here silently breaks proxy routing, so it is
	// rejected at load time.
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

// AuthConfig configures who may administer the hub and reach the per-box HTTP
// proxies. When a provider is enabled, the admin UI and the proxies require the
// visitor to sign in with that provider and be on the admin allow-list. Each
// provider is a dedicated sub-block so more can be added later without changing
// the existing ones.
type AuthConfig struct {
	SessionTTL Duration     `yaml:"session_ttl"`
	Google     GoogleConfig `yaml:"google"`
	Admin      AdminConfig  `yaml:"admin"`
	// CookieDomain, when set, is the Domain attribute placed on the login-session
	// cookie so one sign-in is shared across sub-domains: a bare parent domain
	// (e.g. "example.com", NOT ".example.com" and NOT "example.com:8443") that lets
	// the session reach both the main UI and the per-proxy <slug>.example.com hosts.
	// It must cover both the public_url host and proxy.base_domain, and is validated
	// as such at load time. Empty keeps the cookie host-only (the default), which is
	// correct unless the proxy feature is used with sub-domain hosts.
	CookieDomain string `yaml:"cookie_domain"`
}

// AdminConfig lists the signed-in identities allowed to use the admin web UI
// (/admin) for managing spokes and boxes and to reach the per-box HTTP proxies.
// When Emails is empty the admin UI (and provider sign-in) is disabled entirely.
type AdminConfig struct {
	Emails []string `yaml:"emails"`
}

// GoogleConfig configures Sign in with Google (OIDC). Only identities on the
// admin allow-list (auth.admin.emails) are authorized after signing in.
type GoogleConfig struct {
	Enabled          bool   `yaml:"enabled"`
	ClientID         string `yaml:"client_id"`
	ClientSecretFile string `yaml:"client_secret_file"`
	RedirectURL      string `yaml:"redirect_url"`

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
// @testcase TestLoadGoogleMissingSecretFile errors when the client secret file is absent.
// @testcase TestLoadRejectsBoxRunningKey rejects box-running keys that moved to the spoke.
// @testcase TestExampleConfigParses parses the shipped llmbox.example.yaml.
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

// validate rejects internally-inconsistent configuration. An enabled auth
// provider must carry credentials. The proxy base domain and the login cookie
// domain must each be a bare host, and the cookie domain must span both the
// public_url host and the proxy base domain — shapes that otherwise silently
// break proxy routing or the shared login (a port or leading dot slips past at
// load and only surfaces as a dead proxy or a rejected cookie later).
//
// @error error if an enabled provider is missing credentials.
// @error error if proxy.base_domain or auth.cookie_domain is not a bare host.
// @error error if auth.cookie_domain does not cover public_url or proxy.base_domain.
//
// @testcase TestLoadTLSRequiresCertAndKey rejects enabled TLS with no cert_file or key_file.
// @testcase TestLoadRejectsBaseDomainWithPort rejects a proxy.base_domain carrying a port.
// @testcase TestLoadRejectsCookieDomainLeadingDot rejects an auth.cookie_domain with a leading dot.
// @testcase TestLoadRejectsCookieDomainNotCoveringBase rejects a cookie domain that is not a parent of the base domain.
// @testcase TestLoadRejectsCookieDomainNotCoveringPublicURL rejects a cookie domain the public_url host is outside of.
// @testcase TestLoadAcceptsConsistentProxyAndCookieDomains accepts a bare cookie domain that spans the base domain and public_url.
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
	}
	if c.Proxy.BaseDomain != "" {
		if err := validateBareDomain("proxy.base_domain", c.Proxy.BaseDomain); err != nil {
			return err
		}
	}
	if c.Auth.CookieDomain != "" {
		if err := validateBareDomain("auth.cookie_domain", c.Auth.CookieDomain); err != nil {
			return err
		}
		// The cookie is set by the public_url host, and a browser only accepts a
		// Domain attribute that host belongs to. A cookie domain the public_url is
		// outside of yields a silently-rejected login cookie.
		if u, err := url.Parse(c.PublicURL); err == nil && u.Host != "" && !domainCovers(c.Auth.CookieDomain, u.Hostname()) {
			return fmt.Errorf("auth.cookie_domain %q does not cover public_url host %q; the browser would reject the login cookie", c.Auth.CookieDomain, u.Hostname())
		}
		// The shared login must reach the per-proxy sub-domains, so the cookie domain
		// has to be the proxy base domain or a parent of it.
		if c.Proxy.BaseDomain != "" && !domainCovers(c.Auth.CookieDomain, c.Proxy.BaseDomain) {
			return fmt.Errorf("auth.cookie_domain %q must equal or be a parent of proxy.base_domain %q so the login cookie covers proxy sub-domains", c.Auth.CookieDomain, c.Proxy.BaseDomain)
		}
	}
	return nil
}

// validateBareDomain rejects a domain value that is not a plain DNS host. The
// proxy base domain and the login cookie domain must each be a bare host (like
// "example.com") because a scheme, path, wildcard, port, or leading/trailing dot
// silently breaks either proxy Host matching (which strips the port before
// comparing) or the cookie's Domain attribute (which is host-only and dotless).
//
// @arg field The config field name, used in the error message.
// @arg v The configured domain value (assumed non-empty).
// @error error if v carries a scheme, path, wildcard, port, whitespace, or a leading/trailing dot.
//
// @testcase TestLoadRejectsBaseDomainWithPort rejects a base domain with a port.
// @testcase TestLoadRejectsCookieDomainLeadingDot rejects a cookie domain with a leading dot.
func validateBareDomain(field, v string) error {
	switch {
	case strings.ContainsAny(v, " \t\r\n"):
		return fmt.Errorf("%s %q must not contain whitespace", field, v)
	case strings.Contains(v, "://"):
		return fmt.Errorf("%s %q must be a bare domain, not a URL (drop the scheme)", field, v)
	case strings.Contains(v, "/"):
		return fmt.Errorf("%s %q must be a bare domain with no path", field, v)
	case strings.Contains(v, "*"):
		return fmt.Errorf("%s %q must not contain a wildcard; it is the parent domain sub-domains hang off", field, v)
	case strings.Contains(v, ":"):
		return fmt.Errorf("%s %q must not include a port (the listen port comes from public_url and the incoming request)", field, v)
	case strings.HasPrefix(v, "."), strings.HasSuffix(v, "."):
		return fmt.Errorf("%s %q must not begin or end with a dot", field, v)
	}
	return nil
}

// domainCovers reports whether cookie domain parent scopes host: true when host
// equals parent or is a sub-domain of it. Both are lower-cased so the comparison
// is case-insensitive, matching how browsers and the proxy Host matcher treat
// domains. It assumes parent is already a bare host (see validateBareDomain).
//
// @arg parent The bare cookie/base domain being checked as an ancestor.
// @arg host The host that must fall under parent.
// @return bool True when host is parent or a sub-domain of it.
//
// @testcase TestLoadAcceptsConsistentProxyAndCookieDomains accepts a host under the cookie domain.
// @testcase TestLoadRejectsCookieDomainNotCoveringBase rejects a base domain outside the cookie domain.
func domainCovers(parent, host string) bool {
	parent = strings.ToLower(parent)
	host = strings.ToLower(host)
	return host == parent || strings.HasSuffix(host, "."+parent)
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
