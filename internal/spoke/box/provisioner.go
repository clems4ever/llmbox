// Package box holds the backend-neutral box manager. A Manager turns the box
// verbs (cluster.BoxManager) into two collaborators: a Provisioner that creates
// and tears down the compute (a Docker container or a Firecracker microVM) and
// exposes a control channel to the in-box guest, and the guest client
// that runs the actual box behaviour (login, exec, logs, port dialing) over that
// channel. All the lifecycle logic that is the same regardless of backend —
// box-id validation and uniqueness, the create/login handshake, the box count
// cap, orphan reaping — lives here, so a backend only implements the small
// Provisioner/Instance surface.
package box

import (
	"context"
	"net"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// Provisioner is the backend that owns box compute. It creates boxes, lists and
// resolves them, and nothing more — box behaviour (login, exec, logs, dialing)
// runs through the guest over an Instance's control channel, not through the
// Provisioner.
type Provisioner interface {
	// Provision creates a new box (in the pending auth phase) from opts and
	// returns a handle to it. The box's guest must be reachable via the returned
	// Instance's Control once Provision returns.
	Provision(ctx context.Context, opts sandbox.CreateOptions) (Instance, error)
	// List returns a handle to every managed box.
	List(ctx context.Context) ([]Instance, error)
	// Find resolves a box handle — the InstanceID returned by Provision or the
	// caller-assigned BoxID — to the single managed box it identifies, returning a
	// wrapped sandbox.ErrBoxNotFound when none matches. A box is created with an
	// optional BoxID alias and afterwards addressed by whichever handle the caller
	// kept (its backend instance ID or its box ID).
	Find(ctx context.Context, idOrName string) (Instance, error)
}

// NetworkAuditor is an optional Instance capability: a box whose backend records
// egress flow metadata (from the host conntrack table) returns it here. A backend
// that cannot audit — or a box booted without egress — simply does not implement
// it, and Manager.NetworkFlows reports no flows rather than failing. It is kept off
// the core Instance surface so a backend opts in without every backend having to.
type NetworkAuditor interface {
	// NetworkFlows returns the recorded outbound flow metadata for this box, newest
	// first. It is metadata only (destinations and byte counts), never payloads.
	NetworkFlows(ctx context.Context) ([]sandbox.NetworkFlow, error)
}

// Instance is a handle to one managed box.
type Instance interface {
	// Meta returns the box's current view (ID, name, phase, state, timestamps).
	Meta() sandbox.Box
	// Control opens a new control connection to the box's guest. The caller owns
	// the connection and must close it.
	Control(ctx context.Context) (net.Conn, error)
	// MarkReady moves the box from the pending auth phase to ready, once it has
	// authenticated, so the orphan reaper spares it.
	MarkReady(ctx context.Context) error
	// Pause stops the box's compute to free CPU/RAM while keeping its disk (auth,
	// workspace) and identity, so it can later be resumed. The box keeps appearing
	// in List (reported with sandbox.StatePaused) and its running guest/workload
	// process is lost. Pausing an already-gone box returns a wrapped
	// sandbox.ErrBoxNotFound.
	Pause(ctx context.Context) error
	// Resume restarts a paused box's compute from its kept disk and blocks until the
	// guest's control channel is reachable again. It restores only the compute; the
	// caller (the Manager) re-drives the guest handshake to restart the workload. Resuming
	// an already-gone box returns a wrapped sandbox.ErrBoxNotFound.
	Resume(ctx context.Context) error
	// Destroy stops and removes the box. Destroying an already-gone box returns a
	// wrapped sandbox.ErrBoxNotFound, which the Manager treats as success.
	Destroy(ctx context.Context) error
}
