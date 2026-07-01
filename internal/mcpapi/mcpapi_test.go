package mcpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clems4ever/llmbox/internal/mcpapi"
	"github.com/clems4ever/llmbox/internal/mcpserver"
	"github.com/clems4ever/llmbox/internal/sandbox"
)

// stubBackend is an in-memory mcpserver.Backend that records the arguments it
// receives and returns canned results, so the round-trip test can assert both
// directions of every route. When fail is set, the fallible methods return an
// error to exercise the client's error path.
type stubBackend struct {
	fail bool

	gotOpts       sandbox.CreateOptions
	gotAuthToken  string
	gotLookup     string
	gotDestroyID  string
	gotLogsBox    string
	gotLogsTail   int
	gotExecBox    string
	gotExecCmd    string
	gotProxyBox   string
	gotProxyPort  int
	gotProxyDesc  string
	gotDeleteBox  string
	gotDeletePort int
	gotListBox    string
}

var errStub = errors.New("stub failure")

// CreateBox records the options and returns a canned session (or an error when fail is set).
func (s *stubBackend) CreateBox(_ context.Context, opts sandbox.CreateOptions) (mcpserver.BoxSession, error) {
	s.gotOpts = opts
	if s.fail {
		return mcpserver.BoxSession{}, errStub
	}
	return mcpserver.BoxSession{BoxID: opts.BoxID, ContainerID: "cid123", Token: "tok"}, nil
}

// AuthPageURL records the token and returns a canned auth page URL.
func (s *stubBackend) AuthPageURL(token string) string {
	s.gotAuthToken = token
	return "https://boxes.example.com/auth/" + token
}

// LookupByBoxID records the box ID and returns a canned session, or not-found for "missing".
func (s *stubBackend) LookupByBoxID(boxID string) (mcpserver.BoxSession, bool) {
	s.gotLookup = boxID
	if boxID == "missing" {
		return mcpserver.BoxSession{}, false
	}
	return mcpserver.BoxSession{BoxID: boxID, ContainerID: "cid123", Status: "ready"}, true
}

// ListBoxes returns two canned boxes (or an error when fail is set).
func (s *stubBackend) ListBoxes(context.Context) ([]sandbox.Box, error) {
	if s.fail {
		return nil, errStub
	}
	return []sandbox.Box{{BoxID: "b1"}, {BoxID: "b2"}}, nil
}

// SpokeStatuses returns a single canned local spoke.
func (s *stubBackend) SpokeStatuses(context.Context) ([]mcpserver.SpokeStatus, error) {
	return []mcpserver.SpokeStatus{{Name: "local", Connected: true, Local: true}}, nil
}

// DestroyBox records the container ID (returning an error when fail is set).
func (s *stubBackend) DestroyBox(_ context.Context, containerID string) error {
	s.gotDestroyID = containerID
	if s.fail {
		return errStub
	}
	return nil
}

// BoxLogs records the box ID and tail and returns canned log output.
func (s *stubBackend) BoxLogs(_ context.Context, boxID string, tail int) (string, error) {
	s.gotLogsBox, s.gotLogsTail = boxID, tail
	return "log output", nil
}

// BoxExec records the box ID and command and returns a canned exec result.
func (s *stubBackend) BoxExec(_ context.Context, boxID, command string) (sandbox.ExecResult, error) {
	s.gotExecBox, s.gotExecCmd = boxID, command
	return sandbox.ExecResult{Stdout: "out", Stderr: "err", ExitCode: 7}, nil
}

// ProxyEnabled reports that proxying is enabled.
func (s *stubBackend) ProxyEnabled() bool { return true }

// CreateProxy records the box ID, port, and description and returns a canned proxy.
func (s *stubBackend) CreateProxy(_ context.Context, boxID string, port int, description string) (mcpserver.ProxyInfo, error) {
	s.gotProxyBox, s.gotProxyPort, s.gotProxyDesc = boxID, port, description
	return mcpserver.ProxyInfo{BoxID: boxID, Port: port, URL: "https://slug.example.com", Slug: "slug", Description: description}, nil
}

// DeleteProxy records the box ID and port.
func (s *stubBackend) DeleteProxy(_ context.Context, boxID string, port int) error {
	s.gotDeleteBox, s.gotDeletePort = boxID, port
	return nil
}

// ListProxies records the box-ID filter and returns a single canned proxy.
func (s *stubBackend) ListProxies(_ context.Context, boxID string) ([]mcpserver.ProxyInfo, error) {
	s.gotListBox = boxID
	return []mcpserver.ProxyInfo{{BoxID: "b1", Port: 8000}}, nil
}

// TestBackendAPIRoundTrip drives every Backend method through the HTTP client
// against a handler backed by a stub, asserting both that each argument reaches
// the backend and that each result comes back intact.
func TestBackendAPIRoundTrip(t *testing.T) {
	stub := &stubBackend{}
	ts := httptest.NewServer(mcpapi.NewHandler(stub))
	defer ts.Close()
	c := mcpapi.NewClient(ts.URL, ts.Client())
	ctx := context.Background()

	sess, err := c.CreateBox(ctx, sandbox.CreateOptions{BoxID: "web", Image: "img", Description: "d", SpokeName: "local"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if sess.ContainerID != "cid123" || sess.Token != "tok" || sess.BoxID != "web" {
		t.Fatalf("CreateBox session = %+v", sess)
	}
	if stub.gotOpts.Image != "img" || stub.gotOpts.SpokeName != "local" {
		t.Fatalf("CreateBox opts not forwarded: %+v", stub.gotOpts)
	}

	if url := c.AuthPageURL("tok"); url != "https://boxes.example.com/auth/tok" || stub.gotAuthToken != "tok" {
		t.Fatalf("AuthPageURL = %q (got token %q)", url, stub.gotAuthToken)
	}

	if got, ok := c.LookupByBoxID("web"); !ok || got.Status != "ready" || stub.gotLookup != "web" {
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
	if err != nil || len(spokes) != 1 || !spokes[0].Local {
		t.Fatalf("SpokeStatuses = %v, %v", spokes, err)
	}

	if err := c.DestroyBox(ctx, "cid123"); err != nil || stub.gotDestroyID != "cid123" {
		t.Fatalf("DestroyBox err=%v id=%q", err, stub.gotDestroyID)
	}

	logs, err := c.BoxLogs(ctx, "web", 42)
	if err != nil || logs != "log output" || stub.gotLogsBox != "web" || stub.gotLogsTail != 42 {
		t.Fatalf("BoxLogs = %q err=%v (box %q tail %d)", logs, err, stub.gotLogsBox, stub.gotLogsTail)
	}

	res, err := c.BoxExec(ctx, "web", "ls -la")
	if err != nil || res.ExitCode != 7 || res.Stdout != "out" || stub.gotExecCmd != "ls -la" {
		t.Fatalf("BoxExec = %+v err=%v cmd=%q", res, err, stub.gotExecCmd)
	}

	if !c.ProxyEnabled() {
		t.Fatal("ProxyEnabled = false, want true")
	}

	p, err := c.CreateProxy(ctx, "web", 8000, "app")
	if err != nil || p.Slug != "slug" || stub.gotProxyPort != 8000 || stub.gotProxyDesc != "app" {
		t.Fatalf("CreateProxy = %+v err=%v", p, err)
	}

	if err := c.DeleteProxy(ctx, "web", 8000); err != nil || stub.gotDeleteBox != "web" || stub.gotDeletePort != 8000 {
		t.Fatalf("DeleteProxy err=%v box=%q port=%d", err, stub.gotDeleteBox, stub.gotDeletePort)
	}

	proxies, err := c.ListProxies(ctx, "b1")
	if err != nil || len(proxies) != 1 || stub.gotListBox != "b1" {
		t.Fatalf("ListProxies = %v err=%v box=%q", proxies, err, stub.gotListBox)
	}
}

// TestClientSurfacesServerError checks a backend error is carried across the wire
// and surfaced as a Go error by the client.
func TestClientSurfacesServerError(t *testing.T) {
	ts := httptest.NewServer(mcpapi.NewHandler(&stubBackend{fail: true}))
	defer ts.Close()
	c := mcpapi.NewClient(ts.URL, ts.Client())

	_, err := c.CreateBox(context.Background(), sandbox.CreateOptions{BoxID: "x"})
	if err == nil || !strings.Contains(err.Error(), "stub failure") {
		t.Fatalf("CreateBox err = %v, want the server error message", err)
	}
}

// TestHandlerRejectsBadJSON checks the handler returns 400 for a malformed JSON
// request body rather than treating it as an empty request.
func TestHandlerRejectsBadJSON(t *testing.T) {
	ts := httptest.NewServer(mcpapi.NewHandler(&stubBackend{}))
	defer ts.Close()

	res, err := ts.Client().Post(ts.URL+mcpapi.PathCreateBox, "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}
