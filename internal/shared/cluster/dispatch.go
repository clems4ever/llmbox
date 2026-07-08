package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// serve runs the spoke-side request loop: it reads verb requests off the
// transport and dispatches each (concurrently) to the local BoxManager, sending
// the result back as a response frame. It returns when the transport fails
// (hub disconnected) or ctx is cancelled.
//
// Requests are handled in their own goroutines so a slow verb (a long exec)
// doesn't block others; the transport serializes the concurrent responses.
//
// @arg ctx Context whose cancellation stops the loop.
// @arg tr The transport to the hub.
// @arg mgr The local box manager verbs are dispatched to.
// @error error The transport error that ended the loop (nil on a clean context cancel).
//
// @testcase TestDispatchHandlesVerbs dispatches each verb to a fake manager and replies.
// @testcase TestDispatchUnknownMethod replies with an error for an unknown method.
func serve(ctx context.Context, tr transport, mgr BoxManager) error {
	streams := newSpokeStreams()
	defer streams.closeAll()
	for {
		f, err := tr.Recv(ctx)
		if err != nil {
			return err
		}
		switch f.Type {
		case frameReq:
			// Verb requests run in their own goroutine so a slow one doesn't block
			// others; the transport serializes the concurrent responses.
			go handleRequest(ctx, tr, mgr, f)
		case frameStreamOpen:
			// Open handling (including the box dial) runs inline so the stream is
			// registered before any following data frame for it is processed.
			handleStreamOpen(ctx, tr, mgr, f, streams)
		case frameStreamData:
			if ss := streams.get(f.ID); ss != nil {
				ss.writeInbound(f.Data)
			}
		case frameStreamClose:
			if ss := streams.get(f.ID); ss != nil {
				streams.del(f.ID)
				ss.teardown(false)
			}
		default:
			// enroll/welcome/resp/err are not expected on the spoke's serve loop.
		}
	}
}

// handleStreamOpen dials the requested box port and starts a tunnel, or replies
// with a frameStreamClose carrying the failure. Only a manager that can dial boxes
// (the in-process *box.Manager) services tunnels; the dial is managed-only, so it
// never reaches a host address outside one of the spoke's own boxes.
//
// @arg ctx Context bounding the dial.
// @arg tr The transport to reply on and stream over.
// @arg mgr The local box manager.
// @arg req The frameStreamOpen request (its ID is the stream ID).
// @arg streams The spoke's live-stream registry.
//
// @testcase TestStreamTunnelRoundTrip opens a tunnel to a dialable box.
// @testcase TestStreamOpenUnsupportedSpoke closes the stream when the spoke cannot dial.
func handleStreamOpen(ctx context.Context, tr transport, mgr BoxManager, req frame, streams *spokeStreams) {
	closeErr := func(msg string) {
		_ = tr.Send(ctx, frame{Type: frameStreamClose, ID: req.ID, Error: msg})
	}
	var in streamOpenReq
	if err := decodePayload(req.Payload, &in); err != nil {
		closeErr(err.Error())
		return
	}
	dialer, ok := mgr.(BoxDialer)
	if !ok {
		closeErr("this spoke does not support proxying")
		return
	}
	conn, err := dialer.DialBox(ctx, in.BoxID, in.Port)
	if err != nil {
		closeErr(err.Error())
		return
	}
	ss := newServerStream(req.ID, tr, conn, streams.del)
	streams.add(req.ID, ss)
	ss.start()
}

// handleRequest executes one verb and sends its response.
//
// @arg ctx Context for the verb call.
// @arg tr The transport to reply on.
// @arg mgr The local box manager.
// @arg req The request frame.
//
// @testcase TestDispatchHandlesVerbs covers each verb path through handleRequest.
func handleRequest(ctx context.Context, tr transport, mgr BoxManager, req frame) {
	payload, err := dispatch(ctx, mgr, req)
	resp := frame{Type: frameResp, ID: req.ID}
	if err != nil {
		resp.Error = err.Error()
	} else {
		resp.Payload = payload
	}
	// A failed send means the hub is gone; the read loop will observe it too.
	_ = tr.Send(ctx, resp)
}

// dispatch decodes a verb request, calls the matching BoxManager method, and
// returns the encoded response payload. A creation request's box id is validated
// at the wire boundary — defense-in-depth so a spoke never provisions from a
// malformed (and potentially shell-injecting) id, rather than trusting the hub.
//
// @arg ctx Context for the verb call.
// @arg mgr The local box manager.
// @arg req The request frame to dispatch.
// @return json.RawMessage The encoded response payload (nil for void verbs).
// @error error if the method is unknown, the payload is malformed, a create names a malformed box id, or the verb fails.
//
// @testcase TestDispatchHandlesVerbs decodes and runs each verb.
// @testcase TestDispatchUnknownMethod errors on an unrecognized method.
// @testcase TestDispatchBadPayload errors on a malformed request payload.
// @testcase TestDispatchRejectsInvalidCreate rejects a create whose box id is malformed.
func dispatch(ctx context.Context, mgr BoxManager, req frame) (json.RawMessage, error) {
	switch req.Method {
	case methodCreate:
		var in createReq
		if err := decodePayload(req.Payload, &in); err != nil {
			return nil, err
		}
		if in.Opts.BoxID != "" && !sandbox.ValidBoxID(in.Opts.BoxID) {
			return nil, fmt.Errorf("invalid box id %q: must be 1-63 chars of lowercase letters, digits, or hyphens (not starting or ending with a hyphen)", in.Opts.BoxID)
		}
		id, url, err := mgr.Create(ctx, in.Opts)
		if err != nil {
			return nil, err
		}
		return encodePayload(createResp{ID: id, AuthorizeURL: url})
	case methodSubmitCode:
		var in submitCodeReq
		if err := decodePayload(req.Payload, &in); err != nil {
			return nil, err
		}
		url, err := mgr.SubmitCode(ctx, in.ID, in.Code)
		if err != nil {
			return nil, err
		}
		return encodePayload(submitCodeResp{SessionURL: url})
	case methodList:
		boxes, err := mgr.List(ctx)
		if err != nil {
			return nil, err
		}
		return encodePayload(listResp{Boxes: boxes})
	case methodDestroy:
		var in destroyReq
		if err := decodePayload(req.Payload, &in); err != nil {
			return nil, err
		}
		if err := mgr.Destroy(ctx, in.IDOrName); err != nil {
			return nil, err
		}
		return nil, nil
	case methodLogs:
		var in logsReq
		if err := decodePayload(req.Payload, &in); err != nil {
			return nil, err
		}
		logs, err := mgr.Logs(ctx, in.IDOrName, in.Tail)
		if err != nil {
			return nil, err
		}
		return encodePayload(logsResp{Logs: logs})
	case methodExec:
		var in execReq
		if err := decodePayload(req.Payload, &in); err != nil {
			return nil, err
		}
		res, err := mgr.Exec(ctx, in.IDOrName, in.Cmd)
		if err != nil {
			return nil, err
		}
		return encodePayload(res)
	case methodReap:
		var in reapReq
		if err := decodePayload(req.Payload, &in); err != nil {
			return nil, err
		}
		reaped, err := mgr.ReapOrphans(ctx, time.Duration(in.TTLNanos))
		if err != nil {
			return nil, err
		}
		return encodePayload(reapResp{Reaped: reaped})
	default:
		return nil, fmt.Errorf("unknown method %q", req.Method)
	}
}
