package server

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/clems4ever/llmbox/internal/docker"
)

// MCPServer builds an MCP server exposing the box tools. The OAuth secret is
// never an input or output of any tool: create returns only an auth page URL.
//
// @arg name The MCP server implementation name.
// @arg version The MCP server implementation version.
// @return *mcp.Server An MCP server with the create/get/list/destroy tools registered.
//
// @testcase TestMCPToolsRegisteredAndCreate checks all tools are registered and create works.
func (s *Server) MCPServer(name, version string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: version}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "create_llmbox",
		Description: "Create a new sandboxed Claude (an 'llmbox'). Returns a URL the " +
			"user must open to authenticate the box with their own Claude account. " +
			"The user authenticates in their browser; no token or code is handled here.",
	}, s.toolCreate)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_llmbox",
		Description: "Check an llmbox's status by its box ID (the one given to create_llmbox). Once authenticated, returns the remote-control session URL.",
	}, s.toolGet)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_llmboxes",
		Description: "List the sandboxed Claude boxes managed by this server.",
	}, s.toolList)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "destroy_llmbox",
		Description: "Stop and remove an llmbox by its box ID (the one given to create_llmbox).",
	}, s.toolDestroy)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_llmbox_logs",
		Description: "Read the recent console output (logs) of an llmbox by its box ID (the one given to create_llmbox). Optionally limit the number of trailing lines with 'tail'.",
	}, s.toolLogs)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "exec_llmbox",
		Description: "Run a shell command inside an llmbox by its box ID (the one given to create_llmbox). The command runs via /bin/sh -c; returns its stdout, stderr, and exit code.",
	}, s.toolExec)

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
	ContainerID  string `json:"container_id" jsonschema:"the short Docker container ID of the new box"`
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
// @return createOutput The box ID, container ID, auth URL, token, status, and instructions.
// @error error if box_id is empty, or the box cannot be created.
//
// @testcase TestMCPToolsRegisteredAndCreate calls create_llmbox and checks the auth URL.
// @testcase TestCreateRequiresBoxID rejects a create_llmbox call with an empty box ID.
func (s *Server) toolCreate(ctx context.Context, _ *mcp.CallToolRequest, in createInput) (*mcp.CallToolResult, createOutput, error) {
	if in.BoxID == "" {
		return nil, createOutput{}, fmt.Errorf("box_id is required")
	}
	sess, err := s.CreateBox(ctx, docker.CreateOptions{
		Image:       in.Image,
		BoxID:       in.BoxID,
		Description: in.Description,
		SpokeName:   in.Spoke,
	})
	if err != nil {
		return nil, createOutput{}, err
	}
	return nil, createOutput{
		BoxID:       sess.BoxID,
		ContainerID: sess.ContainerID[:12],
		AuthURL:     s.AuthPageURL(sess.Token),
		AuthToken:   sess.Token,
		Status:      "pending",
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
// @testcase TestGetByBoxID returns a box's status looked up by box ID.
func (s *Server) toolGet(_ context.Context, _ *mcp.CallToolRequest, in getInput) (*mcp.CallToolResult, getOutput, error) {
	if in.BoxID == "" {
		return nil, getOutput{}, fmt.Errorf("box_id is required")
	}
	sess := s.lookupByBoxID(in.BoxID)
	if sess == nil {
		return nil, getOutput{}, fmt.Errorf("no box found with box ID %q (it may have expired, or was created without a box ID)", in.BoxID)
	}
	status, url, errMsg := sess.snapshot()
	return nil, getOutput{
		Status:      status,
		BoxID:       sess.BoxID,
		Description: sess.Description,
		SessionURL:  url,
		Error:       errMsg,
	}, nil
}

type listOutput struct {
	Boxes []docker.Box `json:"boxes" jsonschema:"the boxes managed by this server"`
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
// @testcase TestMCPToolsRegisteredAndCreate checks the list_llmboxes tool is registered.
// @testcase TestListLlmboxesReturnsBoxID checks the output carries each box's box ID and description.
func (s *Server) toolList(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listOutput, error) {
	boxes, err := s.ListBoxes(ctx)
	if err != nil {
		return nil, listOutput{}, err
	}
	return nil, listOutput{Boxes: boxes}, nil
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
// @testcase TestMCPToolsRegisteredAndCreate checks the destroy_llmbox tool is registered.
func (s *Server) toolDestroy(ctx context.Context, _ *mcp.CallToolRequest, in destroyInput) (*mcp.CallToolResult, destroyOutput, error) {
	if in.BoxID == "" {
		return nil, destroyOutput{}, fmt.Errorf("box_id is required")
	}
	sess := s.lookupByBoxID(in.BoxID)
	if sess == nil {
		return nil, destroyOutput{}, fmt.Errorf("no box found with box ID %q (it may have expired, or was created without a box ID)", in.BoxID)
	}
	if err := s.DestroyBox(ctx, sess.ContainerID); err != nil {
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
// @testcase TestBoxLogsByBoxID returns a box's logs looked up by box ID.
func (s *Server) toolLogs(ctx context.Context, _ *mcp.CallToolRequest, in logsInput) (*mcp.CallToolResult, logsOutput, error) {
	if in.BoxID == "" {
		return nil, logsOutput{}, fmt.Errorf("box_id is required")
	}
	logs, err := s.BoxLogs(ctx, in.BoxID, in.Tail)
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
// @testcase TestBoxExecByBoxID runs a command in a box looked up by box ID.
func (s *Server) toolExec(ctx context.Context, _ *mcp.CallToolRequest, in execInput) (*mcp.CallToolResult, execOutput, error) {
	if in.BoxID == "" {
		return nil, execOutput{}, fmt.Errorf("box_id is required")
	}
	res, err := s.BoxExec(ctx, in.BoxID, in.Command)
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
