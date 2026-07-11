package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// errSpokeDisconnected is returned by in-flight (and subsequent) verb calls when
// the spoke's connection drops.
var errSpokeDisconnected = errors.New("spoke disconnected")

// remoteSpoke is the hub-side BoxManager for one connected spoke. Each verb call
// sends a request frame over the transport and waits for the matching response,
// correlated by an incrementing ID. A single read loop demultiplexes responses
// to waiting callers, so many verb calls can be in flight over one connection.
type remoteSpoke struct {
	name  string
	tr    transport
	done  chan struct{}  // closed when the read loop exits (connection gone)
	ports BoxPortService // handles spoke-originated box-port requests; nil rejects them

	mu       sync.Mutex
	nextID   uint64
	pending  map[uint64]chan frame    // in-flight verb calls, by ID
	streams  map[uint64]*clientStream // live proxy tunnels, by stream ID
	closed   bool
	closeErr error
}

// newRemoteSpoke wraps a transport as a BoxManager and starts its read loop. The
// read loop runs until the transport errors (peer closed or network failure),
// at which point all pending and future calls fail with the connection error.
//
// @arg name The spoke's name (for diagnostics and registry keying).
// @arg tr The established transport to the spoke.
// @arg ports The service handling this spoke's box-port requests; nil rejects them.
// @return *remoteSpoke A BoxManager that round-trips verbs to the spoke.
//
// @testcase TestRemoteSpokeRoundTrip routes every verb through a remoteSpoke to a fake dispatcher.
// @testcase TestRemoteSpokeDisconnect fails pending and later calls once the transport drops.
func newRemoteSpoke(name string, tr transport, ports BoxPortService) *remoteSpoke {
	r := &remoteSpoke{
		name:    name,
		tr:      tr,
		done:    make(chan struct{}),
		ports:   ports,
		pending: make(map[uint64]chan frame),
		streams: make(map[uint64]*clientStream),
	}
	go r.readLoop()
	return r
}

// readLoop reads response frames and hands each to its waiting caller until the
// transport fails, then fails every pending and future call.
//
// @testcase TestRemoteSpokeDisconnect exercises the loop tearing down on transport error.
func (r *remoteSpoke) readLoop() {
	for {
		f, err := r.tr.Recv(context.Background())
		if err != nil {
			r.shutdown(err)
			return
		}
		switch f.Type {
		case frameResp, frameErr:
			// A verb response: hand it to the waiting caller.
			r.mu.Lock()
			ch := r.pending[f.ID]
			delete(r.pending, f.ID)
			r.mu.Unlock()
			if ch != nil {
				ch <- f // buffered (cap 1), so this never blocks
			}
		case frameStreamData:
			r.mu.Lock()
			cs := r.streams[f.ID]
			r.mu.Unlock()
			if cs != nil {
				cs.push(f.Data)
			}
		case frameStreamClose:
			r.mu.Lock()
			cs := r.streams[f.ID]
			delete(r.streams, f.ID)
			r.mu.Unlock()
			if cs != nil {
				var cause error
				if f.Error != "" {
					cause = errors.New(f.Error)
				}
				cs.closeRemote(cause)
			}
		case frameSpokeReq:
			// A spoke-originated request: served in its own goroutine so a slow
			// store operation never blocks response routing for in-flight verbs.
			go r.serveSpokeRequest(f)
		default:
			// Enroll/welcome/req are never received on the hub side; ignore.
		}
	}
}

// serveSpokeRequest dispatches one spoke-originated box-port request to the
// hub's BoxPortService and replies with a frameSpokeResp (Error set on
// failure). The service receives the spoke's authenticated name — bound to
// this connection at enrollment — never a caller-supplied identity.
//
// @arg f The frameSpokeReq to serve.
//
// @testcase TestSpokeCallerRoundTrip serves each box-port verb against a fake service.
// @testcase TestSpokeCallerServiceError returns the service's error to the spoke.
// @testcase TestHubWithoutBoxPortServiceRejects rejects requests when no service is configured.
func (r *remoteSpoke) serveSpokeRequest(f frame) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	payload, err := r.dispatchSpokeRequest(ctx, f)
	resp := frame{Type: frameSpokeResp, ID: f.ID}
	if err != nil {
		resp.Error = err.Error()
	} else {
		resp.Payload = payload
	}
	// A failed send means the connection is gone; the read loop observes it too.
	_ = r.tr.Send(ctx, resp)
}

// dispatchSpokeRequest decodes a spoke-originated request, calls the matching
// BoxPortService method with this connection's authenticated spoke name, and
// returns the encoded response payload.
//
// @arg ctx Context bounding the service call.
// @arg f The request frame to dispatch.
// @return json.RawMessage The encoded response payload (nil for void verbs).
// @error error if no service is configured, the method is unknown, the payload is malformed, or the service fails.
//
// @testcase TestSpokeCallerRoundTrip decodes and runs each box-port verb.
// @testcase TestSpokeCallerUnknownMethod errors on an unrecognized method.
func (r *remoteSpoke) dispatchSpokeRequest(ctx context.Context, f frame) (json.RawMessage, error) {
	if r.ports == nil {
		return nil, errors.New("this hub does not support box-port requests")
	}
	switch f.Method {
	case methodOpenBoxPort:
		var in openBoxPortReq
		if err := decodePayload(f.Payload, &in); err != nil {
			return nil, err
		}
		info, err := r.ports.OpenBoxPort(ctx, r.name, in.BoxID, in.Port, in.Description)
		if err != nil {
			return nil, err
		}
		return encodePayload(openBoxPortResp{Port: info})
	case methodCloseBoxPort:
		var in closeBoxPortReq
		if err := decodePayload(f.Payload, &in); err != nil {
			return nil, err
		}
		return nil, r.ports.CloseBoxPort(ctx, r.name, in.BoxID, in.Port)
	case methodListBoxPorts:
		var in listBoxPortsReq
		if err := decodePayload(f.Payload, &in); err != nil {
			return nil, err
		}
		ports, err := r.ports.ListBoxPorts(ctx, r.name, in.BoxID)
		if err != nil {
			return nil, err
		}
		return encodePayload(listBoxPortsResp{Ports: ports})
	default:
		return nil, fmt.Errorf("unknown method %q", f.Method)
	}
}

// shutdown marks the spoke disconnected and fails all pending calls and live tunnels.
//
// @arg cause The transport error that ended the connection.
//
// @testcase TestRemoteSpokeDisconnect checks pending calls observe the disconnect.
// @testcase TestStreamTunnelDisconnect checks a live tunnel is torn down on disconnect.
func (r *remoteSpoke) shutdown(cause error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.closeErr = cause
	pending := r.pending
	r.pending = map[uint64]chan frame{}
	streams := r.streams
	r.streams = map[uint64]*clientStream{}
	r.mu.Unlock()
	for _, ch := range pending {
		close(ch)
	}
	// Fail every live tunnel so its reverse-proxy conn unblocks with the cause.
	for _, cs := range streams {
		cs.closeRemote(cause)
	}
	close(r.done)
}

// Done returns a channel closed when the spoke's connection is gone, so the
// registry can drop it.
//
// @return <-chan struct{} Closed once the connection has dropped.
//
// @testcase TestRemoteSpokeDisconnect waits on Done after the transport drops.
func (r *remoteSpoke) Done() <-chan struct{} { return r.done }

// Close tears down the connection to the spoke; the read loop then fails any
// pending calls. Closing more than once is harmless.
//
// @error error if closing the underlying transport fails.
//
// @testcase TestHubReconnectSupersedes closes a superseded spoke connection.
func (r *remoteSpoke) Close() error { return r.tr.Close() }

// call sends a verb request and waits for its response. resp may be nil for
// verbs with no return payload.
//
// @arg ctx Context bounding the call.
// @arg method The verb method name.
// @arg req The request payload to send.
// @arg resp Pointer to decode the response payload into, or nil.
// @error error if the connection is gone, the send fails, the context is cancelled, or the spoke returns a verb error.
//
// @testcase TestRemoteSpokeRoundTrip drives each verb through call.
// @testcase TestRemoteSpokeVerbError surfaces a spoke-side verb error to the caller.
func (r *remoteSpoke) call(ctx context.Context, method string, req, resp any) error {
	payload, err := encodePayload(req)
	if err != nil {
		return err
	}

	r.mu.Lock()
	if r.closed {
		err := r.closeErr
		r.mu.Unlock()
		return err
	}
	r.nextID++
	id := r.nextID
	ch := make(chan frame, 1)
	r.pending[id] = ch
	r.mu.Unlock()

	if err := r.tr.Send(ctx, frame{Type: frameReq, ID: id, Method: method, Payload: payload}); err != nil {
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
		return err
	}

	select {
	case <-ctx.Done():
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
		return ctx.Err()
	case f, ok := <-ch:
		if !ok {
			return errSpokeDisconnected
		}
		if f.Error != "" {
			return errors.New(f.Error)
		}
		if resp != nil {
			return decodePayload(f.Payload, resp)
		}
		return nil
	}
}

// Create forwards a box creation to the spoke.
//
// @arg ctx Context for the call.
// @arg opts The box creation options.
// @return sandbox.CreateResult The box's generation token and configured publish ports, or the init-script failure and its output.
// @error error if the call fails or the spoke returns an error.
//
// @testcase TestRemoteSpokeRoundTrip creates a box through the remote spoke.
func (r *remoteSpoke) Create(ctx context.Context, opts sandbox.CreateOptions) (sandbox.CreateResult, error) {
	var resp createResp
	if err := r.call(ctx, methodCreate, createReq{Opts: opts}, &resp); err != nil {
		return sandbox.CreateResult{}, err
	}
	return sandbox.CreateResult{
		InstanceID:       resp.ID,
		InitScriptFailed: resp.InitScriptFailed,
		InitScriptOutput: resp.InitScriptOutput,
		PublishPorts:     resp.PublishPorts,
	}, nil
}

// List returns the boxes the spoke manages.
//
// @arg ctx Context for the call.
// @return []sandbox.Box The spoke's managed boxes.
// @error error if the call fails or the spoke returns an error.
//
// @testcase TestRemoteSpokeRoundTrip lists boxes through the remote spoke.
func (r *remoteSpoke) List(ctx context.Context) ([]sandbox.Box, error) {
	var resp listResp
	if err := r.call(ctx, methodList, struct{}{}, &resp); err != nil {
		return nil, err
	}
	return resp.Boxes, nil
}

// Destroy removes a box on the spoke.
//
// @arg ctx Context for the call.
// @arg idOrName The box ID or name to destroy.
// @error error if the call fails or the spoke returns an error.
//
// @testcase TestRemoteSpokeRoundTrip destroys a box through the remote spoke.
func (r *remoteSpoke) Destroy(ctx context.Context, idOrName string) error {
	return r.call(ctx, methodDestroy, destroyReq{IDOrName: idOrName}, nil)
}

// Pause stops a box's compute on the spoke to save CPU/RAM while keeping its disk.
//
// @arg ctx Context for the call.
// @arg idOrName The box ID or name to pause.
// @error error if the call fails or the spoke returns an error.
//
// @testcase TestRemoteSpokeRoundTrip pauses a box through the remote spoke.
func (r *remoteSpoke) Pause(ctx context.Context, idOrName string) error {
	return r.call(ctx, methodPause, pauseReq{IDOrName: idOrName}, nil)
}

// Resume restarts a paused box's compute on the spoke.
//
// @arg ctx Context for the call.
// @arg idOrName The box ID or name to resume.
// @error error if the call fails or the spoke returns an error.
//
// @testcase TestRemoteSpokeRoundTrip resumes a box through the remote spoke.
func (r *remoteSpoke) Resume(ctx context.Context, idOrName string) error {
	return r.call(ctx, methodResume, resumeReq{IDOrName: idOrName}, nil)
}

// Exec runs a command inside a box on the spoke.
//
// @arg ctx Context for the call.
// @arg idOrName The box ID or name.
// @arg cmd The command and arguments.
// @return sandbox.ExecResult The command's captured output and exit code.
// @error error if the call fails or the spoke returns an error.
//
// @testcase TestRemoteSpokeRoundTrip execs a command through the remote spoke.
func (r *remoteSpoke) Exec(ctx context.Context, idOrName string, cmd []string) (sandbox.ExecResult, error) {
	var resp sandbox.ExecResult
	if err := r.call(ctx, methodExec, execReq{IDOrName: idOrName, Cmd: cmd}, &resp); err != nil {
		return sandbox.ExecResult{}, err
	}
	return resp, nil
}

// DialBox opens a raw byte tunnel to a box's port on the spoke and returns it as a
// net.Conn, implementing BoxDialer over the cluster transport. It is how the hub
// reaches a box's TCP port on a remote spoke with full streaming, so the reverse
// proxy carries WebSocket and SSE (not just buffered request/response). The tunnel
// is opened optimistically: if the spoke cannot dial the box, the failure surfaces
// on the first read of the returned conn (as a frameStreamClose carrying the
// error), which the proxy turns into a 502 — matching how a dead upstream behaves.
//
// @arg ctx Context bounding the open handshake (not the tunnel's lifetime).
// @arg idOrName The box whose port to reach.
// @arg port The port inside the box.
// @return net.Conn The tunnel connection; the caller must close it.
// @error error if the spoke is disconnected or the open frame cannot be sent.
//
// @testcase TestStreamTunnelRoundTrip round-trips bytes through a dialed tunnel.
// @testcase TestStreamTunnelDisconnect fails a live tunnel when the connection drops.
func (r *remoteSpoke) DialBox(ctx context.Context, idOrName string, port int) (net.Conn, error) {
	payload, err := encodePayload(streamOpenReq{BoxID: idOrName, Port: port})
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.closed {
		err := r.closeErr
		r.mu.Unlock()
		return nil, err
	}
	r.nextID++
	id := r.nextID
	cs := newClientStream(id, r.tr, r.unregisterStream)
	r.streams[id] = cs
	r.mu.Unlock()

	if err := r.tr.Send(ctx, frame{Type: frameStreamOpen, ID: id, Payload: payload}); err != nil {
		r.unregisterStream(id)
		return nil, err
	}
	return cs, nil
}

// unregisterStream drops a tunnel from the registry (called when the local end
// closes). Deleting a missing ID is a no-op, so it is safe to race the read loop's
// own removal on a remote close.
//
// @arg id The stream ID to remove.
//
// @testcase TestStreamTunnelRoundTrip removes a closed tunnel from the registry.
func (r *remoteSpoke) unregisterStream(id uint64) {
	r.mu.Lock()
	delete(r.streams, id)
	r.mu.Unlock()
}
