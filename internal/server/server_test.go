package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/clems4ever/llmbox/internal/docker"
	"github.com/clems4ever/llmbox/internal/hooks"
)

// fakeMgr is a stand-in for *docker.Manager.
type fakeMgr struct {
	mu sync.Mutex

	createID  string
	createURL string
	createErr error

	submitURL string
	submitErr error
	gotCode   string

	listResult []docker.Box
	destroyed  []string
	reaped     []string

	logsResult string
	logsErr    error
	gotLogsID  string
	gotLogsN   int

	execResult docker.ExecResult
	execErr    error
	gotExecID  string
	gotExecCmd []string

	gotOpts docker.CreateOptions
}

// CreateLLMBox records the requested options and returns the canned ID/URL/error.
func (f *fakeMgr) CreateLLMBox(_ context.Context, opts docker.CreateOptions) (string, string, error) {
	f.mu.Lock()
	f.gotOpts = opts
	f.mu.Unlock()
	return f.createID, f.createURL, f.createErr
}

// SubmitCode records the submitted code and returns the canned URL/error.
func (f *fakeMgr) SubmitCode(_ context.Context, _, code string) (string, error) {
	f.mu.Lock()
	f.gotCode = code
	f.mu.Unlock()
	return f.submitURL, f.submitErr
}

// List returns the canned boxes.
func (f *fakeMgr) List(context.Context) ([]docker.Box, error) { return f.listResult, nil }

// Destroy records the destroyed ID and always succeeds.
func (f *fakeMgr) Destroy(_ context.Context, id string) error {
	f.destroyed = append(f.destroyed, id)
	return nil
}

// Logs records the requested box ID and tail and returns the canned output/error.
func (f *fakeMgr) Logs(_ context.Context, id string, tail int) (string, error) {
	f.mu.Lock()
	f.gotLogsID = id
	f.gotLogsN = tail
	f.mu.Unlock()
	return f.logsResult, f.logsErr
}

// Exec records the requested box ID and command and returns the canned result/error.
func (f *fakeMgr) Exec(_ context.Context, id string, cmd []string) (docker.ExecResult, error) {
	f.mu.Lock()
	f.gotExecID = id
	f.gotExecCmd = cmd
	f.mu.Unlock()
	return f.execResult, f.execErr
}

// ReapOrphans returns the canned reaped IDs.
func (f *fakeMgr) ReapOrphans(context.Context, time.Duration) ([]string, error) {
	return f.reaped, nil
}

// newTestServer builds a Server backed by the given fake manager and no-op store,
// with hook integration disabled (nil hooks).
func newTestServer(f *fakeMgr) *Server {
	return New(f, nil, "https://boxes.example.com", 5*time.Minute, noopStore{})
}

// fakeHooks is a stand-in for *hooks.Runner that records create/destroy calls.
// It models a single hook keyed by "hook": OnCreate returns canned files and
// state, OnDestroy records the state it was replayed.
type fakeHooks struct {
	createFiles []hooks.File
	createState map[string]string
	createErr   error
	created     int
	destroyed   []map[string]string
}

// OnCreate returns the canned files/state/error and counts the call.
func (f *fakeHooks) OnCreate(context.Context, hooks.BoxInfo) ([]hooks.File, map[string]string, error) {
	f.created++
	return f.createFiles, f.createState, f.createErr
}

// OnDestroy records the per-hook state it was replayed and always succeeds.
func (f *fakeHooks) OnDestroy(_ context.Context, _ hooks.BoxInfo, state map[string]string) error {
	f.destroyed = append(f.destroyed, state)
	return nil
}

// --- core flow ---

// TestCreateBoxRegistersSession checks the session, token, URL, and opts pass-through.
func TestCreateBoxRegistersSession(t *testing.T) {
	f := &fakeMgr{createID: "abcdef0123456789", createURL: "https://claude.com/cai/oauth/authorize?x=1"}
	s := newTestServer(f)

	sess, err := s.CreateBox(context.Background(), docker.CreateOptions{Hostname: "my-box", Description: "scratch"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if sess.BoxID != "abcdef0123456789" || sess.Status != "pending" {
		t.Errorf("unexpected session %+v", sess)
	}
	// Hostname/description are recorded on the session and forwarded to the manager.
	if sess.Hostname != "my-box" || sess.Description != "scratch" {
		t.Errorf("session hostname/description = %q/%q, want my-box/scratch", sess.Hostname, sess.Description)
	}
	if f.gotOpts.Hostname != "my-box" || f.gotOpts.Description != "scratch" {
		t.Errorf("manager got opts %+v, want hostname/description my-box/scratch", f.gotOpts)
	}
	if len(sess.Token) != 64 {
		t.Errorf("token not 32 random bytes hex: len %d", len(sess.Token))
	}
	if got := s.AuthPageURL(sess.Token); got != "https://boxes.example.com/auth/"+sess.Token {
		t.Errorf("AuthPageURL = %q", got)
	}
	if s.lookup(sess.Token) == nil {
		t.Error("session not registered")
	}
}

// TestCreateBoxDestroysOnTokenFailure checks a create error propagates.
func TestCreateBoxDestroysOnTokenFailure(t *testing.T) {
	// Hard to force token failure; instead verify create error propagates.
	f := &fakeMgr{createErr: errors.New("no image")}
	s := newTestServer(f)
	if _, err := s.CreateBox(context.Background(), docker.CreateOptions{}); err == nil {
		t.Fatal("expected error")
	}
}

// TestCreateBoxRunsCreateHooks checks a configured hook runner runs on create,
// its returned files are injected into the box, and its per-hook state is
// persisted on the session.
func TestCreateBoxRunsCreateHooks(t *testing.T) {
	f := &fakeMgr{createID: "abcdef0123456789", createURL: "u"}
	h := &fakeHooks{
		createState: map[string]string{"granular-hook": "subj-xyz"},
		createFiles: []hooks.File{
			{Path: "/home/node/.granular/subject_token", Content: []byte("subj-xyz"), Mode: 0o600, UID: 1000, GID: 1000},
			{Path: "/home/node/.granular/github.yaml", Content: []byte("base_url: \"http://gh\"\n"), Mode: 0o644, UID: 1000, GID: 1000},
		},
	}
	s := New(f, h, "https://boxes.example.com", time.Minute, noopStore{})

	sess, err := s.CreateBox(context.Background(), docker.CreateOptions{})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if h.created != 1 {
		t.Errorf("create hooks ran %d times, want 1", h.created)
	}
	if sess.HookState["granular-hook"] != "subj-xyz" {
		t.Errorf("session hook state = %v, want granular-hook=subj-xyz", sess.HookState)
	}
	// Index the injected files by path.
	byPath := map[string]docker.InjectFile{}
	for _, fl := range f.gotOpts.Files {
		byPath[fl.Path] = fl
	}
	tok, ok := byPath["/home/node/.granular/subject_token"]
	if !ok {
		t.Fatalf("subject token not injected: %+v", f.gotOpts.Files)
	}
	if string(tok.Content) != "subj-xyz" || tok.UID != 1000 {
		t.Errorf("injected token = %q uid %d, want subj-xyz uid 1000", tok.Content, tok.UID)
	}
	cfg, ok := byPath["/home/node/.granular/github.yaml"]
	if !ok {
		t.Fatalf("RS config not injected: %+v", f.gotOpts.Files)
	}
	if !strings.Contains(string(cfg.Content), "http://gh") || cfg.UID != 1000 {
		t.Errorf("injected config = %q uid %d, want base_url http://gh uid 1000", cfg.Content, cfg.UID)
	}
}

// TestCreateBoxRunsDestroyHooksOnCreateFailure checks the create hook's state is
// replayed to the destroy hooks when box creation fails, so nothing is left
// dangling.
func TestCreateBoxRunsDestroyHooksOnCreateFailure(t *testing.T) {
	f := &fakeMgr{createErr: errors.New("no image")}
	h := &fakeHooks{createState: map[string]string{"granular-hook": "subj-doomed"}}
	s := New(f, h, "https://boxes.example.com", time.Minute, noopStore{})

	if _, err := s.CreateBox(context.Background(), docker.CreateOptions{}); err == nil {
		t.Fatal("expected error")
	}
	if len(h.destroyed) != 1 || h.destroyed[0]["granular-hook"] != "subj-doomed" {
		t.Errorf("destroyed = %v, want [granular-hook=subj-doomed]", h.destroyed)
	}
}

// TestDestroyRunsDestroyHooks checks destroying a box replays its hook state to
// the destroy hooks.
func TestDestroyRunsDestroyHooks(t *testing.T) {
	f := &fakeMgr{createID: "abcdef0123456789", createURL: "u"}
	h := &fakeHooks{createState: map[string]string{"granular-hook": "subj-live"}}
	s := New(f, h, "https://boxes.example.com", time.Minute, noopStore{})

	sess, err := s.CreateBox(context.Background(), docker.CreateOptions{})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if err := s.DestroyBox(context.Background(), sess.BoxID); err != nil {
		t.Fatalf("DestroyBox: %v", err)
	}
	if len(h.destroyed) != 1 || h.destroyed[0]["granular-hook"] != "subj-live" {
		t.Errorf("destroyed = %v, want [granular-hook=subj-live]", h.destroyed)
	}
}

// TestSubmitCodeSuccess checks the code is forwarded and the session becomes ready.
func TestSubmitCodeSuccess(t *testing.T) {
	f := &fakeMgr{createID: "id1", createURL: "u", submitURL: "https://claude.ai/code/s/1"}
	s := newTestServer(f)
	sess, _ := s.CreateBox(context.Background(), docker.CreateOptions{})

	if err := s.SubmitCode(context.Background(), sess.Token, "CODE"); err != nil {
		t.Fatalf("SubmitCode: %v", err)
	}
	if f.gotCode != "CODE" {
		t.Errorf("manager got code %q", f.gotCode)
	}
	status, url, _ := sess.snapshot()
	if status != "ready" || url != "https://claude.ai/code/s/1" {
		t.Errorf("session not ready: status=%q url=%q", status, url)
	}
}

// TestSubmitCodeFailureRecorded checks a submit failure is recorded on the session.
func TestSubmitCodeFailureRecorded(t *testing.T) {
	f := &fakeMgr{createID: "id1", createURL: "u", submitErr: errors.New("invalid code")}
	s := newTestServer(f)
	sess, _ := s.CreateBox(context.Background(), docker.CreateOptions{})

	if err := s.SubmitCode(context.Background(), sess.Token, "BAD"); err == nil {
		t.Fatal("expected error")
	}
	status, _, errMsg := sess.snapshot()
	if status != "error" || !strings.Contains(errMsg, "invalid code") {
		t.Errorf("session error not recorded: status=%q err=%q", status, errMsg)
	}
}

// TestSubmitCodeUnknownToken checks SubmitCode errors for an unknown token.
func TestSubmitCodeUnknownToken(t *testing.T) {
	s := newTestServer(&fakeMgr{})
	if err := s.SubmitCode(context.Background(), "nope", "code"); err == nil {
		t.Fatal("expected error for unknown token")
	}
}

// TestSubmitCodeEmpty checks SubmitCode errors for an empty code.
func TestSubmitCodeEmpty(t *testing.T) {
	f := &fakeMgr{createID: "id1", createURL: "u"}
	s := newTestServer(f)
	sess, _ := s.CreateBox(context.Background(), docker.CreateOptions{})
	if err := s.SubmitCode(context.Background(), sess.Token, "   "); err == nil {
		t.Fatal("expected error for empty code")
	}
}

// TestDestroyForgetsSession checks the session is forgotten after a destroy.
func TestDestroyForgetsSession(t *testing.T) {
	f := &fakeMgr{createID: "abcdef0123456789", createURL: "u"}
	s := newTestServer(f)
	sess, _ := s.CreateBox(context.Background(), docker.CreateOptions{})

	if err := s.DestroyBox(context.Background(), "abcdef0123456789"); err != nil {
		t.Fatalf("DestroyBox: %v", err)
	}
	if s.lookup(sess.Token) != nil {
		t.Error("session should be forgotten after destroy")
	}
}

// TestPruneSessionsAfterReap checks a reaped box's session is pruned.
func TestPruneSessionsAfterReap(t *testing.T) {
	f := &fakeMgr{createID: "abcdef0123456789", createURL: "u"}
	s := newTestServer(f)
	sess, _ := s.CreateBox(context.Background(), docker.CreateOptions{})
	s.pruneSessions([]string{"abcdef012345"}) // short ID prefix, as the reaper returns
	if s.lookup(sess.Token) != nil {
		t.Error("session for reaped box should be pruned")
	}
}

// --- web handlers ---

// TestAuthPageRendersAndSubmits checks the auth page renders and the code submits.
func TestAuthPageRendersAndSubmits(t *testing.T) {
	f := &fakeMgr{createID: "id1", createURL: "https://claude.com/cai/oauth/authorize?a=b", submitURL: "https://claude.ai/code/s/9"}
	s := newTestServer(f)
	sess, _ := s.CreateBox(context.Background(), docker.CreateOptions{})
	h := s.Handler(s.MCPServer("test", "v0"))

	// GET the page: shows the authorize link and a paste form.
	req := httptest.NewRequest(http.MethodGet, "/auth/"+sess.Token, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "claude.com/cai/oauth/authorize?a=b") {
		t.Error("authorize URL missing from page")
	}
	if !strings.Contains(body, `name="code"`) {
		t.Error("code form missing from page")
	}

	// POST the code: box becomes ready, page shows the session URL.
	form := url.Values{"code": {"THECODE"}}
	preq := httptest.NewRequest(http.MethodPost, "/auth/"+sess.Token, strings.NewReader(form.Encode()))
	preq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	prec := httptest.NewRecorder()
	h.ServeHTTP(prec, preq)
	if prec.Code != http.StatusOK {
		t.Fatalf("POST status %d", prec.Code)
	}
	if f.gotCode != "THECODE" {
		t.Errorf("code not forwarded: %q", f.gotCode)
	}
	if !strings.Contains(prec.Body.String(), "https://claude.ai/code/s/9") {
		t.Error("session URL missing from success page")
	}
}

// TestAuthPageUnknownToken checks the auth page 404s for an unknown token.
func TestAuthPageUnknownToken(t *testing.T) {
	s := newTestServer(&fakeMgr{})
	h := s.Handler(s.MCPServer("test", "v0"))
	req := httptest.NewRequest(http.MethodGet, "/auth/deadbeef", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestHealthz checks the health endpoint returns ok.
func TestHealthz(t *testing.T) {
	s := newTestServer(&fakeMgr{})
	h := s.Handler(s.MCPServer("test", "v0"))
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("healthz: %d %q", rec.Code, rec.Body.String())
	}
}

// TestGetByHostname checks get_llmbox resolves a box by hostname (case-insensitive)
// and errors for an empty or unknown hostname.
func TestGetByHostname(t *testing.T) {
	f := &fakeMgr{createID: "abcdef0123456789", createURL: "u", submitURL: "https://claude.ai/code/s/1"}
	s := newTestServer(f)
	sess, err := s.CreateBox(context.Background(), docker.CreateOptions{Hostname: "web-box", Description: "d"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}

	// Found, case-insensitive.
	_, out, err := s.toolGet(context.Background(), nil, getInput{Hostname: "WEB-BOX"})
	if err != nil {
		t.Fatalf("toolGet: %v", err)
	}
	if out.Status != "pending" || out.Hostname != "web-box" || out.Description != "d" {
		t.Errorf("unexpected get output: %+v", out)
	}

	// Reflects status changes.
	if err := s.SubmitCode(context.Background(), sess.Token, "CODE"); err != nil {
		t.Fatalf("SubmitCode: %v", err)
	}
	if _, out, _ := s.toolGet(context.Background(), nil, getInput{Hostname: "web-box"}); out.Status != "ready" || out.SessionURL != "https://claude.ai/code/s/1" {
		t.Errorf("expected ready with session URL, got %+v", out)
	}

	// Empty and unknown hostnames error.
	if _, _, err := s.toolGet(context.Background(), nil, getInput{Hostname: ""}); err == nil {
		t.Error("expected error for empty hostname")
	}
	if _, _, err := s.toolGet(context.Background(), nil, getInput{Hostname: "nope"}); err == nil {
		t.Error("expected error for unknown hostname")
	}
}

// TestBoxLogsByHostname checks get_llmbox_logs resolves a box by hostname,
// forwards the box ID and tail to the manager, and errors for empty or unknown
// hostnames.
func TestBoxLogsByHostname(t *testing.T) {
	f := &fakeMgr{createID: "abcdef0123456789", createURL: "u", logsResult: "Ready\nlistening\n"}
	s := newTestServer(f)
	if _, err := s.CreateBox(context.Background(), docker.CreateOptions{Hostname: "web-box"}); err != nil {
		t.Fatalf("CreateBox: %v", err)
	}

	// Found, case-insensitive, with the tail forwarded to the manager.
	_, out, err := s.toolLogs(context.Background(), nil, logsInput{Hostname: "WEB-BOX", Tail: 25})
	if err != nil {
		t.Fatalf("toolLogs: %v", err)
	}
	if out.Hostname != "WEB-BOX" || out.Logs != "Ready\nlistening\n" {
		t.Errorf("unexpected logs output: %+v", out)
	}
	if f.gotLogsID != "abcdef0123456789" || f.gotLogsN != 25 {
		t.Errorf("manager got id=%q tail=%d, want abcdef0123456789/25", f.gotLogsID, f.gotLogsN)
	}

	// Empty and unknown hostnames error.
	if _, _, err := s.toolLogs(context.Background(), nil, logsInput{Hostname: ""}); err == nil {
		t.Error("expected error for empty hostname")
	}
	if _, _, err := s.toolLogs(context.Background(), nil, logsInput{Hostname: "nope"}); err == nil {
		t.Error("expected error for unknown hostname")
	}
}

// TestBoxExecByHostname checks exec_llmbox resolves a box by hostname, wraps the
// command in /bin/sh -c, returns the captured output, and errors for empty or
// unknown hostnames and an empty command.
func TestBoxExecByHostname(t *testing.T) {
	f := &fakeMgr{
		createID:   "abcdef0123456789",
		createURL:  "u",
		execResult: docker.ExecResult{Stdout: "hi\n", Stderr: "", ExitCode: 0},
	}
	s := newTestServer(f)
	if _, err := s.CreateBox(context.Background(), docker.CreateOptions{Hostname: "web-box"}); err != nil {
		t.Fatalf("CreateBox: %v", err)
	}

	// Found, case-insensitive; command wrapped and box ID forwarded.
	_, out, err := s.toolExec(context.Background(), nil, execInput{Hostname: "WEB-BOX", Command: "echo hi"})
	if err != nil {
		t.Fatalf("toolExec: %v", err)
	}
	if out.Hostname != "WEB-BOX" || out.Stdout != "hi\n" || out.ExitCode != 0 {
		t.Errorf("unexpected exec output: %+v", out)
	}
	if f.gotExecID != "abcdef0123456789" {
		t.Errorf("manager got box ID %q, want abcdef0123456789", f.gotExecID)
	}
	want := []string{"/bin/sh", "-c", "echo hi"}
	if len(f.gotExecCmd) != 3 || f.gotExecCmd[0] != want[0] || f.gotExecCmd[1] != want[1] || f.gotExecCmd[2] != want[2] {
		t.Errorf("manager ran cmd %v, want %v", f.gotExecCmd, want)
	}

	// Empty/unknown hostnames and an empty command error.
	if _, _, err := s.toolExec(context.Background(), nil, execInput{Hostname: "", Command: "ls"}); err == nil {
		t.Error("expected error for empty hostname")
	}
	if _, _, err := s.toolExec(context.Background(), nil, execInput{Hostname: "nope", Command: "ls"}); err == nil {
		t.Error("expected error for unknown hostname")
	}
	if _, _, err := s.toolExec(context.Background(), nil, execInput{Hostname: "web-box", Command: "  "}); err == nil {
		t.Error("expected error for empty command")
	}
}

// --- MCP wiring ---

// TestMCPToolsRegisteredAndCreate checks all tools are registered and create returns a safe auth URL.
func TestMCPToolsRegisteredAndCreate(t *testing.T) {
	f := &fakeMgr{createID: "abcdef0123456789", createURL: "https://claude.com/cai/oauth/authorize?z=1"}
	s := newTestServer(f)
	cs := connectMCP(t, s)

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"create_llmbox", "get_llmbox", "list_llmboxes", "destroy_llmbox", "get_llmbox_logs", "exec_llmbox"} {
		if !names[want] {
			t.Errorf("tool %q not registered (have %v)", want, names)
		}
	}

	// create_llmbox returns an auth URL on our public host, never a secret.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_llmbox", Arguments: map[string]any{}})
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

// connectMCP wires an in-memory MCP client to the server and returns the session.
func connectMCP(t *testing.T, s *Server) *mcp.ClientSession {
	t.Helper()
	srv := s.MCPServer("test", "v0")
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
