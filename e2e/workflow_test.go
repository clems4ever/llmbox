//go:build e2e

// Package e2e holds the end-to-end tests for llmbox. They wire the real server
// (MCP tools + the admin web UI) on a real HTTP listener, drive the chatbot side
// over a real MCP client and the human side through a real browser via WebDriver,
// and simulate only the Docker box layer.
//
// Run them with the e2e build tag (they are excluded from the default unit suite):
//
//	make test-e2e        # or: go test -tags e2e ./e2e/...
package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/clems4ever/llmbox/internal/hub"
	"github.com/clems4ever/llmbox/internal/hub/apikey"
	"github.com/clems4ever/llmbox/internal/shared/api"
	"github.com/clems4ever/llmbox/testutils"
)

// waitHealthy blocks until the server answers /healthz, so the test does not
// race the listener's startup.
func waitHealthy(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never became healthy at %s: %v", base, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// connectMCP builds an MCP session standing in for the chatbot: it wraps an
// api client pointed at the server's box-control API in an MCP server (exactly
// what the llmbox-mcp binary does) and connects an in-memory MCP client to it, so
// tool calls travel over the real box-control HTTP API to the server. The API is
// authenticated, so a key is minted into st — exactly what a deployed llmbox-mcp
// is given.
//
// @arg t The test, used for fatal errors and cleanup.
// @arg base The server's box-control API base URL.
// @arg st The hub's store an API key is minted into.
// @return *mcp.ClientSession A connected MCP client session.
func connectMCP(t *testing.T, base string, st hub.Store) *mcp.ClientSession {
	t.Helper()
	key, err := apikey.Create(st, "e2e-mcp", time.Hour, time.Now())
	if err != nil {
		t.Fatalf("mint api key: %v", err)
	}
	c := api.NewClient(base, nil)
	c.SetAPIKey(key)
	return testutils.ConnectMCP(t, c, "llmbox", "e2e")
}

// callTool calls an MCP tool and returns its structured output, failing the test
// on a transport or tool error.
//
// @arg t The test, failed on any error.
// @arg cs The MCP client session.
// @arg name The tool name.
// @arg args The tool arguments.
// @return map[string]any The tool's structured output.
func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	out, err := callToolRaw(t, cs, name, args)
	if err != nil {
		t.Fatalf("tool %s: %v", name, err)
	}
	return out
}

// callToolRaw calls an MCP tool and returns its structured output and any tool
// error, so callers can assert on the error path. A transport-level failure is
// still fatal.
//
// @arg t The test, failed only on transport errors.
// @arg cs The MCP client session.
// @arg name The tool name.
// @arg args The tool arguments.
// @return map[string]any The tool's structured output (nil on a tool error).
// @error error the tool's reported error, if any.
func callToolRaw(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (map[string]any, error) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("calling %s: %v", name, err)
	}
	if res.IsError {
		return nil, &toolError{name: name}
	}
	out, _ := res.StructuredContent.(map[string]any)
	return out, nil
}

// toolError reports that an MCP tool returned an error result.
type toolError struct{ name string }

// Error renders the tool error.
func (e *toolError) Error() string { return e.name + " returned a tool error" }
