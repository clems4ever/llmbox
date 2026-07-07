package cluster

import (
	"context"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// maxStreamChunk bounds the payload of one frameStreamData. Stream writes are
// split into chunks this size so a single frame stays small enough to interleave
// with other streams and verb responses over the shared connection (rather than
// monopolizing the socket with one huge frame).
const maxStreamChunk = 32 << 10 // 32 KiB

// streamInboundBuffer is how many inbound data frames a stream buffers before the
// reader must catch up. It bounds memory per stream; when full the shared read
// loop backpressures (blocks), which is acceptable for the low-concurrency proxy.
const streamInboundBuffer = 64

// streamAddr is the placeholder net.Addr a tunnel net.Conn reports. The tunnel
// reaches a box's port through the spoke, so there is no meaningful host address.
type streamAddr struct{}

// Network names the tunnel's pseudo-network.
//
// @return string The constant network name.
//
// @testcase TestClientStreamRoundTrip dials a tunnel whose addr reports this network.
func (streamAddr) Network() string { return "llmbox-tunnel" }

// String is the tunnel's placeholder address.
//
// @return string The constant address label.
//
// @testcase TestClientStreamRoundTrip reads the tunnel addr's string form.
func (streamAddr) String() string { return "box" }

// connDeadline implements a net.Conn deadline as a channel that closes when the
// deadline passes, mirroring the standard library's pipe deadline. A Read/Write
// selects on wait() to abort with a timeout. The zero value is unusable; build
// one with newConnDeadline.
type connDeadline struct {
	mu     sync.Mutex
	timer  *time.Timer
	cancel chan struct{} // closed when the deadline is exceeded
}

// newConnDeadline builds a connDeadline with no deadline set.
//
// @return *connDeadline A deadline with an open (never-fired) cancel channel.
//
// @testcase TestClientStreamDeadline sets a read deadline via this helper.
func newConnDeadline() *connDeadline { return &connDeadline{cancel: make(chan struct{})} }

// set arms the deadline for time t: a zero t clears it, a future t fires the
// cancel channel then, and a past t fires it immediately.
//
// @arg t The absolute deadline, or the zero time to clear it.
//
// @testcase TestClientStreamDeadline arms and observes a read deadline.
func (d *connDeadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	closed := isClosedChan(d.cancel)
	// A fresh channel is needed whenever the previous one has already fired, so a
	// new deadline can be waited on again.
	if closed {
		d.cancel = make(chan struct{})
	}
	if t.IsZero() {
		return
	}
	if dur := time.Until(t); dur > 0 {
		cancel := d.cancel
		d.timer = time.AfterFunc(dur, func() { close(cancel) })
		return
	}
	close(d.cancel)
}

// wait returns the channel that closes when the deadline is exceeded.
//
// @return chan struct{} The current cancel channel.
//
// @testcase TestClientStreamDeadline selects on the returned channel.
func (d *connDeadline) wait() chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cancel
}

// isClosedChan reports whether c has been closed without consuming a value.
//
// @arg c The channel to test.
// @return bool True when c is closed.
//
// @testcase TestClientStreamDeadline relies on re-arming a fired deadline.
func isClosedChan(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// clientStream is the hub-side net.Conn end of a tunnel to a box's port on a
// remote spoke. Writes are chunked into frameStreamData frames sent over the
// transport; reads drain bytes the read loop pushes from the spoke's data frames;
// Close sends a frameStreamClose. It satisfies net.Conn so the existing streaming
// reverse-proxy path (an http.Transport whose DialContext returns this) reaches a
// remote box exactly as it reached a local one.
type clientStream struct {
	id      uint64
	tr      transport
	onClose func(id uint64) // unregisters the stream from the owning remoteSpoke

	dataCh chan []byte   // inbound bytes from the spoke, closed on remote EOF
	closed chan struct{} // closed by a local Close()

	readMu sync.Mutex
	buf    []byte // leftover bytes from a frame not fully consumed by one Read

	remoteMu  sync.Mutex
	remoteErr error // set on remote close/disconnect; nil means clean EOF

	closeOnce sync.Once
	rDeadline *connDeadline
	wDeadline *connDeadline
}

// newClientStream builds a hub-side tunnel conn for stream id. onClose removes the
// stream from the remoteSpoke registry when the local end closes.
//
// @arg id The stream/correlation ID.
// @arg tr The transport data and close frames are sent on.
// @arg onClose Callback to unregister the stream when it closes locally.
// @return *clientStream A ready net.Conn end of the tunnel.
//
// @testcase TestClientStreamRoundTrip round-trips bytes through a client stream.
func newClientStream(id uint64, tr transport, onClose func(id uint64)) *clientStream {
	return &clientStream{
		id:        id,
		tr:        tr,
		onClose:   onClose,
		dataCh:    make(chan []byte, streamInboundBuffer),
		closed:    make(chan struct{}),
		rDeadline: newConnDeadline(),
		wDeadline: newConnDeadline(),
	}
}

// push delivers one inbound data frame's bytes to the stream's reader. It is
// called only from the single read loop, in order, before any closeRemote.
//
// @arg b The bytes received in a frameStreamData.
//
// @testcase TestClientStreamRoundTrip delivers spoke bytes to the reader via push.
func (c *clientStream) push(b []byte) {
	select {
	case c.dataCh <- b:
	case <-c.closed:
	}
}

// closeRemote signals that the spoke ended the stream (clean EOF when err is nil,
// otherwise the failure). It closes the data channel so a pending Read drains the
// buffered bytes and then observes the end. Called only from the read loop.
//
// @arg err The remote cause, or nil for a clean EOF.
//
// @testcase TestClientStreamRoundTrip ends a stream with a clean EOF via closeRemote.
func (c *clientStream) closeRemote(err error) {
	c.remoteMu.Lock()
	c.remoteErr = err
	c.remoteMu.Unlock()
	c.closeOnce.Do(func() { close(c.dataCh) })
}

// Read returns tunnel bytes, blocking until data arrives, the stream ends, the
// deadline passes, or the conn is closed.
//
// @arg p The buffer to read into.
// @return int The number of bytes read.
// @error error io.EOF on a clean end, the remote failure, os.ErrDeadlineExceeded, or net.ErrClosed.
//
// @testcase TestClientStreamRoundTrip reads bytes the spoke sent.
// @testcase TestClientStreamDeadline returns a timeout when the read deadline passes.
func (c *clientStream) Read(p []byte) (int, error) {
	c.readMu.Lock()
	if len(c.buf) > 0 {
		n := copy(p, c.buf)
		c.buf = c.buf[n:]
		c.readMu.Unlock()
		return n, nil
	}
	c.readMu.Unlock()

	select {
	case <-c.closed:
		return 0, net.ErrClosed
	case <-c.rDeadline.wait():
		return 0, os.ErrDeadlineExceeded
	case b, ok := <-c.dataCh:
		if !ok {
			return 0, c.readEndErr()
		}
		c.readMu.Lock()
		n := copy(p, b)
		if n < len(b) {
			c.buf = b[n:]
		}
		c.readMu.Unlock()
		return n, nil
	}
}

// readEndErr is the error a Read returns once the stream has ended: io.EOF for a
// clean close, or the remote failure.
//
// @error error io.EOF on a clean end, or the recorded remote failure.
//
// @testcase TestClientStreamRoundTrip surfaces io.EOF after the spoke closes.
func (c *clientStream) readEndErr() error {
	c.remoteMu.Lock()
	defer c.remoteMu.Unlock()
	if c.remoteErr != nil {
		return c.remoteErr
	}
	return io.EOF
}

// Write sends p to the box in chunked data frames, blocking on transport
// backpressure until all bytes are sent, the deadline passes, or the conn closes.
//
// @arg p The bytes to send.
// @return int The number of bytes written.
// @error error net.ErrClosed, os.ErrDeadlineExceeded, or a transport send failure.
//
// @testcase TestClientStreamRoundTrip writes bytes the spoke delivers to the box.
func (c *clientStream) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		select {
		case <-c.closed:
			return total, net.ErrClosed
		case <-c.wDeadline.wait():
			return total, os.ErrDeadlineExceeded
		default:
		}
		n := len(p)
		if n > maxStreamChunk {
			n = maxStreamChunk
		}
		if err := c.tr.Send(context.Background(), frame{Type: frameStreamData, ID: c.id, Data: p[:n]}); err != nil {
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}

// Close tears down the local end of the tunnel and tells the spoke to close the
// box connection. Closing more than once is harmless.
//
// @error error Always nil (the best-effort close frame's failure is ignored).
//
// @testcase TestClientStreamRoundTrip closes the tunnel when done.
func (c *clientStream) Close() error {
	c.closeOnce.Do(func() {
		close(c.dataCh)
		close(c.closed)
		_ = c.tr.Send(context.Background(), frame{Type: frameStreamClose, ID: c.id})
		if c.onClose != nil {
			c.onClose(c.id)
		}
	})
	return nil
}

// LocalAddr reports the tunnel's placeholder local address.
//
// @return net.Addr The placeholder tunnel address.
//
// @testcase TestClientStreamRoundTrip reads the local addr.
func (c *clientStream) LocalAddr() net.Addr { return streamAddr{} }

// RemoteAddr reports the tunnel's placeholder remote address.
//
// @return net.Addr The placeholder tunnel address.
//
// @testcase TestClientStreamRoundTrip reads the remote addr.
func (c *clientStream) RemoteAddr() net.Addr { return streamAddr{} }

// SetDeadline sets both the read and write deadlines.
//
// @arg t The absolute deadline, or the zero time to clear it.
// @error error Always nil.
//
// @testcase TestClientStreamDeadline sets a deadline through this method.
func (c *clientStream) SetDeadline(t time.Time) error {
	c.rDeadline.set(t)
	c.wDeadline.set(t)
	return nil
}

// SetReadDeadline sets the read deadline.
//
// @arg t The absolute deadline, or the zero time to clear it.
// @error error Always nil.
//
// @testcase TestClientStreamDeadline arms a read deadline.
func (c *clientStream) SetReadDeadline(t time.Time) error {
	c.rDeadline.set(t)
	return nil
}

// SetWriteDeadline sets the write deadline.
//
// @arg t The absolute deadline, or the zero time to clear it.
// @error error Always nil.
//
// @testcase TestClientStreamDeadline arms a write deadline.
func (c *clientStream) SetWriteDeadline(t time.Time) error {
	c.wDeadline.set(t)
	return nil
}

// serverStream is the spoke-side end of a tunnel: it owns the dialed box net.Conn
// and pumps bytes both ways between it and the stream frames. box→hub is pumped by
// a goroutine reading the conn; hub→box is fed by data frames the serve loop
// hands to writeInbound.
type serverStream struct {
	id   uint64
	tr   transport
	conn net.Conn

	teardownOnce sync.Once
	onTeardown   func(id uint64) // removes the stream from the serve loop's registry
}

// newServerStream builds a spoke-side stream over a freshly dialed box connection.
// The caller must register it before calling start, so an immediate box EOF cannot
// tear it down before it is in the registry.
//
// @arg id The stream/correlation ID.
// @arg tr The transport data/close frames are sent on.
// @arg conn The dialed box connection to pump.
// @arg onTeardown Callback to unregister the stream on teardown.
// @return *serverStream The spoke-side stream, not yet pumping.
//
// @testcase TestStreamTunnelRoundTrip pumps box bytes back to the hub.
func newServerStream(id uint64, tr transport, conn net.Conn, onTeardown func(id uint64)) *serverStream {
	return &serverStream{id: id, tr: tr, conn: conn, onTeardown: onTeardown}
}

// start launches the box→hub reader. hub→box bytes are delivered via writeInbound.
//
// @testcase TestStreamTunnelRoundTrip starts pumping once the stream is registered.
func (s *serverStream) start() { go s.pumpBoxToHub() }

// pumpBoxToHub copies box output into frameStreamData frames until the box closes
// or a send fails, then tells the hub the stream ended and tears down.
//
// @testcase TestStreamTunnelRoundTrip delivers box bytes to the hub over data frames.
func (s *serverStream) pumpBoxToHub() {
	buf := make([]byte, maxStreamChunk)
	for {
		n, err := s.conn.Read(buf)
		if n > 0 {
			// Copy: the frame's Data is retained until it is JSON-encoded by Send.
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if sendErr := s.tr.Send(context.Background(), frame{Type: frameStreamData, ID: s.id, Data: chunk}); sendErr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	s.teardown(true)
}

// writeInbound writes hub bytes to the box. A write failure tears the stream down.
//
// @arg b The bytes received in a frameStreamData from the hub.
//
// @testcase TestStreamTunnelRoundTrip forwards hub bytes to the box.
func (s *serverStream) writeInbound(b []byte) {
	if _, err := s.conn.Write(b); err != nil {
		s.teardown(true)
	}
}

// teardown closes the box connection and unregisters the stream, optionally
// notifying the hub with a close frame. It runs at most once.
//
// @arg notifyHub Whether to send a frameStreamClose to the hub.
//
// @testcase TestStreamTunnelRoundTrip tears the stream down when the box closes.
func (s *serverStream) teardown(notifyHub bool) {
	s.teardownOnce.Do(func() {
		_ = s.conn.Close()
		if notifyHub {
			_ = s.tr.Send(context.Background(), frame{Type: frameStreamClose, ID: s.id})
		}
		if s.onTeardown != nil {
			s.onTeardown(s.id)
		}
	})
}

// spokeStreams is the spoke serve loop's registry of live tunnels, keyed by stream
// ID. It is guarded because the serve loop adds/reads entries while each tunnel's
// pump goroutine removes its own on teardown.
type spokeStreams struct {
	mu sync.Mutex
	m  map[uint64]*serverStream
}

// newSpokeStreams builds an empty stream registry.
//
// @return *spokeStreams An empty registry.
//
// @testcase TestStreamTunnelRoundTrip tracks a spoke's live tunnels via this registry.
func newSpokeStreams() *spokeStreams { return &spokeStreams{m: map[uint64]*serverStream{}} }

// add registers a stream under its ID.
//
// @arg id The stream ID.
// @arg s The stream to track.
//
// @testcase TestStreamTunnelRoundTrip registers a freshly opened tunnel.
func (r *spokeStreams) add(id uint64, s *serverStream) {
	r.mu.Lock()
	r.m[id] = s
	r.mu.Unlock()
}

// get returns the stream for an ID, or nil.
//
// @arg id The stream ID.
// @return *serverStream The tracked stream, or nil.
//
// @testcase TestStreamTunnelRoundTrip routes data frames to a tracked tunnel.
func (r *spokeStreams) get(id uint64) *serverStream {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[id]
}

// del removes a stream by ID; removing a missing one is a no-op.
//
// @arg id The stream ID to remove.
//
// @testcase TestStreamTunnelRoundTrip removes a torn-down tunnel.
func (r *spokeStreams) del(id uint64) {
	r.mu.Lock()
	delete(r.m, id)
	r.mu.Unlock()
}

// closeAll tears down every live tunnel, used when the serve loop exits (the hub
// disconnected) so no box connection is left dangling.
//
// @testcase TestStreamTunnelRoundTrip closes remaining tunnels when serve returns.
func (r *spokeStreams) closeAll() {
	r.mu.Lock()
	all := r.m
	r.m = map[uint64]*serverStream{}
	r.mu.Unlock()
	for _, s := range all {
		s.teardown(false)
	}
}
