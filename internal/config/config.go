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
	HTTPAddr    string   `yaml:"http_addr"`
	PublicURL   string   `yaml:"public_url"`
	ClaudeImage string   `yaml:"claude_image"`
	ClaudeBin   string   `yaml:"claude_bin"`
	RemoteArgs  string   `yaml:"remote_args"`
	AuthTTL     Duration `yaml:"auth_ttl"`
	StateFile   string   `yaml:"state_file"`
	Hooks       []string `yaml:"hooks"`
	BoxPeers    []string `yaml:"box_peers"`
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
	return cfg, nil
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
}
