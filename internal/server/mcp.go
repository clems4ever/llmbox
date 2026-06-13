package server

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/clems4ever/llmbox-mcp/internal/docker"
)

// MCPServer builds an MCP server exposing the box tools. The OAuth secret is
// never an input or output of any tool: create returns only an auth page URL.
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
		Description: "Check an llmbox's status using the auth token from its auth URL. Once authenticated, returns the remote-control session URL.",
	}, s.toolGet)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_llmboxes",
		Description: "List the sandboxed Claude boxes managed by this server.",
	}, s.toolList)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "destroy_llmbox",
		Description: "Stop and remove an llmbox by ID or name.",
	}, s.toolDestroy)

	return srv
}

type createInput struct {
	Image string `json:"image,omitempty" jsonschema:"optional image to launch; defaults to the configured Claude image"`
}

type createOutput struct {
	BoxID        string `json:"box_id" jsonschema:"the ID of the new box"`
	AuthURL      string `json:"auth_url" jsonschema:"URL the user opens to authenticate the box in their browser"`
	AuthToken    string `json:"auth_token" jsonschema:"token identifying this box's auth session; pass to get_llmbox to check status"`
	Status       string `json:"status" jsonschema:"current status; starts as 'pending' until the user authenticates"`
	Instructions string `json:"instructions" jsonschema:"human-readable next steps for the user"`
}

func (s *Server) toolCreate(ctx context.Context, _ *mcp.CallToolRequest, in createInput) (*mcp.CallToolResult, createOutput, error) {
	sess, err := s.CreateBox(ctx, in.Image)
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
	AuthToken string `json:"auth_token" jsonschema:"the token from the box's auth URL"`
}

type getOutput struct {
	Status     string `json:"status" jsonschema:"pending, ready, or error"`
	SessionURL string `json:"session_url,omitempty" jsonschema:"remote-control session URL, present once ready"`
	Error      string `json:"error,omitempty" jsonschema:"error detail when status is error"`
}

func (s *Server) toolGet(_ context.Context, _ *mcp.CallToolRequest, in getInput) (*mcp.CallToolResult, getOutput, error) {
	sess := s.lookup(in.AuthToken)
	if sess == nil {
		return nil, getOutput{}, fmt.Errorf("unknown auth token (the box may have expired)")
	}
	status, url, errMsg := sess.snapshot()
	return nil, getOutput{Status: status, SessionURL: url, Error: errMsg}, nil
}

type listOutput struct {
	Boxes []docker.Box `json:"boxes" jsonschema:"the boxes managed by this server"`
}

func (s *Server) toolList(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listOutput, error) {
	boxes, err := s.ListBoxes(ctx)
	if err != nil {
		return nil, listOutput{}, err
	}
	return nil, listOutput{Boxes: boxes}, nil
}

type destroyInput struct {
	Box string `json:"box" jsonschema:"the ID or name of the box to destroy"`
}

type destroyOutput struct {
	Destroyed string `json:"destroyed" jsonschema:"the box that was destroyed"`
}

func (s *Server) toolDestroy(ctx context.Context, _ *mcp.CallToolRequest, in destroyInput) (*mcp.CallToolResult, destroyOutput, error) {
	if in.Box == "" {
		return nil, destroyOutput{}, fmt.Errorf("box ID or name is required")
	}
	if err := s.DestroyBox(ctx, in.Box); err != nil {
		return nil, destroyOutput{}, err
	}
	return nil, destroyOutput{Destroyed: in.Box}, nil
}
