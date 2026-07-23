// Package cluster implements llmbox's hub-and-spoke model: a single hub (the
// box-control server the chatbot talks to) drives box operations on one or more
// spokes, each of which owns a local Docker daemon. A spoke dials the hub over
// a WebSocket and the hub pushes box verbs down that connection; the spoke
// executes them against its local *box.Manager and replies.
//
// The wire surface is deliberately the box verbs of BoxManager and nothing
// more: a spoke is never a generic Docker proxy. See docs/hub-and-spoke.md.
package cluster

import (
	"context"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// BoxManager is the box-lifecycle surface the hub needs from a spoke. The local
// in-process implementation is *box.Manager; the remote implementation
// (remoteSpoke) round-trips each call over a transport to a spoke process. It is
// the complete RPC allowlist of the cluster protocol — no operation outside it
// can cross the hub/spoke boundary.
type BoxManager interface {
	Create(ctx context.Context, opts sandbox.CreateOptions) (sandbox.CreateResult, error)
	List(ctx context.Context) ([]sandbox.Box, error)
	Destroy(ctx context.Context, idOrName string) error
	Pause(ctx context.Context, idOrName string) error
	Resume(ctx context.Context, idOrName string) error
	Exec(ctx context.Context, idOrName string, cmd []string) (sandbox.ExecResult, error)
	// SetNetworkPolicy pushes a box's effective egress allowlist to the spoke so
	// its network-isolation enforcement (llmbox-dnsd + firewall) reflects the
	// hub's current allowlist configuration. It is a no-op on a spoke that does
	// not run isolation. Applying a policy for an unknown box is not an error —
	// the spoke keeps it and applies it when the box appears — so a policy push
	// racing box creation is not lost.
	SetNetworkPolicy(ctx context.Context, boxID string, policy sandbox.NetworkPolicy) error
}
