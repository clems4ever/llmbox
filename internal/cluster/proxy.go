package cluster

import (
	"context"
	"net"
)

// BoxDialer is the box-reachability capability the spoke-side stream tunnel needs
// from its local box manager: open a connection to a port inside a box. The
// in-process *box.Manager implements it. It is kept here (not on BoxManager) so
// the box-verb RPC allowlist is unchanged and only a spoke that can dial boxes
// services proxy tunnels. The dial resolves through the box layer's managed-only
// check (see box.Manager.DialBox), so a tunnel can only ever reach a port inside a
// box the spoke created — never an arbitrary host address.
type BoxDialer interface {
	DialBox(ctx context.Context, idOrName string, port int) (net.Conn, error)
}
