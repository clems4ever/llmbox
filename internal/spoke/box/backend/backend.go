// Package backend is the registry that decouples box-backend selection from the
// composition roots. Each isolation backend (internal/docker, internal/firecracker)
// registers a Factory under a name in its init, and the server and spoke pick one
// by name through New — so adding a backend is a new self-registering package plus
// a name in config, with no change to the wiring in cmd/ or internal/spoke.
//
// Options is the neutral superset of every backend's construction inputs: common
// fields (image, socket dir, peers, limits, namespace) plus backend-specific
// fields each factory reads only if it applies (GPU/registry auth for Docker; the
// kernel, rootfs, and state dir for Firecracker). A factory ignores the fields
// that do not concern it.
package backend

import (
	"fmt"
	"io"
	"sort"

	"github.com/docker/docker/api/types/registry"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/box"
	"github.com/clems4ever/llmbox/internal/spoke/boxapi"
)

// DefaultName is the backend selected when none is configured, preserving the
// pre-registry behaviour where every deployment ran on Docker.
const DefaultName = "docker"

// Options carries every construction input a backend may need. Common fields
// apply to all backends; the grouped backend-specific fields are read only by the
// backend they name and ignored by the others.
type Options struct {
	// DefaultImage is the image/rootfs launched when a create supplies none; empty
	// lets the backend fall back to its own default.
	DefaultImage string
	// SocketDir is the host directory a backend uses for per-box control endpoints
	// (a bind-mounted Unix socket for Docker; the vsock UDS for Firecracker).
	SocketDir string
	// Peers are resource-server endpoints wired into every box so boxes can reach
	// shared services while staying isolated from one another.
	Peers []string
	// Limits caps each box's resources; MaxBoxes is enforced by box.Manager.
	Limits sandbox.Limits
	// Namespace scopes a backend to the boxes it created so two processes sharing a
	// host never list, reap, or destroy each other's boxes.
	Namespace string
	// BoxPorts serves box-originated port-publishing requests toward the hub. When
	// set, each box gets a per-box boxapi socket bound to its identity; nil
	// disables the box-port API entirely.
	BoxPorts boxapi.PortService

	// GPUs is a `docker run --gpus`-style spec attached to every box (Docker only).
	GPUs string
	// RegistryAuths holds image-pull credentials keyed by registry host (Docker
	// only). registry.AuthConfig is a standalone OCI-registry credential type, not
	// the Docker client, so depending on it here couples nothing to the daemon.
	RegistryAuths map[string]registry.AuthConfig

	// KernelImagePath is the guest kernel (vmlinux) a microVM backend boots
	// (Firecracker only).
	KernelImagePath string
	// RootfsImagePath is the default guest root filesystem image a microVM backend
	// boots when a create supplies no image (Firecracker only).
	RootfsImagePath string
	// PayloadImagePath is an optional read-only ext4 carrying the guest agent (plus
	// claude and its trust seed), attached to every box as a shared second drive so
	// the agent can be updated without rebuilding the base rootfs (Firecracker only).
	// Empty keeps the all-in-one rootfs with the agent baked in.
	PayloadImagePath string
	// StateDir is where a backend without a daemon registry persists per-box
	// metadata so List/Find/reap survive a process restart (Firecracker only).
	StateDir string
	// DisableEgress boots control-only boxes with no TAP/NAT egress interface
	// (Firecracker only), removing the CAP_NET_ADMIN requirement at the cost of the
	// guest having no outbound network.
	DisableEgress bool
	// PoolSize is the number of egress TAP devices provisioned at startup and reused
	// across boxes (Firecracker only); it caps concurrent networked boxes. 0 uses
	// the backend default.
	PoolSize int
}

// Provisioner is a box.Provisioner that also releases backend resources on Close,
// so the composition root can defer a single Close regardless of which backend it
// selected.
type Provisioner interface {
	box.Provisioner
	io.Closer
}

// Factory builds a Provisioner from Options. Each backend registers one.
type Factory func(Options) (Provisioner, error)

// registered maps a backend name to its factory. It is written only from package
// init functions (single-threaded), so it needs no lock.
var registered = map[string]Factory{}

// Register records a backend's factory under name. A backend calls it from its
// init so importing the backend package is enough to make it selectable. It
// panics on a duplicate name, which can only be a programming error.
//
// @arg name The backend name callers select it by (e.g. "docker", "firecracker").
// @arg f The factory that builds the backend's Provisioner from Options.
//
// @testcase TestRegisterAndNew registers a fake backend and builds it through New.
// @testcase TestRegisterPanicsOnDuplicate panics when a name is registered twice.
func Register(name string, f Factory) {
	if _, dup := registered[name]; dup {
		panic(fmt.Sprintf("box backend %q registered twice", name))
	}
	registered[name] = f
}

// New builds the backend named name from opts. An empty name selects DefaultName,
// so a deployment that configures nothing keeps running on Docker.
//
// @arg name The backend to build; empty selects DefaultName.
// @arg opts The construction inputs passed to the selected factory.
// @return Provisioner The constructed backend, ready to provision boxes.
// @error error if no backend is registered under name, or the factory fails.
//
// @testcase TestRegisterAndNew builds a registered backend by name.
// @testcase TestNewUnknownBackend errors on a name no backend registered.
// @testcase TestNewEmptyNameUsesDefault builds the default backend for an empty name.
func New(name string, opts Options) (Provisioner, error) {
	if name == "" {
		name = DefaultName
	}
	f, ok := registered[name]
	if !ok {
		return nil, fmt.Errorf("unknown box backend %q (available: %v)", name, Names())
	}
	return f(opts)
}

// Names returns the registered backend names in sorted order, for error messages
// and help text.
//
// @return []string The registered backend names, sorted.
//
// @testcase TestRegisterAndNew sees the registered name reported by Names.
func Names() []string {
	names := make([]string, 0, len(registered))
	for n := range registered {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
