// Package api is the llmbox HTTP box-control API: the JSON surface the server
// exposes for box operations (create/get/list/destroy/logs/exec, proxies, spokes)
// and a Client that speaks it. The UI and any programmatic caller drive boxes
// through this API; the stand-alone llmbox-mcp binary is one such caller, wrapping
// the Client so it can serve those operations as MCP tools.
//
// Backend is the operation contract both sides share: the server implements it,
// NewHandler serves an implementation over HTTP, and Client is an implementation
// backed by a remote server.
package api

import (
	"context"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// BoxSession is the subset of a box's state the API surfaces. It is a flat value
// (no locks, no pointers into the server) so callers never reach back into the
// server's internals and tests can construct one directly.
type BoxSession struct {
	BoxID       string
	ContainerID string
	Token       string
	Description string
	Status      string // "pending" | "ready" | "error"
	SessionURL  string
	Error       string
}

// ProxyInfo describes one enabled HTTP proxy: the box and port it exposes and the
// URL the user opens to reach it.
type ProxyInfo struct {
	BoxID string `json:"box_id" jsonschema:"the box ID whose port is exposed"`
	Port  int    `json:"port" jsonschema:"the port inside the box that is exposed"`
	URL   string `json:"url" jsonschema:"the URL the user opens to reach the box's port"`
	Slug  string `json:"slug" jsonschema:"the unguessable sub-domain label identifying the proxy"`
	Spoke string `json:"spoke,omitempty" jsonschema:"the spoke the box runs on"`
	// Description is the optional human-readable note supplied when the proxy was created.
	Description string `json:"description,omitempty" jsonschema:"the optional human-readable note supplied when the proxy was created"`
}

// SpokeStatus describes one enrolled cluster spoke and its health: whether it
// currently holds a live hub connection, and whether it is the default spoke that
// unqualified box creates run on.
type SpokeStatus struct {
	Name       string    `json:"name" jsonschema:"the spoke's name"`
	Connected  bool      `json:"connected" jsonschema:"whether the spoke currently has a live connection to the hub"`
	Default    bool      `json:"default,omitempty" jsonschema:"true for the default spoke unqualified box creates run on"`
	EnrolledAt time.Time `json:"enrolled_at,omitempty" jsonschema:"when the spoke enrolled"`
}

// Backend is the box-operation contract the API layer needs. The server
// implements it; tests supply a fake. The OAuth secret is intentionally absent:
// CreateBox returns only a token, and AuthPageURL turns that token into the public
// auth page URL, so no secret ever flows through the API.
type Backend interface {
	// CreateBox launches a box and returns its registered auth session.
	CreateBox(ctx context.Context, opts sandbox.CreateOptions) (BoxSession, error)
	// AuthPageURL is the URL the user opens to finish authenticating a box.
	AuthPageURL(token string) string
	// LookupByBoxID finds a box's session by its caller-assigned box ID
	// (case-insensitive); ok is false when none matches.
	LookupByBoxID(boxID string) (sess BoxSession, ok bool)
	// ListBoxes returns all boxes managed across every spoke.
	ListBoxes(ctx context.Context) ([]sandbox.Box, error)
	// SpokeStatuses returns every spoke and whether it is currently connected.
	SpokeStatuses(ctx context.Context) ([]SpokeStatus, error)
	// DestroyBox stops and removes the box with the given container ID.
	DestroyBox(ctx context.Context, containerID string) error
	// BoxLogs returns the recent console output of the box with the given box ID.
	BoxLogs(ctx context.Context, boxID string, tail int) (string, error)
	// BoxExec runs a shell command inside the box with the given box ID.
	BoxExec(ctx context.Context, boxID, command string) (sandbox.ExecResult, error)
	// ProxyEnabled reports whether the HTTP proxy feature is configured.
	ProxyEnabled() bool
	// CreateProxy enables an HTTP proxy to a box's port and returns it. description
	// is an optional human-readable note stamped onto the proxy, or "" for none.
	CreateProxy(ctx context.Context, boxID string, port int, description string) (ProxyInfo, error)
	// DeleteProxy disables the proxy for a box and port.
	DeleteProxy(ctx context.Context, boxID string, port int) error
	// ListProxies returns the enabled proxies, optionally filtered to one box.
	ListProxies(ctx context.Context, boxID string) ([]ProxyInfo, error)
}
