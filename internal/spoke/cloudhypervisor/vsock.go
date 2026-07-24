package cloudhypervisor

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// guestVsockPort is the guest AF_VSOCK port the in-box guest listens on. It is
// fixed (baked into the guest's invocation inside the rootfs) and shared by every
// box: Cloud Hypervisor multiplexes each box's guest onto this same guest port over
// that box's own hypervisor UDS, so the port need not be unique across boxes. It
// matches the Firecracker backend's value so the same guest rootfs boots on either.
const guestVsockPort = 5000

// boxAPIVsockPort is the host-side vsock port the guest bridges its in-guest
// box-port API socket to. It is reserved for parity with the Firecracker backend's
// guest-initiated box-port channel; phase 1 does not yet serve it.
const boxAPIVsockPort = 5001

// dialVsock opens a connection to a guest process listening on AF_VSOCK port through
// Cloud Hypervisor's host-side vsock Unix socket, performing the hybrid-vsock text
// CONNECT handshake. Cloud Hypervisor listens on udsPath for host->guest
// connections; the caller writes "CONNECT <port>\n" and the VMM replies
// "OK <hostport>\n" when a guest process is listening on that port, after which the
// same connection is a byte pipe to the guest listener. This is the same protocol
// Firecracker implements, so the control channel is backend-neutral.
//
// The returned net.Conn reads through a bufio.Reader that consumed the OK line,
// because that reader may already hold guest bytes sent right after the handshake;
// reading the raw connection instead would drop them.
//
// @arg ctx Context whose deadline/cancellation bounds the dial and handshake.
// @arg udsPath The Cloud Hypervisor host vsock Unix-socket path for the box.
// @arg port The guest AF_VSOCK port to connect to.
// @return net.Conn A byte pipe to the guest listener once the handshake succeeds.
// @error error if the socket cannot be dialled, the handshake cannot be written/read, or the VMM rejects it (no guest listener yet).
//
// @testcase TestDialVsockHandshake connects through a fake CH socket that speaks the CONNECT/OK protocol.
// @testcase TestDialVsockRejected errors when the fake socket replies with anything but OK.
func dialVsock(ctx context.Context, udsPath string, port uint32) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, fmt.Errorf("dialling vsock uds %s: %w", udsPath, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(deadline)
	}
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("writing vsock CONNECT: %w", err)
	}
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("reading vsock CONNECT reply: %w", err)
	}
	if !strings.HasPrefix(line, "OK ") {
		_ = c.Close()
		return nil, fmt.Errorf("vsock connect to port %d rejected: %q", port, strings.TrimSpace(line))
	}
	// Clear the handshake deadline; the caller owns the connection's timeouts now.
	_ = c.SetDeadline(time.Time{})
	return &bufConn{Conn: c, r: br}, nil
}

// bufConn is a net.Conn whose Read drains a bufio.Reader first, so bytes the peer
// sent immediately after the vsock handshake (and buffered while reading the OK
// line) are not lost.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

// Read reads through the buffered reader.
//
// @arg p The buffer to read into.
// @return int The number of bytes read.
// @error error from the underlying buffered reader.
//
// @testcase TestDialVsockHandshake reads guest bytes buffered past the OK line.
func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }
