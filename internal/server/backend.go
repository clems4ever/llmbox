package server

import (
	"context"

	"github.com/clems4ever/llmbox/internal/api"
	"github.com/clems4ever/llmbox/internal/sandbox"
	"github.com/clems4ever/llmbox/internal/store"
)

// boxBackend adapts the server to the api.Backend interface — the box-control API
// mounted on the server's HTTP mux (see APIHandler). It flattens the server's
// internal session type into the api.BoxSession callers consume and holds no
// transport concerns.
//
// @return api.Backend The backend the box-control API serves.
//
// @testcase TestBoxToolsOverBackend drives the backend through the box-control API.
func (s *Server) boxBackend() api.Backend {
	return apiBackend{s: s}
}

// apiBackend adapts *Server to api.Backend, translating the server's internal
// session type into the flat api.BoxSession the API surfaces.
type apiBackend struct{ s *Server }

// CreateBox launches a box and returns the flattened session for the API caller.
// Only the box ID, container ID, and token are surfaced; the box's OAuth
// authorize URL is deliberately not exposed so no secret reaches a tool.
//
// @arg ctx Context for the box creation.
// @arg opts The image, box ID, description, and target spoke for the box.
// @return api.BoxSession The new box's ID, container ID, and auth token.
// @error error if the box cannot be created.
//
// @testcase TestBoxToolsOverBackend creates a box through the backend.
func (b apiBackend) CreateBox(ctx context.Context, opts sandbox.CreateOptions) (api.BoxSession, error) {
	sess, err := b.s.createBox(ctx, opts)
	if err != nil {
		return api.BoxSession{}, err
	}
	return api.BoxSession{
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
// @testcase TestBoxToolsOverBackend checks the create output carries the auth page URL.
func (b apiBackend) AuthPageURL(token string) string {
	return b.s.AuthPageURL(token)
}

// LookupByBoxID finds a box's session by its box ID and flattens its mutable
// state (status, session URL, error) into an api.BoxSession.
//
// @arg boxID The box ID to look up.
// @return api.BoxSession The matching box's flattened session (zero value when ok is false).
// @return bool Whether a box with that box ID exists.
//
// @testcase TestGetByBoxID looks a box up by its box ID through the backend.
func (b apiBackend) LookupByBoxID(boxID string) (api.BoxSession, bool) {
	sess := b.s.lookupByBoxID(boxID)
	if sess == nil {
		return api.BoxSession{}, false
	}
	status, url, errMsg := sess.snapshot()
	return api.BoxSession{
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
// @return []sandbox.Box The boxes managed by this server.
// @error error if listing boxes fails.
//
// @testcase TestListLlmboxesReturnsBoxID lists boxes through the backend.
func (b apiBackend) ListBoxes(ctx context.Context) ([]sandbox.Box, error) {
	return b.s.listBoxes(ctx)
}

// SpokeStatuses returns every spoke and its connection status, translated to the
// api.SpokeStatus shape the tool reports.
//
// @arg ctx Context for the request.
// @return []api.SpokeStatus The spokes and their connection status.
// @error error if the enrolled spokes cannot be read.
//
// @testcase TestListSpokesTool reports the spoke statuses through the backend.
func (b apiBackend) SpokeStatuses(ctx context.Context) ([]api.SpokeStatus, error) {
	spokes, err := b.s.SpokeStatuses(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]api.SpokeStatus, len(spokes))
	for i, sp := range spokes {
		out[i] = api.SpokeStatus{
			Name:       sp.Name,
			Connected:  sp.Connected,
			Default:    sp.Default,
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
func (b apiBackend) DestroyBox(ctx context.Context, containerID string) error {
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
func (b apiBackend) BoxLogs(ctx context.Context, boxID string, tail int) (string, error) {
	return b.s.boxLogs(ctx, boxID, tail)
}

// BoxExec runs a shell command inside the box with the given box ID.
//
// @arg ctx Context for the exec request.
// @arg boxID The box ID of the box to run the command in.
// @arg command The shell command line to run inside the box.
// @return sandbox.ExecResult The command's stdout, stderr, and exit code.
// @error error if the command is empty, no box has that box ID, or it cannot be run.
//
// @testcase TestBoxExecByBoxID runs a command through the backend.
func (b apiBackend) BoxExec(ctx context.Context, boxID, command string) (sandbox.ExecResult, error) {
	return b.s.boxExec(ctx, boxID, command)
}

// ProxyEnabled reports whether the HTTP proxy feature is configured.
//
// @return bool True when proxying is enabled.
//
// @testcase TestBackendProxies reports proxy enablement through the backend.
func (b apiBackend) ProxyEnabled() bool { return b.s.ProxyEnabled() }

// CreateProxy enables an HTTP proxy to a box's port and flattens it (with its
// public URL) into the api.ProxyInfo the tool returns.
//
// @arg _ Context (unused; the registry is in-memory and in the store).
// @arg boxID The box ID whose port to expose.
// @arg port The port inside the box to forward to.
// @arg description An optional human-readable note for the proxy, or "" for none.
// @return api.ProxyInfo The new proxy's box ID, port, URL, slug, spoke, and description.
// @error error if proxying is disabled, the port is invalid, or no box has that box ID.
//
// @testcase TestBackendProxies enables a proxy through the backend and carries its description.
func (b apiBackend) CreateProxy(_ context.Context, boxID string, port int, description string) (api.ProxyInfo, error) {
	rec, err := b.s.createProxy(boxID, port, "", description)
	if err != nil {
		return api.ProxyInfo{}, err
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
// @testcase TestBackendProxies disables a proxy through the backend.
func (b apiBackend) DeleteProxy(_ context.Context, boxID string, port int) error {
	_, err := b.s.deleteProxy(boxID, port)
	return err
}

// ListProxies returns the enabled proxies (optionally filtered to one box) as
// api.ProxyInfo values carrying each proxy's public URL.
//
// @arg _ Context (unused).
// @arg boxID The box ID to filter by, or "" for all proxies.
// @return []api.ProxyInfo The matching proxies.
// @error error if the proxies cannot be listed.
//
// @testcase TestBackendProxies lists proxies through the backend.
func (b apiBackend) ListProxies(_ context.Context, boxID string) ([]api.ProxyInfo, error) {
	recs, err := b.s.listProxies(boxID)
	if err != nil {
		return nil, err
	}
	out := make([]api.ProxyInfo, len(recs))
	for i, rec := range recs {
		out[i] = b.proxyInfo(rec)
	}
	return out, nil
}

// proxyInfo flattens a stored proxy record into the api.ProxyInfo the
// tools surface, resolving the public URL from the slug and carrying the
// optional description through.
//
// @arg rec The stored proxy record.
// @return api.ProxyInfo The flattened proxy with its public URL and description.
//
// @testcase TestBackendProxies checks the proxy info carries the URL and description.
func (b apiBackend) proxyInfo(rec store.ProxyRecord) api.ProxyInfo {
	return api.ProxyInfo{
		BoxID:       rec.BoxID,
		Port:        rec.Port,
		URL:         b.s.proxyURL(rec.Slug),
		Slug:        rec.Slug,
		Spoke:       rec.Spoke,
		Description: rec.Description,
	}
}
