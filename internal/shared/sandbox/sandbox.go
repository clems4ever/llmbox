// Package sandbox holds the backend-neutral box lifecycle types shared across
// llmbox: the inputs and views that cross the hub/spoke boundary and the
// box-id validation rule. They live here, rather than in a specific isolation
// backend, so the cluster and server layers (and every backend — Docker,
// Firecracker, or a future one) depend on a neutral contract instead of any one
// implementation. Backends map these onto their own primitives (a container, a
// microVM) and never leak backend-specific concepts back into this package.
package sandbox

import (
	"errors"
	"regexp"
)

// ErrBoxNotFound reports that no managed box matches a given identifier — e.g.
// because its underlying instance was removed out of band. Destroying an
// already-gone box surfaces this so callers can treat removal as idempotent. It
// is backend-neutral (matched by the server/cluster layers) so it lives here
// rather than in a specific isolation backend.
var ErrBoxNotFound = errors.New("no managed box matches")

// Hub-derived box states surfaced in Box.State alongside the backend's own
// instance states (e.g. "running", "exited"). They exist at the hub layer, not
// in any backend: a backend always reports what it actually sees, while the hub
// derives these from its records and live spoke connectivity.
const (
	// StateUnreachable marks a box whose spoke currently has no live connection
	// to the hub — the box may well still be running, the hub just cannot
	// observe it right now.
	StateUnreachable = "unreachable"
	// StateTerminated marks a box confirmed gone from its (reachable) spoke; its
	// record is kept as a tombstone until removed.
	StateTerminated = "terminated"
	// StatePaused marks a box whose compute has been intentionally stopped to save
	// CPU/RAM while its disk (auth, workspace) is kept, so it can be resumed. Unlike
	// the backends' own stopped/exited states it is deliberate, so backends report
	// it explicitly (a marker distinct from a crash) and the reaper and the hub's
	// tombstoning sync leave it be — a paused box still appears in List.
	StatePaused = "paused"
)

// PhaseBroken marks a box whose init script failed during creation: the box was
// provisioned and is left running for inspection, but its workload never started.
// It is the only value the hub sets in Box.Phase (empty otherwise), and
// Box.LastError carries the init script's captured output so an operator can see
// why it broke.
const PhaseBroken = "broken"

// Box is a view of a managed box returned to callers.
type Box struct {
	InstanceID  string `json:"instance_id" jsonschema:"an opaque backend generation token for the box's current incarnation; informational only — reference the box by its box_id, never parse or address by this"`
	Name        string `json:"name" jsonschema:"the backend instance name"`
	BoxID       string `json:"box_id,omitempty" jsonschema:"the box ID the caller assigned, if any (also set as the box hostname)"`
	Description string `json:"description,omitempty" jsonschema:"the caller-supplied description label, if any"`
	Spoke       string `json:"spoke,omitempty" jsonschema:"the cluster spoke the box runs on"`
	Image       string `json:"image" jsonschema:"the image or rootfs the box runs (may be empty for backends without an image concept)"`
	State       string `json:"state" jsonschema:"the instance state, e.g. running or exited; unreachable when the box's spoke is offline, terminated when the box is confirmed gone from its spoke"`
	Status      string `json:"status" jsonschema:"a human readable status string"`
	Phase       string `json:"phase,omitempty" jsonschema:"broken when the box's init script failed during creation; empty otherwise"`
	LastError   string `json:"last_error,omitempty" jsonschema:"the init script's captured output when the box is broken; empty otherwise"`
	Created     int64  `json:"created" jsonschema:"creation time as a unix timestamp"`
	LastSeen    int64  `json:"last_seen,omitempty" jsonschema:"when the hub last observed the box on its spoke, as a unix timestamp (0 when never observed)"`
}

// CreateResult is the outcome of provisioning a box. On the happy path it carries
// the box's generation token and the ports the spoke publishes for it. If the
// box's init script failed, InitScriptFailed is set and InitScriptOutput carries
// the script's captured output: the box was provisioned and is left running (as a
// broken box to inspect). It is returned instead of a bare id so the init-script
// outcome and published ports can cross the hub/spoke boundary as data rather than
// a flattened error.
type CreateResult struct {
	// InstanceID is the box's opaque backend generation token.
	InstanceID string `json:"instance_id"`
	// InitScriptFailed is true when the box's init script failed. The box was still
	// provisioned and is left running so the failure can be inspected.
	InitScriptFailed bool `json:"init_script_failed,omitempty"`
	// InitScriptOutput is the init script's captured output (and failure reason),
	// set only when InitScriptFailed is true.
	InitScriptOutput string `json:"init_script_output,omitempty"`
	// PublishPorts are the in-box TCP ports the spoke is configured to expose as
	// HTTP proxies for every box it creates (the spoke's --publish-port). They are
	// returned so the hub — which owns proxy state — can publish them right after it
	// registers the box, when the box id, spoke, and generation are all known. Empty
	// for a spoke with no configured ports or a box that failed to come up.
	PublishPorts []PublishPort `json:"publish_ports,omitempty"`
}

// PublishPort is one in-box TCP port a spoke publishes as an HTTP proxy for every
// box it creates, with an optional human-readable description carried onto the
// proxy record. It is spoke configuration (see the spoke's --publish-port), not a
// per-request input, so it travels back from the spoke on CreateResult rather than
// in on CreateOptions.
type PublishPort struct {
	// Port is the TCP port inside the box to expose (1-65535).
	Port int `json:"port"`
	// Description is an optional note recorded on the proxy (e.g. the service name).
	Description string `json:"description,omitempty"`
}

// ExecResult is the captured outcome of a command run inside a box.
type ExecResult struct {
	Stdout   string `json:"stdout" jsonschema:"the command's standard output"`
	Stderr   string `json:"stderr" jsonschema:"the command's standard error"`
	ExitCode int    `json:"exit_code" jsonschema:"the command's exit code (0 means success)"`
}

// CreateOptions holds the caller-controlled inputs for a new box. It carries no
// image: the box image is not a per-request input but a property of the spoke
// that runs the box (each spoke launches its own configured default), so nothing
// about the image crosses the hub/spoke boundary.
type CreateOptions struct {
	// BoxID is the caller-assigned identifier for the box. When set, it is also
	// applied as the box's hostname (what `hostname` reports inside it), so it must
	// be a valid hostname label. It must be unique across managed boxes.
	BoxID string
	// Description is a free-form label shown by list/get to help the caller tell
	// boxes apart. It has no effect on the box itself.
	Description string
	// SpokeName selects which cluster spoke the box is created on (empty means the
	// admin-chosen default spoke). It is routing metadata used by the server's
	// cluster layer; the box backend itself ignores it.
	SpokeName string
	// Files are written into the box's filesystem after it is created but before
	// it starts, so they are present when the entrypoint runs. Used to inject
	// per-box secrets (e.g. a granular subject token) without baking them into
	// the image or an env var where the backend's introspection would expose them.
	Files []InjectFile
}

// InjectFile is one file to write into a new box. Path is absolute inside the
// box; Content is its bytes; Mode/UID/GID set its permissions and owner
// (UID/GID matter so a file landing in a non-root user's home stays readable by
// that user).
type InjectFile struct {
	Path    string
	Content []byte
	Mode    int64
	UID     int
	GID     int
}

// Limits caps the resources a single box may consume and the total number of
// concurrent boxes a Manager will run. It bounds resource-exhaustion
// (CPU/memory/PID fork-bombs, unbounded box counts) by a caller that can reach
// the by-design-unauthenticated create/exec path. A zero field means "no limit"
// for that dimension, so the zero Limits preserves the original unbounded
// behaviour for a deployment that opts out.
type Limits struct {
	// MemoryBytes is the hard memory limit per box, in bytes (0 = unlimited).
	// Backends map it onto their own primitive (a cgroup memory cap for
	// containers, the VM's guest memory size for a microVM).
	MemoryBytes int64
	// NanoCPUs is the CPU quota per box in units of 1e-9 CPUs, i.e. 1_000_000_000
	// is one full CPU (0 = unlimited). Backends map it onto a CPU quota or a
	// rounded vCPU count as appropriate.
	NanoCPUs int64
	// PidsLimit caps the number of processes/threads in a box, blunting fork
	// bombs (0 = unlimited). Backends without a native process cap may ignore it.
	PidsLimit int64
	// MaxBoxes caps how many managed boxes may exist at once; Create rejects a new
	// box once the count is reached (0 = unlimited).
	MaxBoxes int
}

// boxIDRe is the canonical box-id format: a single DNS hostname label (1-63
// chars of lowercase letters, digits, or hyphens, not starting or ending with a
// hyphen). The box ID is interpolated into the box entrypoint and applied as the
// box hostname, so it must carry no shell metacharacters; this is the
// authoritative definition the cluster admission policy also enforces.
var boxIDRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidBoxID reports whether id is a well-formed box ID (a single DNS hostname
// label). It is the single source of truth for box-id validation: Create calls
// it so the local box-creation path validates inputs exactly as the cluster
// admission policy does on the remote path, rather than relying on a backend's
// implicit hostname check to reject a malformed (and potentially shell-injecting)
// box ID.
//
// @arg id The candidate box ID.
// @return bool True when id is a valid 1-63 char lowercase hostname label.
//
// @testcase TestValidBoxID accepts well-formed ids and rejects malformed ones.
func ValidBoxID(id string) bool {
	return boxIDRe.MatchString(id)
}
