package cluster

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Hub is the hub-side of the cluster: it accepts spoke connections on an HTTP
// route, authenticates their enrollment, and keeps a registry of connected
// spokes that the server routes box verbs to. A spoke connection is one
// long-lived WebSocket; the hub pushes verb requests down it via a remoteSpoke.
type Hub struct {
	store Store
	ctx   context.Context // base context; cancellation force-closes spoke connections
	now   func() time.Time
	log   *slog.Logger
	ports BoxPortService // handles spoke-originated box-port requests; nil rejects them

	mu     sync.Mutex
	spokes map[string]*remoteSpoke
}

// NewHub builds a Hub over the given store. ctx bounds the lifetime of accepted
// spoke connections (cancelling it closes them). now defaults to time.Now and
// log to slog.Default when nil.
//
// @arg ctx Base context; its cancellation closes all spoke connections.
// @arg store The cluster store holding join tokens and enrolled spokes.
// @arg now Clock for token-expiry checks; nil uses time.Now.
// @arg log Logger for connection lifecycle; nil uses slog.Default.
// @arg ports The service handling spoke-originated box-port requests; nil rejects them.
// @return *Hub A ready hub with an empty connected-spoke registry.
//
// @testcase TestHubEnrollAndRoute enrolls a spoke and routes a verb to it.
// @testcase TestHubWithoutBoxPortServiceRejects rejects box-port requests when ports is nil.
func NewHub(ctx context.Context, store Store, now func() time.Time, log *slog.Logger, ports BoxPortService) *Hub {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Hub{store: store, ctx: ctx, now: now, log: log, ports: ports, spokes: map[string]*remoteSpoke{}}
}

// Spoke returns the connected spoke with the given name as a BoxManager.
//
// @arg name The spoke name.
// @return BoxManager The connected spoke, or nil when not connected.
// @return bool True when a spoke with that name is currently connected.
//
// @testcase TestHubEnrollAndRoute looks up the enrolled spoke by name.
func (h *Hub) Spoke(name string) (BoxManager, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs, ok := h.spokes[name]
	if !ok {
		return nil, false
	}
	return rs, true
}

// Spokes returns a snapshot of the currently connected spokes keyed by name.
//
// @return map[string]BoxManager One entry per connected spoke.
//
// @testcase TestHubEnrollAndRoute lists the connected spokes after enrollment.
func (h *Hub) Spokes() map[string]BoxManager {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]BoxManager, len(h.spokes))
	for name, rs := range h.spokes {
		out[name] = rs
	}
	return out
}

// ConnectHandler is the HTTP handler for the spoke connection route
// (/spoke/connect). It upgrades to a WebSocket, performs the enrollment
// handshake, registers the spoke, and serves verb requests over the connection
// until it drops or the hub's context is cancelled.
//
// SECURITY — like the spoke dialer (see WebSocketDialer), this endpoint relies on
// the DEPLOYMENT for transport security. The route is unauthenticated until the
// enrollment frame arrives (the handshake IS the auth), so it must be served over
// TLS (terminate wss:// at a trusted reverse proxy in front of the hub) so the
// join token / bearer credential a spoke presents are not exposed on the wire.
// The credential is compared timing-safely against a stored hash, but it is a
// static bearer secret: protect it in transit and at rest, and revoke a spoke if
// it may have leaked.
//
// @arg w The response writer (upgraded to a WebSocket).
// @arg r The upgrade request.
//
// @testcase TestHubEnrollAndRoute drives a real spoke through this handler over loopback.
// @testcase TestHubRejectsBadEnrollment closes the connection when enrollment is rejected.
func (h *Hub) ConnectHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept already wrote the error response
	}
	tr := newWSTransport(conn)

	name, err := h.enroll(r.Context(), tr)
	if err != nil {
		// Tell the spoke why (vaguely) then close; do not leak the failure mode
		// over the wire. The server-side log carries the real reason so an
		// operator can diagnose without the rejection being self-describing to
		// an attacker.
		_ = tr.Send(r.Context(), frame{Type: frameErr, Error: errEnrollRejected.Error()})
		_ = tr.Close()
		h.log.Warn("spoke enrollment rejected", "remote", r.RemoteAddr, "reason", err)
		return
	}

	rs := newRemoteSpoke(name, tr, h.ports)
	h.register(name, rs)
	h.log.Info("spoke connected", "name", name)
	defer func() {
		h.unregister(name, rs)
		h.log.Info("spoke disconnected", "name", name)
	}()

	select {
	case <-rs.Done():
	case <-h.ctx.Done():
		_ = rs.Close()
	}
}

// enroll runs the enrollment handshake: read the spoke's enroll frame,
// authenticate it, and reply with a welcome (carrying a minted credential on
// first enrollment). It returns the authorized spoke name.
//
// @arg ctx Context bounding the handshake.
// @arg tr The transport to the spoke.
// @return string The authorized spoke name.
// @error error if the first frame is not a valid enroll, or authentication fails.
//
// @testcase TestHubEnrollAndRoute completes the handshake for a valid join token.
// @testcase TestHubRejectsBadEnrollment fails the handshake for a bad token.
func (h *Hub) enroll(ctx context.Context, tr transport) (string, error) {
	f, err := tr.Recv(ctx)
	if err != nil {
		return "", err
	}
	if f.Type != frameEnroll {
		return "", errEnrollRejected
	}
	var req enrollReq
	if err := decodePayload(f.Payload, &req); err != nil {
		return "", err
	}
	name, credential, err := authenticateEnroll(h.store, req, h.now())
	if err != nil {
		return "", err
	}
	welcome, err := encodePayload(welcomeResp{Name: name, Credential: credential})
	if err != nil {
		return "", err
	}
	if err := tr.Send(ctx, frame{Type: frameWelcome, Payload: welcome}); err != nil {
		return "", err
	}
	return name, nil
}

// register adds a connected spoke, replacing (and closing) any prior connection
// under the same name so a reconnect supersedes a stale link.
//
// @arg name The spoke name.
// @arg rs The connected spoke.
//
// @testcase TestHubReconnectSupersedes replaces a prior connection on reconnect.
func (h *Hub) register(name string, rs *remoteSpoke) {
	h.mu.Lock()
	old := h.spokes[name]
	h.spokes[name] = rs
	h.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

// Disconnect force-closes the live connection for a named spoke, if any, so the
// spoke is dropped immediately (e.g. after an admin revokes its enrollment). The
// read loop tears down and unregisters the connection as a result; disconnecting
// an unknown or already-gone spoke is a no-op. It does not delete the spoke's
// enrolled record — the caller does that so the spoke cannot simply reconnect.
//
// @arg name The spoke name whose live connection should be closed.
//
// @testcase TestHubDisconnectClosesConnection closes a connected spoke's link.
func (h *Hub) Disconnect(name string) {
	h.mu.Lock()
	rs := h.spokes[name]
	h.mu.Unlock()
	if rs != nil {
		_ = rs.Close()
	}
}

// unregister removes a spoke only if it is still the registered one (a newer
// reconnect must not be evicted by an older connection's teardown).
//
// @arg name The spoke name.
// @arg rs The connection being torn down.
//
// @testcase TestHubReconnectSupersedes keeps the newer connection when the old one tears down.
func (h *Hub) unregister(name string, rs *remoteSpoke) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.spokes[name] == rs {
		delete(h.spokes, name)
	}
}
