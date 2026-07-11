// Package cluster implements llmbox's hub-and-spoke model: a single hub (the
// MCP front-end the chatbot talks to) drives box operations on one or more
// spokes, each of which owns a local Docker daemon. A spoke dials the hub over
// a WebSocket and the hub pushes box verbs down that connection; the spoke
// executes them against its local *box.Manager and replies.
//
// The wire surface is deliberately the box verbs of BoxManager and nothing
// more: a spoke is never a generic Docker proxy. See docs/hub-and-spoke.md.
package cluster

import (
	"context"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// BoxManager is the box-lifecycle surface the hub needs from a spoke. The local
// in-process implementation is *box.Manager; the remote implementation
// (remoteSpoke) round-trips each call over a transport to a spoke process. It is
// the complete RPC allowlist of the cluster protocol — no operation outside it
// can cross the hub/spoke boundary.
type BoxManager interface {
	Create(ctx context.Context, opts sandbox.CreateOptions) (id, authorizeURL string, err error)
	SubmitCode(ctx context.Context, idOrName, code string) (sessionURL string, err error)
	List(ctx context.Context) ([]sandbox.Box, error)
	Destroy(ctx context.Context, idOrName string) error
	Pause(ctx context.Context, idOrName string) error
	Resume(ctx context.Context, idOrName string) (sessionURL string, err error)
	Logs(ctx context.Context, idOrName string, tail int) (string, error)
	Exec(ctx context.Context, idOrName string, cmd []string) (sandbox.ExecResult, error)
	ReapOrphans(ctx context.Context, ttl time.Duration) ([]string, error)
}
