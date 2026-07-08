// Package boxconfig holds the box-provisioning configuration shared by the hub
// (which fills these from its YAML config) and the spoke (which fills them from
// command-line flags): the per-box resource caps and the registry credentials,
// their built-in defaults, and the conversion into the Docker registry auth map
// the box layer consumes. Keeping these in a small shared package lets the
// hub-only config loader and the config-free spoke share one definition of a
// box's knobs without either depending on the other. The spoke-only conversion
// of these caps into sandbox.Limits lives with the spoke (see BoxLimits there).
package boxconfig

import (
	"github.com/docker/docker/api/types/registry"
)

// Default per-box resource caps applied when the box block is unset. They are
// generous (a box runs Claude Code and its builds) but finite, so a single box
// cannot exhaust the host's memory/CPU/PIDs. Set a field to 0 to lift a cap.
const (
	DefaultBoxMemoryMB  = 4096
	DefaultBoxCPUs      = 2.0
	DefaultBoxPidsLimit = 4096
)

// BoxConfig caps the resources each box may consume and how many boxes may run
// at once. The limits bound resource-exhaustion (CPU/memory/PID fork-bombs,
// unbounded box counts) by a caller that can reach the by-design-unauthenticated
// create/exec path (see the MCP endpoint comment in internal/hub/http.go).
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

// RegistryAuths turns the configured registry credentials into the per-host auth
// map the Docker provisioner consumes, keyed by registry host. It returns nil when
// no registries are configured, which leaves every image pull anonymous.
//
// @arg regs The configured registry credentials (each carrying a resolved password).
// @return map[string]registry.AuthConfig Pull credentials keyed by registry host, or nil when none are configured.
//
// @testcase TestRegistryAuthsKeyedByHost maps each entry by its registry host and returns nil when empty.
func RegistryAuths(regs []RegistryConfig) map[string]registry.AuthConfig {
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
