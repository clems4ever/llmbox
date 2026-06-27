package server

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/clems4ever/llmbox/internal/docker"
	"github.com/clems4ever/llmbox/internal/mcpserver"
	"github.com/clems4ever/llmbox/internal/store"
)

// MCPServer builds an MCP server exposing this server's box tools. It is a thin
// wrapper that adapts the server to the mcpserver.Backend contract; the tool
// logic itself lives in the mcpserver package.
//
// @arg name The MCP server implementation name.
// @arg version The MCP server implementation version.
// @return *mcp.Server An MCP server with the create/get/list/destroy tools registered.
//
// @testcase TestMCPToolsRegisteredAndCreate checks all tools are registered and create works.
func (s *Server) MCPServer(name, version string) *mcp.Server {
	return mcpserver.NewServer(s.MCPBackend(), name, version)
}

// MCPBackend returns the server adapted to the mcpserver.Backend interface, so
// the MCP tool layer can drive box operations without depending on the server's
// internals.
//
// @return mcpserver.Backend The backend the MCP tools call into.
//
// @testcase TestMCPToolsRegisteredAndCreate drives the backend through the registered tools.
func (s *Server) MCPBackend() mcpserver.Backend {
	return mcpBackend{s: s}
}

// mcpBackend adapts *Server to mcpserver.Backend, translating the server's
// internal session type into the flat mcpserver.BoxSession the tools consume.
type mcpBackend struct{ s *Server }

// CreateBox launches a box and returns the flattened session for the MCP layer.
// Only the box ID, container ID, and token are surfaced; the box's OAuth
// authorize URL is deliberately not exposed so no secret reaches a tool.
//
// @arg ctx Context for the box creation.
// @arg opts The image, box ID, description, and target spoke for the box.
// @return mcpserver.BoxSession The new box's ID, container ID, and auth token.
// @error error if the box cannot be created.
//
// @testcase TestMCPToolsRegisteredAndCreate creates a box through the backend.
func (b mcpBackend) CreateBox(ctx context.Context, opts docker.CreateOptions) (mcpserver.BoxSession, error) {
	sess, err := b.s.createBox(ctx, opts)
	if err != nil {
		return mcpserver.BoxSession{}, err
	}
	return mcpserver.BoxSession{
		BoxID:       sess.BoxID,
		ContainerID: sess.ContainerID,
		Token:       sess.Token,
	}, nil
}

// AuthPageURL is the URL the user opens to finish authenticating a box.
//
// @arg token The session token identifying the auth session.
// @return string The absolute auth page URL for the token.
//
// @testcase TestMCPToolsRegisteredAndCreate checks the create output carries the auth page URL.
func (b mcpBackend) AuthPageURL(token string) string {
	return b.s.AuthPageURL(token)
}

// LookupByBoxID finds a box's session by its box ID and flattens its mutable
// state (status, session URL, error) into an mcpserver.BoxSession.
//
// @arg boxID The box ID to look up.
// @return mcpserver.BoxSession The matching box's flattened session (zero value when ok is false).
// @return bool Whether a box with that box ID exists.
//
// @testcase TestGetByBoxID looks a box up by its box ID through the backend.
func (b mcpBackend) LookupByBoxID(boxID string) (mcpserver.BoxSession, bool) {
	sess := b.s.lookupByBoxID(boxID)
	if sess == nil {
		return mcpserver.BoxSession{}, false
	}
	status, url, errMsg := sess.snapshot()
	return mcpserver.BoxSession{
		BoxID:       sess.BoxID,
		ContainerID: sess.ContainerID,
		Description: sess.Description,
		Status:      status,
		SessionURL:  url,
		Error:       errMsg,
	}, true
}

// ListBoxes returns all boxes managed across every spoke.
//
// @arg ctx Context for the list request.
// @return []docker.Box The boxes managed by this server.
// @error error if listing boxes fails.
//
// @testcase TestListLlmboxesReturnsBoxID lists boxes through the backend.
func (b mcpBackend) ListBoxes(ctx context.Context) ([]docker.Box, error) {
	return b.s.listBoxes(ctx)
}

// SpokeStatuses returns every spoke and its connection status, translated to the
// mcpserver.SpokeStatus shape the tool reports.
//
// @arg ctx Context for the request.
// @return []mcpserver.SpokeStatus The spokes and their connection status.
// @error error if the enrolled spokes cannot be read.
//
// @testcase TestListSpokesTool reports the spoke statuses through the backend.
func (b mcpBackend) SpokeStatuses(ctx context.Context) ([]mcpserver.SpokeStatus, error) {
	spokes, err := b.s.SpokeStatuses(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]mcpserver.SpokeStatus, len(spokes))
	for i, sp := range spokes {
		out[i] = mcpserver.SpokeStatus{
			Name:       sp.Name,
			Connected:  sp.Connected,
			Local:      sp.Local,
			EnrolledAt: sp.EnrolledAt,
		}
	}
	return out, nil
}

// DestroyBox stops and removes the box with the given container ID.
//
// @arg ctx Context for the destroy request.
// @arg containerID The container ID of the box to destroy.
// @error error if the box cannot be destroyed.
//
// @testcase TestDestroyForgetsSession destroys a box through the backend.
func (b mcpBackend) DestroyBox(ctx context.Context, containerID string) error {
	return b.s.destroyBox(ctx, containerID)
}

// BoxLogs returns the recent console output of the box with the given box ID.
//
// @arg ctx Context for the logs request.
// @arg boxID The box ID of the box to read logs from.
// @arg tail The maximum number of trailing log lines to return.
// @return string The box's recent console output.
// @error error if no box has that box ID or its logs cannot be read.
//
// @testcase TestBoxLogsByBoxID reads a box's logs through the backend.
func (b mcpBackend) BoxLogs(ctx context.Context, boxID string, tail int) (string, error) {
	return b.s.boxLogs(ctx, boxID, tail)
}

// BoxExec runs a shell command inside the box with the given box ID.
//
// @arg ctx Context for the exec request.
// @arg boxID The box ID of the box to run the command in.
// @arg command The shell command line to run inside the box.
// @return docker.ExecResult The command's stdout, stderr, and exit code.
// @error error if the command is empty, no box has that box ID, or it cannot be run.
//
// @testcase TestBoxExecByBoxID runs a command through the backend.
func (b mcpBackend) BoxExec(ctx context.Context, boxID, command string) (docker.ExecResult, error) {
	return b.s.boxExec(ctx, boxID, command)
}

// ProxyEnabled reports whether the HTTP proxy feature is configured.
//
// @return bool True when proxying is enabled.
//
// @testcase TestMCPBackendProxies reports proxy enablement through the backend.
func (b mcpBackend) ProxyEnabled() bool { return b.s.ProxyEnabled() }

// CreateProxy enables an HTTP proxy to a box's port and flattens it (with its
// public URL) into the mcpserver.ProxyInfo the tool returns.
//
// @arg _ Context (unused; the registry is in-memory and in the store).
// @arg boxID The box ID whose port to expose.
// @arg port The port inside the box to forward to.
// @return mcpserver.ProxyInfo The new proxy's box ID, port, URL, slug, and spoke.
// @error error if proxying is disabled, the port is invalid, or no box has that box ID.
//
// @testcase TestMCPBackendProxies enables a proxy through the backend.
func (b mcpBackend) CreateProxy(_ context.Context, boxID string, port int) (mcpserver.ProxyInfo, error) {
	rec, err := b.s.createProxy(boxID, port, "")
	if err != nil {
		return mcpserver.ProxyInfo{}, err
	}
	return b.proxyInfo(rec), nil
}

// DeleteProxy disables the proxy for a box and port.
//
// @arg _ Context (unused).
// @arg boxID The box ID of the proxy to remove.
// @arg port The port of the proxy to remove.
// @error error if no such proxy exists.
//
// @testcase TestMCPBackendProxies disables a proxy through the backend.
func (b mcpBackend) DeleteProxy(_ context.Context, boxID string, port int) error {
	_, err := b.s.deleteProxy(boxID, port)
	return err
}

// ListProxies returns the enabled proxies (optionally filtered to one box) as
// mcpserver.ProxyInfo values carrying each proxy's public URL.
//
// @arg _ Context (unused).
// @arg boxID The box ID to filter by, or "" for all proxies.
// @return []mcpserver.ProxyInfo The matching proxies.
// @error error if the proxies cannot be listed.
//
// @testcase TestMCPBackendProxies lists proxies through the backend.
func (b mcpBackend) ListProxies(_ context.Context, boxID string) ([]mcpserver.ProxyInfo, error) {
	recs, err := b.s.listProxies(boxID)
	if err != nil {
		return nil, err
	}
	out := make([]mcpserver.ProxyInfo, len(recs))
	for i, rec := range recs {
		out[i] = b.proxyInfo(rec)
	}
	return out, nil
}

// proxyInfo flattens a stored proxy record into the mcpserver.ProxyInfo the
// tools surface, resolving the public URL from the slug.
//
// @arg rec The stored proxy record.
// @return mcpserver.ProxyInfo The flattened proxy with its public URL.
//
// @testcase TestMCPBackendProxies checks the proxy info carries the URL.
func (b mcpBackend) proxyInfo(rec store.ProxyRecord) mcpserver.ProxyInfo {
	return mcpserver.ProxyInfo{
		BoxID: rec.BoxID,
		Port:  rec.Port,
		URL:   b.s.proxyURL(rec.Slug),
		Slug:  rec.Slug,
		Spoke: rec.Spoke,
	}
}
