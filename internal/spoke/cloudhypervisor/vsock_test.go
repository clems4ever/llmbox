package cloudhypervisor

import (
	"bufio"
	"context"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// serveFakeVsock listens on a Unix socket and runs handler for one accepted
// connection, emulating a VMM's host-side vsock endpoint. It returns the socket path.
//
// @arg t The test the listener is scoped to.
// @arg handler Handles the single accepted connection (reads CONNECT, writes reply).
// @return string The vsock Unix-socket path to dial.
//
// @testcase TestDialVsockHandshake serves a CONNECT/OK responder through this.
func serveFakeVsock(t *testing.T, handler func(net.Conn)) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "vsock.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		handler(c)
	}()
	return sock
}

// TestDialVsockHandshake connects through a fake VMM socket that speaks the CONNECT/OK
// protocol and delivers guest bytes buffered right after the OK line, checking they
// are not dropped.
func TestDialVsockHandshake(t *testing.T) {
	sock := serveFakeVsock(t, func(c net.Conn) {
		br := bufio.NewReader(c)
		line, _ := br.ReadString('\n')
		if line != "CONNECT 5000\n" {
			t.Errorf("server saw %q, want CONNECT 5000", line)
		}
		// Reply OK and immediately push guest bytes, so the client's buffered reader
		// must not lose them.
		_, _ = c.Write([]byte("OK 12345\nhello-from-guest"))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := dialVsock(ctx, sock, guestVsockPort)
	if err != nil {
		t.Fatalf("dialVsock: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, len("hello-from-guest"))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("reading guest bytes: %v", err)
	}
	if string(buf) != "hello-from-guest" {
		t.Errorf("guest bytes = %q, want hello-from-guest", buf)
	}
}

// TestDialVsockRejected errors when the VMM replies with anything but OK (no guest
// listening on the port yet).
func TestDialVsockRejected(t *testing.T) {
	sock := serveFakeVsock(t, func(c net.Conn) {
		br := bufio.NewReader(c)
		_, _ = br.ReadString('\n')
		_, _ = c.Write([]byte("FAILED no listener\n"))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := dialVsock(ctx, sock, guestVsockPort); err == nil {
		t.Fatal("dialVsock should error when the VMM rejects the connect")
	}
}

// TestDialVsockNoSocket errors cleanly when the vsock UDS does not exist.
func TestDialVsockNoSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := dialVsock(ctx, filepath.Join(t.TempDir(), "absent.sock"), guestVsockPort); err == nil {
		t.Fatal("dialVsock should error when the socket is absent")
	}
}
