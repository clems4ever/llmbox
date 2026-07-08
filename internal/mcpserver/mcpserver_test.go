package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/clems4ever/llmbox/internal/shared/api"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// fakeBackend is an in-memory Backend used to drive the tool handlers in
// isolation. It records what each tool forwarded and returns canned results, so
// tests assert the tool layer's behavior without Docker, a store, or a cluster.
type fakeBackend struct {
	createSess   BoxSession
	createErr    error
	gotCreate    sandbox.CreateOptions
	createCalled bool

	sessions map[string]BoxSession // keyed by lowercased box ID

	boxes   []api.BoxView
	listErr error

	spokes    []SpokeStatus
	spokesErr error

	destroyedID string
	destroyErr  error

	logs        string
	gotLogsID   string
	gotLogsTail int
	logsErr     error

	exec       sandbox.ExecResult
	gotExecID  string
	gotExecCmd string
	execErr    error

	proxyEnabled    bool
	createProxyInfo ProxyInfo
	createProxyErr  error
	gotProxyBoxID   string
	gotProxyPort    int
	gotProxyDesc    string
	deletedProxy    [2]any // {boxID, port} of the last DeleteProxy
	deleteProxyErr  error
	proxies         []ProxyInfo
	gotListBoxID    string
	listProxiesErr  error
}

// CreateBox records the create options and returns the canned session/error.
func (f *fakeBackend) CreateBox(_ context.Context, opts sandbox.CreateOptions) (BoxSession, error) {
	f.createCalled = true
	f.gotCreate = opts
	if f.createErr != nil {
		return BoxSession{}, f.createErr
	}
	return f.createSess, nil
}

// AuthPageURL returns a canned auth page URL for the token.
func (f *fakeBackend) AuthPageURL(token string) string {
	return "https://boxes.example.com/auth/" + token
}

// LookupByBoxID returns the canned session for a box ID (case-insensitive).
func (f *fakeBackend) LookupByBoxID(boxID string) (BoxSession, bool) {
	sess, ok := f.sessions[strings.ToLower(boxID)]
	return sess, ok
}

// ListBoxes returns the canned boxes and list error.
func (f *fakeBackend) ListBoxes(context.Context) ([]api.BoxView, error) {
	return f.boxes, f.listErr
}

// SpokeStatuses returns the canned spoke statuses and error.
func (f *fakeBackend) SpokeStatuses(context.Context) ([]SpokeStatus, error) {
	return f.spokes, f.spokesErr
}

// CreateSpoke returns a canned spoke enrollment.
func (f *fakeBackend) CreateSpoke(_ context.Context, name, backend string, _ time.Duration) (api.SpokeEnrollment, error) {
	return api.SpokeEnrollment{Name: name, Token: "tok", Command: "llmbox-spoke " + backend}, nil
}

// DropSpoke is a no-op for the tool tests.
func (f *fakeBackend) DropSpoke(context.Context, string) error { return nil }

// SetDefaultSpoke is a no-op for the tool tests.
func (f *fakeBackend) SetDefaultSpoke(context.Context, string) error { return nil }

// ListJoinTokens returns no tokens for the tool tests.
func (f *fakeBackend) ListJoinTokens(context.Context) ([]api.JoinTokenInfo, error) {
	return nil, nil
}

// RevokeJoinToken is a no-op for the tool tests.
func (f *fakeBackend) RevokeJoinToken(context.Context, string) error { return nil }

// DestroyBox records the destroyed container ID and returns the canned error.
func (f *fakeBackend) DestroyBox(_ context.Context, containerID string) error {
	f.destroyedID = containerID
	return f.destroyErr
}

// BoxLogs records the box ID and tail and returns the canned logs/error.
func (f *fakeBackend) BoxLogs(_ context.Context, boxID string, tail int) (string, error) {
	f.gotLogsID, f.gotLogsTail = boxID, tail
	return f.logs, f.logsErr
}

// BoxExec records the box ID and command and returns the canned result/error.
func (f *fakeBackend) BoxExec(_ context.Context, boxID, command string) (sandbox.ExecResult, error) {
	f.gotExecID, f.gotExecCmd = boxID, command
	return f.exec, f.execErr
}

// ProxyEnabled reports the canned proxy-enabled flag.
func (f *fakeBackend) ProxyEnabled() bool { return f.proxyEnabled }

// CreateProxy records the box ID, port, and description and returns the canned
// proxy info/error.
func (f *fakeBackend) CreateProxy(_ context.Context, boxID string, port int, description string) (ProxyInfo, error) {
	f.gotProxyBoxID, f.gotProxyPort, f.gotProxyDesc = boxID, port, description
	if f.createProxyErr != nil {
		return ProxyInfo{}, f.createProxyErr
	}
	return f.createProxyInfo, nil
}

// DeleteProxy records the deleted box ID and port and returns the canned error.
func (f *fakeBackend) DeleteProxy(_ context.Context, boxID string, port int) error {
	f.deletedProxy = [2]any{boxID, port}
	return f.deleteProxyErr
}

// ListProxies records the box-ID filter and returns the canned proxies/error.
func (f *fakeBackend) ListProxies(_ context.Context, boxID string) ([]ProxyInfo, error) {
	f.gotListBoxID = boxID
	return f.proxies, f.listProxiesErr
}

// TestToolCreateProxy checks create_llmbox_proxy forwards its inputs and returns
// the proxy URL.
func TestToolCreateProxy(t *testing.T) {
	f := &fakeBackend{
		proxyEnabled:    true,
		createProxyInfo: ProxyInfo{BoxID: "web-box", Port: 8000, URL: "https://slug123.proxy.example.com/", Slug: "slug123", Description: "preview server"},
	}
	h := &handlers{b: f}
	_, out, err := h.toolCreateProxy(context.Background(), nil, createProxyInput{BoxID: "web-box", Port: 8000, Description: "preview server"})
	if err != nil {
		t.Fatalf("toolCreateProxy: %v", err)
	}
	if f.gotProxyBoxID != "web-box" || f.gotProxyPort != 8000 {
		t.Errorf("forwarded box/port = %q/%d", f.gotProxyBoxID, f.gotProxyPort)
	}
	if f.gotProxyDesc != "preview server" {
		t.Errorf("forwarded description = %q, want %q", f.gotProxyDesc, "preview server")
	}
	if out.URL != "https://slug123.proxy.example.com/" {
		t.Errorf("URL = %q", out.URL)
	}
	if out.Description != "preview server" {
		t.Errorf("output description = %q, want %q", out.Description, "preview server")
	}
}

// TestToolCreateProxyRequiresBoxID rejects a create with no box ID or a bad port.
func TestToolCreateProxyRequiresBoxID(t *testing.T) {
	f := &fakeBackend{proxyEnabled: true}
	h := &handlers{b: f}
	if _, _, err := h.toolCreateProxy(context.Background(), nil, createProxyInput{Port: 8000}); err == nil {
		t.Error("expected an error for an empty box ID")
	}
	if _, _, err := h.toolCreateProxy(context.Background(), nil, createProxyInput{BoxID: "web-box", Port: 0}); err == nil {
		t.Error("expected an error for a non-positive port")
	}
}

// TestToolCreateProxyDisabled surfaces a clear error when proxying is disabled.
func TestToolCreateProxyDisabled(t *testing.T) {
	h := &handlers{b: &fakeBackend{proxyEnabled: false}}
	if _, _, err := h.toolCreateProxy(context.Background(), nil, createProxyInput{BoxID: "web-box", Port: 8000}); err == nil {
		t.Error("expected an error when proxying is disabled")
	}
}

// TestToolDeleteProxy disables a proxy by box and port.
func TestToolDeleteProxy(t *testing.T) {
	f := &fakeBackend{}
	h := &handlers{b: f}
	if _, _, err := h.toolDeleteProxy(context.Background(), nil, deleteProxyInput{BoxID: "web-box", Port: 8000}); err != nil {
		t.Fatalf("toolDeleteProxy: %v", err)
	}
	if f.deletedProxy != [2]any{"web-box", 8000} {
		t.Errorf("deleted = %v", f.deletedProxy)
	}
}

// TestToolListProxies lists proxies, forwarding the optional box-ID filter.
func TestToolListProxies(t *testing.T) {
	f := &fakeBackend{proxies: []ProxyInfo{{BoxID: "web-box", Port: 8000, URL: "https://s.proxy.example.com/", Description: "preview server"}}}
	h := &handlers{b: f}
	_, out, err := h.toolListProxies(context.Background(), nil, listProxiesInput{BoxID: "web-box"})
	if err != nil {
		t.Fatalf("toolListProxies: %v", err)
	}
	if f.gotListBoxID != "web-box" {
		t.Errorf("forwarded box ID = %q", f.gotListBoxID)
	}
	if len(out.Proxies) != 1 || out.Proxies[0].URL != "https://s.proxy.example.com/" {
		t.Errorf("proxies = %+v", out.Proxies)
	}
	if out.Proxies[0].Description != "preview server" {
		t.Errorf("listed description = %q, want %q", out.Proxies[0].Description, "preview server")
	}
}

// TestToolCreate checks create_llmbox forwards its inputs, returns the auth page
// URL and token, shortens the container ID, and starts the box pending.
func TestToolCreate(t *testing.T) {
	f := &fakeBackend{createSess: BoxSession{BoxID: "web-box", ContainerID: "abcdef0123456789", Token: "tok-123"}}
	h := &handlers{b: f}

	_, out, err := h.toolCreate(context.Background(), nil, createInput{
		BoxID:       "web-box",
		Description: "front-end work",
		Spoke:       "edge",
	})
	if err != nil {
		t.Fatalf("toolCreate: %v", err)
	}
	if out.BoxID != "web-box" || out.InstanceID != "abcdef012345" {
		t.Errorf("box/container = %q/%q, want web-box/abcdef012345", out.BoxID, out.InstanceID)
	}
	if out.AuthURL != "https://boxes.example.com/auth/tok-123" || out.AuthToken != "tok-123" {
		t.Errorf("auth url/token = %q/%q", out.AuthURL, out.AuthToken)
	}
	if out.Status != "pending" || out.Instructions == "" {
		t.Errorf("status/instructions = %q/%q", out.Status, out.Instructions)
	}
	if f.gotCreate.BoxID != "web-box" || f.gotCreate.Description != "front-end work" || f.gotCreate.SpokeName != "edge" {
		t.Errorf("backend got opts %+v, want all inputs forwarded", f.gotCreate)
	}
}

// TestToolCreateRequiresBoxID checks create_llmbox rejects an empty box ID and
// never touches the backend, so every box stays reachable by its box ID.
func TestToolCreateRequiresBoxID(t *testing.T) {
	f := &fakeBackend{}
	h := &handlers{b: f}

	if _, _, err := h.toolCreate(context.Background(), nil, createInput{Description: "no box id"}); err == nil {
		t.Fatal("expected error for empty box ID")
	}
	if f.createCalled {
		t.Error("backend was called despite missing box ID")
	}
}

// TestToolCreatePropagatesError checks a backend create failure surfaces as a
// tool error.
func TestToolCreatePropagatesError(t *testing.T) {
	h := &handlers{b: &fakeBackend{createErr: errors.New("boom")}}
	if _, _, err := h.toolCreate(context.Background(), nil, createInput{BoxID: "web-box"}); err == nil {
		t.Fatal("expected the backend error to propagate")
	}
}

// TestToolGet checks get_llmbox surfaces a box's flattened state and errors on an
// empty or unknown box ID.
func TestToolGet(t *testing.T) {
	f := &fakeBackend{sessions: map[string]BoxSession{
		"web-box": {BoxID: "web-box", Description: "d", Status: "ready", SessionURL: "https://claude.ai/code/s/1"},
	}}
	h := &handlers{b: f}

	_, out, err := h.toolGet(context.Background(), nil, getInput{BoxID: "WEB-BOX"})
	if err != nil {
		t.Fatalf("toolGet: %v", err)
	}
	if out.Status != "ready" || out.BoxID != "web-box" || out.Description != "d" || out.SessionURL != "https://claude.ai/code/s/1" {
		t.Errorf("unexpected get output: %+v", out)
	}

	if _, _, err := h.toolGet(context.Background(), nil, getInput{BoxID: ""}); err == nil {
		t.Error("expected error for empty box ID")
	}
	if _, _, err := h.toolGet(context.Background(), nil, getInput{BoxID: "nope"}); err == nil {
		t.Error("expected error for unknown box ID")
	}
}

// TestToolList checks list_llmboxes returns the backend's boxes and propagates a
// listing error.
func TestToolList(t *testing.T) {
	f := &fakeBackend{boxes: []api.BoxView{
		{Box: sandbox.Box{InstanceID: "abcdef0123456789", BoxID: "web-box", Description: "front-end work"}},
		{Box: sandbox.Box{InstanceID: "0123456789abcdef"}},
	}}
	h := &handlers{b: f}

	_, out, err := h.toolList(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatalf("toolList: %v", err)
	}
	if len(out.Boxes) != 2 || out.Boxes[0].BoxID != "web-box" || out.Boxes[1].BoxID != "" {
		t.Errorf("unexpected boxes: %+v", out.Boxes)
	}

	if _, _, err := (&handlers{b: &fakeBackend{listErr: errors.New("x")}}).toolList(context.Background(), nil, struct{}{}); err == nil {
		t.Error("expected the listing error to propagate")
	}
}

// TestToolListSpokes checks list_spokes returns the backend's spoke statuses and
// propagates an error.
func TestToolListSpokes(t *testing.T) {
	f := &fakeBackend{spokes: []SpokeStatus{{Name: "edge", Connected: true, Default: true}, {Name: "edge2"}}}
	h := &handlers{b: f}

	_, out, err := h.toolListSpokes(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatalf("toolListSpokes: %v", err)
	}
	if len(out.Spokes) != 2 || out.Spokes[0].Name != "edge" || out.Spokes[1].Name != "edge2" {
		t.Errorf("unexpected spokes: %+v", out.Spokes)
	}

	if _, _, err := (&handlers{b: &fakeBackend{spokesErr: errors.New("x")}}).toolListSpokes(context.Background(), nil, struct{}{}); err == nil {
		t.Error("expected the spokes error to propagate")
	}
}

// TestToolDestroy checks destroy_llmbox resolves the box by box ID, destroys it
// by container ID, and errors on an empty/unknown box ID and a destroy failure.
func TestToolDestroy(t *testing.T) {
	f := &fakeBackend{sessions: map[string]BoxSession{
		"web-box": {BoxID: "web-box", ContainerID: "abcdef0123456789"},
	}}
	h := &handlers{b: f}

	_, out, err := h.toolDestroy(context.Background(), nil, destroyInput{BoxID: "WEB-BOX"})
	if err != nil {
		t.Fatalf("toolDestroy: %v", err)
	}
	if out.Destroyed != "WEB-BOX" {
		t.Errorf("destroyed = %q, want WEB-BOX", out.Destroyed)
	}
	if f.destroyedID != "abcdef0123456789" {
		t.Errorf("backend destroyed %q, want the container ID", f.destroyedID)
	}

	if _, _, err := h.toolDestroy(context.Background(), nil, destroyInput{BoxID: ""}); err == nil {
		t.Error("expected error for empty box ID")
	}
	if _, _, err := h.toolDestroy(context.Background(), nil, destroyInput{BoxID: "nope"}); err == nil {
		t.Error("expected error for unknown box ID")
	}

	failing := &fakeBackend{
		sessions:   map[string]BoxSession{"web-box": {ContainerID: "cid"}},
		destroyErr: errors.New("x"),
	}
	if _, _, err := (&handlers{b: failing}).toolDestroy(context.Background(), nil, destroyInput{BoxID: "web-box"}); err == nil {
		t.Error("expected the destroy error to propagate")
	}
}

// TestToolLogs checks get_llmbox_logs forwards the box ID and tail, returns the
// logs, and errors on an empty box ID and a read failure.
func TestToolLogs(t *testing.T) {
	f := &fakeBackend{logs: "Ready\nlistening\n"}
	h := &handlers{b: f}

	_, out, err := h.toolLogs(context.Background(), nil, logsInput{BoxID: "web-box", Tail: 25})
	if err != nil {
		t.Fatalf("toolLogs: %v", err)
	}
	if out.BoxID != "web-box" || out.Logs != "Ready\nlistening\n" {
		t.Errorf("unexpected logs output: %+v", out)
	}
	if f.gotLogsID != "web-box" || f.gotLogsTail != 25 {
		t.Errorf("backend got id=%q tail=%d, want web-box/25", f.gotLogsID, f.gotLogsTail)
	}

	if _, _, err := h.toolLogs(context.Background(), nil, logsInput{BoxID: ""}); err == nil {
		t.Error("expected error for empty box ID")
	}
	if _, _, err := (&handlers{b: &fakeBackend{logsErr: errors.New("x")}}).toolLogs(context.Background(), nil, logsInput{BoxID: "web-box"}); err == nil {
		t.Error("expected the logs error to propagate")
	}
}

// TestToolExec checks exec_llmbox forwards the box ID and command, returns the
// captured output, and errors on an empty box ID and a run failure.
func TestToolExec(t *testing.T) {
	f := &fakeBackend{exec: sandbox.ExecResult{Stdout: "hi\n", ExitCode: 0}}
	h := &handlers{b: f}

	_, out, err := h.toolExec(context.Background(), nil, execInput{BoxID: "web-box", Command: "echo hi"})
	if err != nil {
		t.Fatalf("toolExec: %v", err)
	}
	if out.BoxID != "web-box" || out.Stdout != "hi\n" || out.ExitCode != 0 {
		t.Errorf("unexpected exec output: %+v", out)
	}
	if f.gotExecID != "web-box" || f.gotExecCmd != "echo hi" {
		t.Errorf("backend got id=%q cmd=%q, want web-box/echo hi", f.gotExecID, f.gotExecCmd)
	}

	if _, _, err := h.toolExec(context.Background(), nil, execInput{BoxID: "", Command: "ls"}); err == nil {
		t.Error("expected error for empty box ID")
	}
	if _, _, err := (&handlers{b: &fakeBackend{execErr: errors.New("x")}}).toolExec(context.Background(), nil, execInput{BoxID: "web-box", Command: "ls"}); err == nil {
		t.Error("expected the exec error to propagate")
	}
}

// TestShortID checks the container ID is shortened to 12 chars and left untouched
// when already short.
func TestShortID(t *testing.T) {
	if got := shortID("abcdef0123456789"); got != "abcdef012345" {
		t.Errorf("shortID(long) = %q, want abcdef012345", got)
	}
	if got := shortID("short"); got != "short" {
		t.Errorf("shortID(short) = %q, want short", got)
	}
}

// TestToolsRegistered checks NewServer registers every box tool on the MCP
// server, listed over an in-memory client session.
func TestToolsRegistered(t *testing.T) {
	cs := connectMCP(t, &fakeBackend{})

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"create_llmbox", "get_llmbox", "list_llmboxes", "list_spokes", "destroy_llmbox", "get_llmbox_logs", "exec_llmbox", "create_llmbox_proxy", "delete_llmbox_proxy", "list_llmbox_proxies"} {
		if !names[want] {
			t.Errorf("tool %q not registered (have %v)", want, names)
		}
	}
}

// TestCreateReturnsSafeAuthURL checks create_llmbox returns only the public auth
// page URL, never the box's raw OAuth authorize URL or any other secret.
func TestCreateReturnsSafeAuthURL(t *testing.T) {
	f := &fakeBackend{createSess: BoxSession{BoxID: "web-box", ContainerID: "abcdef0123456789", Token: "tok-123"}}
	cs := connectMCP(t, f)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_llmbox", Arguments: map[string]any{"box_id": "web-box"}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool error: %v", res.Content)
	}
	out, _ := res.StructuredContent.(map[string]any)
	authURL, _ := out["auth_url"].(string)
	if !strings.HasPrefix(authURL, "https://boxes.example.com/auth/") {
		t.Errorf("auth_url = %q, want our public auth page", authURL)
	}
	if strings.Contains(authURL, "oauth/authorize") {
		t.Error("auth_url must not leak the raw OAuth URL into MCP output")
	}
}

// connectMCP wires an in-memory MCP client to a server built from b and returns
// the client session.
func connectMCP(t *testing.T, b Backend) *mcp.ClientSession {
	t.Helper()
	srv := NewServer(b, "test", "v0")
	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}
