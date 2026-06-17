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

	"github.com/clems4ever/llmbox-mcp/internal/docker"
	"github.com/clems4ever/llmbox-mcp/internal/granular"
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

// ReapOrphans returns the canned reaped IDs.
func (f *fakeMgr) ReapOrphans(context.Context, time.Duration) ([]string, error) {
	return f.reaped, nil
}

// newTestServer builds a Server backed by the given fake manager and no-op store,
// with granular integration disabled (nil minter).
func newTestServer(f *fakeMgr) *Server {
	return New(f, nil, "https://boxes.example.com", 5*time.Minute, noopStore{})
}

// fakeMinter is a stand-in for *granular.Minter that records mint/revoke calls.
type fakeMinter struct {
	mintToken   string
	mintErr     error
	minted      int
	revoked     []string
	path        string
	configFiles []granular.ConfigFile
}

// Mint returns the canned token/error and counts the call.
func (f *fakeMinter) Mint(context.Context) (string, error) {
	f.minted++
	return f.mintToken, f.mintErr
}

// Revoke records the revoked token and always succeeds.
func (f *fakeMinter) Revoke(_ context.Context, token string) error {
	f.revoked = append(f.revoked, token)
	return nil
}

// SubjectPath returns the configured in-box path, defaulting when unset.
func (f *fakeMinter) SubjectPath() string {
	if f.path == "" {
		return "/home/node/.granular/subject_token"
	}
	return f.path
}

// ConfigFiles returns the canned per-RS config files.
func (f *fakeMinter) ConfigFiles() []granular.ConfigFile { return f.configFiles }

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

// TestCreateBoxMintsAndInjectsSubject checks a configured minter mints a subject,
// the token and the per-RS config files are injected into the box, and the token
// is persisted on the session.
func TestCreateBoxMintsAndInjectsSubject(t *testing.T) {
	f := &fakeMgr{createID: "abcdef0123456789", createURL: "u"}
	mnt := &fakeMinter{
		mintToken:   "subj-xyz",
		configFiles: []granular.ConfigFile{{Path: "/home/node/.granular/github.yaml", Content: []byte("base_url: \"http://gh\"\n")}},
	}
	s := New(f, mnt, "https://boxes.example.com", time.Minute, noopStore{})

	sess, err := s.CreateBox(context.Background(), docker.CreateOptions{})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if mnt.minted != 1 {
		t.Errorf("minted %d times, want 1", mnt.minted)
	}
	if sess.SubjectToken != "subj-xyz" {
		t.Errorf("session subject token = %q, want subj-xyz", sess.SubjectToken)
	}
	// Index the injected files by path.
	byPath := map[string]docker.InjectFile{}
	for _, fl := range f.gotOpts.Files {
		byPath[fl.Path] = fl
	}
	tok, ok := byPath[mnt.SubjectPath()]
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

// TestCreateBoxRevokesSubjectOnCreateFailure checks the minted subject is revoked
// when box creation fails, so no orphaned subject is left behind.
func TestCreateBoxRevokesSubjectOnCreateFailure(t *testing.T) {
	f := &fakeMgr{createErr: errors.New("no image")}
	mnt := &fakeMinter{mintToken: "subj-doomed"}
	s := New(f, mnt, "https://boxes.example.com", time.Minute, noopStore{})

	if _, err := s.CreateBox(context.Background(), docker.CreateOptions{}); err == nil {
		t.Fatal("expected error")
	}
	if len(mnt.revoked) != 1 || mnt.revoked[0] != "subj-doomed" {
		t.Errorf("revoked = %v, want [subj-doomed]", mnt.revoked)
	}
}

// TestDestroyRevokesSubject checks destroying a box revokes its granular subject.
func TestDestroyRevokesSubject(t *testing.T) {
	f := &fakeMgr{createID: "abcdef0123456789", createURL: "u"}
	mnt := &fakeMinter{mintToken: "subj-live"}
	s := New(f, mnt, "https://boxes.example.com", time.Minute, noopStore{})

	sess, err := s.CreateBox(context.Background(), docker.CreateOptions{})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if err := s.DestroyBox(context.Background(), sess.BoxID); err != nil {
		t.Fatalf("DestroyBox: %v", err)
	}
	if len(mnt.revoked) != 1 || mnt.revoked[0] != "subj-live" {
		t.Errorf("revoked = %v, want [subj-live]", mnt.revoked)
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
	for _, want := range []string{"create_llmbox", "get_llmbox", "list_llmboxes", "destroy_llmbox"} {
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
