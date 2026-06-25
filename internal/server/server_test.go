package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/clems4ever/llmbox/internal/docker"
	"github.com/clems4ever/llmbox/internal/hooks"
	"github.com/clems4ever/llmbox/testutils"
)

// newTestServer builds a Server backed by the given fake manager and no-op store,
// with hook integration disabled (nil hooks).
func newTestServer(f *testutils.FakeMgr) *Server {
	return New(f, nil, "https://boxes.example.com", 5*time.Minute, noopStore{}, nil)
}

// fakeHooks is a stand-in for *hooks.Runner that records create/destroy calls.
// It models a single hook keyed by "hook": OnCreate returns canned files and
// state, OnDestroy records the state it was replayed.
type fakeHooks struct {
	createFiles []hooks.File
	createState map[string]string
	CreateErr   error
	created     int
	destroyed   []map[string]string
}

// OnCreate returns the canned files/state/error and counts the call.
func (f *fakeHooks) OnCreate(context.Context, hooks.BoxInfo) ([]hooks.File, map[string]string, error) {
	f.created++
	return f.createFiles, f.createState, f.CreateErr
}

// OnDestroy records the per-hook state it was replayed and always succeeds.
func (f *fakeHooks) OnDestroy(_ context.Context, _ hooks.BoxInfo, state map[string]string) error {
	f.destroyed = append(f.destroyed, state)
	return nil
}

// --- core flow ---

// TestCreateBoxRegistersSession checks the session, token, URL, and opts pass-through.
func TestCreateBoxRegistersSession(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "https://claude.com/cai/oauth/authorize?x=1"}
	s := newTestServer(f)

	sess, err := s.createBox(context.Background(), docker.CreateOptions{BoxID: "my-box", Description: "scratch"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if sess.ContainerID != "abcdef0123456789" || sess.Status != "pending" {
		t.Errorf("unexpected session %+v", sess)
	}
	// BoxID/description are recorded on the session and forwarded to the manager.
	if sess.BoxID != "my-box" || sess.Description != "scratch" {
		t.Errorf("session box ID/description = %q/%q, want my-box/scratch", sess.BoxID, sess.Description)
	}
	if f.GotOpts.BoxID != "my-box" || f.GotOpts.Description != "scratch" {
		t.Errorf("manager got opts %+v, want box ID/description my-box/scratch", f.GotOpts)
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

// TestCreateBoxDefaultsImageToBoxImage checks that a creation request naming no
// image inherits the hub's configured box image, so the box image is resolved on
// the hub and remote spokes stay config-free.
func TestCreateBoxDefaultsImageToBoxImage(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "u"}
	s := newTestServer(f)
	s.SetBoxImage("ghcr.io/clems4ever/granular-llmbox-box:latest")

	if _, err := s.createBox(context.Background(), docker.CreateOptions{BoxID: "my-box"}); err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if f.GotOpts.Image != "ghcr.io/clems4ever/granular-llmbox-box:latest" {
		t.Errorf("manager got image %q, want the hub's configured box image", f.GotOpts.Image)
	}
}

// TestCreateBoxKeepsExplicitImage checks that a request naming its own image is
// left untouched and not overridden by the hub's configured box image.
func TestCreateBoxKeepsExplicitImage(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "u"}
	s := newTestServer(f)
	s.SetBoxImage("ghcr.io/clems4ever/granular-llmbox-box:latest")

	if _, err := s.createBox(context.Background(), docker.CreateOptions{BoxID: "my-box", Image: "custom:tag"}); err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if f.GotOpts.Image != "custom:tag" {
		t.Errorf("manager got image %q, want the request's explicit image", f.GotOpts.Image)
	}
}

// TestCreateBoxDestroysOnTokenFailure checks a create error propagates.
func TestCreateBoxDestroysOnTokenFailure(t *testing.T) {
	// Hard to force token failure; instead verify create error propagates.
	f := &testutils.FakeMgr{CreateErr: errors.New("no image")}
	s := newTestServer(f)
	if _, err := s.createBox(context.Background(), docker.CreateOptions{}); err == nil {
		t.Fatal("expected error")
	}
}

// TestCreateBoxRunsCreateHooks checks a configured hook runner runs on create,
// its returned files are injected into the box, and its per-hook state is
// persisted on the session.
func TestCreateBoxRunsCreateHooks(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "u"}
	h := &fakeHooks{
		createState: map[string]string{"granular-hook": "subj-xyz"},
		createFiles: []hooks.File{
			{Path: "/home/node/.granular/subject_token", Content: []byte("subj-xyz"), Mode: 0o600, UID: 1000, GID: 1000},
			{Path: "/home/node/.granular/github.yaml", Content: []byte("base_url: \"http://gh\"\n"), Mode: 0o644, UID: 1000, GID: 1000},
		},
	}
	s := New(f, h, "https://boxes.example.com", time.Minute, noopStore{}, nil)

	sess, err := s.createBox(context.Background(), docker.CreateOptions{})
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
	for _, fl := range f.GotOpts.Files {
		byPath[fl.Path] = fl
	}
	tok, ok := byPath["/home/node/.granular/subject_token"]
	if !ok {
		t.Fatalf("subject token not injected: %+v", f.GotOpts.Files)
	}
	if string(tok.Content) != "subj-xyz" || tok.UID != 1000 {
		t.Errorf("injected token = %q uid %d, want subj-xyz uid 1000", tok.Content, tok.UID)
	}
	cfg, ok := byPath["/home/node/.granular/github.yaml"]
	if !ok {
		t.Fatalf("RS config not injected: %+v", f.GotOpts.Files)
	}
	if !strings.Contains(string(cfg.Content), "http://gh") || cfg.UID != 1000 {
		t.Errorf("injected config = %q uid %d, want base_url http://gh uid 1000", cfg.Content, cfg.UID)
	}
}

// TestCreateBoxRunsDestroyHooksOnCreateFailure checks the create hook's state is
// replayed to the destroy hooks when box creation fails, so nothing is left
// dangling.
func TestCreateBoxRunsDestroyHooksOnCreateFailure(t *testing.T) {
	f := &testutils.FakeMgr{CreateErr: errors.New("no image")}
	h := &fakeHooks{createState: map[string]string{"granular-hook": "subj-doomed"}}
	s := New(f, h, "https://boxes.example.com", time.Minute, noopStore{}, nil)

	if _, err := s.createBox(context.Background(), docker.CreateOptions{}); err == nil {
		t.Fatal("expected error")
	}
	if len(h.destroyed) != 1 || h.destroyed[0]["granular-hook"] != "subj-doomed" {
		t.Errorf("destroyed = %v, want [granular-hook=subj-doomed]", h.destroyed)
	}
}

// TestDestroyRunsDestroyHooks checks destroying a box replays its hook state to
// the destroy hooks.
func TestDestroyRunsDestroyHooks(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "u"}
	h := &fakeHooks{createState: map[string]string{"granular-hook": "subj-live"}}
	s := New(f, h, "https://boxes.example.com", time.Minute, noopStore{}, nil)

	sess, err := s.createBox(context.Background(), docker.CreateOptions{})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if err := s.destroyBox(context.Background(), sess.ContainerID); err != nil {
		t.Fatalf("DestroyBox: %v", err)
	}
	if len(h.destroyed) != 1 || h.destroyed[0]["granular-hook"] != "subj-live" {
		t.Errorf("destroyed = %v, want [granular-hook=subj-live]", h.destroyed)
	}
}

// TestSubmitCodeSuccess checks the code is forwarded and the session becomes ready.
func TestSubmitCodeSuccess(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "id1", CreateURL: "u", SubmitURL: "https://claude.ai/code/s/1"}
	s := newTestServer(f)
	sess, _ := s.createBox(context.Background(), docker.CreateOptions{})

	if err := s.submitCode(context.Background(), sess.Token, "CODE"); err != nil {
		t.Fatalf("SubmitCode: %v", err)
	}
	if f.GotCode != "CODE" {
		t.Errorf("manager got code %q", f.GotCode)
	}
	status, url, _ := sess.snapshot()
	if status != "ready" || url != "https://claude.ai/code/s/1" {
		t.Errorf("session not ready: status=%q url=%q", status, url)
	}
}

// TestSubmitCodeFailureRecorded checks a submit failure is recorded on the session.
func TestSubmitCodeFailureRecorded(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "id1", CreateURL: "u", SubmitErr: errors.New("invalid code")}
	s := newTestServer(f)
	sess, _ := s.createBox(context.Background(), docker.CreateOptions{})

	if err := s.submitCode(context.Background(), sess.Token, "BAD"); err == nil {
		t.Fatal("expected error")
	}
	status, _, errMsg := sess.snapshot()
	if status != "error" || !strings.Contains(errMsg, "invalid code") {
		t.Errorf("session error not recorded: status=%q err=%q", status, errMsg)
	}
}

// TestSubmitCodeUnknownToken checks SubmitCode errors for an unknown token.
func TestSubmitCodeUnknownToken(t *testing.T) {
	s := newTestServer(&testutils.FakeMgr{})
	if err := s.submitCode(context.Background(), "nope", "code"); err == nil {
		t.Fatal("expected error for unknown token")
	}
}

// TestSubmitCodeEmpty checks SubmitCode errors for an empty code.
func TestSubmitCodeEmpty(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "id1", CreateURL: "u"}
	s := newTestServer(f)
	sess, _ := s.createBox(context.Background(), docker.CreateOptions{})
	if err := s.submitCode(context.Background(), sess.Token, "   "); err == nil {
		t.Fatal("expected error for empty code")
	}
}

// TestDestroyForgetsSession checks the session is forgotten after a destroy.
func TestDestroyForgetsSession(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "u"}
	s := newTestServer(f)
	sess, _ := s.createBox(context.Background(), docker.CreateOptions{})

	if err := s.destroyBox(context.Background(), "abcdef0123456789"); err != nil {
		t.Fatalf("DestroyBox: %v", err)
	}
	if s.lookup(sess.Token) != nil {
		t.Error("session should be forgotten after destroy")
	}
}

// TestPruneSessionsAfterReap checks a reaped box's session is pruned.
func TestPruneSessionsAfterReap(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "u"}
	s := newTestServer(f)
	sess, _ := s.createBox(context.Background(), docker.CreateOptions{})
	s.pruneSessions([]string{"abcdef012345"}) // short ID prefix, as the reaper returns
	if s.lookup(sess.Token) != nil {
		t.Error("session for reaped box should be pruned")
	}
}

// --- web handlers ---

// TestAuthPageRendersAndSubmits checks the auth page renders and the code submits.
func TestAuthPageRendersAndSubmits(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "id1", CreateURL: "https://claude.com/cai/oauth/authorize?a=b", SubmitURL: "https://claude.ai/code/s/9"}
	s := newTestServer(f)
	sess, _ := s.createBox(context.Background(), docker.CreateOptions{})
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
	if f.GotCode != "THECODE" {
		t.Errorf("code not forwarded: %q", f.GotCode)
	}
	if !strings.Contains(prec.Body.String(), "https://claude.ai/code/s/9") {
		t.Error("session URL missing from success page")
	}
}

// TestAuthPageShowsBoxAndSpoke checks the activation page names the box and the
// spoke it runs on, so the user can tell which box they are activating.
func TestAuthPageShowsBoxAndSpoke(t *testing.T) {
	s := newTestServer(&testutils.FakeMgr{CreateID: "id1", CreateURL: "https://c", SubmitURL: "https://s"})
	sess, _ := s.createBox(context.Background(), docker.CreateOptions{BoxID: "refactor-auth"})
	h := s.Handler(s.MCPServer("test", "v0"))

	req := httptest.NewRequest(http.MethodGet, "/auth/"+sess.Token, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "refactor-auth") {
		t.Error("box id missing from activation page")
	}
	if !strings.Contains(body, "spoke <b>local</b>") {
		t.Error("spoke name missing from activation page")
	}
}

// TestAuthPageUnknownToken checks the auth page 404s for an unknown token.
func TestAuthPageUnknownToken(t *testing.T) {
	s := newTestServer(&testutils.FakeMgr{})
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
	s := newTestServer(&testutils.FakeMgr{})
	h := s.Handler(s.MCPServer("test", "v0"))
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("healthz: %d %q", rec.Code, rec.Body.String())
	}
}

// TestFaviconServed checks the favicon route returns the embedded SVG.
func TestFaviconServed(t *testing.T) {
	s := newTestServer(&testutils.FakeMgr{})
	h := s.Handler(s.MCPServer("test", "v0"))
	for _, path := range []string{"/favicon.ico", "/favicon.svg"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "image/svg+xml" {
			t.Errorf("%s: content-type %q, want image/svg+xml", path, ct)
		}
		if !strings.Contains(rec.Body.String(), "<svg") {
			t.Errorf("%s: body is not an SVG", path)
		}
	}
}

// TestGetByBoxID checks get_llmbox resolves a box by box ID (case-insensitive)
// and errors for an empty or unknown box ID.
func TestGetByBoxID(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "u", SubmitURL: "https://claude.ai/code/s/1"}
	s := newTestServer(f)
	sess, err := s.createBox(context.Background(), docker.CreateOptions{BoxID: "web-box", Description: "d"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}

	// Found, case-insensitive.
	_, out, err := s.toolGet(context.Background(), nil, getInput{BoxID: "WEB-BOX"})
	if err != nil {
		t.Fatalf("toolGet: %v", err)
	}
	if out.Status != "pending" || out.BoxID != "web-box" || out.Description != "d" {
		t.Errorf("unexpected get output: %+v", out)
	}

	// Reflects status changes.
	if err := s.submitCode(context.Background(), sess.Token, "CODE"); err != nil {
		t.Fatalf("SubmitCode: %v", err)
	}
	if _, out, _ := s.toolGet(context.Background(), nil, getInput{BoxID: "web-box"}); out.Status != "ready" || out.SessionURL != "https://claude.ai/code/s/1" {
		t.Errorf("expected ready with session URL, got %+v", out)
	}

	// Empty and unknown box IDs error.
	if _, _, err := s.toolGet(context.Background(), nil, getInput{BoxID: ""}); err == nil {
		t.Error("expected error for empty box ID")
	}
	if _, _, err := s.toolGet(context.Background(), nil, getInput{BoxID: "nope"}); err == nil {
		t.Error("expected error for unknown box ID")
	}
}

// TestListLlmboxesReturnsBoxID checks list_llmboxes surfaces each box's box ID
// (the hostname the user sees) along with its description in the tool output.
func TestListLlmboxesReturnsBoxID(t *testing.T) {
	f := &testutils.FakeMgr{ListResult: []docker.Box{
		{ContainerID: "abcdef0123456789", BoxID: "web-box", Description: "front-end work"},
		{ContainerID: "0123456789abcdef"},
	}}
	s := newTestServer(f)

	_, out, err := s.toolList(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatalf("toolList: %v", err)
	}
	if len(out.Boxes) != 2 {
		t.Fatalf("got %d boxes, want 2", len(out.Boxes))
	}
	if out.Boxes[0].BoxID != "web-box" || out.Boxes[0].Description != "front-end work" {
		t.Errorf("box0 box ID/description = %q/%q, want web-box/front-end work", out.Boxes[0].BoxID, out.Boxes[0].Description)
	}
	if out.Boxes[1].BoxID != "" {
		t.Errorf("box1 box ID = %q, want empty", out.Boxes[1].BoxID)
	}
}

// TestBoxLogsByBoxID checks get_llmbox_logs resolves a box by box ID,
// forwards the box ID and tail to the manager, and errors for empty or unknown
// box IDs.
func TestBoxLogsByBoxID(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "u", LogsResult: "Ready\nlistening\n"}
	s := newTestServer(f)
	if _, err := s.createBox(context.Background(), docker.CreateOptions{BoxID: "web-box"}); err != nil {
		t.Fatalf("CreateBox: %v", err)
	}

	// Found, case-insensitive, with the tail forwarded to the manager.
	_, out, err := s.toolLogs(context.Background(), nil, logsInput{BoxID: "WEB-BOX", Tail: 25})
	if err != nil {
		t.Fatalf("toolLogs: %v", err)
	}
	if out.BoxID != "WEB-BOX" || out.Logs != "Ready\nlistening\n" {
		t.Errorf("unexpected logs output: %+v", out)
	}
	if f.GotLogsID != "abcdef0123456789" || f.GotLogsN != 25 {
		t.Errorf("manager got id=%q tail=%d, want abcdef0123456789/25", f.GotLogsID, f.GotLogsN)
	}

	// Empty and unknown box IDs error.
	if _, _, err := s.toolLogs(context.Background(), nil, logsInput{BoxID: ""}); err == nil {
		t.Error("expected error for empty box ID")
	}
	if _, _, err := s.toolLogs(context.Background(), nil, logsInput{BoxID: "nope"}); err == nil {
		t.Error("expected error for unknown box ID")
	}
}

// TestBoxExecByBoxID checks exec_llmbox resolves a box by box ID, wraps the
// command in /bin/sh -c, returns the captured output, and errors for empty or
// unknown box IDs and an empty command.
func TestBoxExecByBoxID(t *testing.T) {
	f := &testutils.FakeMgr{
		CreateID:   "abcdef0123456789",
		CreateURL:  "u",
		ExecResult: docker.ExecResult{Stdout: "hi\n", Stderr: "", ExitCode: 0},
	}
	s := newTestServer(f)
	if _, err := s.createBox(context.Background(), docker.CreateOptions{BoxID: "web-box"}); err != nil {
		t.Fatalf("CreateBox: %v", err)
	}

	// Found, case-insensitive; command wrapped and box ID forwarded.
	_, out, err := s.toolExec(context.Background(), nil, execInput{BoxID: "WEB-BOX", Command: "echo hi"})
	if err != nil {
		t.Fatalf("toolExec: %v", err)
	}
	if out.BoxID != "WEB-BOX" || out.Stdout != "hi\n" || out.ExitCode != 0 {
		t.Errorf("unexpected exec output: %+v", out)
	}
	if f.GotExecID != "abcdef0123456789" {
		t.Errorf("manager got box ID %q, want abcdef0123456789", f.GotExecID)
	}
	want := []string{"/bin/sh", "-c", "echo hi"}
	if len(f.GotExecCmd) != 3 || f.GotExecCmd[0] != want[0] || f.GotExecCmd[1] != want[1] || f.GotExecCmd[2] != want[2] {
		t.Errorf("manager ran cmd %v, want %v", f.GotExecCmd, want)
	}

	// Empty/unknown box IDs and an empty command.error.
	if _, _, err := s.toolExec(context.Background(), nil, execInput{BoxID: "", Command: "ls"}); err == nil {
		t.Error("expected error for empty box ID")
	}
	if _, _, err := s.toolExec(context.Background(), nil, execInput{BoxID: "nope", Command: "ls"}); err == nil {
		t.Error("expected error for unknown box ID")
	}
	if _, _, err := s.toolExec(context.Background(), nil, execInput{BoxID: "web-box", Command: "  "}); err == nil {
		t.Error("expected error for empty command")
	}
}

// --- MCP wiring ---

// TestMCPToolsRegisteredAndCreate checks all tools are registered and create returns a safe auth URL.
func TestMCPToolsRegisteredAndCreate(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "https://claude.com/cai/oauth/authorize?z=1"}
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

// TestCreateRequiresBoxID checks create_llmbox rejects a call with an empty box
// ID and does not create a box, so every box stays reachable by its box ID.
func TestCreateRequiresBoxID(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "u"}
	s := newTestServer(f)

	_, _, err := s.toolCreate(context.Background(), nil, createInput{Description: "no box id"})
	if err == nil {
		t.Fatal("expected error for empty box ID")
	}
	if f.GotOpts.Description != "" {
		t.Errorf("manager was called despite missing box ID: %+v", f.GotOpts)
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
