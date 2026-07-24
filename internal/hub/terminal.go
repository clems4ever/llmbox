package hub

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/clems4ever/llmbox/internal/hub/apikey"
	"github.com/coder/websocket"
)

// boxPTYer is the box-reachability capability the in-browser terminal needs from a
// spoke's box manager: open an interactive pseudo-terminal inside a box and return
// it as a raw byte tunnel. A remote spoke satisfies it over the cluster transport
// (cluster's remoteSpoke implements OpenPTY), so a terminal reaches a box on a
// remote spoke with full bidirectional streaming — exactly like the reverse proxy
// reaches a box's port. Keeping it a separate optional interface (not part of
// cluster.BoxManager) preserves the cluster protocol's box-verb RPC allowlist.
type boxPTYer interface {
	OpenPTY(ctx context.Context, idOrName string, cmd []string, cols, rows uint16) (net.Conn, error)
}

// terminalReadLimit bounds a single WebSocket message the browser terminal may
// send. Terminal input (keystrokes, paste, resize) arrives in small pty-control
// frames, so this cap is generous while still refusing a client that tries to make
// the hub buffer without limit.
const terminalReadLimit = 1 << 20

// defaultTerminalCols and defaultTerminalRows size a terminal whose open request
// omits a size, matching the guest's own fallback so a shell starts sane.
const (
	defaultTerminalCols = 80
	defaultTerminalRows = 24
)

// handleTerminal upgrades a request to a WebSocket and bridges it to an
// interactive shell inside a box: it authorizes the caller, resolves the box to
// its spoke, opens a pseudo-terminal there, and splices the two connections. The
// browser speaks the pty-control protocol (see internal/guest): it sends framed
// keystrokes and resizes and receives the shell's raw output. Because the box is
// reached through the spoke's live PTY tunnel, a box on a remote spoke works just
// as a local one does.
//
// Auth mirrors the proxy: an admin login session (the same gate the admin UI and
// proxy use) or a bearer API key. The WebSocket handshake is same-origin (enforced
// by websocket.Accept), which stands in for the CSRF header a WebSocket cannot
// carry.
//
// @arg w The response writer (upgraded to a WebSocket).
// @arg r The upgrade request; its path names the box and ?cols/?rows the size.
//
// @testcase TestHandleTerminalStreamsShell round-trips shell I/O over the WebSocket.
// @testcase TestHandleTerminalRequiresAuth refuses an unauthenticated request when auth is on.
// @testcase TestHandleTerminalUnknownBox 404s a box with no session.
// @testcase TestHandleTerminalUnsupportedSpoke fails a box on a spoke that cannot open terminals.
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	if allowed, code := s.terminalAuthorized(r); !allowed {
		http.Error(w, "Unauthorized", code)
		return
	}
	boxID := r.PathValue("id")
	sess := s.lookupByBoxID(boxID)
	if sess == nil {
		http.Error(w, "no box found with that box ID", http.StatusNotFound)
		return
	}
	if sess.terminated() {
		http.Error(w, "box is terminated", http.StatusConflict)
		return
	}
	mgr, err := s.spoke(sess.SpokeName)
	if err != nil {
		http.Error(w, "the box's spoke is not available: "+err.Error(), http.StatusBadGateway)
		return
	}
	ptyer, ok := mgr.(boxPTYer)
	if !ok {
		http.Error(w, "this box's spoke does not support terminals", http.StatusBadGateway)
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept already wrote the error response.
	}
	conn.SetReadLimit(terminalReadLimit)
	// The tunnel and the shell live as long as the socket; cancel them when it goes.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	cols, rows := terminalSize(r)
	// The open handshake is bounded; the returned tunnel itself streams unbounded
	// for the session's lifetime (its reads/writes use the parent ctx, not this).
	openCtx, openCancel := context.WithTimeout(ctx, terminalDialTimeout)
	defer openCancel()
	tunnel, err := ptyer.OpenPTY(openCtx, boxID, nil, cols, rows)
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "opening terminal failed")
		return
	}
	defer tunnel.Close()

	// Splice the WebSocket to the PTY tunnel as a plain byte pipe: the browser's
	// framed input flows straight to the guest (which parses the pty-control
	// frames), and the shell's raw output flows straight back as binary messages.
	// The hub stays a dumb relay — the framing lives at the two ends.
	wsConn := websocket.NetConn(ctx, conn, websocket.MessageBinary)
	spliceConn(wsConn, tunnel)
}

// terminalSize reads the requested initial terminal geometry from the ?cols and
// ?rows query parameters, falling back to a sane default for a missing or
// malformed value.
//
// @arg r The terminal request.
// @return uint16 The initial column count.
// @return uint16 The initial row count.
//
// @testcase TestHandleTerminalStreamsShell opens a terminal at the requested size.
func terminalSize(r *http.Request) (uint16, uint16) {
	return dimOr(r.URL.Query().Get("cols"), defaultTerminalCols), dimOr(r.URL.Query().Get("rows"), defaultTerminalRows)
}

// dimOr parses a terminal dimension query value, returning fallback when it is
// missing or not a positive number that fits a uint16.
//
// @arg s The raw query value.
// @arg fallback The default to use when s is absent or invalid.
// @return uint16 The parsed dimension, or fallback.
//
// @testcase TestHandleTerminalStreamsShell relies on dimOr to size the terminal.
func dimOr(s string, fallback uint16) uint16 {
	if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 65535 {
		return uint16(n)
	}
	return fallback
}

// terminalAuthorized reports whether a terminal request may proceed. It admits a
// bearer API key (the headless path) or, when sign-in is configured, an admin
// login session — the same gate the reverse proxy uses. When no provider is
// configured the terminal is open, matching the proxy, which then relies on a
// front reverse proxy for authn.
//
// @arg r The incoming terminal request.
// @return bool True when the request is authorized.
// @return int The HTTP status to reply with when not authorized (0 when authorized).
//
// @testcase TestHandleTerminalRequiresAuth refuses an unauthenticated request when auth is on.
// @testcase TestHandleTerminalAcceptsAPIKey admits a valid bearer key.
func (s *Server) terminalAuthorized(r *http.Request) (bool, int) {
	if key, ok := bearerToken(r); ok {
		if _, valid, err := apikey.Authenticate(s.store, key, time.Now()); err == nil && valid {
			return true, 0
		}
		return false, http.StatusUnauthorized
	}
	if s.auth == nil {
		return true, 0
	}
	ls, ok := s.auth.CurrentLogin(r)
	if !ok {
		return false, http.StatusUnauthorized
	}
	if !ls.CanAdmin {
		return false, http.StatusForbidden
	}
	return true, 0
}

// spliceConn moves bytes in both directions between two connections until either
// closes, then closes both. It is the terminal's relay between the WebSocket and
// the box PTY tunnel.
//
// @arg a One end of the relay (the WebSocket net.Conn).
// @arg b The other end (the box PTY tunnel).
//
// @testcase TestHandleTerminalStreamsShell moves bytes both ways through spliceConn.
func spliceConn(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		_ = dst.Close()
		_ = src.Close()
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}

// terminalDialTimeout bounds the PTY open handshake so a wedged spoke cannot hang
// the upgrade indefinitely.
const terminalDialTimeout = 15 * time.Second
