// Package mcpserver exposes the llmbox box operations as MCP tools. It is
// deliberately decoupled from the HTTP server: every box operation it needs is
// reached through the Backend interface, so the tool layer can be built and
// tested in isolation from Docker, the session store, and the cluster.
package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/clems4ever/llmbox/internal/shared/api"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// The box-operation contract and its data types live in internal/api (the neutral
// HTTP box-control layer). They are re-exported here so the MCP tools can refer to
// them without importing api at every use site.
type (
	// Backend is everything the MCP tool layer needs from the box-control backend.
	Backend = api.Backend
	// BoxSession is the subset of a box's state the tools surface.
	BoxSession = api.BoxSession
	// ProxyInfo describes one enabled HTTP proxy surfaced by the proxy tools.
	ProxyInfo = api.ProxyInfo
	// SpokeStatus describes one cluster spoke and its health for the list_spokes tool.
	SpokeStatus = api.SpokeStatus
)

// handlers binds the tool handlers to a backend so they can be registered on an
// MCP server.
type handlers struct{ b Backend }

// NewServer builds an MCP server exposing the box tools backed by b. The OAuth
// secret is never an input or output of any tool: create returns only an auth
// page URL.
//
// @arg b The backend providing the box operations the tools invoke.
// @arg name The MCP server implementation name.
// @arg version The MCP server implementation version.
// @return *mcp.Server An MCP server with the create/get/list/destroy tools registered.
//
// @testcase TestToolsRegistered checks all tools are registered.
// @testcase TestCreateReturnsSafeAuthURL checks create returns the auth page URL, never a secret.
func NewServer(b Backend, name, version string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: version}, nil)
	h := &handlers{b: b}

	mcp.AddTool(srv, &mcp.Tool{
		Name: "create_llmbox",
		Description: "Create a new sandboxed Claude (an 'llmbox'). Returns a URL the " +
			"user must open to authenticate the box with their own Claude account. " +
			"The user authenticates in their browser; no token or code is handled here.",
	}, h.toolCreate)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_llmbox",
		Description: "Check an llmbox's status by its box ID (the one given to create_llmbox). Once authenticated, returns the remote-control session URL.",
	}, h.toolGet)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_llmboxes",
		Description: "List the sandboxed Claude boxes managed by this server.",
	}, h.toolList)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_spokes",
		Description: "List the cluster spokes (hosts that can run boxes) and whether each is currently connected. 'local' is this server's own host; pass a connected spoke's name to create_llmbox's 'spoke' argument to place a box there.",
	}, h.toolListSpokes)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "destroy_llmbox",
		Description: "Stop and remove an llmbox by its box ID (the one given to create_llmbox).",
	}, h.toolDestroy)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_llmbox_logs",
		Description: "Read the recent console output (logs) of an llmbox by its box ID (the one given to create_llmbox). Optionally limit the number of trailing lines with 'tail'.",
	}, h.toolLogs)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "exec_llmbox",
		Description: "Run a shell command inside an llmbox by its box ID (the one given to create_llmbox). The command runs via /bin/sh -c; returns its stdout, stderr, and exit code.",
	}, h.toolExec)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "create_llmbox_proxy",
		Description: "Expose an HTTP server running inside an llmbox so the user can reach it from their browser. " +
			"Give the box ID and the port the server listens on inside the box; returns a URL the user opens to reach it. " +
			"Optionally attach a human-readable description to record what the proxy is for; it is shown by list_llmbox_proxies. " +
			"No port is reachable until you enable it here (default-deny). Use this after starting a server in the box (e.g. via exec_llmbox or pm2).",
	}, h.toolCreateProxy)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "delete_llmbox_proxy",
		Description: "Disable a previously created llmbox proxy, identified by its box ID and port, so the URL stops working.",
	}, h.toolDeleteProxy)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_llmbox_proxies",
		Description: "List the enabled llmbox HTTP proxies, their URLs, and any descriptions. Optionally filter to one box by its box ID.",
	}, h.toolListProxies)

	return srv
}

type createInput struct {
	Image       string `json:"image,omitempty" jsonschema:"optional image to launch; defaults to the configured Claude image"`
	BoxID       string `json:"box_id" jsonschema:"required box ID to assign; used to reference the box later (get/destroy/logs/exec) and used as the box's hostname, which is the name the user sees in claude.ai/code. Pick a unique, human-readable string that conveys what the box is for (e.g. 'refactor-auth-service'), since it is what identifies the box to the user. Must be a valid hostname (lowercase letters, digits and hyphens) and unique across boxes (creation fails if another box already uses it)"`
	Description string `json:"description,omitempty" jsonschema:"optional human-readable description shown in list and get to tell boxes apart"`
	Spoke       string `json:"spoke,omitempty" jsonschema:"optional cluster spoke to create the box on; omit (or 'local') to use the server's own host. Use a spoke name returned by list_llmboxes when boxes should run on a remote Docker host"`
}

type createOutput struct {
	BoxID        string `json:"box_id" jsonschema:"the box ID you assigned; pass it to get/destroy/logs/exec to reference this box"`
	InstanceID   string `json:"instance_id" jsonschema:"the backend instance ID of the new box (e.g. a short container ID or microVM ID)"`
	AuthURL      string `json:"auth_url" jsonschema:"URL the user opens to authenticate the box in their browser"`
	AuthToken    string `json:"auth_token" jsonschema:"token identifying this box's auth session (already embedded in auth_url); to poll status, call get_llmbox with the box's box ID"`
	Status       string `json:"status" jsonschema:"current status; starts as 'pending' until the user authenticates"`
	Instructions string `json:"instructions" jsonschema:"human-readable next steps for the user"`
}

// toolCreate handles the create_llmbox tool: it launches a box with the given
// image, box ID, description, and optional target spoke, and returns the auth
// page URL and token.
//
// @arg ctx Context for the box creation.
// @arg _ The MCP call request (unused).
// @arg in The create input carrying the required box ID and an optional image, description, and spoke.
// @return *mcp.CallToolResult Always nil; structured output is returned instead.
// @return createOutput The box ID, instance ID, auth URL, token, status, and instructions.
// @error error if box_id is empty, or the box cannot be created.
//
// @testcase TestToolCreate calls create_llmbox and checks the auth URL and token.
// @testcase TestToolCreateRequiresBoxID rejects a create_llmbox call with an empty box ID.
func (h *handlers) toolCreate(ctx context.Context, _ *mcp.CallToolRequest, in createInput) (*mcp.CallToolResult, createOutput, error) {
	if in.BoxID == "" {
		return nil, createOutput{}, fmt.Errorf("box_id is required")
	}
	sess, err := h.b.CreateBox(ctx, sandbox.CreateOptions{
		Image:       in.Image,
		BoxID:       in.BoxID,
		Description: in.Description,
		SpokeName:   in.Spoke,
	})
	if err != nil {
		return nil, createOutput{}, err
	}
	return nil, createOutput{
		BoxID:      sess.BoxID,
		InstanceID: shortID(sess.ContainerID),
		AuthURL:    h.b.AuthPageURL(sess.Token),
		AuthToken:  sess.Token,
		Status:     "pending",
		Instructions: "Open the auth_url in a browser, sign in with your Claude account, " +
			"and paste the code shown by Claude into that page. The box activates once you finish.",
	}, nil
}

type getInput struct {
	BoxID string `json:"box_id" jsonschema:"the box ID of the box (the one passed to create_llmbox)"`
}

type getOutput struct {
	Status      string `json:"status" jsonschema:"pending, ready, or error"`
	BoxID       string `json:"box_id,omitempty" jsonschema:"the box ID assigned to the box, if any"`
	Description string `json:"description,omitempty" jsonschema:"the description supplied when the box was created, if any"`
	SessionURL  string `json:"session_url,omitempty" jsonschema:"remote-control session URL, present once ready"`
	Error       string `json:"error,omitempty" jsonschema:"error detail when status is error"`
}

// toolGet handles the get_llmbox tool: it looks up a box's session by box ID
// and returns its status, box ID, description, and session URL.
//
// @arg _ Context (unused).
// @arg _ The MCP call request (unused).
// @arg in The get input carrying the box ID.
// @return *mcp.CallToolResult Always nil; structured output is returned instead.
// @return getOutput The box's status, box ID, description, session URL, and any error.
// @error error if no box ID is given or no box has that box ID.
//
// @testcase TestToolGet returns a box's status looked up by box ID.
func (h *handlers) toolGet(_ context.Context, _ *mcp.CallToolRequest, in getInput) (*mcp.CallToolResult, getOutput, error) {
	if in.BoxID == "" {
		return nil, getOutput{}, fmt.Errorf("box_id is required")
	}
	sess, ok := h.b.LookupByBoxID(in.BoxID)
	if !ok {
		return nil, getOutput{}, fmt.Errorf("no box found with box ID %q (it may have expired, or was created without a box ID)", in.BoxID)
	}
	return nil, getOutput{
		Status:      sess.Status,
		BoxID:       sess.BoxID,
		Description: sess.Description,
		SessionURL:  sess.SessionURL,
		Error:       sess.Error,
	}, nil
}

type listOutput struct {
	Boxes []sandbox.Box `json:"boxes" jsonschema:"the boxes managed by this server"`
}

// toolList handles the list_llmboxes tool: it returns all managed boxes.
//
// @arg ctx Context for the list request.
// @arg _ The MCP call request (unused).
// @arg _ The empty tool input (unused).
// @return *mcp.CallToolResult Always nil; structured output is returned instead.
// @return listOutput The managed boxes.
// @error error if listing boxes fails.
//
// @testcase TestToolList returns the managed boxes with their box IDs and descriptions.
func (h *handlers) toolList(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listOutput, error) {
	boxes, err := h.b.ListBoxes(ctx)
	if err != nil {
		return nil, listOutput{}, err
	}
	return nil, listOutput{Boxes: boxes}, nil
}

type listSpokesOutput struct {
	Spokes []SpokeStatus `json:"spokes" jsonschema:"the cluster spokes and their connection status"`
}

// toolListSpokes handles the list_spokes tool: it returns every spoke (the
// in-process 'local' spoke plus each enrolled remote spoke) and whether each is
// currently connected, so a caller can pick a healthy spoke for create_llmbox.
//
// @arg ctx Context for the request.
// @arg _ The MCP call request (unused).
// @arg _ The empty tool input (unused).
// @return *mcp.CallToolResult Always nil; structured output is returned instead.
// @return listSpokesOutput The spokes and their connection status.
// @error error if the enrolled spokes cannot be read.
//
// @testcase TestToolListSpokes returns the spokes with their connection status.
func (h *handlers) toolListSpokes(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listSpokesOutput, error) {
	spokes, err := h.b.SpokeStatuses(ctx)
	if err != nil {
		return nil, listSpokesOutput{}, err
	}
	return nil, listSpokesOutput{Spokes: spokes}, nil
}

type destroyInput struct {
	BoxID string `json:"box_id" jsonschema:"the box ID of the box to destroy (the one passed to create_llmbox)"`
}

type destroyOutput struct {
	Destroyed string `json:"destroyed" jsonschema:"the box ID of the box that was destroyed"`
}

// toolDestroy handles the destroy_llmbox tool: it looks up a box's session by
// box ID and stops and removes that box.
//
// @arg ctx Context for the destroy request.
// @arg _ The MCP call request (unused).
// @arg in The destroy input carrying the box ID.
// @return *mcp.CallToolResult Always nil; structured output is returned instead.
// @return destroyOutput The box ID of the box that was destroyed.
// @error error if no box ID is given, no box has that box ID, or the box cannot be destroyed.
//
// @testcase TestToolDestroy destroys a box looked up by box ID.
func (h *handlers) toolDestroy(ctx context.Context, _ *mcp.CallToolRequest, in destroyInput) (*mcp.CallToolResult, destroyOutput, error) {
	if in.BoxID == "" {
		return nil, destroyOutput{}, fmt.Errorf("box_id is required")
	}
	sess, ok := h.b.LookupByBoxID(in.BoxID)
	if !ok {
		return nil, destroyOutput{}, fmt.Errorf("no box found with box ID %q (it may have expired, or was created without a box ID)", in.BoxID)
	}
	if err := h.b.DestroyBox(ctx, sess.ContainerID); err != nil {
		return nil, destroyOutput{}, err
	}
	return nil, destroyOutput{Destroyed: in.BoxID}, nil
}

type logsInput struct {
	BoxID string `json:"box_id" jsonschema:"the box ID of the box (the one passed to create_llmbox)"`
	Tail  int    `json:"tail,omitempty" jsonschema:"optional maximum number of trailing log lines to return; a sensible default is used when omitted or non-positive"`
}

type logsOutput struct {
	BoxID string `json:"box_id" jsonschema:"the box ID of the box the logs belong to"`
	Logs  string `json:"logs" jsonschema:"the box's recent console output"`
}

// toolLogs handles the get_llmbox_logs tool: it looks up a box's session by
// box ID and returns that box's recent console output.
//
// @arg ctx Context for the logs request.
// @arg _ The MCP call request (unused).
// @arg in The logs input carrying the box ID and optional tail count.
// @return *mcp.CallToolResult Always nil; structured output is returned instead.
// @return logsOutput The box's box ID and recent console output.
// @error error if no box ID is given, no box has that box ID, or the logs cannot be read.
//
// @testcase TestToolLogs returns a box's logs looked up by box ID.
func (h *handlers) toolLogs(ctx context.Context, _ *mcp.CallToolRequest, in logsInput) (*mcp.CallToolResult, logsOutput, error) {
	if in.BoxID == "" {
		return nil, logsOutput{}, fmt.Errorf("box_id is required")
	}
	logs, err := h.b.BoxLogs(ctx, in.BoxID, in.Tail)
	if err != nil {
		return nil, logsOutput{}, err
	}
	return nil, logsOutput{BoxID: in.BoxID, Logs: logs}, nil
}

type execInput struct {
	BoxID   string `json:"box_id" jsonschema:"the box ID of the box (the one passed to create_llmbox)"`
	Command string `json:"command" jsonschema:"the shell command line to run inside the box; executed with /bin/sh -c"`
}

type execOutput struct {
	BoxID    string `json:"box_id" jsonschema:"the box ID of the box the command ran in"`
	Stdout   string `json:"stdout" jsonschema:"the command's standard output"`
	Stderr   string `json:"stderr" jsonschema:"the command's standard error"`
	ExitCode int    `json:"exit_code" jsonschema:"the command's exit code (0 means success)"`
}

// toolExec handles the exec_llmbox tool: it looks up a box's session by box ID
// and runs the given shell command inside it, returning the captured output.
//
// @arg ctx Context for the exec request.
// @arg _ The MCP call request (unused).
// @arg in The exec input carrying the box ID and the command to run.
// @return *mcp.CallToolResult Always nil; structured output is returned instead.
// @return execOutput The box ID plus the command's stdout, stderr, and exit code.
// @error error if no box ID or command is given, no box has that box ID, or the command cannot be run.
//
// @testcase TestToolExec runs a command in a box looked up by box ID.
func (h *handlers) toolExec(ctx context.Context, _ *mcp.CallToolRequest, in execInput) (*mcp.CallToolResult, execOutput, error) {
	if in.BoxID == "" {
		return nil, execOutput{}, fmt.Errorf("box_id is required")
	}
	res, err := h.b.BoxExec(ctx, in.BoxID, in.Command)
	if err != nil {
		return nil, execOutput{}, err
	}
	return nil, execOutput{
		BoxID:    in.BoxID,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		ExitCode: res.ExitCode,
	}, nil
}

type createProxyInput struct {
	BoxID       string `json:"box_id" jsonschema:"the box ID of the box running the server (the one passed to create_llmbox)"`
	Port        int    `json:"port" jsonschema:"the TCP port the server listens on inside the box, e.g. 8000"`
	Description string `json:"description,omitempty" jsonschema:"optional human-readable description of what the proxy is for, shown by list_llmbox_proxies"`
}

type createProxyOutput struct {
	BoxID        string `json:"box_id" jsonschema:"the box ID the proxy points at"`
	Port         int    `json:"port" jsonschema:"the exposed port inside the box"`
	URL          string `json:"url" jsonschema:"the URL to give the user to reach the box's server in their browser"`
	Description  string `json:"description,omitempty" jsonschema:"the optional description recorded for the proxy"`
	Instructions string `json:"instructions" jsonschema:"human-readable next steps for the user"`
}

// toolCreateProxy handles create_llmbox_proxy: it enables an HTTP proxy to a
// box's port and returns the URL the user opens to reach it.
//
// @arg ctx Context for the create.
// @arg _ The MCP call request (unused).
// @arg in The input carrying the box ID, port, and optional description.
// @return *mcp.CallToolResult Always nil; structured output is returned instead.
// @return createProxyOutput The box ID, port, URL, description, and instructions.
// @error error if box_id is empty, the port is invalid, proxying is disabled, or no box has that box ID.
//
// @testcase TestToolCreateProxy enables a proxy, passes the description through, and returns its URL.
// @testcase TestToolCreateProxyRequiresBoxID rejects an empty box ID.
// @testcase TestToolCreateProxyDisabled surfaces the disabled-feature error.
func (h *handlers) toolCreateProxy(ctx context.Context, _ *mcp.CallToolRequest, in createProxyInput) (*mcp.CallToolResult, createProxyOutput, error) {
	if in.BoxID == "" {
		return nil, createProxyOutput{}, fmt.Errorf("box_id is required")
	}
	if in.Port <= 0 {
		return nil, createProxyOutput{}, fmt.Errorf("a positive port is required")
	}
	if !h.b.ProxyEnabled() {
		return nil, createProxyOutput{}, fmt.Errorf("HTTP proxying is not enabled on this server")
	}
	p, err := h.b.CreateProxy(ctx, in.BoxID, in.Port, in.Description)
	if err != nil {
		return nil, createProxyOutput{}, err
	}
	return nil, createProxyOutput{
		BoxID:        p.BoxID,
		Port:         p.Port,
		URL:          p.URL,
		Description:  p.Description,
		Instructions: "Give the user this URL. They must be signed in to llmbox to open it, and the server must be listening on the given port inside the box.",
	}, nil
}

type deleteProxyInput struct {
	BoxID string `json:"box_id" jsonschema:"the box ID of the proxy to disable"`
	Port  int    `json:"port" jsonschema:"the port of the proxy to disable"`
}

type deleteProxyOutput struct {
	BoxID string `json:"box_id" jsonschema:"the box ID whose proxy was disabled"`
	Port  int    `json:"port" jsonschema:"the port whose proxy was disabled"`
}

// toolDeleteProxy handles delete_llmbox_proxy: it disables the proxy for a box
// and port.
//
// @arg ctx Context for the delete.
// @arg _ The MCP call request (unused).
// @arg in The input carrying the box ID and port.
// @return *mcp.CallToolResult Always nil; structured output is returned instead.
// @return deleteProxyOutput The box ID and port whose proxy was disabled.
// @error error if box_id is empty, the port is invalid, or no such proxy exists.
//
// @testcase TestToolDeleteProxy disables a proxy by box and port.
func (h *handlers) toolDeleteProxy(ctx context.Context, _ *mcp.CallToolRequest, in deleteProxyInput) (*mcp.CallToolResult, deleteProxyOutput, error) {
	if in.BoxID == "" {
		return nil, deleteProxyOutput{}, fmt.Errorf("box_id is required")
	}
	if in.Port <= 0 {
		return nil, deleteProxyOutput{}, fmt.Errorf("a positive port is required")
	}
	if err := h.b.DeleteProxy(ctx, in.BoxID, in.Port); err != nil {
		return nil, deleteProxyOutput{}, err
	}
	return nil, deleteProxyOutput{BoxID: in.BoxID, Port: in.Port}, nil
}

type listProxiesInput struct {
	BoxID string `json:"box_id,omitempty" jsonschema:"optional box ID to filter by; omit to list every proxy"`
}

type listProxiesOutput struct {
	Proxies []ProxyInfo `json:"proxies" jsonschema:"the enabled proxies and their URLs"`
}

// toolListProxies handles list_llmbox_proxies: it returns the enabled proxies,
// optionally filtered to one box.
//
// @arg ctx Context for the list.
// @arg _ The MCP call request (unused).
// @arg in The input carrying an optional box ID filter.
// @return *mcp.CallToolResult Always nil; structured output is returned instead.
// @return listProxiesOutput The enabled proxies.
// @error error if the proxies cannot be listed.
//
// @testcase TestToolListProxies lists proxies and filters by box ID.
func (h *handlers) toolListProxies(ctx context.Context, _ *mcp.CallToolRequest, in listProxiesInput) (*mcp.CallToolResult, listProxiesOutput, error) {
	proxies, err := h.b.ListProxies(ctx, in.BoxID)
	if err != nil {
		return nil, listProxiesOutput{}, err
	}
	return nil, listProxiesOutput{Proxies: proxies}, nil
}

// shortID truncates a Docker container ID to its conventional 12-character short
// form, returning it unchanged when it is already shorter.
//
// @arg id The full container ID.
// @return string The 12-character short ID, or id unchanged when shorter.
//
// @testcase TestToolCreate checks the create output carries the short container ID.
func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
