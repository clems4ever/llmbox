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
	Generation  string
	Description string
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

// BoxView is one listed box. It is the underlying box as tracked by the hub; a
// box whose init script failed carries phase "broken" and its captured output in
// LastError.
type BoxView struct {
	sandbox.Box
}

// SpokeEnrollment is the result of minting a join token for a new spoke: the
// one-time token and the ready-to-run command that starts the spoke with it.
// The token is shown once and never recoverable.
type SpokeEnrollment struct {
	Name    string `json:"name" jsonschema:"the spoke name the token enrolls"`
	Token   string `json:"token" jsonschema:"the one-time join token (shown once)"`
	Command string `json:"command" jsonschema:"the copy-pasteable command that starts the spoke and enrolls it"`
}

// TokenPlaceholder stands in for the join-token secret in commands re-rendered
// after creation: the secret is stored only hashed, so once the create response
// is gone it can never be shown again.
const TokenPlaceholder = "<one-time-token>"

// JoinTokenInfo describes one outstanding spoke join token: an opaque ID to
// revoke it by, the spoke name it enrolls, the box backend recorded at
// creation, the enrollment command with TokenPlaceholder standing in for the
// secret, and its expiry. The token secret is never recoverable.
type JoinTokenInfo struct {
	ID        string    `json:"id" jsonschema:"the opaque token ID used to revoke it"`
	Name      string    `json:"name" jsonschema:"the spoke name the token enrolls"`
	Backend   string    `json:"backend" jsonschema:"the box backend recorded when the token was created"`
	Command   string    `json:"command" jsonschema:"the enrollment command with <one-time-token> in place of the secret (the real token is shown only at creation)"`
	ExpiresAt time.Time `json:"expires_at" jsonschema:"when the token stops being accepted"`
}

// Backend is the box-operation contract the API layer needs. The server
// implements it; tests supply a fake.
type Backend interface {
	// CreateBox launches a box and returns its registered session.
	CreateBox(ctx context.Context, opts sandbox.CreateOptions) (BoxSession, error)
	// LookupByBoxID finds a box's session by its caller-assigned box ID
	// (case-insensitive); ok is false when none matches.
	LookupByBoxID(boxID string) (sess BoxSession, ok bool)
	// ListBoxes returns all boxes managed across every spoke.
	ListBoxes(ctx context.Context) ([]BoxView, error)
	// SpokeStatuses returns every spoke and whether it is currently connected.
	SpokeStatuses(ctx context.Context) ([]SpokeStatus, error)
	// CreateSpoke mints a one-time join token enrolling a new spoke and returns it
	// with the ready-to-run start command. backend picks the command's box backend
	// ("docker" or "firecracker"; empty means docker); ttl<=0 uses the default.
	CreateSpoke(ctx context.Context, name, backend string, ttl time.Duration) (SpokeEnrollment, error)
	// DropSpoke removes a spoke's enrollment, revokes its join tokens, and
	// disconnects it.
	DropSpoke(ctx context.Context, name string) error
	// SetDefaultSpoke makes an enrolled spoke the default that unqualified box
	// creates run on.
	SetDefaultSpoke(ctx context.Context, name string) error
	// ListJoinTokens returns every outstanding spoke join token.
	ListJoinTokens(ctx context.Context) ([]JoinTokenInfo, error)
	// RevokeJoinToken deletes one outstanding join token by its ID.
	RevokeJoinToken(ctx context.Context, id string) error
	// RegenerateJoinToken replaces an outstanding join token with a freshly
	// minted one for the same spoke (same name and backend), returning the new
	// enrollment. The old token stops working; the new secret is shown once.
	RegenerateJoinToken(ctx context.Context, id string) (SpokeEnrollment, error)
	// DestroyBox stops and removes the box with the given box ID.
	DestroyBox(ctx context.Context, boxID string) error
	// PauseBox stops the compute of the box with the given box ID to save CPU/RAM,
	// keeping its disk so it can be resumed later.
	PauseBox(ctx context.Context, boxID string) error
	// ResumeBox restarts a paused box's compute; the box comes back on the next
	// ListBoxes.
	ResumeBox(ctx context.Context, boxID string) error
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
