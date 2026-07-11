//go:build e2e

package clustere2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/hub"
	"github.com/clems4ever/llmbox/internal/hub/auth"
	storepkg "github.com/clems4ever/llmbox/internal/hub/store"
	"github.com/clems4ever/llmbox/internal/shared/cluster"
)

// clusterFixture is a complete, in-process llmbox cluster wired for driving box
// operations against the admin HTTP endpoints DIRECTLY (the same routes the admin
// page's JavaScript POSTs to) — not the MCP API, and not through a browser. It is
// the fast test layer; the browser-level equivalents live in package e2e
// (cluster_admin_browser_test.go). It stands up a real hub with clustering
// enabled, a real HTTP server (the UI/API handler, which also carries
// /spoke/connect and /healthz) on a loopback listener, and a signed-in admin
// whose login cookie and CSRF token authorize the admin actions.
//
// Spokes are attached with connectSpoke: each is a real spoke process (a
// goroutine) that dials the hub over a real WebSocket and enrolls with a join
// token, backed by an in-memory fakeSpokeMgr. The transport, enrollment, routing
// and the admin UI are all exercised for real; only the per-spoke Docker layer is
// simulated. Reuse one fixture across many tests via newClusterFixture.
type clusterFixture struct {
	t      *testing.T
	ctx    context.Context
	cancel context.CancelFunc

	store hub.Store
	srv   *hub.Server

	baseURL string // http://host:port of the UI/API server
	wsURL   string // ws://host:port/spoke/connect

	client *http.Client
	cookie *http.Cookie
	csrf   string
}

// newClusterFixture builds and starts a clusterFixture: store, admin-enabled
// server with the hub attached, a loopback HTTP listener, and a signed-in admin.
// All resources are torn down via t.Cleanup.
//
// @arg t The test the fixture is scoped to.
// @return *clusterFixture A ready fixture with no spokes attached yet.
func newClusterFixture(t *testing.T) *clusterFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store, err := hub.OpenStore(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	a := auth.NewTestAuthenticator("admin@corp.com")
	srv := hub.New(nil, "http://placeholder", store, a)
	// srv implements cluster.BoxPortService, mirroring the production wiring in
	// internal/hub/serve.go, so spoke-originated box-port requests are served.
	srv.SetHub(cluster.NewHub(ctx, store, nil, nil, srv))
	// Enable HTTP proxying so the proxy-through-spoke path can be exercised. A
	// request whose Host is a proxy sub-domain is reverse-proxied; every other
	// request (the admin UI on the loopback host) falls through unchanged.
	srv.SetProxyBaseDomain("proxy.example.com")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	apiSrv := &http.Server{Handler: srv.APIHandler()}
	go func() { _ = apiSrv.Serve(ln) }()
	t.Cleanup(func() { _ = apiSrv.Close() })

	f := &clusterFixture{
		t:       t,
		ctx:     ctx,
		cancel:  cancel,
		store:   store,
		srv:     srv,
		baseURL: "http://" + addr,
		wsURL:   "ws://" + addr + "/spoke/connect",
		client:  &http.Client{},
	}
	waitHealthy(t, f.baseURL)
	f.signInAdmin()
	return f
}

// signInAdmin persists an admin login session and records its cookie and CSRF
// token, so the fixture's admin requests are authorized.
func (f *clusterFixture) signInAdmin() {
	f.t.Helper()
	if err := f.store.PutIdentitySession(storepkg.HashToken("SID"), storepkg.IdentitySession{
		Email:     "admin@corp.com",
		CSRFToken: "CSRF",
		ExpiresAt: time.Now().Add(time.Hour),
		CanAdmin:  true,
	}); err != nil {
		f.t.Fatalf("save login session: %v", err)
	}
	f.cookie = &http.Cookie{Name: auth.LoginCookie, Value: "SID"}
	f.csrf = "CSRF"
}

// fakeRemote is a handle to a spoke attached to the fixture: its in-memory box
// manager plus the controls to drop and restore its live connection to the hub,
// for exercising disconnect/reconnect.
type fakeRemote struct {
	t       *testing.T
	fixture *clusterFixture
	name    string
	mgr     *fakeSpokeMgr

	mu     sync.Mutex
	creds  *cluster.Credentials // saved after first enrollment, used to reconnect
	cancel context.CancelFunc   // cancels the current Run, dropping the connection
}

// connectSpoke mints a join token for name, starts a real spoke that dials the
// hub and enrolls with it, and waits until the admin UI shows the spoke
// connected.
//
// @arg name The spoke name to enroll.
// @return *fakeRemote A handle to the connected spoke.
func (f *clusterFixture) connectSpoke(name string) *fakeRemote {
	f.t.Helper()
	joinToken, err := cluster.CreateJoinToken(f.store, name, "docker", time.Hour, time.Now())
	if err != nil {
		f.t.Fatalf("create join token: %v", err)
	}
	r := &fakeRemote{t: f.t, fixture: f, name: name, mgr: newFakeSpokeMgr(name, "box:e2e")}
	r.start(joinToken)
	f.waitSpokeConnected(name, true)
	return r
}

// start launches a cluster.Run for the spoke in its own cancellable context. On
// first enrollment it saves the minted credentials; on reconnect it presents the
// saved credentials (and ignores the empty join token).
//
// @arg joinToken The one-time join token for first enrollment; "" on reconnect.
func (r *fakeRemote) start(joinToken string) {
	ctx, cancel := context.WithCancel(r.fixture.ctx)
	r.mu.Lock()
	r.cancel = cancel
	creds := r.creds
	r.mu.Unlock()
	save := func(c cluster.Credentials) error {
		r.mu.Lock()
		r.creds = &c
		r.mu.Unlock()
		return nil
	}
	go func() {
		_ = cluster.Run(ctx, cluster.WebSocketDialer(r.fixture.wsURL), r.mgr, joinToken, creds, save)
	}()
}

// disconnect drops the spoke's live connection (as if the spoke process went
// away) and waits until the admin UI reports it offline. The spoke's enrollment
// record is left intact so it can reconnect.
func (r *fakeRemote) disconnect() {
	r.t.Helper()
	r.mu.Lock()
	cancel := r.cancel
	r.cancel = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	r.fixture.waitSpokeConnected(r.name, false)
}

// reconnect re-dials the hub using the credentials saved at first enrollment and
// waits until the admin UI reports the spoke connected again.
func (r *fakeRemote) reconnect() {
	r.t.Helper()
	r.start("") // saved credentials are used; the join token is ignored
	r.fixture.waitSpokeConnected(r.name, true)
}

// --- Unified-API drivers ------------------------------------------------------

// apiCall posts a JSON body to a box-control API path as the signed-in admin
// (login cookie + X-CSRF-Token header — exactly what the web app sends) and
// decodes the JSON response into out (which may be nil). A non-200 response is
// returned as an error carrying the server's message.
//
// @arg path The API path (e.g. /api/v1/create-box).
// @arg body The request value encoded as the JSON body.
// @arg out The response target, or nil to discard the body.
// @error error if the request fails or the server answers non-200.
func (f *clusterFixture) apiCall(path string, body, out any) error {
	f.t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		f.t.Fatalf("encode %s request: %v", path, err)
	}
	req, err := http.NewRequest(http.MethodPost, f.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		f.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", f.csrf)
	req.AddCookie(f.cookie)
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			f.t.Fatalf("decode response for %s: %v", path, err)
		}
	}
	return nil
}

// createBoxViaAPI creates a box on the given spoke over the box-control API,
// asserting the action succeeds.
//
// @arg boxID The box ID to assign.
// @arg spoke The spoke to create the box on.
func (f *clusterFixture) createBoxViaAPI(boxID, spoke string) {
	f.t.Helper()
	body := map[string]any{"opts": map[string]any{"BoxID": boxID, "SpokeName": spoke}}
	if err := f.apiCall("/api/v1/create-box", body, nil); err != nil {
		f.t.Fatalf("create box %q on spoke %q failed: %v", boxID, spoke, err)
	}
}

// createProxyViaAPI enables an HTTP proxy for a box's port over the box-control
// API and returns the proxy URL it reports.
//
// @arg boxID The box to expose.
// @arg port The port inside the box.
// @return string The proxy URL (https://<slug>.proxy.example.com/).
func (f *clusterFixture) createProxyViaAPI(boxID string, port int) string {
	f.t.Helper()
	var out struct {
		Proxy struct {
			URL string `json:"url"`
		} `json:"proxy"`
	}
	if err := f.apiCall("/api/v1/create-proxy", map[string]any{"box_id": boxID, "port": port}, &out); err != nil {
		f.t.Fatalf("create proxy for %s:%d failed: %v", boxID, port, err)
	}
	if out.Proxy.URL == "" {
		f.t.Fatalf("create proxy for %s:%d returned no URL", boxID, port)
	}
	return out.Proxy.URL
}

// deleteBoxViaAPI removes a box over the box-control API, returning the server's
// error (nil on success) so a test can assert either outcome.
//
// @arg boxID The box ID to remove.
// @error error The server's error, or nil when the box was removed.
func (f *clusterFixture) deleteBoxViaAPI(boxID string) error {
	f.t.Helper()
	return f.apiCall("/api/v1/destroy-box", map[string]any{"box_id": boxID}, nil)
}

// spokeConnectedViaAPI reads the spoke's connection status from the box-control
// API — the same source the web app renders.
//
// @arg name The spoke name to look up.
// @return connected Whether the API reports the spoke connected.
// @return present Whether the spoke is enrolled at all.
func (f *clusterFixture) spokeConnectedViaAPI(name string) (connected, present bool) {
	f.t.Helper()
	var out struct {
		Spokes []struct {
			Name      string `json:"name"`
			Connected bool   `json:"connected"`
		} `json:"spokes"`
	}
	if err := f.apiCall("/api/v1/spoke-statuses", struct{}{}, &out); err != nil {
		f.t.Fatalf("spoke-statuses: %v", err)
	}
	for _, sp := range out.Spokes {
		if sp.Name == name {
			return sp.Connected, true
		}
	}
	return false, false
}

// boxOnSpokeViaAPI reads which spoke a box runs on from the box-control API.
//
// @arg boxID The box ID to look up.
// @return spoke The spoke the box is listed on.
// @return present Whether the box is listed at all.
func (f *clusterFixture) boxOnSpokeViaAPI(boxID string) (spoke string, present bool) {
	f.t.Helper()
	var out struct {
		Boxes []struct {
			BoxID string `json:"box_id"`
			Spoke string `json:"spoke"`
		} `json:"boxes"`
	}
	if err := f.apiCall("/api/v1/list-boxes", struct{}{}, &out); err != nil {
		f.t.Fatalf("list-boxes: %v", err)
	}
	for _, b := range out.Boxes {
		if b.BoxID == boxID {
			return b.Spoke, true
		}
	}
	return "", false
}

// waitSpokeConnected polls the box-control API until the spoke's connection
// status matches want, failing the test if it does not converge in time.
//
// @arg name The spoke name to watch.
// @arg want The connection status to wait for.
func (f *clusterFixture) waitSpokeConnected(name string, want bool) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got, present := f.spokeConnectedViaAPI(name); present && got == want {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("spoke %q never reached connected=%v over the API", name, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
