package cluster

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/clems4ever/llmbox/internal/sandbox"
)

// errSpokeDisconnected is returned by in-flight (and subsequent) verb calls when
// the spoke's connection drops.
var errSpokeDisconnected = errors.New("spoke disconnected")

// remoteSpoke is the hub-side BoxManager for one connected spoke. Each verb call
// sends a request frame over the transport and waits for the matching response,
// correlated by an incrementing ID. A single read loop demultiplexes responses
// to waiting callers, so many verb calls can be in flight over one connection.
type remoteSpoke struct {
	name string
	tr   transport
	done chan struct{} // closed when the read loop exits (connection gone)

	mu       sync.Mutex
	nextID   uint64
	pending  map[uint64]chan frame
	closed   bool
	closeErr error
}

// newRemoteSpoke wraps a transport as a BoxManager and starts its read loop. The
// read loop runs until the transport errors (peer closed or network failure),
// at which point all pending and future calls fail with the connection error.
//
// @arg name The spoke's name (for diagnostics and registry keying).
// @arg tr The established transport to the spoke.
// @return *remoteSpoke A BoxManager that round-trips verbs to the spoke.
//
// @testcase TestRemoteSpokeRoundTrip routes every verb through a remoteSpoke to a fake dispatcher.
// @testcase TestRemoteSpokeDisconnect fails pending and later calls once the transport drops.
func newRemoteSpoke(name string, tr transport) *remoteSpoke {
	r := &remoteSpoke{
		name:    name,
		tr:      tr,
		done:    make(chan struct{}),
		pending: make(map[uint64]chan frame),
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
		// Only responses are expected on the hub side; ignore anything else.
		if f.Type != frameResp && f.Type != frameErr {
			continue
		}
		r.mu.Lock()
		ch := r.pending[f.ID]
		delete(r.pending, f.ID)
		r.mu.Unlock()
		if ch != nil {
			ch <- f // buffered (cap 1), so this never blocks
		}
	}
}

// shutdown marks the spoke disconnected and fails all pending calls.
//
// @arg cause The transport error that ended the connection.
//
// @testcase TestRemoteSpokeDisconnect checks pending calls observe the disconnect.
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
	r.mu.Unlock()
	for _, ch := range pending {
		close(ch)
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
// @return id The new container ID on the spoke.
// @return authorizeURL The OAuth authorize URL captured on the spoke.
// @error error if the call fails or the spoke returns an error.
//
// @testcase TestRemoteSpokeRoundTrip creates a box through the remote spoke.
func (r *remoteSpoke) Create(ctx context.Context, opts sandbox.CreateOptions) (id, authorizeURL string, err error) {
	var resp createResp
	if err := r.call(ctx, methodCreate, createReq{Opts: opts}, &resp); err != nil {
		return "", "", err
	}
	return resp.ID, resp.AuthorizeURL, nil
}

// SubmitCode forwards an OAuth code submission to the spoke.
//
// @arg ctx Context for the call.
// @arg id The container ID on the spoke.
// @arg code The OAuth code.
// @return sessionURL The remote-control session URL.
// @error error if the call fails or the spoke returns an error.
//
// @testcase TestRemoteSpokeRoundTrip submits a code through the remote spoke.
func (r *remoteSpoke) SubmitCode(ctx context.Context, id, code string) (sessionURL string, err error) {
	var resp submitCodeResp
	if err := r.call(ctx, methodSubmitCode, submitCodeReq{ID: id, Code: code}, &resp); err != nil {
		return "", err
	}
	return resp.SessionURL, nil
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

// Logs returns recent console output of a box on the spoke.
//
// @arg ctx Context for the call.
// @arg idOrName The box ID or name.
// @arg tail The maximum number of trailing lines.
// @return string The box's recent console output.
// @error error if the call fails or the spoke returns an error.
//
// @testcase TestRemoteSpokeRoundTrip reads logs through the remote spoke.
func (r *remoteSpoke) Logs(ctx context.Context, idOrName string, tail int) (string, error) {
	var resp logsResp
	if err := r.call(ctx, methodLogs, logsReq{IDOrName: idOrName, Tail: tail}, &resp); err != nil {
		return "", err
	}
	return resp.Logs, nil
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

// ProxyHTTP forwards a buffered HTTP request to a box's port on the spoke and
// returns the buffered response, implementing HTTPProxier over the cluster
// transport. It is how the hub reaches a box's HTTP server on a remote spoke
// (a box on the local spoke is proxied directly, with streaming).
//
// @arg ctx Context bounding the call.
// @arg boxID The box whose port to reach.
// @arg port The port inside the box.
// @arg method The HTTP method.
// @arg path The request URI (path plus raw query).
// @arg header The request headers.
// @arg body The request body (buffered).
// @return status The response status code.
// @return respHeader The response headers.
// @return respBody The response body (buffered).
// @error error if the call fails or the spoke returns an error.
//
// @testcase TestRemoteSpokeProxyHTTP round-trips a proxy request to the spoke.
func (r *remoteSpoke) ProxyHTTP(ctx context.Context, boxID string, port int, method, path string, header http.Header, body []byte) (status int, respHeader http.Header, respBody []byte, err error) {
	var resp proxyHTTPResp
	if err := r.call(ctx, methodProxyHTTP, proxyHTTPReq{
		BoxID:  boxID,
		Port:   port,
		Method: method,
		Path:   path,
		Header: header,
		Body:   body,
	}, &resp); err != nil {
		return 0, nil, nil, err
	}
	return resp.Status, resp.Header, resp.Body, nil
}

// ReapOrphans reaps never-authenticated boxes on the spoke older than ttl.
//
// @arg ctx Context for the call.
// @arg ttl How long a box may stay un-authenticated before being reaped.
// @return []string The short IDs of reaped boxes.
// @error error if the call fails or the spoke returns an error.
//
// @testcase TestRemoteSpokeRoundTrip reaps orphans through the remote spoke.
func (r *remoteSpoke) ReapOrphans(ctx context.Context, ttl time.Duration) ([]string, error) {
	var resp reapResp
	if err := r.call(ctx, methodReap, reapReq{TTLNanos: int64(ttl)}, &resp); err != nil {
		return nil, err
	}
	return resp.Reaped, nil
}
