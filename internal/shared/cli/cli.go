// Package cli holds helpers shared by the llmbox command binaries (the server and
// the spoke): loading the YAML config with the same defaulting/warning behaviour,
// and converting the operator-facing config blocks into the domain types the box
// layer consumes. Keeping them here lets each binary stay a thin main package
// without duplicating this glue.
package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/docker/docker/api/types/registry"

	"github.com/clems4ever/llmbox/internal/shared/config"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// LoadConfig loads the YAML config at path. When the path was not given
// explicitly on the command line and the default file is absent, it prints a
// warning to stderr and returns the built-in defaults so the command runs without
// a config file; an explicitly named missing or invalid file is an error. The
// warning exists because a silent default state_file is a common cause of a
// command (e.g. `spoke token create`) writing to a different store than the
// running hub reads.
//
// @arg path The config file path.
// @arg explicit Whether --config was set on the command line.
// @return *config.Config The loaded (or default) configuration.
// @error error if an explicitly named file is missing, or any named file is invalid.
//
// @testcase TestLoadConfigDefaultsWhenAbsent returns defaults for a missing implicit file.
// @testcase TestLoadConfigErrorsWhenExplicitMissing errors for a missing explicit file.
// @testcase TestLoadConfigReadsFile parses an existing config file.
func LoadConfig(path string, explicit bool) (*config.Config, error) {
	if !explicit {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			// Falling back to built-in defaults silently has bitten operators:
			// a command run without the hub's config (e.g. `spoke token create`
			// via docker exec) ends up using DefaultStateFile, a different store
			// than the running hub, so its tokens are never seen. Make the
			// fallback visible.
			fmt.Fprintf(os.Stderr, "warning: no config file at %q; using built-in defaults (state_file %q)\n", path, config.DefaultStateFile)
			return config.Default(), nil
		}
	}
	return config.Load(path)
}

// BoxLimits converts the YAML box block into the per-box sandbox.Limits,
// translating the operator-friendly units (mebibytes, fractional CPUs) into the
// raw byte / nano-CPU counts the Docker API expects. A zero field stays zero
// (unlimited) so the conversion preserves "no limit" semantics.
//
// @arg b The box resource configuration from the YAML config.
// @return sandbox.Limits The equivalent per-box caps and max-box ceiling.
//
// @testcase TestBoxLimitsConvertsUnits converts mebibytes and CPUs to bytes and nano-CPUs.
func BoxLimits(b config.BoxConfig) sandbox.Limits {
	return sandbox.Limits{
		MemoryBytes: int64(b.MemoryMB) * 1024 * 1024,
		NanoCPUs:    int64(b.CPUs * 1e9),
		PidsLimit:   b.PidsLimit,
		MaxBoxes:    b.MaxBoxes,
	}
}

// RegistryAuths turns the configured registry credentials into the per-host auth
// map the Docker provisioner consumes, keyed by registry host. It returns nil when
// no registries are configured, which leaves every image pull anonymous.
//
// @arg regs The configured registry credentials (each carrying a resolved password).
// @return map[string]registry.AuthConfig Pull credentials keyed by registry host, or nil when none are configured.
//
// @testcase TestRegistryAuthsKeyedByHost maps each entry by its registry host and returns nil when empty.
func RegistryAuths(regs []config.RegistryConfig) map[string]registry.AuthConfig {
	if len(regs) == 0 {
		return nil
	}
	auths := make(map[string]registry.AuthConfig, len(regs))
	for _, r := range regs {
		auths[r.Registry] = registry.AuthConfig{
			Username:      r.Username,
			Password:      r.Password,
			ServerAddress: r.Registry,
		}
	}
	return auths
}
