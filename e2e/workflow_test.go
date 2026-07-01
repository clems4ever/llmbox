//go:build e2e

// Package e2e holds the end-to-end workflow test for llmbox. It wires the real
// server (MCP tools + the auth web UI) on a real HTTP listener, drives the
// chatbot side over a real MCP client and the human side through a real browser
// via WebDriver, and simulates only the two external dependencies: the Docker
// box layer and the Anthropic OAuth platform.
//
// Run it with the e2e build tag (it is excluded from the default unit suite):
//
//	make test-e2e        # or: go test -tags e2e ./e2e/...
package e2e

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/clems4ever/llmbox/internal/api"
	"github.com/clems4ever/llmbox/internal/server"
	"github.com/clems4ever/llmbox/testutils"
)

// TestEndToEndWorkflow exercises the full llmbox workflow against simulated
// external dependencies:
//
//  1. the chatbot creates a box over MCP and gets back an auth page URL;
//  2. a real browser opens that page, "signs in with Claude" on the simulated
//     platform, approves access, copies the one-time code, pastes it into the
//     activation form, and submits it;
//  3. the box becomes ready and the page shows its session URL;
//  4. the chatbot confirms readiness over MCP, runs a command in the box, lists
//     it, and finally destroys it.
//
// It also asserts the security property the whole design exists for: the OAuth
// authorize URL and code never appear in any MCP tool output.
func TestEndToEndWorkflow(t *testing.T) {
	// --- simulated external dependencies ---
	platform := newFakeAnthropic()
	t.Cleanup(platform.close)
	mgr := newFakeBoxManager(platform)

	// --- the real llmbox server on a single real listener (UI + box-control API) ---
	uiLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	base := "http://" + uiLn.Addr().String()

	store, err := server.OpenStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// public_url is the server base, so the auth links the user follows live there.
	srv := server.New(mgr, nil, base, 5*time.Minute, store, nil)
	httpSrv := &http.Server{Handler: srv.APIHandler()}
	go func() { _ = httpSrv.Serve(uiLn) }()
	t.Cleanup(func() { _ = httpSrv.Close() })
	waitHealthy(t, base)

	// --- chatbot side: create the box over the box-control API ---
	cs := connectMCP(t, base)

	createOut := callTool(t, cs, "create_llmbox", map[string]any{
		"box_id":      "e2e-box",
		"description": "end-to-end workflow box",
	})
	authURL, _ := createOut["auth_url"].(string)
	boxID, _ := createOut["box_id"].(string)
	if boxID != "e2e-box" {
		t.Fatalf("box_id = %q, want e2e-box", boxID)
	}
	if !strings.HasPrefix(authURL, base+"/auth/") {
		t.Fatalf("auth_url = %q, want it under %s/auth/", authURL, base)
	}
	// The OAuth secret must never travel through MCP: no tool output may carry the
	// authorize URL or the platform host.
	assertNoOAuthLeak(t, "create_llmbox", createOut)

	if got := callTool(t, cs, "get_llmbox", map[string]any{"box_id": boxID})["status"]; got != "pending" {
		t.Fatalf("status before activation = %v, want pending", got)
	}

	// --- human side: drive the auth UI through a real browser ---
	b := newBrowser(t) // skips the test if no chromedriver is available
	t.Cleanup(b.close)
	// When set (CI sets it), the run saves screenshots of the auth page here.
	// Resolve it against the module root so a relative path (as CI passes) lands
	// at the repo-root .github/screenshots the workflow commits, not under e2e/
	// where `go test` would otherwise put it.
	shotDir, err := resolveScreenshotDir(os.Getenv("LLMBOX_E2E_SCREENSHOT_DIR"))
	if err != nil {
		t.Fatalf("resolving screenshot dir: %v", err)
	}

	// Open the activation page; it offers the "Sign in with Claude" link.
	if err := b.wd.Get(authURL); err != nil {
		t.Fatalf("loading auth page: %v", err)
	}
	if src, _ := b.wd.PageSource(); !strings.Contains(src, "Activate your llmbox") {
		t.Fatalf("auth page did not render the activation view:\n%s", src)
	}
	// Capture the activation page for the README when a screenshot dir is set
	// (CI does this); it is otherwise skipped, so a plain test run writes nothing.
	if shotDir != "" {
		b.resizeForScreenshot(t)
		b.saveScreenshot(t, shotDir, "auth-page.png")
		// Also capture the same activation page at a phone viewport for the
		// README, then restore the desktop size so the later "ready" capture
		// (taken without its own resize) still matches auth-page.png.
		b.resizeForMobileScreenshot(t)
		b.saveScreenshot(t, shotDir, "auth-page-mobile.png")
		b.resizeForScreenshot(t)
	}
	signIn := b.waitFor(t, by("css"), "a.btn-link")
	authorizeURL, err := signIn.GetAttribute("href")
	if err != nil {
		t.Fatalf("reading sign-in link: %v", err)
	}
	if !strings.HasPrefix(authorizeURL, platform.srv.URL) {
		t.Fatalf("sign-in link = %q, want it on the simulated platform %s", authorizeURL, platform.srv.URL)
	}

	// "Sign in with Claude": approve access on the platform and read the code.
	if err := b.wd.Get(authorizeURL); err != nil {
		t.Fatalf("opening the platform consent page: %v", err)
	}
	if err := b.waitFor(t, by("id"), "approve").Click(); err != nil {
		t.Fatalf("clicking approve: %v", err)
	}
	code, err := b.waitFor(t, by("id"), "oauth-code").Text()
	if err != nil || strings.TrimSpace(code) == "" {
		t.Fatalf("reading one-time code: code=%q err=%v", code, err)
	}

	// Back on the activation page, paste the code and activate.
	if err := b.wd.Get(authURL); err != nil {
		t.Fatalf("returning to auth page: %v", err)
	}
	if err := b.waitFor(t, by("name"), "code").SendKeys(code); err != nil {
		t.Fatalf("typing the code: %v", err)
	}
	if err := b.waitFor(t, by("id"), "activate").Click(); err != nil {
		t.Fatalf("clicking activate: %v", err)
	}

	// The box is now ready and the page shows its session URL.
	b.waitFor(t, by("css"), ".banner.ok")
	sessionLink := b.waitFor(t, by("css"), ".result a")
	sessionURL, err := sessionLink.GetAttribute("href")
	if err != nil {
		t.Fatalf("reading session URL: %v", err)
	}
	if !strings.HasPrefix(sessionURL, "https://claude.ai/code/") {
		t.Fatalf("session URL = %q, want a claude.ai/code session", sessionURL)
	}
	// Capture the activated ("ready") page too, as a before/after for the README.
	if shotDir != "" {
		b.saveScreenshot(t, shotDir, "auth-ready.png")
	}

	// --- chatbot side: confirm readiness, exec, list, then destroy ---
	getOut := callTool(t, cs, "get_llmbox", map[string]any{"box_id": boxID})
	if getOut["status"] != "ready" {
		t.Fatalf("status after activation = %v, want ready", getOut["status"])
	}
	if getOut["session_url"] != sessionURL {
		t.Fatalf("get_llmbox session_url = %v, want %q (what the UI showed)", getOut["session_url"], sessionURL)
	}

	execOut := callTool(t, cs, "exec_llmbox", map[string]any{"box_id": boxID, "command": "echo hello-from-box"})
	if execOut["stdout"] != "hello-from-box\n" {
		t.Fatalf("exec stdout = %q, want hello-from-box", execOut["stdout"])
	}
	if execOut["exit_code"].(float64) != 0 {
		t.Fatalf("exec exit_code = %v, want 0", execOut["exit_code"])
	}

	listOut := callTool(t, cs, "list_llmboxes", map[string]any{})
	if !listMentionsReadyBox(listOut, boxID) {
		t.Fatalf("list_llmboxes did not show %q as ready: %v", boxID, listOut)
	}

	if got := callTool(t, cs, "destroy_llmbox", map[string]any{"box_id": boxID})["destroyed"]; got != boxID {
		t.Fatalf("destroyed = %v, want %q", got, boxID)
	}
	// After destruction the box is gone, so a lookup by box ID errors.
	if _, err := callToolRaw(t, cs, "get_llmbox", map[string]any{"box_id": boxID}); err == nil {
		t.Fatal("get_llmbox should error after the box is destroyed")
	}
}

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
// tool calls travel over the real box-control HTTP API to the server.
//
// @arg t The test, used for fatal errors and cleanup.
// @arg base The server's box-control API base URL.
// @return *mcp.ClientSession A connected MCP client session.
func connectMCP(t *testing.T, base string) *mcp.ClientSession {
	t.Helper()
	return testutils.ConnectMCP(t, api.NewClient(base, nil), "llmbox", "e2e")
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

// assertNoOAuthLeak fails the test if any string value in a tool's output
// carries the OAuth authorize URL or the platform host — the secret the design
// keeps out of the chatbot's context.
//
// @arg t The test, failed on a leak.
// @arg tool The tool name, for the failure message.
// @arg out The tool's structured output.
func assertNoOAuthLeak(t *testing.T, tool string, out map[string]any) {
	t.Helper()
	for k, v := range out {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if strings.Contains(s, "/oauth/authorize") || strings.Contains(s, "code_challenge") {
			t.Fatalf("%s leaked an OAuth secret in field %q: %q", tool, k, s)
		}
	}
}

// listMentionsReadyBox reports whether list_llmboxes output includes a box with
// the given box ID in the ready phase.
//
// @arg out The list_llmboxes structured output.
// @arg boxID The box ID to look for.
// @return bool True if a ready box with that box ID is present.
func listMentionsReadyBox(out map[string]any, boxID string) bool {
	boxes, _ := out["boxes"].([]any)
	for _, raw := range boxes {
		box, _ := raw.(map[string]any)
		if box["box_id"] == boxID && box["phase"] == "ready" {
			return true
		}
	}
	return false
}

// by maps a short selector name to the WebDriver By* strategy constant, so the
// workflow reads cleanly without repeating the selenium package qualifier.
//
// @arg kind One of "css", "id", or "name".
// @return string The corresponding WebDriver selector strategy.
func by(kind string) string {
	switch kind {
	case "css":
		return "css selector"
	case "id":
		return "id"
	case "name":
		return "name"
	default:
		panic("unknown selector kind: " + kind)
	}
}
