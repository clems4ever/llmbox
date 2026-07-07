package api

import (
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
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
	Session BoxSession `json:"session"`
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
	Session BoxSession `json:"session"`
	// Found distinguishes "no box with that ID" (a normal miss the tool reports
	// itself) from a transport/backend error, so a miss is never an HTTP error.
	Found bool `json:"found"`
}

type listBoxesResponse struct {
	Boxes []sandbox.Box `json:"boxes"`
}

type spokeStatusesResponse struct {
	Spokes []SpokeStatus `json:"spokes"`
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
	Proxy ProxyInfo `json:"proxy"`
}

type deleteProxyRequest struct {
	BoxID string `json:"box_id"`
	Port  int    `json:"port"`
}

type listProxiesRequest struct {
	BoxID string `json:"box_id,omitempty"`
}
type listProxiesResponse struct {
	Proxies []ProxyInfo `json:"proxies"`
}
