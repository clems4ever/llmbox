package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
		CreateSess:        api.BoxSession{BoxID: "web", Generation: "cid123"},
		Sessions:          map[string]api.BoxSession{"web": {BoxID: "web", Generation: "cid123", Description: "ready"}},
		Boxes:             []api.BoxView{{Box: sandbox.Box{BoxID: "b1"}}, {Box: sandbox.Box{BoxID: "b2"}}},
		Spokes:            []api.SpokeStatus{{Name: "edge", Connected: true, Default: true}},
		CreateSpokeResult: api.SpokeEnrollment{Name: "edge", Token: "tok-1", Command: "llmbox-spoke firecracker --hub wss://x --token tok-1"},
		JoinTokens:        []api.JoinTokenInfo{{ID: "tid", Name: "edge", ExpiresAt: time.Now().Add(time.Hour)}},
		RegenTokenResult:  api.SpokeEnrollment{Name: "edge", Token: "tok-2", Command: "llmbox-spoke docker --hub wss://x --token tok-2"},
		ExecResult:        sandbox.ExecResult{Stdout: "out", Stderr: "err", ExitCode: 7},
		ProxyOn:           true,
		CreateProxyResult: api.ProxyInfo{BoxID: "web", Port: 8000, URL: "https://slug.example.com", Slug: "slug", Description: "app"},
		Proxies:           []api.ProxyInfo{{BoxID: "b1", Port: 8000}},
		PingProxyResult:   api.ProxyPing{OK: true, Status: 200, LatencyMs: 12},
	}
	ts := httptest.NewServer(api.NewHandler(fb))
	defer ts.Close()
	c := api.NewClient(ts.URL, ts.Client())
	ctx := context.Background()

	sess, err := c.CreateBox(ctx, sandbox.CreateOptions{BoxID: "web", Description: "d", SpokeName: "local"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if sess.Generation != "cid123" || sess.BoxID != "web" {
		t.Fatalf("CreateBox session = %+v", sess)
	}
	if fb.GotCreate.BoxID != "web" || fb.GotCreate.Description != "d" || fb.GotCreate.SpokeName != "local" {
		t.Fatalf("CreateBox opts not forwarded: %+v", fb.GotCreate)
	}

	if got, ok := c.LookupByBoxID("web"); !ok || got.Description != "ready" || fb.GotLookup != "web" {
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

	sp, err := c.CreateSpoke(ctx, "edge", "firecracker", time.Hour)
	if err != nil || sp.Token != "tok-1" {
		t.Fatalf("CreateSpoke = %+v err=%v", sp, err)
	}
	if fb.GotCreateSpoke != "edge" || fb.GotCreateSpokeBk != "firecracker" || fb.GotCreateSpokeTTL != time.Hour {
		t.Fatalf("CreateSpoke inputs = %q/%q/%v", fb.GotCreateSpoke, fb.GotCreateSpokeBk, fb.GotCreateSpokeTTL)
	}

	if err := c.DropSpoke(ctx, "edge"); err != nil || fb.GotDropSpoke != "edge" {
		t.Fatalf("DropSpoke err=%v name=%q", err, fb.GotDropSpoke)
	}

	if err := c.SetDefaultSpoke(ctx, "edge"); err != nil || fb.GotDefaultSpoke != "edge" {
		t.Fatalf("SetDefaultSpoke err=%v name=%q", err, fb.GotDefaultSpoke)
	}

	tokens, err := c.ListJoinTokens(ctx)
	if err != nil || len(tokens) != 1 || tokens[0].ID != "tid" {
		t.Fatalf("ListJoinTokens = %v err=%v", tokens, err)
	}

	if err := c.RevokeJoinToken(ctx, "tid"); err != nil || fb.GotRevokeToken != "tid" {
		t.Fatalf("RevokeJoinToken err=%v id=%q", err, fb.GotRevokeToken)
	}

	regen, err := c.RegenerateJoinToken(ctx, "tid")
	if err != nil || regen.Token != "tok-2" || fb.GotRegenToken != "tid" {
		t.Fatalf("RegenerateJoinToken = %+v err=%v id=%q", regen, err, fb.GotRegenToken)
	}

	if err := c.DestroyBox(ctx, "cid123"); err != nil || fb.GotDestroyID != "cid123" {
		t.Fatalf("DestroyBox err=%v id=%q", err, fb.GotDestroyID)
	}

	if err := c.PauseBox(ctx, "pz"); err != nil || fb.GotPauseID != "pz" {
		t.Fatalf("PauseBox err=%v id=%q", err, fb.GotPauseID)
	}
	if err := c.ResumeBox(ctx, "pz"); err != nil || fb.GotResumeID != "pz" {
		t.Fatalf("ResumeBox err=%v id=%q", err, fb.GotResumeID)
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

	ping, err := c.PingProxy(ctx, "web", 8000)
	if err != nil || !ping.OK || ping.Status != 200 || fb.GotPingBoxID != "web" || fb.GotPingPort != 8000 {
		t.Fatalf("PingProxy = %+v err=%v box=%q port=%d", ping, err, fb.GotPingBoxID, fb.GotPingPort)
	}
}

// TestClientOverHTTP drives the full split — the HTTP client forwarding a call
// through to a handler-backed fake — proving the client/handler split works end
// to end.
func TestClientOverHTTP(t *testing.T) {
	fb := &testutils.FakeBackend{
		CreateSess: api.BoxSession{BoxID: "web", Generation: "abcdef012345"},
	}
	ts := httptest.NewServer(api.NewHandler(fb))
	defer ts.Close()

	c := api.NewClient(ts.URL, ts.Client())
	sess, err := c.CreateBox(context.Background(), sandbox.CreateOptions{BoxID: "web"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if sess.BoxID != "web" {
		t.Errorf("box_id = %q, want web", sess.BoxID)
	}
	if sess.Generation != "abcdef012345" {
		t.Errorf("instance_id = %q, want abcdef012345", sess.Generation)
	}
	if fb.GotCreate.BoxID != "web" {
		t.Errorf("create not forwarded to backend: %+v", fb.GotCreate)
	}
}

// TestClientSendsAPIKey checks a client configured with SetAPIKey sends the key
// as a bearer token on every request, and sends no Authorization header without
// one.
func TestClientSendsAPIKey(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"boxes":[]}`))
	}))
	defer ts.Close()

	c := api.NewClient(ts.URL, ts.Client())
	if _, err := c.ListBoxes(context.Background()); err != nil {
		t.Fatalf("ListBoxes: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("keyless request sent Authorization %q, want none", gotAuth)
	}

	c.SetAPIKey("lbx_secret")
	if _, err := c.ListBoxes(context.Background()); err != nil {
		t.Fatalf("ListBoxes with key: %v", err)
	}
	if gotAuth != "Bearer lbx_secret" {
		t.Errorf("Authorization = %q, want the bearer key", gotAuth)
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
