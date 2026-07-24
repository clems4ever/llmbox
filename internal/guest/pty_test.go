package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

// readUntil reads from conn until want appears in the accumulated output or the
// deadline passes, returning everything read. It drives the PTY tests, whose shell
// output arrives in unpredictable chunks.
func readUntil(t *testing.T, conn net.Conn, want string, deadline time.Time) string {
	t.Helper()
	var buf bytes.Buffer
	tmp := make([]byte, 4096)
	for {
		_ = conn.SetReadDeadline(deadline)
		n, err := conn.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
			if strings.Contains(buf.String(), want) {
				return buf.String()
			}
		}
		if err != nil {
			t.Fatalf("waiting for %q: read error %v (got %q)", want, err, buf.String())
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q (got %q)", want, buf.String())
		}
	}
}

// TestClientOpenPTY runs a shell inside the box through OpenPTY and checks a typed
// command's output round-trips over the tunnel.
func TestClientOpenPTY(t *testing.T) {
	_, c := startGuest(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := c.Init(ctx, InitReq{Env: boxEnv(t)}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	conn, err := c.OpenPTY(ctx, []string{"/bin/sh"}, 80, 24)
	if err != nil {
		t.Fatalf("OpenPTY: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write(EncodePTYInput([]byte("echo PTY_MARKER_OK\n"))); err != nil {
		t.Fatalf("write input: %v", err)
	}
	out := readUntil(t, conn, "PTY_MARKER_OK", time.Now().Add(10*time.Second))
	if !strings.Contains(out, "PTY_MARKER_OK") {
		t.Fatalf("output %q missing marker", out)
	}
}

// TestGuestPTYEchoesInput opens a PTY with the default (login-shell) command and
// checks input reaches the shell and its output streams back, exercising ptyShell.
func TestGuestPTYEchoesInput(t *testing.T) {
	_, c := startGuest(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// A minimal env (no SHELL) so ptyShell falls through to bash or /bin/sh.
	if _, err := c.Init(ctx, InitReq{Env: boxEnv(t)}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	conn, err := c.OpenPTY(ctx, nil, 80, 24)
	if err != nil {
		t.Fatalf("OpenPTY: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write(EncodePTYInput([]byte("echo DEFAULT_SHELL_OK\n"))); err != nil {
		t.Fatalf("write input: %v", err)
	}
	readUntil(t, conn, "DEFAULT_SHELL_OK", time.Now().Add(10*time.Second))
}

// TestGuestPTYResize applies a resize control frame and checks the shell observes
// the new geometry via `stty size` (which prints "rows cols").
func TestGuestPTYResize(t *testing.T) {
	_, c := startGuest(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := c.Init(ctx, InitReq{Env: boxEnv(t)}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	conn, err := c.OpenPTY(ctx, []string{"/bin/sh"}, 80, 24)
	if err != nil {
		t.Fatalf("OpenPTY: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write(EncodePTYResize(132, 50)); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	if _, err := conn.Write(EncodePTYInput([]byte("stty size\n"))); err != nil {
		t.Fatalf("write input: %v", err)
	}
	// stty size prints "<rows> <cols>", i.e. "50 132" after the resize.
	readUntil(t, conn, "50 132", time.Now().Add(10*time.Second))
}

// TestGuestPTYRejectsBadRequest checks the guest replies with an error frame (and
// does not splice) when the PTY request payload is malformed.
func TestGuestPTYRejectsBadRequest(t *testing.T) {
	_, c := startGuest(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, err := c.Dial(ctx)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	// A verbPTY request whose data is not a valid ptyReq.
	if err := writeFrame(conn, req{Verb: verbPTY, Data: json.RawMessage(`"not-an-object"`)}); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	var r resp
	if err := readFrame(conn, &r); err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if r.Err == "" {
		t.Fatalf("expected an error response for a malformed pty request, got %+v", r)
	}
}
