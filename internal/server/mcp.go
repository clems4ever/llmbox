package server

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/clems4ever/llmbox-mcp/internal/docker"
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
		Description: "Check an llmbox's status by its hostname (the one given to create_llmbox). Once authenticated, returns the remote-control session URL.",
	}, s.toolGet)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_llmboxes",
		Description: "List the sandboxed Claude boxes managed by this server.",
	}, s.toolList)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "destroy_llmbox",
		Description: "Stop and remove an llmbox by its hostname (the one given to create_llmbox).",
	}, s.toolDestroy)

	return srv
}

type createInput struct {
	Image       string `json:"image,omitempty" jsonschema:"optional image to launch; defaults to the configured Claude image"`
	Hostname    string `json:"hostname,omitempty" jsonschema:"optional hostname to set on the box; must be a valid hostname and unique across boxes (creation fails if another box already uses it)"`
	Description string `json:"description,omitempty" jsonschema:"optional human-readable description shown in list and get to tell boxes apart"`
}

type createOutput struct {
	BoxID        string `json:"box_id" jsonschema:"the ID of the new box"`
	AuthURL      string `json:"auth_url" jsonschema:"URL the user opens to authenticate the box in their browser"`
	AuthToken    string `json:"auth_token" jsonschema:"token identifying this box's auth session (already embedded in auth_url); to poll status, call get_llmbox with the box's hostname"`
	Status       string `json:"status" jsonschema:"current status; starts as 'pending' until the user authenticates"`
	Instructions string `json:"instructions" jsonschema:"human-readable next steps for the user"`
}

// toolCreate handles the create_llmbox tool: it launches a box with the given
// image, hostname, and description, and returns the auth page URL and token.
//
// @arg ctx Context for the box creation.
// @arg _ The MCP call request (unused).
// @arg in The create input carrying optional image, hostname, and description.
// @return *mcp.CallToolResult Always nil; structured output is returned instead.
// @return createOutput The box ID, auth URL, token, status, and instructions.
// @error error if the box cannot be created.
//
// @testcase TestMCPToolsRegisteredAndCreate calls create_llmbox and checks the auth URL.
func (s *Server) toolCreate(ctx context.Context, _ *mcp.CallToolRequest, in createInput) (*mcp.CallToolResult, createOutput, error) {
	sess, err := s.CreateBox(ctx, docker.CreateOptions{
		Image:       in.Image,
		Hostname:    in.Hostname,
		Description: in.Description,
	})
	if err != nil {
		return nil, createOutput{}, err
	}
	return nil, createOutput{
		BoxID:     sess.BoxID[:12],
		AuthURL:   s.AuthPageURL(sess.Token),
		AuthToken: sess.Token,
		Status:    "pending",
		Instructions: "Open the auth_url in a browser, sign in with your Claude account, " +
			"and paste the code shown by Claude into that page. The box activates once you finish.",
	}, nil
}

type getInput struct {
	Hostname string `json:"hostname" jsonschema:"the hostname of the box (the one passed to create_llmbox)"`
}

type getOutput struct {
	Status      string `json:"status" jsonschema:"pending, ready, or error"`
	Hostname    string `json:"hostname,omitempty" jsonschema:"the hostname set on the box, if any"`
	Description string `json:"description,omitempty" jsonschema:"the description supplied when the box was created, if any"`
	SessionURL  string `json:"session_url,omitempty" jsonschema:"remote-control session URL, present once ready"`
	Error       string `json:"error,omitempty" jsonschema:"error detail when status is error"`
}

// toolGet handles the get_llmbox tool: it looks up a box's session by hostname
// and returns its status, hostname, description, and session URL.
//
// @arg _ Context (unused).
// @arg _ The MCP call request (unused).
// @arg in The get input carrying the box hostname.
// @return *mcp.CallToolResult Always nil; structured output is returned instead.
// @return getOutput The box's status, hostname, description, session URL, and any error.
// @error error if no hostname is given or no box has that hostname.
//
// @testcase TestGetByHostname returns a box's status looked up by hostname.
func (s *Server) toolGet(_ context.Context, _ *mcp.CallToolRequest, in getInput) (*mcp.CallToolResult, getOutput, error) {
	if in.Hostname == "" {
		return nil, getOutput{}, fmt.Errorf("hostname is required")
	}
	sess := s.lookupByHostname(in.Hostname)
	if sess == nil {
		return nil, getOutput{}, fmt.Errorf("no box found with hostname %q (it may have expired, or was created without a hostname)", in.Hostname)
	}
	status, url, errMsg := sess.snapshot()
	return nil, getOutput{
		Status:      status,
		Hostname:    sess.Hostname,
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
func (s *Server) toolList(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listOutput, error) {
	boxes, err := s.ListBoxes(ctx)
	if err != nil {
		return nil, listOutput{}, err
	}
	return nil, listOutput{Boxes: boxes}, nil
}

type destroyInput struct {
	Hostname string `json:"hostname" jsonschema:"the hostname of the box to destroy (the one passed to create_llmbox)"`
}

type destroyOutput struct {
	Destroyed string `json:"destroyed" jsonschema:"the hostname of the box that was destroyed"`
}

// toolDestroy handles the destroy_llmbox tool: it looks up a box's session by
// hostname and stops and removes that box.
//
// @arg ctx Context for the destroy request.
// @arg _ The MCP call request (unused).
// @arg in The destroy input carrying the box hostname.
// @return *mcp.CallToolResult Always nil; structured output is returned instead.
// @return destroyOutput The hostname of the box that was destroyed.
// @error error if no hostname is given, no box has that hostname, or the box cannot be destroyed.
//
// @testcase TestMCPToolsRegisteredAndCreate checks the destroy_llmbox tool is registered.
func (s *Server) toolDestroy(ctx context.Context, _ *mcp.CallToolRequest, in destroyInput) (*mcp.CallToolResult, destroyOutput, error) {
	if in.Hostname == "" {
		return nil, destroyOutput{}, fmt.Errorf("hostname is required")
	}
	sess := s.lookupByHostname(in.Hostname)
	if sess == nil {
		return nil, destroyOutput{}, fmt.Errorf("no box found with hostname %q (it may have expired, or was created without a hostname)", in.Hostname)
	}
	if err := s.DestroyBox(ctx, sess.BoxID); err != nil {
		return nil, destroyOutput{}, err
	}
	return nil, destroyOutput{Destroyed: in.Hostname}, nil
}
