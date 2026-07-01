// Package mcpapi is the thin HTTP seam between the llmbox server and a stand-alone
// MCP front-end. It re-exposes the mcpserver.Backend contract — the exact set of
// box operations the MCP tools need — over a small JSON API, so the two can run
// as separate processes:
//
//   - NewHandler(b) serves a Backend over HTTP; the llmbox server mounts it on its
//     box-control port, handing it its own in-process backend.
//   - NewClient(url) is a Backend that forwards every call to that HTTP API; the
//     `llmbox mcp` command wraps it in mcpserver.NewServer to serve MCP without any
//     in-process access to Docker, the store, or the cluster.
//
// The wire types below are a 1:1 mapping of the Backend methods (one request/
// response pair per method), so the handler and client stay trivial and the
// contract is defined in a single place.
package mcpapi

import (
	"github.com/clems4ever/llmbox/internal/mcpserver"
	"github.com/clems4ever/llmbox/internal/sandbox"
)

// API route paths. Each maps 1:1 to a Backend method and is served/called with a
// single POST carrying a JSON body (an empty body for the no-argument methods).
const (
	PathCreateBox     = "/api/v1/create-box"
	PathAuthPageURL   = "/api/v1/auth-page-url"
	PathLookupBox     = "/api/v1/lookup-box"
	PathListBoxes     = "/api/v1/list-boxes"
	PathSpokeStatuses = "/api/v1/spoke-statuses"
	PathDestroyBox    = "/api/v1/destroy-box"
	PathBoxLogs       = "/api/v1/box-logs"
	PathBoxExec       = "/api/v1/box-exec"
	PathProxyEnabled  = "/api/v1/proxy-enabled"
	PathCreateProxy   = "/api/v1/create-proxy"
	PathDeleteProxy   = "/api/v1/delete-proxy"
	PathListProxies   = "/api/v1/list-proxies"
)

// errorResponse is the body of every non-2xx API response; the client turns its
// message back into a Go error.
type errorResponse struct {
	Error string `json:"error"`
}

// emptyResponse is the body of a successful call that returns nothing but success
// (destroy-box, delete-proxy).
type emptyResponse struct{}

type createBoxRequest struct {
	Opts sandbox.CreateOptions `json:"opts"`
}
type createBoxResponse struct {
	Session mcpserver.BoxSession `json:"session"`
}

type authPageURLRequest struct {
	Token string `json:"token"`
}
type authPageURLResponse struct {
	URL string `json:"url"`
}

type lookupBoxRequest struct {
	BoxID string `json:"box_id"`
}
type lookupBoxResponse struct {
	Session mcpserver.BoxSession `json:"session"`
	// Found distinguishes "no box with that ID" (a normal miss the tool reports
	// itself) from a transport/backend error, so a miss is never an HTTP error.
	Found bool `json:"found"`
}

type listBoxesResponse struct {
	Boxes []sandbox.Box `json:"boxes"`
}

type spokeStatusesResponse struct {
	Spokes []mcpserver.SpokeStatus `json:"spokes"`
}

type destroyBoxRequest struct {
	ContainerID string `json:"container_id"`
}

type boxLogsRequest struct {
	BoxID string `json:"box_id"`
	Tail  int    `json:"tail"`
}
type boxLogsResponse struct {
	Logs string `json:"logs"`
}

type boxExecRequest struct {
	BoxID   string `json:"box_id"`
	Command string `json:"command"`
}
type boxExecResponse struct {
	Result sandbox.ExecResult `json:"result"`
}

type proxyEnabledResponse struct {
	Enabled bool `json:"enabled"`
}

type createProxyRequest struct {
	BoxID       string `json:"box_id"`
	Port        int    `json:"port"`
	Description string `json:"description,omitempty"`
}
type createProxyResponse struct {
	Proxy mcpserver.ProxyInfo `json:"proxy"`
}

type deleteProxyRequest struct {
	BoxID string `json:"box_id"`
	Port  int    `json:"port"`
}

type listProxiesRequest struct {
	BoxID string `json:"box_id,omitempty"`
}
type listProxiesResponse struct {
	Proxies []mcpserver.ProxyInfo `json:"proxies"`
}
