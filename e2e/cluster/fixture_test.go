//go:build e2e

package clustere2e

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/hub"
	"github.com/clems4ever/llmbox/internal/shared/auth"
	"github.com/clems4ever/llmbox/internal/shared/cluster"
	storepkg "github.com/clems4ever/llmbox/internal/shared/store"
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
	clusterHub := cluster.NewHub(ctx, store, nil, nil)
	srv := hub.New(nil, "http://placeholder", 5*time.Minute, store, a)
	srv.SetHub(clusterHub)
	// The hub is the sole source of the box image: it stamps this onto every
	// create so config-free spokes launch exactly what they are sent.
	srv.SetBoxImage("box:e2e")
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
	if err := f.store.SaveLoginSession("SID", storepkg.LoginSession{
		Email:     "admin@corp.com",
		CSRF:      "CSRF",
		ExpiresAt: time.Now().Add(time.Hour),
		Admin:     true,
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
	joinToken, err := cluster.CreateJoinToken(f.store, name, time.Hour, time.Now())
	if err != nil {
		f.t.Fatalf("create join token: %v", err)
	}
	r := &fakeRemote{t: f.t, fixture: f, name: name, mgr: newFakeSpokeMgr(name)}
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
		_ = cluster.Run(ctx, cluster.WebSocketDialer(r.fixture.wsURL), r.mgr, joinToken, creds, save, cluster.ValidationPolicy{})
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

// --- Admin UI drivers -------------------------------------------------------

// adminResult is the {ok,msg,err} (plus optional one-time results) JSON every
// admin action returns.
type uiResult struct {
	OK  bool   `json:"ok"`
	Msg string `json:"msg"`
	Err string `json:"err"`
}

// post submits an authorized admin form action and decodes its JSON result,
// failing the test on a transport error or a non-200 status.
//
// @arg path The admin action path (e.g. /admin/boxes/delete).
// @arg form The form fields (the CSRF token is added automatically).
// @return uiResult The decoded {ok,msg,err} result.
func (f *clusterFixture) post(path string, form url.Values) uiResult {
	f.t.Helper()
	form.Set("csrf", f.csrf)
	req, err := http.NewRequest(http.MethodPost, f.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		f.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(f.cookie)
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("POST %s: status %d", path, resp.StatusCode)
	}
	var r uiResult
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		f.t.Fatalf("decode result for %s: %v", path, err)
	}
	return r
}

// createBoxViaAPI creates a box on the given spoke by POSTing to the admin
// /admin/boxes endpoint, asserting the action succeeds.
//
// @arg boxID The box ID to assign.
// @arg spoke The spoke to create the box on.
func (f *clusterFixture) createBoxViaAPI(boxID, spoke string) {
	f.t.Helper()
	// The create action embeds its one-time newBox payload alongside ok; only the
	// ok flag matters here.
	res := f.post("/admin/boxes", url.Values{"box_id": {boxID}, "spoke": {spoke}})
	if !res.OK {
		f.t.Fatalf("create box %q on spoke %q failed: %s", boxID, spoke, res.Err)
	}
}

// createProxyViaAPI enables an HTTP proxy for a box's port via the admin
// /admin/proxies endpoint and returns the proxy URL it reports.
//
// @arg boxID The box to expose.
// @arg port The port inside the box.
// @return string The proxy URL (https://<slug>.proxy.example.com/).
func (f *clusterFixture) createProxyViaAPI(boxID string, port int) string {
	f.t.Helper()
	form := url.Values{"box_id": {boxID}, "port": {strconv.Itoa(port)}, "csrf": {f.csrf}}
	req, err := http.NewRequest(http.MethodPost, f.baseURL+"/admin/proxies", strings.NewReader(form.Encode()))
	if err != nil {
		f.t.Fatalf("build proxy request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(f.cookie)
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatalf("POST /admin/proxies: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		OK       bool `json:"ok"`
		Err      string
		NewProxy struct {
			URL string `json:"url"`
		} `json:"newProxy"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		f.t.Fatalf("decode proxy result: %v", err)
	}
	if !out.OK || out.NewProxy.URL == "" {
		f.t.Fatalf("create proxy failed: ok=%v err=%q url=%q", out.OK, out.Err, out.NewProxy.URL)
	}
	return out.NewProxy.URL
}

// deleteBoxViaAPI removes a box by POSTing to the admin /admin/boxes/delete
// endpoint and returns the action result so a test can assert success (or inspect
// the error).
//
// @arg boxID The box ID to remove.
// @return uiResult The decoded {ok,msg,err} result.
func (f *clusterFixture) deleteBoxViaAPI(boxID string) uiResult {
	f.t.Helper()
	return f.post("/admin/boxes/delete", url.Values{"box_id": {boxID}})
}

// dashboard fetches the rendered admin dashboard HTML as the signed-in admin.
//
// @return string The dashboard page body.
func (f *clusterFixture) dashboard() string {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.baseURL+"/admin", nil)
	if err != nil {
		f.t.Fatalf("build dashboard request: %v", err)
	}
	req.AddCookie(f.cookie)
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatalf("GET /admin: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("GET /admin: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatalf("read dashboard: %v", err)
	}
	return string(body)
}

// spokeConnectedViaAPI reads the spoke's connection status from the admin
// dashboard HTML (fetched over HTTP).
//
// @arg name The spoke name to look up.
// @return connected Whether the dashboard marks the spoke connected.
// @return present Whether a row for the spoke is shown at all.
func (f *clusterFixture) spokeConnectedViaAPI(name string) (connected, present bool) {
	f.t.Helper()
	row, ok := rowInSection(f.dashboard(), "spokes-card", "boxes-card", name)
	if !ok {
		return false, false
	}
	return strings.Contains(row, `pill on">connected`), true
}

// boxOnSpokeViaAPI reads which spoke a box is shown on in the admin dashboard
// HTML (fetched over HTTP).
//
// @arg boxID The box ID to look up.
// @return spoke The spoke the box is listed under.
// @return present Whether a row for the box is shown at all.
func (f *clusterFixture) boxOnSpokeViaAPI(boxID string) (spoke string, present bool) {
	f.t.Helper()
	row, ok := rowInSection(f.dashboard(), "boxes-card", "", boxID)
	if !ok {
		return "", false
	}
	// The box row renders the box ID cell first, then the spoke cell; the spoke is
	// the value of the next mono cell after the one holding the box ID.
	return nextMonoCell(row), true
}

// waitSpokeConnected polls the admin dashboard until the spoke's connection
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
			f.t.Fatalf("spoke %q never reached connected=%v in the admin UI", name, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// --- HTML scraping helpers --------------------------------------------------

// rowInSection returns the substring of html that holds the table row for the
// mono-cell value name, scoped to the card between startMarker and endMarker
// (endMarker "" means to the end of the page). It returns the row text from the
// matching <td class="mono">name</td> to the next </tr>, and ok=false when no
// such row is present in the section.
//
// @arg html The full dashboard HTML.
// @arg startMarker The id= marker that begins the section to search.
// @arg endMarker The id= marker that ends the section, or "" for end-of-page.
// @arg name The mono-cell value identifying the row.
// @return string The row's HTML.
// @return bool Whether the row was found.
func rowInSection(html, startMarker, endMarker, name string) (string, bool) {
	section := html
	if i := strings.Index(section, `id="`+startMarker+`"`); i >= 0 {
		section = section[i:]
	}
	if endMarker != "" {
		if j := strings.Index(section, `id="`+endMarker+`"`); j >= 0 {
			section = section[:j]
		}
	}
	cell := `<td class="mono">` + name + `</td>`
	i := strings.Index(section, cell)
	if i < 0 {
		return "", false
	}
	row := section[i:]
	if end := strings.Index(row, "</tr>"); end >= 0 {
		row = row[:end]
	}
	return row, true
}

// nextMonoCell returns the text of the first <td class="mono">…</td> cell in row
// after the cell the row begins on, or "" when there is none. It is used to read
// the spoke cell that follows a box-ID cell.
//
// @arg row The row HTML, beginning at a mono cell.
// @return string The next mono cell's text content.
func nextMonoCell(row string) string {
	const open = `<td class="mono">`
	// Skip the first cell (the row begins on it), then find the next.
	rest := row
	if i := strings.Index(rest, open); i >= 0 {
		rest = rest[i+len(open):]
	}
	i := strings.Index(rest, open)
	if i < 0 {
		return ""
	}
	rest = rest[i+len(open):]
	end := strings.Index(rest, "</td>")
	if end < 0 {
		return ""
	}
	return rest[:end]
}
