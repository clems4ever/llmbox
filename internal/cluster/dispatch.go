package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
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
// @arg policy The admission policy applied to creation requests.
// @error error The transport error that ended the loop (nil on a clean context cancel).
//
// @testcase TestDispatchHandlesVerbs dispatches each verb to a fake manager and replies.
// @testcase TestDispatchUnknownMethod replies with an error for an unknown method.
func serve(ctx context.Context, tr transport, mgr BoxManager, policy ValidationPolicy) error {
	for {
		f, err := tr.Recv(ctx)
		if err != nil {
			return err
		}
		if f.Type != frameReq {
			continue
		}
		go handleRequest(ctx, tr, mgr, f, policy)
	}
}

// handleRequest executes one verb and sends its response.
//
// @arg ctx Context for the verb call.
// @arg tr The transport to reply on.
// @arg mgr The local box manager.
// @arg req The request frame.
// @arg policy The admission policy applied to creation requests.
//
// @testcase TestDispatchHandlesVerbs covers each verb path through handleRequest.
func handleRequest(ctx context.Context, tr transport, mgr BoxManager, req frame, policy ValidationPolicy) {
	payload, err := dispatch(ctx, mgr, req, policy)
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
// returns the encoded response payload. A creation request is admission-checked
// against policy before it reaches the manager.
//
// @arg ctx Context for the verb call.
// @arg mgr The local box manager.
// @arg req The request frame to dispatch.
// @arg policy The admission policy applied to creation requests.
// @return json.RawMessage The encoded response payload (nil for void verbs).
// @error error if the method is unknown, the payload is malformed, the request is rejected by policy, or the verb fails.
//
// @testcase TestDispatchHandlesVerbs decodes and runs each verb.
// @testcase TestDispatchUnknownMethod errors on an unrecognized method.
// @testcase TestDispatchBadPayload errors on a malformed request payload.
// @testcase TestDispatchRejectsInvalidCreate rejects a creation that fails the policy.
// @testcase TestRemoteSpokeProxyHTTP runs the proxy_http verb end to end.
// @testcase TestProxyHTTPUnsupportedSpoke rejects proxy_http when the spoke cannot dial boxes.
func dispatch(ctx context.Context, mgr BoxManager, req frame, policy ValidationPolicy) (json.RawMessage, error) {
	switch req.Method {
	case methodCreate:
		var in createReq
		if err := decodePayload(req.Payload, &in); err != nil {
			return nil, err
		}
		if err := policy.validateCreate(in.Opts); err != nil {
			return nil, err
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
	case methodProxyHTTP:
		var in proxyHTTPReq
		if err := decodePayload(req.Payload, &in); err != nil {
			return nil, err
		}
		// Only a manager that can dial boxes (the in-process *docker.Manager)
		// services proxy requests; the dial is managed-only, so this never reaches
		// a host address outside one of the spoke's own boxes.
		dialer, ok := mgr.(BoxDialer)
		if !ok {
			return nil, fmt.Errorf("this spoke does not support proxying")
		}
		resp, err := roundTripToBox(ctx, dialer, in)
		if err != nil {
			return nil, err
		}
		return encodePayload(resp)
	default:
		return nil, fmt.Errorf("unknown method %q", req.Method)
	}
}
