package hub

import (
	"context"
	"encoding/json"
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
	return wireSpoke(New(nil, "https://boxes.example.com", 5*time.Minute, newTestStore(), nil), f)
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

	sess, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "my-box", Description: "scratch"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if sess.Generation != "abcdef0123456789" || sess.Status != "pending" {
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
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "u"}
	h := &fakeHooks{
		createState: map[string]string{"granular-hook": "subj-xyz"},
		createFiles: []hooks.File{
			{Path: "/home/node/.granular/subject_token", Content: []byte("subj-xyz"), Mode: 0o600, UID: 1000, GID: 1000},
			{Path: "/home/node/.granular/github.yaml", Content: []byte("base_url: \"http://gh\"\n"), Mode: 0o644, UID: 1000, GID: 1000},
		},
	}
	s := wireSpoke(New(h, "https://boxes.example.com", time.Minute, newTestStore(), nil), f)

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
	s := wireSpoke(New(h, "https://boxes.example.com", time.Minute, newTestStore(), nil), f)

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
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "u"}
	h := &fakeHooks{createState: map[string]string{"granular-hook": "subj-live"}}
	s := wireSpoke(New(h, "https://boxes.example.com", time.Minute, newTestStore(), nil), f)

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

// TestSubmitCodeSuccess checks the code is forwarded and the session becomes ready.
func TestSubmitCodeSuccess(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "id1", CreateURL: "u", SubmitURL: "https://claude.ai/code/s/1"}
	s := newTestServer(f)
	sess, _ := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "box-1"})

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
	sess, _ := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "box-1"})

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
	sess, _ := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "box-1"})
	if err := s.submitCode(context.Background(), sess.Token, "   "); err == nil {
		t.Fatal("expected error for empty code")
	}
}

// TestDestroyForgetsSession checks the session is forgotten after a destroy.
func TestDestroyForgetsSession(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "u"}
	s := newTestServer(f)
	sess, _ := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "box-1"})

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
	sess, _ := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "box-1"})
	s.pruneSessions([]string{"abcdef0123456789"}) // the box's generation token, as the reaper returns
	if s.lookup(sess.Token) != nil {
		t.Error("session for reaped box should be pruned")
	}
}

// --- web handlers ---

// authStateJSON GETs an auth session's JSON state and decodes it.
func authStateJSON(t *testing.T, h http.Handler, token string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/auth/"+token+"/state", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decoding state: %v", err)
		}
	}
	return rec.Code, out
}

// TestAuthPageRendersAndSubmits checks the activation state endpoint carries the
// authorize URL and the code submit endpoint returns the session URL.
func TestAuthPageRendersAndSubmits(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "id1", CreateURL: "https://claude.com/cai/oauth/authorize?a=b", SubmitURL: "https://claude.ai/code/s/9"}
	s := newTestServer(f)
	sess, _ := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "box-1"})
	h := s.APIHandler()

	// GET the state: with auth disabled, the full state (authorize URL) is open.
	code, st := authStateJSON(t, h, sess.Token)
	if code != http.StatusOK {
		t.Fatalf("GET state status %d", code)
	}
	if st["authorize_url"] != "https://claude.com/cai/oauth/authorize?a=b" {
		t.Errorf("authorize URL missing from state: %v", st)
	}
	if st["status"] != "pending" {
		t.Errorf("status = %v, want pending", st["status"])
	}

	// POST the code: box becomes ready, the response carries the session URL.
	preq := httptest.NewRequest(http.MethodPost, "/auth/"+sess.Token+"/code", strings.NewReader(`{"code":"THECODE"}`))
	preq.Header.Set("Content-Type", "application/json")
	prec := httptest.NewRecorder()
	h.ServeHTTP(prec, preq)
	if prec.Code != http.StatusOK {
		t.Fatalf("POST status %d: %s", prec.Code, prec.Body.String())
	}
	if f.GotCode != "THECODE" {
		t.Errorf("code not forwarded: %q", f.GotCode)
	}
	var out map[string]any
	if err := json.Unmarshal(prec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding submit response: %v", err)
	}
	if out["status"] != "ready" || out["session_url"] != "https://claude.ai/code/s/9" {
		t.Errorf("submit response = %v, want ready with session URL", out)
	}
}

// TestAuthPageShowsBoxAndSpoke checks the activation state names the box and the
// runner it runs on, so the user can tell which workspace they are activating.
func TestAuthPageShowsBoxAndSpoke(t *testing.T) {
	s := newTestServer(&testutils.FakeMgr{CreateID: "id1", CreateURL: "https://c", SubmitURL: "https://s"})
	sess, _ := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "refactor-auth"})

	code, st := authStateJSON(t, s.APIHandler(), sess.Token)
	if code != http.StatusOK {
		t.Fatalf("GET state status %d", code)
	}
	if st["box_id"] != "refactor-auth" {
		t.Errorf("box id missing from state: %v", st)
	}
	if st["spoke"] != testSpoke {
		t.Errorf("runner (spoke) name missing from state: %v", st)
	}
}

// TestAuthPageServesShell checks GET /auth/{token} serves the static activation
// shell (the built web page) rather than any server-rendered state.
func TestAuthPageServesShell(t *testing.T) {
	s := newTestServer(&testutils.FakeMgr{})
	h := s.APIHandler()
	req := httptest.NewRequest(http.MethodGet, "/auth/anytoken", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET shell status %d (is the web app built? run `make web`)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<!doctype html>") && !strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
		t.Error("expected the HTML shell")
	}
}

// TestAuthPageUnknownToken checks the state endpoint 404s for an unknown token.
func TestAuthPageUnknownToken(t *testing.T) {
	s := newTestServer(&testutils.FakeMgr{})
	code, _ := authStateJSON(t, s.APIHandler(), "deadbeef")
	if code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}

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
// (case-insensitive), reflects its status changes, and misses an unknown box ID.
func TestGetByBoxID(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "u", SubmitURL: "https://claude.ai/code/s/1"}
	s := newTestServer(f)
	b := s.boxBackend()
	sess, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "web-box", Description: "d"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}

	// Found, case-insensitive; the flattened session carries the box's state.
	got, ok := b.LookupByBoxID("WEB-BOX")
	if !ok {
		t.Fatal("expected to find box by box ID")
	}
	if got.Status != "pending" || got.BoxID != "web-box" || got.Description != "d" {
		t.Errorf("unexpected lookup: %+v", got)
	}

	// Reflects status changes via snapshot.
	if err := s.submitCode(context.Background(), sess.Token, "CODE"); err != nil {
		t.Fatalf("SubmitCode: %v", err)
	}
	if got, _ := b.LookupByBoxID("web-box"); got.Status != "ready" || got.SessionURL != "https://claude.ai/code/s/1" {
		t.Errorf("expected ready with session URL, got %+v", got)
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

// TestBoxLogsByBoxID checks the MCP backend resolves a box by box ID, forwards
// the box ID and tail to the manager, and errors for an unknown box ID.
func TestBoxLogsByBoxID(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "u", LogsResult: "Ready\nlistening\n"}
	s := newTestServer(f)
	b := s.boxBackend()
	if _, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "web-box"}); err != nil {
		t.Fatalf("CreateBox: %v", err)
	}

	// Found, case-insensitive, with the tail forwarded to the manager.
	logs, err := b.BoxLogs(context.Background(), "WEB-BOX", 25)
	if err != nil {
		t.Fatalf("BoxLogs: %v", err)
	}
	if logs != "Ready\nlistening\n" {
		t.Errorf("unexpected logs: %q", logs)
	}
	if f.GotLogsID != "web-box" || f.GotLogsN != 25 {
		t.Errorf("manager got id=%q tail=%d, want web-box/25 (the hub addresses boxes by box ID)", f.GotLogsID, f.GotLogsN)
	}

	// Unknown box IDs error.
	if _, err := b.BoxLogs(context.Background(), "nope", 0); err == nil {
		t.Error("expected error for unknown box ID")
	}
}

// TestBoxExecByBoxID checks the MCP backend resolves a box by box ID, wraps the
// command in /bin/sh -c, returns the captured output, and errors for an unknown
// box ID and an empty command.
func TestBoxExecByBoxID(t *testing.T) {
	f := &testutils.FakeMgr{
		CreateID:   "abcdef0123456789",
		CreateURL:  "u",
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

// TestBoxToolsOverBackend checks all tools are registered and create returns a safe auth URL.
func TestBoxToolsOverBackend(t *testing.T) {
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
