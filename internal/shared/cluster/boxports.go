package cluster

import (
	"context"
	"errors"
	"sync"
)

// errNotConnected is returned by HubCaller calls made while the spoke has no
// live connection to the hub.
var errNotConnected = errors.New("not connected to hub")

// BoxPortService handles spoke-originated box-port requests on the hub. It is
// implemented by the hub server and injected into NewHub so the cluster layer
// stays free of hub imports. spokeName is the authenticated name of the
// connection a request arrived on (bound at enrollment, never caller-supplied),
// and boxID is the identity the SPOKE stamped from its own record of the
// originating box — implementations MUST verify that box actually lives on that
// spoke before acting, so a spoke (or a compromised box) can never manipulate
// another spoke's boxes.
type BoxPortService interface {
	// OpenBoxPort publishes a box's port and returns its public view.
	OpenBoxPort(ctx context.Context, spokeName, boxID string, port int, description string) (BoxPortInfo, error)
	// CloseBoxPort unpublishes a box's port.
	CloseBoxPort(ctx context.Context, spokeName, boxID string, port int) error
	// ListBoxPorts returns the box's published ports, and only that box's.
	ListBoxPorts(ctx context.Context, spokeName, boxID string) ([]BoxPortInfo, error)
}

// HubCaller lets spoke-side components (the per-box port API) issue requests to
// the hub over whichever cluster connection is currently live. A spoke creates
// exactly one HubCaller for its whole lifetime and hands it to both the box
// backend and RunWithCaller: each (re)connection attaches its transport after
// enrollment and detaches it when the connection drops, failing the calls that
// were in flight. Calls made while detached fail immediately with a clear
// "not connected to hub" error instead of blocking.
type HubCaller struct {
	mu      sync.Mutex
	tr      transport // nil while disconnected
	nextID  uint64
	pending map[uint64]chan frame // in-flight calls, by ID
}

// NewHubCaller returns a detached HubCaller; calls fail until a connection is
// attached by RunWithCaller.
//
// @return *HubCaller A caller with no live connection yet.
//
// @testcase TestSpokeCallerDisconnected fails a call made before any connection is attached.
func NewHubCaller() *HubCaller {
	return &HubCaller{pending: make(map[uint64]chan frame)}
}

// attach binds the caller to a freshly enrolled connection's transport, so
// subsequent calls ride that connection.
//
// @arg tr The enrolled transport to the hub.
//
// @testcase TestSpokeCallerRoundTrip attaches a transport and completes a call over it.
// @testcase TestSpokeCallerReconnects attaches a second transport after the first dropped.
func (c *HubCaller) attach(tr transport) {
	c.mu.Lock()
	c.tr = tr
	c.mu.Unlock()
}

// detach unbinds the caller from its connection and fails every in-flight call,
// which observes the disconnect as a closed channel.
//
// @testcase TestSpokeCallerDisconnected fails an in-flight call when the connection detaches.
func (c *HubCaller) detach() {
	c.mu.Lock()
	c.tr = nil
	pending := c.pending
	c.pending = make(map[uint64]chan frame)
	c.mu.Unlock()
	for _, ch := range pending {
		close(ch)
	}
}

// deliver routes one frameSpokeResp from the serve loop to its waiting caller.
// Responses for unknown (cancelled or superseded) IDs are dropped.
//
// @arg f The response frame received from the hub.
//
// @testcase TestSpokeCallerRoundTrip delivers responses to the matching in-flight call.
func (c *HubCaller) deliver(f frame) {
	c.mu.Lock()
	ch := c.pending[f.ID]
	delete(c.pending, f.ID)
	c.mu.Unlock()
	if ch != nil {
		ch <- f // buffered (cap 1), so this never blocks
	}
}

// call sends one spoke→hub request and waits for its response. resp may be nil
// for verbs with no return payload.
//
// @arg ctx Context bounding the call.
// @arg method The spoke→hub verb method name.
// @arg req The request payload to send.
// @arg resp Pointer to decode the response payload into, or nil.
// @error error if the spoke is disconnected, the send fails, the context is cancelled, or the hub returns a verb error.
//
// @testcase TestSpokeCallerRoundTrip drives each box-port verb through call.
// @testcase TestSpokeCallerServiceError surfaces a hub-side verb error to the caller.
func (c *HubCaller) call(ctx context.Context, method string, req, resp any) error {
	payload, err := encodePayload(req)
	if err != nil {
		return err
	}

	c.mu.Lock()
	tr := c.tr
	if tr == nil {
		c.mu.Unlock()
		return errNotConnected
	}
	c.nextID++
	id := c.nextID
	ch := make(chan frame, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := tr.Send(ctx, frame{Type: frameSpokeReq, ID: id, Method: method, Payload: payload}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case f, ok := <-ch:
		if !ok {
			return errNotConnected
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

// OpenBoxPort asks the hub to publish a port of the given box and returns the
// public view of the published port.
//
// @arg ctx Context for the call.
// @arg boxID The spoke-stamped identity of the box the request originated from.
// @arg port The TCP port inside the box to publish.
// @arg description The caller's free-form label for the port.
// @return BoxPortInfo The published port with its public URL.
// @error error if the spoke is disconnected or the hub rejects the request.
//
// @testcase TestSpokeCallerRoundTrip opens a port through the caller.
func (c *HubCaller) OpenBoxPort(ctx context.Context, boxID string, port int, description string) (BoxPortInfo, error) {
	var resp openBoxPortResp
	if err := c.call(ctx, methodOpenBoxPort, openBoxPortReq{BoxID: boxID, Port: port, Description: description}, &resp); err != nil {
		return BoxPortInfo{}, err
	}
	return resp.Port, nil
}

// CloseBoxPort asks the hub to unpublish a port of the given box.
//
// @arg ctx Context for the call.
// @arg boxID The spoke-stamped identity of the box the request originated from.
// @arg port The published port to close.
// @error error if the spoke is disconnected or the hub rejects the request.
//
// @testcase TestSpokeCallerRoundTrip closes a port through the caller.
func (c *HubCaller) CloseBoxPort(ctx context.Context, boxID string, port int) error {
	return c.call(ctx, methodCloseBoxPort, closeBoxPortReq{BoxID: boxID, Port: port}, nil)
}

// ListBoxPorts asks the hub for the given box's published ports.
//
// @arg ctx Context for the call.
// @arg boxID The spoke-stamped identity of the box the request originated from.
// @return []BoxPortInfo The box's published ports.
// @error error if the spoke is disconnected or the hub rejects the request.
//
// @testcase TestSpokeCallerRoundTrip lists ports through the caller.
func (c *HubCaller) ListBoxPorts(ctx context.Context, boxID string) ([]BoxPortInfo, error) {
	var resp listBoxPortsResp
	if err := c.call(ctx, methodListBoxPorts, listBoxPortsReq{BoxID: boxID}, &resp); err != nil {
		return nil, err
	}
	return resp.Ports, nil
}
