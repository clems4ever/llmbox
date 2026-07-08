// Package boxapi serves the box-facing port-publishing API: a small HTTP/JSON
// API on a per-box unix socket that lets the Claude process inside a box open,
// close, and list public URLs for ports of ITS OWN box only. The handler is
// bound to one box's identity at construction — the request body carries only
// a port and description, never a box or spoke identity — so whichever channel
// a request arrives on decides which box it acts on. The socket is deliberately
// a unix socket (not TCP) so the box's own proxy data path, which only dials
// TCP ports, can never publish the control API itself.
//
// The spoke serves the socket host-side for Docker boxes (in the per-box
// bind-mount dir, so it appears in-box at /run/llmbox/boxapi.sock); for
// Firecracker the same handler listens on the per-VM host UDS that the guest
// agent bridges the in-guest socket to. Either way the listener — and thus the
// enforcement — lives outside the sandbox.
package boxapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/cluster"
)

// SocketName is the file name of the box-port API socket: the in-box path is
// /run/llmbox/boxapi.sock (next to the agent's control.sock), and the Docker
// host side is the same name inside the box's private socket dir.
const SocketName = "boxapi.sock"

// callTimeout bounds one box request's round trip to the hub.
const callTimeout = 30 * time.Second

// maxBodyBytes caps a request body; the requests are tiny JSON objects.
const maxBodyBytes = 64 << 10

// PortService is what the box-port API needs from the spoke: forward a
// request, stamped with the originating box's identity, to the hub.
// *cluster.HubCaller satisfies it.
type PortService interface {
	// OpenBoxPort publishes a box port and returns its public view.
	OpenBoxPort(ctx context.Context, boxID string, port int, description string) (cluster.BoxPortInfo, error)
	// CloseBoxPort unpublishes a box port.
	CloseBoxPort(ctx context.Context, boxID string, port int) error
	// ListBoxPorts returns the box's published ports.
	ListBoxPorts(ctx context.Context, boxID string) ([]cluster.BoxPortInfo, error)
}

// Request/response bodies. Deliberately no box or spoke field anywhere: the
// handler's construction-time binding supplies the identity.

type openPortRequest struct {
	Port        int    `json:"port"`
	Description string `json:"description,omitempty"`
}
type openPortResponse struct {
	Port cluster.BoxPortInfo `json:"port"`
}

type closePortRequest struct {
	Port int `json:"port"`
}

type listPortsResponse struct {
	Ports []cluster.BoxPortInfo `json:"ports"`
}

// errorResponse is the body of every non-2xx response.
type errorResponse struct {
	Error string `json:"error"`
}

// NewHandler returns the box-port HTTP API for one box. boxID is the identity
// every forwarded request is stamped with; it is fixed at construction so
// nothing the box sends can change it. A box created without a box ID gets a
// handler whose every call fails with a clear explanation.
//
// Routes (all POST, JSON in/out; errors are {"error":"..."} with status 400 on
// a bad request and 502 when the hub rejects or is unreachable):
//
//	POST /v1/open_port  {"port":3000,"description":"..."} → {"port":{"port":3000,"url":"https://...","description":"..."}}
//	POST /v1/close_port {"port":3000}                     → {}
//	POST /v1/list_ports {}                                → {"ports":[...]}
//
// @arg boxID The identity of the box this handler serves; "" yields a handler that only explains the box cannot publish ports.
// @arg svc The service forwarding requests to the hub.
// @return http.Handler The box-port API for that one box.
//
// @testcase TestBoxAPIOpenPort opens a port and sees the bound box ID stamped.
// @testcase TestBoxAPIClosePort closes a port through the handler.
// @testcase TestBoxAPIListPorts lists ports through the handler.
// @testcase TestBoxAPIRejectsBadRequest rejects malformed JSON and out-of-range ports.
// @testcase TestBoxAPIServiceError maps a service failure to a 502 with the error body.
// @testcase TestBoxAPINoBoxID explains that a box without a box ID cannot publish ports.
func NewHandler(boxID string, svc PortService) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("POST /v1/open_port", jsonEndpoint(func(ctx context.Context, req openPortRequest) (openPortResponse, int, error) {
		if err := checkBox(boxID); err != nil {
			return openPortResponse{}, http.StatusBadRequest, err
		}
		if err := checkPort(req.Port); err != nil {
			return openPortResponse{}, http.StatusBadRequest, err
		}
		info, err := svc.OpenBoxPort(ctx, boxID, req.Port, req.Description)
		if err != nil {
			return openPortResponse{}, http.StatusBadGateway, err
		}
		return openPortResponse{Port: info}, http.StatusOK, nil
	}))

	mux.Handle("POST /v1/close_port", jsonEndpoint(func(ctx context.Context, req closePortRequest) (struct{}, int, error) {
		if err := checkBox(boxID); err != nil {
			return struct{}{}, http.StatusBadRequest, err
		}
		if err := checkPort(req.Port); err != nil {
			return struct{}{}, http.StatusBadRequest, err
		}
		if err := svc.CloseBoxPort(ctx, boxID, req.Port); err != nil {
			return struct{}{}, http.StatusBadGateway, err
		}
		return struct{}{}, http.StatusOK, nil
	}))

	mux.Handle("POST /v1/list_ports", jsonEndpoint(func(ctx context.Context, _ struct{}) (listPortsResponse, int, error) {
		if err := checkBox(boxID); err != nil {
			return listPortsResponse{}, http.StatusBadRequest, err
		}
		ports, err := svc.ListBoxPorts(ctx, boxID)
		if err != nil {
			return listPortsResponse{}, http.StatusBadGateway, err
		}
		return listPortsResponse{Ports: ports}, http.StatusOK, nil
	}))

	return mux
}

// checkBox rejects requests from a box that has no box ID (ports are keyed by
// box ID on the hub, so such a box cannot publish any).
//
// @arg boxID The handler's bound box ID.
// @error error if boxID is empty.
//
// @testcase TestBoxAPINoBoxID surfaces the explanation on every route.
func checkBox(boxID string) error {
	if boxID == "" {
		return fmt.Errorf("this box has no box ID, so it cannot publish ports; recreate it with a box ID")
	}
	return nil
}

// checkPort validates a TCP port number at the box boundary for a fast, clear
// 400 (the hub re-validates).
//
// @arg port The port to validate.
// @error error if port is outside 1-65535.
//
// @testcase TestBoxAPIRejectsBadRequest rejects 0 and 70000.
func checkPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid port %d: must be between 1 and 65535", port)
	}
	return nil
}

// jsonEndpoint adapts a typed handler function to http.Handler: it decodes the
// JSON request body (empty body allowed, size-capped), bounds the call with
// callTimeout, and writes the JSON response or an errorResponse with the
// handler's status.
//
// @arg fn The typed endpoint: request in; response, status, and error out.
// @return http.Handler The wrapping handler.
//
// @testcase TestBoxAPIOpenPort drives a request through the adapter.
// @testcase TestBoxAPIRejectsBadRequest sees a 400 for a malformed body.
func jsonEndpoint[Req, Resp any](fn func(ctx context.Context, req Req) (Resp, int, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Req
		body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
		if err := json.NewDecoder(body).Decode(&req); err != nil && err.Error() != "EOF" {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON body: " + err.Error()})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
		defer cancel()
		resp, status, err := fn(ctx, req)
		if err != nil {
			writeJSON(w, status, errorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, status, resp)
	})
}

// writeJSON writes v as the JSON response body with the given status.
//
// @arg w The response writer.
// @arg status The HTTP status code.
// @arg v The value to encode.
//
// @testcase TestBoxAPIOpenPort reads back a JSON response written by writeJSON.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
