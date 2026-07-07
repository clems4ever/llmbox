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

// Box is a view of a managed box returned to callers.
type Box struct {
	InstanceID  string `json:"instance_id" jsonschema:"the backend instance ID identifying the box (e.g. a short container ID or microVM ID)"`
	Name        string `json:"name" jsonschema:"the backend instance name"`
	BoxID       string `json:"box_id,omitempty" jsonschema:"the box ID the caller assigned, if any (also set as the box hostname)"`
	Description string `json:"description,omitempty" jsonschema:"the caller-supplied description label, if any"`
	Spoke       string `json:"spoke,omitempty" jsonschema:"the cluster spoke the box runs on"`
	Image       string `json:"image" jsonschema:"the image or rootfs the box runs (may be empty for backends without an image concept)"`
	State       string `json:"state" jsonschema:"the instance state, e.g. running or stopped"`
	Status      string `json:"status" jsonschema:"a human readable status string"`
	Phase       string `json:"phase" jsonschema:"auth phase: pending (awaiting login) or ready (authenticated)"`
	Created     int64  `json:"created" jsonschema:"creation time as a unix timestamp"`
}

// ExecResult is the captured outcome of a command run inside a box.
type ExecResult struct {
	Stdout   string `json:"stdout" jsonschema:"the command's standard output"`
	Stderr   string `json:"stderr" jsonschema:"the command's standard error"`
	ExitCode int    `json:"exit_code" jsonschema:"the command's exit code (0 means success)"`
}

// CreateOptions holds the caller-controlled inputs for a new box.
type CreateOptions struct {
	// Image is the image or rootfs reference to launch; empty means the Manager
	// default. Backends without an image concept may ignore it.
	Image string
	// BoxID is the caller-assigned identifier for the box. When set, it is also
	// applied as the box's hostname (what `hostname` reports inside it, and the
	// name shown in claude.ai/code), so it must be a valid hostname label. It must
	// be unique across managed boxes.
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
