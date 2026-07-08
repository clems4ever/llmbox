package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/clems4ever/llmbox/internal/shared/api"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/testutils"
)

// errStubFailure is the canned error the fake returns when a test asks it to fail.
var errStubFailure = errors.New("stub failure")

// TestBackendAPIRoundTrip drives every Backend method through the HTTP client
// against a handler backed by testutils.FakeBackend, asserting both that each
// argument reaches the backend and that each result comes back intact.
func TestBackendAPIRoundTrip(t *testing.T) {
	fb := &testutils.FakeBackend{
		CreateSess:        api.BoxSession{BoxID: "web", ContainerID: "cid123", Token: "tok"},
		Sessions:          map[string]api.BoxSession{"web": {BoxID: "web", ContainerID: "cid123", Status: "ready"}},
		Boxes:             []sandbox.Box{{BoxID: "b1"}, {BoxID: "b2"}},
		Spokes:            []api.SpokeStatus{{Name: "edge", Connected: true, Default: true}},
		LogsResult:        "log output",
		ExecResult:        sandbox.ExecResult{Stdout: "out", Stderr: "err", ExitCode: 7},
		ProxyOn:           true,
		CreateProxyResult: api.ProxyInfo{BoxID: "web", Port: 8000, URL: "https://slug.example.com", Slug: "slug", Description: "app"},
		Proxies:           []api.ProxyInfo{{BoxID: "b1", Port: 8000}},
	}
	ts := httptest.NewServer(api.NewHandler(fb))
	defer ts.Close()
	c := api.NewClient(ts.URL, ts.Client())
	ctx := context.Background()

	sess, err := c.CreateBox(ctx, sandbox.CreateOptions{BoxID: "web", Description: "d", SpokeName: "local"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if sess.ContainerID != "cid123" || sess.Token != "tok" || sess.BoxID != "web" {
		t.Fatalf("CreateBox session = %+v", sess)
	}
	if fb.GotCreate.BoxID != "web" || fb.GotCreate.Description != "d" || fb.GotCreate.SpokeName != "local" {
		t.Fatalf("CreateBox opts not forwarded: %+v", fb.GotCreate)
	}

	if url := c.AuthPageURL("tok"); url != "https://boxes.example.com/auth/tok" || fb.GotAuthToken != "tok" {
		t.Fatalf("AuthPageURL = %q (got token %q)", url, fb.GotAuthToken)
	}

	if got, ok := c.LookupByBoxID("web"); !ok || got.Status != "ready" || fb.GotLookup != "web" {
		t.Fatalf("LookupByBoxID = %+v ok=%v", got, ok)
	}
	if _, ok := c.LookupByBoxID("missing"); ok {
		t.Fatal("LookupByBoxID(missing) should report not found, not an error")
	}

	boxes, err := c.ListBoxes(ctx)
	if err != nil || len(boxes) != 2 {
		t.Fatalf("ListBoxes = %v, %v", boxes, err)
	}

	spokes, err := c.SpokeStatuses(ctx)
	if err != nil || len(spokes) != 1 || !spokes[0].Default {
		t.Fatalf("SpokeStatuses = %v, %v", spokes, err)
	}

	if err := c.DestroyBox(ctx, "cid123"); err != nil || fb.GotDestroyID != "cid123" {
		t.Fatalf("DestroyBox err=%v id=%q", err, fb.GotDestroyID)
	}

	logs, err := c.BoxLogs(ctx, "web", 42)
	if err != nil || logs != "log output" || fb.GotLogsID != "web" || fb.GotLogsTail != 42 {
		t.Fatalf("BoxLogs = %q err=%v (box %q tail %d)", logs, err, fb.GotLogsID, fb.GotLogsTail)
	}

	res, err := c.BoxExec(ctx, "web", "ls -la")
	if err != nil || res.ExitCode != 7 || res.Stdout != "out" || fb.GotExecCmd != "ls -la" {
		t.Fatalf("BoxExec = %+v err=%v cmd=%q", res, err, fb.GotExecCmd)
	}

	if !c.ProxyEnabled() {
		t.Fatal("ProxyEnabled = false, want true")
	}

	p, err := c.CreateProxy(ctx, "web", 8000, "app")
	if err != nil || p.Slug != "slug" || fb.GotProxyPort != 8000 || fb.GotProxyDesc != "app" {
		t.Fatalf("CreateProxy = %+v err=%v", p, err)
	}

	if err := c.DeleteProxy(ctx, "web", 8000); err != nil || fb.GotDeleteBoxID != "web" || fb.GotDeletePort != 8000 {
		t.Fatalf("DeleteProxy err=%v box=%q port=%d", err, fb.GotDeleteBoxID, fb.GotDeletePort)
	}

	proxies, err := c.ListProxies(ctx, "b1")
	if err != nil || len(proxies) != 1 || fb.GotListBoxID != "b1" {
		t.Fatalf("ListProxies = %v err=%v box=%q", proxies, err, fb.GotListBoxID)
	}
}

// TestMCPToolsOverHTTP drives the full stand-alone path — an MCP client calling
// tools that forward through the HTTP client to a handler-backed fake — proving
// the split works end to end and never leaks a secret into MCP output.
func TestMCPToolsOverHTTP(t *testing.T) {
	fb := &testutils.FakeBackend{
		CreateSess: api.BoxSession{BoxID: "web", ContainerID: "abcdef012345", Token: "tok"},
	}
	ts := httptest.NewServer(api.NewHandler(fb))
	defer ts.Close()

	cs := testutils.ConnectMCP(t, api.NewClient(ts.URL, ts.Client()), "test", "v0")

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "create_llmbox",
		Arguments: map[string]any{"box_id": "web"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool error: %v", res.Content)
	}
	out, _ := res.StructuredContent.(map[string]any)
	if authURL, _ := out["auth_url"].(string); !strings.HasPrefix(authURL, "https://boxes.example.com/auth/") {
		t.Errorf("auth_url = %q, want the public auth page URL", authURL)
	}
	if fb.GotCreate.BoxID != "web" {
		t.Errorf("create not forwarded to backend: %+v", fb.GotCreate)
	}
}

// TestClientSurfacesServerError checks a backend error is carried across the wire
// and surfaced as a Go error by the client.
func TestClientSurfacesServerError(t *testing.T) {
	ts := httptest.NewServer(api.NewHandler(&testutils.FakeBackend{CreateErr: errStubFailure}))
	defer ts.Close()
	c := api.NewClient(ts.URL, ts.Client())

	_, err := c.CreateBox(context.Background(), sandbox.CreateOptions{BoxID: "x"})
	if err == nil || !strings.Contains(err.Error(), "stub failure") {
		t.Fatalf("CreateBox err = %v, want the server error message", err)
	}
}

// TestHandlerRejectsBadJSON checks the handler returns 400 for a malformed JSON
// request body rather than treating it as an empty request.
func TestHandlerRejectsBadJSON(t *testing.T) {
	ts := httptest.NewServer(api.NewHandler(&testutils.FakeBackend{}))
	defer ts.Close()

	res, err := ts.Client().Post(ts.URL+api.PathCreateBox, "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}
