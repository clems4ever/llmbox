package hub

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/guest"
	"github.com/clems4ever/llmbox/internal/hub/apikey"
	"github.com/clems4ever/llmbox/internal/hub/auth"
	"github.com/clems4ever/llmbox/testutils"
	"github.com/coder/websocket"
)

// ptyMgr is a FakeMgr that also satisfies boxPTYer: OpenPTY records its arguments
// and dials a fixed address standing in for the box's PTY tunnel.
type ptyMgr struct {
	*testutils.FakeMgr
	target string // host:port OpenPTY connects to (the fake PTY box)

	mu       sync.Mutex
	gotBoxID string
	gotCols  uint16
	gotRows  uint16
}

// OpenPTY records the request and dials the fixed target.
func (m *ptyMgr) OpenPTY(_ context.Context, boxID string, _ []string, cols, rows uint16) (net.Conn, error) {
	m.mu.Lock()
	m.gotBoxID = boxID
	m.gotCols = cols
	m.gotRows = rows
	m.mu.Unlock()
	return net.Dial("tcp", m.target)
}

// fakePTYBox starts a listener that behaves like the guest's PTY end: it reads
// pty-control frames (the host→box direction) and echoes each data frame's payload
// back as raw bytes (the box→host direction), so a terminal round-trip through the
// hub can be asserted. It returns the listener address.
func fakePTYBox(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go echoPTYFrames(conn)
		}
	}()
	return ln.Addr().String()
}

// echoPTYFrames reads pty-control frames off conn and writes each data payload
// back raw, mirroring how the guest emits a PTY's output.
func echoPTYFrames(conn net.Conn) {
	defer conn.Close()
	var hdr [5]byte
	for {
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(hdr[1:])
		payload := make([]byte, n)
		if _, err := io.ReadFull(conn, payload); err != nil {
			return
		}
		if hdr[0] == 0 { // data frame: echo the keystrokes as terminal output
			if _, err := conn.Write(payload); err != nil {
				return
			}
		}
	}
}

// dialTerminal opens a WebSocket to a box's terminal endpoint on the test server,
// returning the connection (or the handshake's HTTP status on failure).
func dialTerminal(t *testing.T, srv *httptest.Server, boxID, query string, hdr http.Header) (*websocket.Conn, int) {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/boxes/" + boxID + "/terminal"
	if query != "" {
		url += "?" + query
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return nil, status
	}
	return c, http.StatusSwitchingProtocols
}

// TestHandleTerminalStreamsShell drives the terminal end to end: a WebSocket to a
// box whose spoke opens a PTY, then a keystroke framed by the client that echoes
// back as terminal output. It also checks the requested size reached OpenPTY.
func TestHandleTerminalStreamsShell(t *testing.T) {
	mgr := &ptyMgr{FakeMgr: &testutils.FakeMgr{CreateID: "gen-123"}, target: fakePTYBox(t)}
	s, _ := newProxyServer(t, mgr, nil) // auth nil => open
	registerBox(t, s, "term-box", "")

	srv := httptest.NewServer(s.APIHandler())
	defer srv.Close()

	c, status := dialTerminal(t, srv, "term-box", "cols=100&rows=30", nil)
	if c == nil {
		t.Fatalf("terminal handshake failed with status %d", status)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, guest.EncodePTYInput([]byte("whoami\n"))); err != nil {
		t.Fatalf("write: %v", err)
	}
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageBinary || string(data) != "whoami\n" {
		t.Fatalf("echo = (%v) %q, want binary \"whoami\\n\"", typ, data)
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if mgr.gotBoxID != "term-box" || mgr.gotCols != 100 || mgr.gotRows != 30 {
		t.Errorf("OpenPTY got box=%q %dx%d, want term-box 100x30", mgr.gotBoxID, mgr.gotCols, mgr.gotRows)
	}
}

// TestHandleTerminalRequiresAuth checks that, with sign-in configured, an
// unauthenticated terminal handshake is refused.
func TestHandleTerminalRequiresAuth(t *testing.T) {
	a := auth.NewTestAuthenticator("admin@corp.com")
	mgr := &ptyMgr{FakeMgr: &testutils.FakeMgr{CreateID: "gen-123"}, target: fakePTYBox(t)}
	s, _ := newProxyServer(t, mgr, a)
	registerBox(t, s, "term-box", "")

	srv := httptest.NewServer(s.APIHandler())
	defer srv.Close()

	c, status := dialTerminal(t, srv, "term-box", "", nil)
	if c != nil {
		c.Close(websocket.StatusNormalClosure, "")
		t.Fatal("expected the unauthenticated handshake to fail")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
}

// TestHandleTerminalAcceptsAPIKey checks a bearer API key admits the terminal
// handshake even when sign-in is configured (the WebSocket cannot carry a cookie
// CSRF header, so the key path is how a headless client connects).
func TestHandleTerminalAcceptsAPIKey(t *testing.T) {
	a := auth.NewTestAuthenticator("admin@corp.com")
	mgr := &ptyMgr{FakeMgr: &testutils.FakeMgr{CreateID: "gen-123"}, target: fakePTYBox(t)}
	s, _ := newProxyServer(t, mgr, a)
	registerBox(t, s, "term-box", "")

	key, err := apikey.Create(s.store, "term", time.Hour, time.Now())
	if err != nil {
		t.Fatalf("mint key: %v", err)
	}
	srv := httptest.NewServer(s.APIHandler())
	defer srv.Close()

	hdr := http.Header{"Authorization": []string{"Bearer " + key}}
	c, status := dialTerminal(t, srv, "term-box", "", hdr)
	if c == nil {
		t.Fatalf("keyed handshake failed with status %d", status)
	}
	c.Close(websocket.StatusNormalClosure, "")
}

// TestHandleTerminalUnknownBox checks a terminal request for a box with no session
// is refused before any upgrade.
func TestHandleTerminalUnknownBox(t *testing.T) {
	mgr := &ptyMgr{FakeMgr: &testutils.FakeMgr{}, target: fakePTYBox(t)}
	s, _ := newProxyServer(t, mgr, nil)

	srv := httptest.NewServer(s.APIHandler())
	defer srv.Close()

	c, status := dialTerminal(t, srv, "ghost-box", "", nil)
	if c != nil {
		c.Close(websocket.StatusNormalClosure, "")
		t.Fatal("expected the handshake for an unknown box to fail")
	}
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

// TestHandleTerminalUnsupportedSpoke checks a box whose spoke cannot open
// terminals (a manager without OpenPTY) is refused with 502.
func TestHandleTerminalUnsupportedSpoke(t *testing.T) {
	s, _ := newProxyServer(t, &testutils.FakeMgr{CreateID: "gen-123"}, nil)
	registerBox(t, s, "term-box", "")

	srv := httptest.NewServer(s.APIHandler())
	defer srv.Close()

	c, status := dialTerminal(t, srv, "term-box", "", nil)
	if c != nil {
		c.Close(websocket.StatusNormalClosure, "")
		t.Fatal("expected the handshake to fail for a spoke without terminal support")
	}
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
}
