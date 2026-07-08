package firecracker

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// agentVsockPort is the guest AF_VSOCK port the in-box agent listens on. It is
// fixed (the host always knows where to reach the agent) and baked into the
// microVM's agent invocation (--vsock-port). Firecracker multiplexes every box's
// agent onto this same guest port over that box's own hypervisor UDS, so the
// port need not be unique across boxes.
const agentVsockPort = 5000

// boxAPIVsockPort is the HOST-side vsock port the guest agent bridges the
// in-guest /run/llmbox/boxapi.sock to (--boxapi-port). This is the
// guest-initiated direction of Firecracker's vsock: when a guest process
// connects to CID 2 on this port, Firecracker dials the host Unix socket at
// "<vsock_uds_path>_<port>" — so the provisioner pre-listens there (see
// boxAPISocketPath), serving the box-port API bound to that one VM's identity.
// Unlike the host→guest direction there is no CONNECT/OK handshake: the
// accepted connection is immediately a raw byte pipe carrying the box's HTTP.
const boxAPIVsockPort = 5001

// boxAPISocketPath is the host Unix-socket path Firecracker dials when the
// guest connects to CID 2 on boxAPIVsockPort, following Firecracker's
// "<vsock_uds_path>_<port>" convention for guest-initiated connections.
//
// @arg vsockUDS The box's Firecracker vsock Unix-socket path.
// @return string The host listener path for the box's guest-initiated box-port API connections.
//
// @testcase TestProvisionStartsBoxAPIListener serves the box-port API at this path.
func boxAPISocketPath(vsockUDS string) string {
	return fmt.Sprintf("%s_%d", vsockUDS, boxAPIVsockPort)
}

// dialVsock opens a connection to a guest process listening on AF_VSOCK port
// through Firecracker's host-side vsock Unix socket, performing Firecracker's
// text CONNECT handshake. Firecracker listens on udsPath for host→guest
// connections; the caller writes "CONNECT <port>\n" and Firecracker replies
// "OK <hostport>\n" when a guest process is listening on that port, after which
// the same connection is a byte pipe to the guest listener.
//
// The returned net.Conn reads through a bufio.Reader that consumed the OK line,
// because that reader may already hold guest bytes sent right after the handshake;
// reading the raw connection instead would drop them.
//
// @arg ctx Context whose deadline/cancellation bounds the dial and handshake.
// @arg udsPath The Firecracker host vsock Unix-socket path for the box.
// @arg port The guest AF_VSOCK port to connect to.
// @return net.Conn A byte pipe to the guest listener once the handshake succeeds.
// @error error if the socket cannot be dialled, the handshake cannot be written/read, or Firecracker rejects it (no guest listener yet).
//
// @testcase TestDialVsockHandshake connects through a fake Firecracker socket that speaks the CONNECT/OK protocol.
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
