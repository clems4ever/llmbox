package cluster

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/sandbox"
)

// errDial is a canned dial failure.
var errDial = errors.New("dial refused")

// echoListener starts a TCP server that echoes back whatever it receives, standing
// in for a box's port. It returns the address and stops on test cleanup.
func echoListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { _, _ = io.Copy(c, c); _ = c.Close() }(c)
		}
	}()
	return ln.Addr().String()
}

// TestStreamTunnelRoundTrip checks the streaming tunnel end to end: a hub-side
// remoteSpoke opens a tunnel over the in-memory pipe, the spoke dials its (fake)
// box port, and bytes round-trip both ways through the dialed connection.
func TestStreamTunnelRoundTrip(t *testing.T) {
	rs := startSpoke(t, &fakeManager{dialTarget: echoListener(t)})

	conn, err := rs.DialBox(context.Background(), "web-box", 8000)
	if err != nil {
		t.Fatalf("DialBox: %v", err)
	}
	defer conn.Close()

	msg := []byte("hello tunnel, streamed not buffered")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(msg) {
		t.Errorf("echo = %q, want %q", got, msg)
	}
}

// TestStreamTunnelBoxEOF checks that when the box closes its side, the hub-side
// tunnel conn reads the remaining bytes and then observes io.EOF.
func TestStreamTunnelBoxEOF(t *testing.T) {
	// A one-shot server that writes a greeting then closes.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = c.Write([]byte("bye"))
		_ = c.Close()
	}()

	rs := startSpoke(t, &fakeManager{dialTarget: ln.Addr().String()})
	conn, err := rs.DialBox(context.Background(), "web-box", 8000)
	if err != nil {
		t.Fatalf("DialBox: %v", err)
	}
	defer conn.Close()

	data, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "bye" {
		t.Errorf("read %q, want \"bye\"", data)
	}
}

// TestStreamOpenUnsupportedSpoke checks that opening a tunnel to a spoke whose
// manager cannot dial boxes fails: the open is optimistic, so the failure surfaces
// on the first read of the returned conn (a stream-close carrying the error).
func TestStreamOpenUnsupportedSpoke(t *testing.T) {
	rs := startSpoke(t, bareManager{})
	conn, err := rs.DialBox(context.Background(), "b", 80)
	if err != nil {
		t.Fatalf("DialBox open should be optimistic: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Read(make([]byte, 8)); err == nil {
		t.Fatal("expected a read error for a spoke that cannot dial boxes")
	}
}

// TestStreamOpenDialError checks that a box dial failure on the spoke surfaces on
// the first read of the tunnel conn.
func TestStreamOpenDialError(t *testing.T) {
	rs := startSpoke(t, &fakeManager{dialErr: errDial})
	conn, err := rs.DialBox(context.Background(), "b", 80)
	if err != nil {
		t.Fatalf("DialBox: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Read(make([]byte, 8)); err == nil {
		t.Fatal("expected a read error when the box dial fails")
	}
}

// TestClientStreamRoundTrip checks the hub-side clientStream net.Conn on its own:
// Write emits a data frame on the transport, pushed bytes are read back, and a
// clean closeRemote surfaces io.EOF after draining.
func TestClientStreamRoundTrip(t *testing.T) {
	a, b := newPipe()
	cs := newClientStream(1, a, func(uint64) {})
	if cs.LocalAddr().Network() != "llmbox-tunnel" || cs.RemoteAddr().String() != "box" {
		t.Errorf("unexpected tunnel addr: %s/%s", cs.LocalAddr().Network(), cs.RemoteAddr())
	}

	// Write emits a stream_data frame carrying the bytes to the peer.
	if _, err := cs.Write([]byte("ping")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	f, err := b.Recv(context.Background())
	if err != nil {
		t.Fatalf("Recv write frame: %v", err)
	}
	if f.Type != frameStreamData || f.ID != 1 || string(f.Data) != "ping" {
		t.Errorf("write frame = %+v, want stream_data id=1 data=ping", f)
	}

	// Inbound bytes are read back; a clean remote close ends the read with io.EOF.
	cs.push([]byte("abc"))
	cs.closeRemote(nil)
	got, err := io.ReadAll(cs)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "abc" {
		t.Errorf("read %q, want \"abc\"", got)
	}
}

// TestClientStreamCloseAfterRemote checks that a local Close after the spoke has
// already ended the stream still runs the local-close actions: it unregisters the
// stream, notifies the spoke with a stream_close frame, and makes a subsequent
// Write fail with net.ErrClosed (not a silently dropped send). This guards the
// separation of the dataCh close from the local-close actions.
func TestClientStreamCloseAfterRemote(t *testing.T) {
	a, b := newPipe()
	var unregistered bool
	cs := newClientStream(2, a, func(uint64) { unregistered = true })

	cs.closeRemote(nil) // the spoke ends the stream first
	if err := cs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !unregistered {
		t.Error("Close after a remote close did not unregister the stream")
	}
	f, err := b.Recv(context.Background())
	if err != nil {
		t.Fatalf("Recv close frame: %v", err)
	}
	if f.Type != frameStreamClose || f.ID != 2 {
		t.Errorf("close frame = %+v, want stream_close id=2", f)
	}
	if _, err := cs.Write([]byte("x")); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Write after Close = %v, want net.ErrClosed", err)
	}
}

// TestClientStreamDeadline checks that an armed read deadline unblocks a stalled
// Read with a timeout error.
func TestClientStreamDeadline(t *testing.T) {
	a, _ := newPipe()
	cs := newClientStream(2, a, func(uint64) {})
	if err := cs.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	_, err := cs.Read(make([]byte, 4))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read err = %v, want deadline exceeded", err)
	}
}

// bareManager implements BoxManager with no-op verbs and deliberately does NOT
// implement BoxDialer, so a proxy tunnel is refused.
type bareManager struct{}

// Create is a no-op stub.
func (bareManager) Create(context.Context, sandbox.CreateOptions) (string, string, error) {
	return "", "", nil
}

// SubmitCode is a no-op stub.
func (bareManager) SubmitCode(context.Context, string, string) (string, error) { return "", nil }

// List is a no-op stub.
func (bareManager) List(context.Context) ([]sandbox.Box, error) { return nil, nil }

// Destroy is a no-op stub.
func (bareManager) Destroy(context.Context, string) error { return nil }

// Logs is a no-op stub.
func (bareManager) Logs(context.Context, string, int) (string, error) { return "", nil }

// Exec is a no-op stub.
func (bareManager) Exec(context.Context, string, []string) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}

// ReapOrphans is a no-op stub.
func (bareManager) ReapOrphans(context.Context, time.Duration) ([]string, error) { return nil, nil }
