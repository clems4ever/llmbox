package boxapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/cluster"
)

// fakeService is a recording PortService asserting the handler stamps its
// bound box ID onto every forwarded call. When err is set, every call fails
// with it.
type fakeService struct {
	mu sync.Mutex

	info  cluster.BoxPortInfo
	ports []cluster.BoxPortInfo
	err   error

	lastBoxID string
	lastPort  int
	lastDesc  string
}

// OpenBoxPort is a test helper.
func (f *fakeService) OpenBoxPort(_ context.Context, boxID string, port int, description string) (cluster.BoxPortInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastBoxID, f.lastPort, f.lastDesc = boxID, port, description
	if f.err != nil {
		return cluster.BoxPortInfo{}, f.err
	}
	return f.info, nil
}

// CloseBoxPort is a test helper.
func (f *fakeService) CloseBoxPort(_ context.Context, boxID string, port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastBoxID, f.lastPort = boxID, port
	return f.err
}

// ListBoxPorts is a test helper.
func (f *fakeService) ListBoxPorts(_ context.Context, boxID string) ([]cluster.BoxPortInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastBoxID = boxID
	if f.err != nil {
		return nil, f.err
	}
	return f.ports, nil
}

// unixClient returns an http.Client that dials the unix socket at path, the
// way curl --unix-socket does inside a box.
func unixClient(path string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
		Timeout: 5 * time.Second,
	}
}

// serveTestAPI starts a ServeUnix server for boxID over svc in a temp dir and
// returns a client for it.
func serveTestAPI(t *testing.T, boxID string, svc PortService) *http.Client {
	t.Helper()
	path := filepath.Join(t.TempDir(), SocketName)
	srv, err := ServeUnix(path, boxID, svc, nil)
	if err != nil {
		t.Fatalf("ServeUnix: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return unixClient(path)
}

// post sends body to route and returns the status and response bytes.
func post(t *testing.T, c *http.Client, route, body string) (int, []byte) {
	t.Helper()
	resp, err := c.Post("http://box"+route, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", route, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, b
}

// TestBoxAPIOpenPort checks open_port forwards the bound box ID plus the
// request's port/description and returns the service's view.
func TestBoxAPIOpenPort(t *testing.T) {
	svc := &fakeService{info: cluster.BoxPortInfo{Port: 3000, URL: "https://ab12.proxy.example.com/", Description: "vite"}}
	c := serveTestAPI(t, "web-box", svc)

	status, body := post(t, c, "/v1/open_port", `{"port":3000,"description":"vite"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body %s", status, body)
	}
	var out openPortResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Port != svc.info {
		t.Errorf("resp = %+v, want %+v", out.Port, svc.info)
	}
	svc.mu.Lock()
	if svc.lastBoxID != "web-box" || svc.lastPort != 3000 || svc.lastDesc != "vite" {
		t.Errorf("service saw box=%q port=%d desc=%q", svc.lastBoxID, svc.lastPort, svc.lastDesc)
	}
	svc.mu.Unlock()
}

// TestBoxAPIClosePort checks close_port forwards the bound box ID and port.
func TestBoxAPIClosePort(t *testing.T) {
	svc := &fakeService{}
	c := serveTestAPI(t, "web-box", svc)

	status, body := post(t, c, "/v1/close_port", `{"port":3000}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body %s", status, body)
	}
	svc.mu.Lock()
	if svc.lastBoxID != "web-box" || svc.lastPort != 3000 {
		t.Errorf("service saw box=%q port=%d", svc.lastBoxID, svc.lastPort)
	}
	svc.mu.Unlock()
}

// TestBoxAPIListPorts checks list_ports works with an empty body and returns
// the service's ports.
func TestBoxAPIListPorts(t *testing.T) {
	svc := &fakeService{ports: []cluster.BoxPortInfo{{Port: 3000, URL: "https://x/"}, {Port: 8080, URL: "https://y/"}}}
	c := serveTestAPI(t, "web-box", svc)

	status, body := post(t, c, "/v1/list_ports", `{}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body %s", status, body)
	}
	var out listPortsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Ports) != 2 || out.Ports[0] != svc.ports[0] || out.Ports[1] != svc.ports[1] {
		t.Errorf("ports = %+v, want %+v", out.Ports, svc.ports)
	}

	// An entirely empty body is also fine.
	if status, _ := post(t, c, "/v1/list_ports", ""); status != http.StatusOK {
		t.Errorf("empty-body status = %d, want 200", status)
	}
}

// TestBoxAPIRejectsBadRequest checks malformed JSON and out-of-range ports get
// a 400 with an error body, without reaching the service.
func TestBoxAPIRejectsBadRequest(t *testing.T) {
	svc := &fakeService{}
	c := serveTestAPI(t, "web-box", svc)

	for _, tc := range []struct{ route, body string }{
		{"/v1/open_port", `not json`},
		{"/v1/open_port", `{"port":0}`},
		{"/v1/open_port", `{"port":70000}`},
		{"/v1/close_port", `{"port":0}`},
	} {
		status, body := post(t, c, tc.route, tc.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s %s: status = %d, want 400", tc.route, tc.body, status)
		}
		var e errorResponse
		if err := json.Unmarshal(body, &e); err != nil || e.Error == "" {
			t.Errorf("%s %s: error body = %s", tc.route, tc.body, body)
		}
	}
	svc.mu.Lock()
	if svc.lastBoxID != "" {
		t.Error("a bad request reached the service")
	}
	svc.mu.Unlock()
}

// TestBoxAPIServiceError checks a hub-side failure surfaces as a 502 carrying
// the hub's message.
func TestBoxAPIServiceError(t *testing.T) {
	svc := &fakeService{err: errors.New("port publishing is disabled on this hub")}
	c := serveTestAPI(t, "web-box", svc)

	status, body := post(t, c, "/v1/open_port", `{"port":3000}`)
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	var e errorResponse
	if err := json.Unmarshal(body, &e); err != nil || !strings.Contains(e.Error, "disabled") {
		t.Errorf("error body = %s", body)
	}
}

// TestBoxAPINoBoxID checks a handler bound to an empty box ID explains itself
// on every route.
func TestBoxAPINoBoxID(t *testing.T) {
	svc := &fakeService{}
	c := serveTestAPI(t, "", svc)

	for _, tc := range []struct{ route, body string }{
		{"/v1/open_port", `{"port":3000}`},
		{"/v1/close_port", `{"port":3000}`},
		{"/v1/list_ports", `{}`},
	} {
		status, body := post(t, c, tc.route, tc.body)
		if status != http.StatusBadRequest || !bytes.Contains(body, []byte("no box ID")) {
			t.Errorf("%s: status = %d body = %s", tc.route, status, body)
		}
	}
}

// TestServeUnixReplacesStaleSocket checks a leftover socket file at the same
// path is replaced on start.
func TestServeUnixReplacesStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), SocketName)
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("stale listen: %v", err)
	}
	_ = stale.Close() // leaves the socket file behind on some platforms; recreate to be sure
	if _, err := net.Listen("unix", path); err == nil {
		// The file was cleaned by Close on this platform; recreate a dead one.
	}
	srv, err := ServeUnix(path, "web-box", &fakeService{}, nil)
	if err != nil {
		t.Fatalf("ServeUnix over stale socket: %v", err)
	}
	defer srv.Close()
	if status, _ := post(t, unixClient(path), "/v1/list_ports", `{}`); status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
}

// TestServeUnixCloseStopsServing checks Close refuses subsequent connections.
func TestServeUnixCloseStopsServing(t *testing.T) {
	path := filepath.Join(t.TempDir(), SocketName)
	srv, err := ServeUnix(path, "web-box", &fakeService{}, nil)
	if err != nil {
		t.Fatalf("ServeUnix: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := unixClient(path).Post("http://box/v1/list_ports", "application/json", strings.NewReader(`{}`)); err == nil {
		t.Error("expected connections to fail after Close")
	}
}
