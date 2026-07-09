package guest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"

	"github.com/mdlayher/vsock"
)

// RunBoxAPIBridge accepts connections on a guest Unix socket and splices each
// to a host-side connection opened by dial — the guest half of the microVM
// box-port API path. The in-box Claude process talks HTTP to the Unix socket
// (the same contract as the Docker backend, where the spoke serves the socket
// directly through the bind mount); here the guest forwards the raw bytes to
// the host over vsock, where the spoke's per-VM listener serves the same API.
// The bridge is a dumb pipe: it neither parses nor authenticates anything —
// identity is assigned host-side by which VM's vsock the bytes arrive on. It
// serves until ctx is cancelled or the listener fails.
//
// @arg ctx Context whose cancellation stops the accept loop and removes the socket.
// @arg socketPath The in-guest Unix socket to accept box-port API connections on.
// @arg dial Opens the host-side connection one accepted connection is spliced to.
// @arg log Logger for per-connection dial failures; nil uses slog.Default.
// @error error if the socket cannot be created or the accept loop fails for a reason other than ctx cancellation.
//
// @testcase TestBoxAPIBridgeSplices splices bytes both ways between a client and the dialled host.
// @testcase TestBoxAPIBridgeDialError closes the client connection when the host dial fails.
func RunBoxAPIBridge(ctx context.Context, socketPath string, dial func(ctx context.Context) (net.Conn, error), log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return fmt.Errorf("creating box API socket dir: %w", err)
	}
	// Remove a stale socket left by a previous run so bind succeeds on restart.
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing stale box API socket: %w", err)
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listening on box API socket: %w", err)
	}
	defer ln.Close()

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accepting box API connection: %w", err)
		}
		go func(conn net.Conn) {
			host, err := dial(ctx)
			if err != nil {
				log.Warn("box API bridge dial failed", "err", err)
				_ = conn.Close()
				return
			}
			splice(conn, host)
		}(conn)
	}
}

// DialHostVsock returns a dialer that connects to the host (CID 2) on the given
// AF_VSOCK port — the guest side of Firecracker's guest-initiated vsock, which
// the hypervisor forwards to the host Unix socket the spoke pre-listens on.
//
// @arg port The host vsock port to connect to.
// @return func A dialer usable with RunBoxAPIBridge.
//
// @testcase TestDialHostVsockDialer checks the dialer fails cleanly (or connects) without a hypervisor; the real vsock path is proven by the live TestBoxAPIOverVsock.
func DialHostVsock(port uint32) func(ctx context.Context) (net.Conn, error) {
	return func(context.Context) (net.Conn, error) {
		return vsock.Dial(vsock.Host, port, nil)
	}
}
