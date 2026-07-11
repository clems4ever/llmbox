package hub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/clems4ever/llmbox/internal/hub/apikey"
	"github.com/clems4ever/llmbox/internal/hub/hooks"
	"github.com/clems4ever/llmbox/internal/hub/store"
	"github.com/clems4ever/llmbox/internal/shared/cluster"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/testutils"
)

// testSpoke is the name the test fake manager is registered under, and the default
// spoke a box created with no explicit spoke routes to.
const testSpoke = "spoke-1"

// hashTok is store.HashToken — the key the registry and store hold a box token
// under. Tests that seed the store or registry with a readable label (e.g.
// "live") use it so the label hashes to the same key production computes, and
// lookup(label) then resolves it.
func hashTok(plain string) string { return store.HashToken(plain) }

// regSession registers sess in the registry under the readable plaintext token,
// keying by the token's hash exactly as createBox does (Token is the hash), and
// returns sess. It replaces the old direct s.byToken[token] = sess seeding, which
// predated hashing the token at rest.
func (s *Server) regSession(plain string, sess *session) *session {
	sess.Token = hashTok(plain)
	s.mu.Lock()
	s.byToken[sess.Token] = sess
	s.mu.Unlock()
	return sess
}

// lookupTok returns the registered session whose token is the hash of plain, or
// nil when none is registered. It is the test seam that replaced the removed
// lookup-by-plaintext helper: tests seed a session under a readable label with
// regSession and resolve it back here.
func (s *Server) lookupTok(plain string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byToken[hashTok(plain)]
}

// testStore is a NoopStore that additionally keeps settings and API keys in
// memory, so a server under test can persist its default spoke and authenticate
// API-key requests.
type testStore struct {
	testutils.NoopStore
	mu       sync.Mutex
	settings map[string]string
	apiKeys  map[string]store.APIKeyRecord
}

// newTestStore builds an empty settings- and API-key-capable test store.
func newTestStore() *testStore {
	return &testStore{settings: map[string]string{}, apiKeys: map[string]store.APIKeyRecord{}}
}

// PutAPIKey records an API key in memory.
func (s *testStore) PutAPIKey(hash string, rec store.APIKeyRecord) error {
	s.mu.Lock()
	s.apiKeys[hash] = rec
	s.mu.Unlock()
	return nil
}

// GetAPIKey reads an API key from memory.
func (s *testStore) GetAPIKey(hash string) (store.APIKeyRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.apiKeys[hash]
	return rec, ok, nil
}

// PutSetting records a setting in memory.
func (s *testStore) PutSetting(key, value string) error {
	s.mu.Lock()
	s.settings[key] = value
	s.mu.Unlock()
	return nil
}

// GetSetting reads a setting from memory.
func (s *testStore) GetSetting(key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.settings[key]
	return v, ok, nil
}

// wireSpoke registers mgr as the single connected spoke (testSpoke), enrolls it in
// the store, and makes it the default, so a box created with no explicit spoke
// routes to mgr and its sessions are not treated as departed. The server's store
// must support settings (newTestStore or a real store) for the default to persist.
// It returns the server for chaining.
func wireSpoke(s *Server, mgr boxManager) *Server {
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{testSpoke: mgr}})
	_ = s.store.PutSpoke(testSpoke, cluster.SpokeRecord{Name: testSpoke, EnrolledAt: time.Now()})
	_ = s.SetDefaultSpoke(testSpoke)
	return s
}

// newTestServer builds a Server whose hub holds the given fake manager as the
// single connected spoke (testSpoke), set as the default, so a box created with no
// explicit spoke routes to it. Hook integration is disabled (nil hooks).
func newTestServer(f *testutils.FakeMgr) *Server {
	return wireSpoke(New(nil, "https://boxes.example.com", newTestStore(), nil), f)
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

// TestCreateBoxRegistersSession checks the session, token, and opts pass-through.
func TestCreateBoxRegistersSession(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789"}
	s := newTestServer(f)

	sess, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "my-box", Description: "scratch"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if sess.Generation != "abcdef0123456789" || sess.Phase != "ready" {
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
	if s.lookupByBoxID(sess.BoxID) == nil {
		t.Error("session not registered")
	}
}

// TestCreateBoxRegistersBrokenBox checks that a box whose init script failed is
// registered (not dropped) with the "broken" phase and the script output as its
// error, and surfaces the same way — phase broken, last_error set, no auth URL —
// through the box list the UI reads.
func TestCreateBoxRegistersBrokenBox(t *testing.T) {
	f := &testutils.FakeMgr{
		CreateID:               "abcdef0123456789",
		CreateInitScriptFailed: true,
		CreateInitScriptOutput: "init script failed: exit status 9\n\nboom-in-init",
	}
	s := newTestServer(f)

	sess, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "bad-box"})
	if err != nil {
		t.Fatalf("createBox should keep a broken box, not error: %v", err)
	}
	if sess.Phase != sandbox.PhaseBroken {
		t.Errorf("phase = %q, want %q", sess.Phase, sandbox.PhaseBroken)
	}
	if sess.InitError != "init script failed: exit status 9\n\nboom-in-init" {
		t.Errorf("session error = %q, want the init script output", sess.InitError)
	}
	if s.lookupByBoxID(sess.BoxID) == nil {
		t.Error("broken session not registered")
	}

	boxes, err := apiBackend{s}.ListBoxes(context.Background())
	if err != nil || len(boxes) != 1 {
		t.Fatalf("ListBoxes = %v, %v", boxes, err)
	}
	if boxes[0].Phase != sandbox.PhaseBroken {
		t.Errorf("listed phase = %q, want broken", boxes[0].Phase)
	}
	if !strings.Contains(boxes[0].LastError, "boom-in-init") {
		t.Errorf("listed last_error = %q, want the script output", boxes[0].LastError)
	}
}

// TestCreateBoxDestroysOnTokenFailure checks a create error propagates.
func TestCreateBoxDestroysOnTokenFailure(t *testing.T) {
	// Hard to force token failure; instead verify create error propagates.
	f := &testutils.FakeMgr{CreateErr: errors.New("no image")}
	s := newTestServer(f)
	if _, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "box-1"}); err == nil {
		t.Fatal("expected error")
	}
}

// TestCreateBoxRunsCreateHooks checks a configured hook runner runs on create,
// its returned files are injected into the box, and its per-hook state is
// persisted on the session.
func TestCreateBoxRunsCreateHooks(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789"}
	h := &fakeHooks{
		createState: map[string]string{"granular-hook": "subj-xyz"},
		createFiles: []hooks.File{
			{Path: "/home/node/.granular/subject_token", Content: []byte("subj-xyz"), Mode: 0o600, UID: 1000, GID: 1000},
			{Path: "/home/node/.granular/github.yaml", Content: []byte("base_url: \"http://gh\"\n"), Mode: 0o644, UID: 1000, GID: 1000},
		},
	}
	s := wireSpoke(New(h, "https://boxes.example.com", newTestStore(), nil), f)

	sess, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "box-1"})
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
	byPath := map[string]sandbox.InjectFile{}
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
	s := wireSpoke(New(h, "https://boxes.example.com", newTestStore(), nil), f)

	if _, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "box-1"}); err == nil {
		t.Fatal("expected error")
	}
	if len(h.destroyed) != 1 || h.destroyed[0]["granular-hook"] != "subj-doomed" {
		t.Errorf("destroyed = %v, want [granular-hook=subj-doomed]", h.destroyed)
	}
}

// TestDestroyRunsDestroyHooks checks destroying a box replays its hook state to
// the destroy hooks.
func TestDestroyRunsDestroyHooks(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789"}
	h := &fakeHooks{createState: map[string]string{"granular-hook": "subj-live"}}
	s := wireSpoke(New(h, "https://boxes.example.com", newTestStore(), nil), f)

	sess, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "box-1"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if err := s.destroyBox(context.Background(), sess.Generation); err != nil {
		t.Fatalf("DestroyBox: %v", err)
	}
	if len(h.destroyed) != 1 || h.destroyed[0]["granular-hook"] != "subj-live" {
		t.Errorf("destroyed = %v, want [granular-hook=subj-live]", h.destroyed)
	}
}

// TestDestroyForgetsSession checks the session is forgotten after a destroy.
func TestDestroyForgetsSession(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789"}
	s := newTestServer(f)
	sess, _ := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "box-1"})

	if err := s.destroyBox(context.Background(), "abcdef0123456789"); err != nil {
		t.Fatalf("DestroyBox: %v", err)
	}
	if s.lookupByBoxID(sess.BoxID) != nil {
		t.Error("session should be forgotten after destroy")
	}
}

// TestPruneSessionsAfterReap checks a reaped box's session is pruned.
func TestPruneSessionsAfterReap(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789"}
	s := newTestServer(f)
	sess, _ := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "box-1"})
	s.pruneSessions([]string{"abcdef0123456789"}) // the box's generation token, as the reaper returns
	if s.lookupByBoxID(sess.BoxID) != nil {
		t.Error("session for reaped box should be pruned")
	}
}

// --- web handlers ---

// TestHealthz checks the health endpoint returns ok.
func TestHealthz(t *testing.T) {
	s := newTestServer(&testutils.FakeMgr{})
	h := s.APIHandler()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("healthz: %d %q", rec.Code, rec.Body.String())
	}
}

// TestAPIHandlerServesUIAndAPI checks the single handler serves both the UI routes
// (e.g. /healthz) and the box-control API (under /api/v1/), and 404s an unrouted
// root.
func TestAPIHandlerServesUIAndAPI(t *testing.T) {
	s := newTestServer(&testutils.FakeMgr{})
	h := s.APIHandler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("healthz on handler: %d %q", rec.Code, rec.Body.String())
	}

	// The box-control API is mounted under /api/v1/ on the same handler, behind
	// the API auth gate: anonymous is rejected, a minted API key is admitted.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/list-boxes", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous list-boxes on handler: status %d, want 401", rec.Code)
	}
	key, err := apikey.Create(s.store, "test", time.Hour, time.Now())
	if err != nil {
		t.Fatalf("mint api key: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/list-boxes", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("keyed list-boxes on handler: status %d, want 200", rec.Code)
	}

	// An unrouted root 404s.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("root on handler: status %d, want 404", rec.Code)
	}
}

// TestFaviconServed checks the favicon route returns the embedded SVG.
func TestFaviconServed(t *testing.T) {
	s := newTestServer(&testutils.FakeMgr{})
	h := s.APIHandler()
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

// TestGetByBoxID checks the MCP backend resolves a box by box ID
// (case-insensitive), flattens its identity, and misses an unknown box ID.
func TestGetByBoxID(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789"}
	s := newTestServer(f)
	b := s.boxBackend()
	if _, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "web-box", Description: "d"}); err != nil {
		t.Fatalf("CreateBox: %v", err)
	}

	// Found, case-insensitive; the flattened session carries the box's identity.
	got, ok := b.LookupByBoxID("WEB-BOX")
	if !ok {
		t.Fatal("expected to find box by box ID")
	}
	if got.BoxID != "web-box" || got.Description != "d" || got.Generation != "abcdef0123456789" {
		t.Errorf("unexpected lookup: %+v", got)
	}

	// Unknown box IDs miss.
	if _, ok := b.LookupByBoxID("nope"); ok {
		t.Error("expected miss for unknown box ID")
	}
}

// TestListLlmboxesReturnsBoxID checks the MCP backend's box listing surfaces
// each box's box ID (the hostname the user sees) along with its description.
func TestListLlmboxesReturnsBoxID(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789"}
	s := newTestServer(f)
	if _, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "web-box", Description: "front-end work"}); err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	f.CreateID = "0123456789abcdef"
	if _, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "api-box"}); err != nil {
		t.Fatalf("CreateBox second box: %v", err)
	}

	boxes, err := s.boxBackend().ListBoxes(context.Background())
	if err != nil {
		t.Fatalf("ListBoxes: %v", err)
	}
	if len(boxes) != 2 {
		t.Fatalf("got %d boxes, want 2", len(boxes))
	}
	byID := map[string]bool{}
	for _, b := range boxes {
		byID[b.BoxID] = true
		if b.BoxID == "web-box" && b.Description != "front-end work" {
			t.Errorf("web-box description = %q, want front-end work", b.Description)
		}
	}
	if !byID["web-box"] || !byID["api-box"] {
		t.Errorf("expected web-box and api-box, got %+v", boxes)
	}
}

// TestBoxExecByBoxID checks the MCP backend resolves a box by box ID, wraps the
// command in /bin/sh -c, returns the captured output, and errors for an unknown
// box ID and an empty command.
func TestBoxExecByBoxID(t *testing.T) {
	f := &testutils.FakeMgr{
		CreateID:   "abcdef0123456789",
		ExecResult: sandbox.ExecResult{Stdout: "hi\n", Stderr: "", ExitCode: 0},
	}
	s := newTestServer(f)
	b := s.boxBackend()
	if _, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "web-box"}); err != nil {
		t.Fatalf("CreateBox: %v", err)
	}

	// Found, case-insensitive; command wrapped and box ID forwarded.
	res, err := b.BoxExec(context.Background(), "WEB-BOX", "echo hi")
	if err != nil {
		t.Fatalf("BoxExec: %v", err)
	}
	if res.Stdout != "hi\n" || res.ExitCode != 0 {
		t.Errorf("unexpected exec result: %+v", res)
	}
	if f.GotExecID != "web-box" {
		t.Errorf("manager got box ID %q, want web-box (the hub addresses boxes by box ID)", f.GotExecID)
	}
	want := []string{"/bin/sh", "-c", "echo hi"}
	if len(f.GotExecCmd) != 3 || f.GotExecCmd[0] != want[0] || f.GotExecCmd[1] != want[1] || f.GotExecCmd[2] != want[2] {
		t.Errorf("manager ran cmd %v, want %v", f.GotExecCmd, want)
	}

	// An unknown box ID and an empty command error.
	if _, err := b.BoxExec(context.Background(), "nope", "ls"); err == nil {
		t.Error("expected error for unknown box ID")
	}
	if _, err := b.BoxExec(context.Background(), "web-box", "  "); err == nil {
		t.Error("expected error for empty command")
	}
}

// --- MCP wiring ---

// TestBoxToolsOverBackend checks all tools are registered and create returns the
// box's ID and instance ID.
func TestBoxToolsOverBackend(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789"}
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
	for _, want := range []string{"create_llmbox", "get_llmbox", "list_llmboxes", "destroy_llmbox", "exec_llmbox"} {
		if !names[want] {
			t.Errorf("tool %q not registered (have %v)", want, names)
		}
	}

	// create_llmbox returns the assigned box ID and the backend instance ID.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_llmbox", Arguments: map[string]any{"box_id": "web-box"}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool error: %v", res.Content)
	}
	out, _ := res.StructuredContent.(map[string]any)
	if out["box_id"] != "web-box" {
		t.Errorf("box_id = %v, want web-box", out["box_id"])
	}
	if out["instance_id"] != "abcdef0123456789" {
		t.Errorf("instance_id = %v, want the backend generation", out["instance_id"])
	}
}

// TestCreateRequiresBoxID checks create_llmbox rejects a call with an empty box
// ID and does not create a box, so every box stays reachable by its box ID.
func TestCreateRequiresBoxID(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789"}
	s := newTestServer(f)
	cs := connectMCP(t, s)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_llmbox", Arguments: map[string]any{"description": "no box id"}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected error for empty box ID")
	}
	if f.GotOpts.Description != "" {
		t.Errorf("manager was called despite missing box ID: %+v", f.GotOpts)
	}
}

// connectMCP wires an in-memory MCP client to an MCP server built over the
// server's backend and returns the session. The production MCP server is the
// stand-alone llmbox-mcp binary; here we build one in-process from the same
// backend (via the shared testutils fixture) to drive the tools end to end.
func connectMCP(t *testing.T, s *Server) *mcp.ClientSession {
	t.Helper()
	return testutils.ConnectMCP(t, s.boxBackend(), "test", "v0")
}
