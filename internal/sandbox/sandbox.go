// Package sandbox holds the backend-neutral box lifecycle types shared across
// llmbox: the inputs and views that cross the hub/spoke boundary and the
// box-id validation rule. They live here, rather than in internal/docker, so the
// cluster and server layers (and any future isolation backend) depend on a
// neutral contract instead of the Docker implementation. internal/docker
// re-exports these as aliases so its own code and tests are unaffected.
package sandbox

import (
	"errors"
	"regexp"
)

// ErrBoxNotFound reports that no managed box matches a given identifier — e.g.
// because its container/instance was removed out of band. Destroying an
// already-gone box surfaces this so callers can treat removal as idempotent. It
// is backend-neutral (matched by the server/cluster layers) so it lives here
// rather than in a specific isolation backend.
var ErrBoxNotFound = errors.New("no managed box matches")

// Box is a view of a managed box returned to callers.
type Box struct {
	ContainerID string `json:"container_id" jsonschema:"the short Docker container ID"`
	Name        string `json:"name" jsonschema:"the container name"`
	BoxID       string `json:"box_id,omitempty" jsonschema:"the box ID the caller assigned, if any (also set as the container hostname)"`
	Description string `json:"description,omitempty" jsonschema:"the caller-supplied description label, if any"`
	Spoke       string `json:"spoke,omitempty" jsonschema:"the cluster spoke the box runs on; 'local' for the in-process spoke"`
	Image       string `json:"image" jsonschema:"the image the box runs"`
	State       string `json:"state" jsonschema:"the container state, e.g. running or exited"`
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
	// Image is the container image to launch; empty means the Manager default.
	Image string
	// BoxID is the caller-assigned identifier for the box. When set, it is also
	// applied as the box's container hostname (what `hostname` reports inside it,
	// and the name shown in claude.ai/code), so it must be a valid hostname or
	// Docker rejects creation. It must be unique across managed boxes.
	BoxID string
	// Description is a free-form label shown by list/get to help the caller tell
	// boxes apart. It has no effect on the box itself.
	Description string
	// SpokeName selects which cluster spoke the box is created on (empty or
	// "local" means the in-process spoke). It is routing metadata used by the
	// server's cluster layer; the Docker manager itself ignores it.
	SpokeName string
	// Files are written into the box's filesystem after it is created but before
	// it starts, so they are present when the entrypoint runs. Used to inject
	// per-box secrets (e.g. a granular subject token) without baking them into
	// the image, an env var, or a label where `docker inspect` would expose them.
	Files []InjectFile
}

// InjectFile is one file to write into a new box. Path is absolute inside the
// container; Content is its bytes; Mode/UID/GID set its permissions and owner
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
	MemoryBytes int64
	// NanoCPUs is the CPU quota per box in units of 1e-9 CPUs, i.e. 1_000_000_000
	// is one full CPU (0 = unlimited).
	NanoCPUs int64
	// PidsLimit caps the number of processes/threads in a box, blunting fork
	// bombs (0 = unlimited).
	PidsLimit int64
	// MaxBoxes caps how many managed boxes may exist at once; Create rejects a new
	// box once the count is reached (0 = unlimited).
	MaxBoxes int
}

// boxIDRe is the canonical box-id format: a single DNS hostname label (1-63
// chars of lowercase letters, digits, or hyphens, not starting or ending with a
// hyphen). The box ID is interpolated into the container entrypoint and applied
// as the container hostname, so it must carry no shell metacharacters; this is
// the authoritative definition the cluster admission policy also enforces.
var boxIDRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidBoxID reports whether id is a well-formed box ID (a single DNS hostname
// label). It is the single source of truth for box-id validation: Create calls
// it so the local box-creation path validates inputs exactly as the cluster
// admission policy does on the remote path, rather than relying on the Docker
// daemon's implicit hostname check to reject a malformed (and potentially
// shell-injecting) box ID.
//
// @arg id The candidate box ID.
// @return bool True when id is a valid 1-63 char lowercase hostname label.
//
// @testcase TestValidBoxID accepts well-formed ids and rejects malformed ones.
func ValidBoxID(id string) bool {
	return boxIDRe.MatchString(id)
}
