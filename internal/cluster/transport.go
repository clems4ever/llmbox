package cluster

import (
	"context"
	"sync"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// maxFrameBytes bounds a single decoded frame. A create request injects files
// into the box, and those can include CLI binaries: a multi-megabyte executable
// becomes ~33% larger again once base64-encoded into JSON, so several tens of
// megabytes in one frame is normal. This is set generously above that so such
// payloads are not truncated; it exists only to stop a peer from forcing
// unbounded allocation.
const maxFrameBytes = 64 << 20 // 64 MiB

// transport is one full-duplex framed connection between a hub and a spoke. It
// abstracts the WebSocket so the hub-side remoteSpoke and the spoke-side
// dispatch loop can be tested over an in-memory pipe. Send must be safe for
// concurrent use (the hub fans out many verb calls over one connection); Recv is
// only ever called from a single read loop.
type transport interface {
	Send(ctx context.Context, f frame) error
	Recv(ctx context.Context) (frame, error)
	Close() error
}

// wsTransport is a transport backed by a coder/websocket connection.
type wsTransport struct {
	conn    *websocket.Conn
	writeMu sync.Mutex // serializes writes: a WebSocket allows only one concurrent writer
}

// newWSTransport wraps a websocket connection as a transport, raising the read
// limit so large verb payloads (exec output, injected files) are not truncated.
//
// @arg conn The established websocket connection.
// @return *wsTransport A transport reading and writing JSON frames over conn.
//
// @testcase TestWSTransportRoundTrip sends and receives a frame over a loopback websocket.
// @testcase TestWSTransportLargeFrame round-trips a frame above the old 8 MiB read limit.
func newWSTransport(conn *websocket.Conn) *wsTransport {
	conn.SetReadLimit(maxFrameBytes)
	return &wsTransport{conn: conn}
}

// Send writes one frame as a JSON text message, serialized against other writers.
//
// @arg ctx Context bounding the write.
// @arg f The frame to send.
// @error error if the websocket write fails.
//
// @testcase TestWSTransportRoundTrip sends a frame the peer reads back.
// @testcase TestWSTransportLargeFrame sends a frame larger than the old read limit.
func (t *wsTransport) Send(ctx context.Context, f frame) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return wsjson.Write(ctx, t.conn, f)
}

// Recv reads the next frame. It must be called from a single goroutine.
//
// @arg ctx Context bounding the read.
// @return frame The decoded frame.
// @error error if the websocket read or JSON decode fails (including a clean close).
//
// @testcase TestWSTransportRoundTrip receives the frame the peer sent.
// @testcase TestWSTransportLargeFrame receives a frame larger than the old read limit.
func (t *wsTransport) Recv(ctx context.Context) (frame, error) {
	var f frame
	if err := wsjson.Read(ctx, t.conn, &f); err != nil {
		return frame{}, err
	}
	return f, nil
}

// Close closes the underlying websocket with a normal-closure status.
//
// @error error if closing the websocket fails.
//
// @testcase TestWSTransportRoundTrip closes both ends when done.
func (t *wsTransport) Close() error {
	return t.conn.Close(websocket.StatusNormalClosure, "")
}
