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
}

func (f *fakeMgr) CreateLLMBox(_ context.Context, _ string) (string, string, error) {
	return f.createID, f.createURL, f.createErr
}
func (f *fakeMgr) SubmitCode(_ context.Context, _, code string) (string, error) {
	f.mu.Lock()
	f.gotCode = code
	f.mu.Unlock()
	return f.submitURL, f.submitErr
}
func (f *fakeMgr) List(context.Context) ([]docker.Box, error) { return f.listResult, nil }
func (f *fakeMgr) Destroy(_ context.Context, id string) error {
	f.destroyed = append(f.destroyed, id)
	return nil
}
func (f *fakeMgr) ReapOrphans(context.Context, time.Duration) ([]string, error) {
	return f.reaped, nil
}

func newTestServer(f *fakeMgr) *Server {
	return New(f, "https://boxes.example.com", 5*time.Minute)
}

// --- core flow ---

func TestCreateBoxRegistersSession(t *testing.T) {
	f := &fakeMgr{createID: "abcdef0123456789", createURL: "https://claude.com/cai/oauth/authorize?x=1"}
	s := newTestServer(f)

	sess, err := s.CreateBox(context.Background(), "")
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if sess.BoxID != "abcdef0123456789" || sess.Status != "pending" {
		t.Errorf("unexpected session %+v", sess)
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

func TestCreateBoxDestroysOnTokenFailure(t *testing.T) {
	// Hard to force token failure; instead verify create error propagates.
	f := &fakeMgr{createErr: errors.New("no image")}
	s := newTestServer(f)
	if _, err := s.CreateBox(context.Background(), ""); err == nil {
		t.Fatal("expected error")
	}
}

func TestSubmitCodeSuccess(t *testing.T) {
	f := &fakeMgr{createID: "id1", createURL: "u", submitURL: "https://claude.ai/code/s/1"}
	s := newTestServer(f)
	sess, _ := s.CreateBox(context.Background(), "")

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

func TestSubmitCodeFailureRecorded(t *testing.T) {
	f := &fakeMgr{createID: "id1", createURL: "u", submitErr: errors.New("invalid code")}
	s := newTestServer(f)
	sess, _ := s.CreateBox(context.Background(), "")

	if err := s.SubmitCode(context.Background(), sess.Token, "BAD"); err == nil {
		t.Fatal("expected error")
	}
	status, _, errMsg := sess.snapshot()
	if status != "error" || !strings.Contains(errMsg, "invalid code") {
		t.Errorf("session error not recorded: status=%q err=%q", status, errMsg)
	}
}

func TestSubmitCodeUnknownToken(t *testing.T) {
	s := newTestServer(&fakeMgr{})
	if err := s.SubmitCode(context.Background(), "nope", "code"); err == nil {
		t.Fatal("expected error for unknown token")
	}
}

func TestSubmitCodeEmpty(t *testing.T) {
	f := &fakeMgr{createID: "id1", createURL: "u"}
	s := newTestServer(f)
	sess, _ := s.CreateBox(context.Background(), "")
	if err := s.SubmitCode(context.Background(), sess.Token, "   "); err == nil {
		t.Fatal("expected error for empty code")
	}
}

func TestDestroyForgetsSession(t *testing.T) {
	f := &fakeMgr{createID: "abcdef0123456789", createURL: "u"}
	s := newTestServer(f)
	sess, _ := s.CreateBox(context.Background(), "")

	if err := s.DestroyBox(context.Background(), "abcdef0123456789"); err != nil {
		t.Fatalf("DestroyBox: %v", err)
	}
	if s.lookup(sess.Token) != nil {
		t.Error("session should be forgotten after destroy")
	}
}

func TestPruneSessionsAfterReap(t *testing.T) {
	f := &fakeMgr{createID: "abcdef0123456789", createURL: "u"}
	s := newTestServer(f)
	sess, _ := s.CreateBox(context.Background(), "")
	s.pruneSessions([]string{"abcdef012345"}) // short ID prefix, as the reaper returns
	if s.lookup(sess.Token) != nil {
		t.Error("session for reaped box should be pruned")
	}
}

// --- web handlers ---

func TestAuthPageRendersAndSubmits(t *testing.T) {
	f := &fakeMgr{createID: "id1", createURL: "https://claude.com/cai/oauth/authorize?a=b", submitURL: "https://claude.ai/code/s/9"}
	s := newTestServer(f)
	sess, _ := s.CreateBox(context.Background(), "")
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

// --- MCP wiring ---

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
