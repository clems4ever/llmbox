//go:build e2e

package e2e

// These end-to-end tests drive the hub-and-spoke box lifecycle through the admin
// page in a REAL headless Chrome, the way a human operates it: they load /admin,
// type into the create form, pick the spoke, and click the Create / Remove
// buttons, letting the page's admin.js intercept each submit and POST it over
// fetch(). They are the slow, full-stack counterpart to the fast endpoint-level
// tests in package clustere2e (e2e/cluster/spoke_lifecycle_e2e_test.go): same
// scenarios, but exercised through the rendered DOM and JavaScript.
//
// Like the rest of the e2e suite they are opt-in via `-tags e2e` and need Chrome
// + ChromeDriver; a missing browser is a fatal failure, not a skip.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tebeka/selenium"

	"github.com/clems4ever/llmbox/internal/auth"
	"github.com/clems4ever/llmbox/internal/cluster"
	"github.com/clems4ever/llmbox/internal/sandbox"
	"github.com/clems4ever/llmbox/internal/server"
)

// browserSpokeMgr is an in-memory cluster.BoxManager used as a spoke's simulated
// Docker layer in the browser tests. It keeps boxes so the admin dashboard
// reflects creates and removals, and mirrors the real manager by failing a
// Destroy of an absent box with sandbox.ErrBoxNotFound.
type browserSpokeMgr struct {
	name string

	mu    sync.Mutex
	boxes map[string]string // containerID -> boxID
}

// newBrowserSpokeMgr builds an empty simulated spoke box manager.
//
// @arg name The spoke name (for diagnostics).
// @return *browserSpokeMgr An empty manager.
func newBrowserSpokeMgr(name string) *browserSpokeMgr {
	return &browserSpokeMgr{name: name, boxes: map[string]string{}}
}

// Create records a box and returns a fake container ID and authorize URL.
//
// @arg ctx Context (unused by the fake).
// @arg opts The create options; only the box ID is recorded.
// @return string The fake container ID.
// @return string A canned authorize URL.
// @error error Always nil.
func (m *browserSpokeMgr) Create(ctx context.Context, opts sandbox.CreateOptions) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := randContainerID()
	m.boxes[id] = opts.BoxID
	return id, "https://auth.example/", nil
}

// SubmitCode returns a canned session URL.
//
// @arg ctx Context (unused by the fake).
// @arg idOrName The box identifier (ignored).
// @arg code The OAuth code (ignored).
// @return string A canned session URL.
// @error error Always nil.
func (m *browserSpokeMgr) SubmitCode(ctx context.Context, idOrName, code string) (string, error) {
	return "https://claude.ai/code/session", nil
}

// List returns the spoke's in-memory boxes as ready boxes.
//
// @arg ctx Context (unused by the fake).
// @return []sandbox.Box One entry per in-memory box.
// @error error Always nil.
func (m *browserSpokeMgr) List(ctx context.Context) ([]sandbox.Box, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []sandbox.Box
	for id, boxID := range m.boxes {
		out = append(out, sandbox.Box{ContainerID: id, BoxID: boxID, State: "running", Phase: "ready"})
	}
	return out, nil
}

// Destroy removes a matching in-memory box, by box ID or container-ID prefix, and
// fails with sandbox.ErrBoxNotFound when none matches (the human-removed case).
//
// @arg ctx Context (unused by the fake).
// @arg idOrName The box ID or container ID to destroy.
// @error error sandbox.ErrBoxNotFound when no box matches.
func (m *browserSpokeMgr) Destroy(ctx context.Context, idOrName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, boxID := range m.boxes {
		if boxID == idOrName || id == idOrName || strings.HasPrefix(id, idOrName) || strings.HasPrefix(idOrName, id) {
			delete(m.boxes, id)
			return nil
		}
	}
	return fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, idOrName)
}

// Logs returns canned output.
//
// @arg ctx Context (unused by the fake).
// @arg idOrName The box identifier (ignored).
// @arg tail The tail count (ignored).
// @return string Canned log output.
// @error error Always nil.
func (m *browserSpokeMgr) Logs(ctx context.Context, idOrName string, tail int) (string, error) {
	return "log from " + m.name, nil
}

// Exec returns canned output.
//
// @arg ctx Context (unused by the fake).
// @arg idOrName The box identifier (ignored).
// @arg cmd The command (ignored).
// @return sandbox.ExecResult Canned exec output.
// @error error Always nil.
func (m *browserSpokeMgr) Exec(ctx context.Context, idOrName string, cmd []string) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{Stdout: "hello-from-" + m.name + "\n", ExitCode: 0}, nil
}

// ReapOrphans reaps nothing in the simulation.
//
// @arg ctx Context (unused by the fake).
// @arg ttl The orphan TTL (ignored).
// @return []string Always nil.
// @error error Always nil.
func (m *browserSpokeMgr) ReapOrphans(ctx context.Context, ttl time.Duration) ([]string, error) {
	return nil, nil
}

// humanDestroy simulates an operator removing a box's container directly on the
// host, out of band: the box vanishes without going through the cluster Destroy
// path, so a later Destroy fails not-found.
//
// @arg boxID The box ID whose container was removed.
func (m *browserSpokeMgr) humanDestroy(boxID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, bid := range m.boxes {
		if bid == boxID {
			delete(m.boxes, id)
		}
	}
}

// hasBox reports whether the spoke currently holds a box with the given box ID.
//
// @arg boxID The box ID to look for.
// @return bool Whether the spoke holds that box.
func (m *browserSpokeMgr) hasBox(boxID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, bid := range m.boxes {
		if bid == boxID {
			return true
		}
	}
	return false
}

// clusterBrowserEnv is a running admin server with clustering enabled, fronted by
// a real httptest listener, for driving the admin page in a browser. It owns the
// store and the base context whose cancellation drops spoke connections.
type clusterBrowserEnv struct {
	t       *testing.T
	ctx     context.Context
	cancel  context.CancelFunc
	store   server.Store
	httpSrv *httptest.Server
	wsURL   string
	cookie  *http.Cookie
}

// newClusterBrowserEnv builds and starts a clusterBrowserEnv: an admin-enabled
// server with the hub attached, served on an httptest listener, plus a signed-in
// admin cookie. All resources are torn down via t.Cleanup.
//
// @arg t The test the env is scoped to.
// @return *clusterBrowserEnv A ready env with no spokes attached yet.
func newClusterBrowserEnv(t *testing.T) *clusterBrowserEnv {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	st, err := server.OpenStore(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	a := auth.NewTestAuthenticator("admin@corp.com")
	hub := cluster.NewHub(ctx, st, nil, nil)
	srv := server.New(newBrowserSpokeMgr("local"), nil, "https://boxes.example.com", time.Minute, st, a)
	srv.SetHub(hub)
	srv.SetBoxImage("box:e2e")

	httpSrv := httptest.NewServer(srv.APIHandler())
	t.Cleanup(httpSrv.Close)

	return &clusterBrowserEnv{
		t:       t,
		ctx:     ctx,
		cancel:  cancel,
		store:   st,
		httpSrv: httpSrv,
		wsURL:   "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/spoke/connect",
		cookie:  signIn(t, st, true, false),
	}
}

// browserRemote is a handle to a spoke attached to a clusterBrowserEnv: its box
// manager plus the controls to drop and restore its live connection.
type browserRemote struct {
	t   *testing.T
	env *clusterBrowserEnv
	mgr *browserSpokeMgr

	mu     sync.Mutex
	creds  *cluster.Credentials
	cancel context.CancelFunc
}

// connectSpoke mints a join token for name, starts a real spoke dialing the hub,
// and returns a handle to it. The caller waits for the dashboard to show it
// connected (the spoke option only appears once connected).
//
// @arg name The spoke name to enroll.
// @return *browserRemote A handle to the spoke.
func (e *clusterBrowserEnv) connectSpoke(name string) *browserRemote {
	e.t.Helper()
	joinToken, err := cluster.CreateJoinToken(e.store, name, time.Hour, time.Now())
	if err != nil {
		e.t.Fatalf("create join token: %v", err)
	}
	r := &browserRemote{t: e.t, env: e, mgr: newBrowserSpokeMgr(name)}
	r.start(joinToken)
	return r
}

// start launches a cluster.Run for the spoke in its own cancellable context,
// saving the minted credentials on first enrollment and presenting the saved
// ones on reconnect.
//
// @arg joinToken The one-time join token for first enrollment; "" on reconnect.
func (r *browserRemote) start(joinToken string) {
	ctx, cancel := context.WithCancel(r.env.ctx)
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
		_ = cluster.Run(ctx, cluster.WebSocketDialer(r.env.wsURL), r.mgr, joinToken, creds, save, cluster.ValidationPolicy{})
	}()
}

// disconnect drops the spoke's live connection (as if the spoke process went
// away). The hub observes the drop and unregisters it.
func (r *browserRemote) disconnect() {
	r.mu.Lock()
	cancel := r.cancel
	r.cancel = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// reconnect re-dials the hub using the credentials saved at first enrollment.
func (r *browserRemote) reconnect() {
	r.start("") // saved credentials are used; the join token is ignored
}

// openAdmin loads /admin in the browser, injects the admin login cookie via
// document.cookie (skipping the OIDC dance, like TestAdminRemoveBoxInBrowser),
// reloads, and stubs window.confirm so the Remove confirmation never blocks.
//
// @arg b The browser session to drive.
func (e *clusterBrowserEnv) openAdmin(b *browser) {
	e.t.Helper()
	if err := b.wd.Get(e.httpSrv.URL + "/admin"); err != nil {
		e.t.Fatalf("loading origin: %v", err)
	}
	setCookie := fmt.Sprintf("document.cookie = %q;", auth.LoginCookie+"="+e.cookie.Value+"; path=/")
	if _, err := b.wd.ExecuteScript(setCookie, nil); err != nil {
		e.t.Fatalf("setting login cookie: %v", err)
	}
	e.reload(b)
}

// reload navigates to /admin afresh and re-stubs window.confirm (a navigation
// resets the page's JS), so the dashboard reflects current server state.
//
// @arg b The browser session to drive.
func (e *clusterBrowserEnv) reload(b *browser) {
	e.t.Helper()
	if err := b.wd.Get(e.httpSrv.URL + "/admin"); err != nil {
		e.t.Fatalf("loading /admin: %v", err)
	}
	if _, err := b.wd.ExecuteScript("window.confirm = function () { return true; };", nil); err != nil {
		e.t.Fatalf("stubbing confirm(): %v", err)
	}
}

// waitSpokeStatus reloads the dashboard until the named spoke's status pill shows
// the wanted state (connected or offline), failing the test if it never does.
//
// @arg b The browser session to drive.
// @arg name The spoke name to watch.
// @arg connected The status to wait for; true for connected, false for offline.
func (e *clusterBrowserEnv) waitSpokeStatus(b *browser, name string, connected bool) {
	e.t.Helper()
	want := "offline"
	if connected {
		want = "connected"
	}
	pill := fmt.Sprintf(
		`//div[@id='spokes-card']//tr[td[contains(@class,'mono') and normalize-space(.)=%q]]//span[contains(@class,'pill')]`,
		name)
	deadline := time.Now().Add(20 * time.Second)
	for {
		e.reload(b)
		if el, err := b.wd.FindElement(selenium.ByXPATH, pill); err == nil {
			if txt, _ := el.Text(); strings.TrimSpace(txt) == want {
				return
			}
		}
		if time.Now().After(deadline) {
			src, _ := b.wd.PageSource()
			e.t.Fatalf("spoke %q never showed %q in the dashboard; page was:\n%s", name, want, src)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// createBox fills the admin create-box form for the given spoke and clicks
// Create, then waits for the box's row to appear in the dashboard.
//
// @arg b The browser session to drive.
// @arg boxID The box ID to create.
// @arg spoke The spoke to create the box on (must be a connected option).
func (e *clusterBrowserEnv) createBox(b *browser, boxID, spoke string) {
	e.t.Helper()
	idInput := b.waitFor(e.t, selenium.ByXPATH, `//form[@action='/admin/boxes']//input[@name='box_id']`)
	if err := idInput.Clear(); err != nil {
		e.t.Fatalf("clearing box id field: %v", err)
	}
	if err := idInput.SendKeys(boxID); err != nil {
		e.t.Fatalf("typing box id: %v", err)
	}
	opt := b.waitFor(e.t, selenium.ByXPATH,
		fmt.Sprintf(`//form[@action='/admin/boxes']//select[@name='spoke']/option[@value=%q]`, spoke))
	if err := opt.Click(); err != nil {
		e.t.Fatalf("selecting spoke %q: %v", spoke, err)
	}
	btn := b.waitFor(e.t, selenium.ByXPATH, `//form[@action='/admin/boxes']//button[@type='submit']`)
	if err := btn.Click(); err != nil {
		e.t.Fatalf("clicking Create: %v", err)
	}
	// admin.js submits over fetch() and refreshes the cards; the new row appears.
	b.waitFor(e.t, selenium.ByXPATH, boxCellXPath(boxID))
}

// removeBox clicks the Remove button on the given box's row and waits for the row
// to disappear from the dashboard.
//
// @arg b The browser session to drive.
// @arg boxID The box ID to remove.
func (e *clusterBrowserEnv) removeBox(b *browser, boxID string) {
	e.t.Helper()
	btn := b.waitFor(e.t, selenium.ByXPATH, fmt.Sprintf(
		`//div[@id='boxes-card']//form[@action='/admin/boxes/delete'][.//input[@name='box_id' and @value=%q]]//button`,
		boxID))
	if err := btn.Click(); err != nil {
		e.t.Fatalf("clicking Remove for %q: %v", boxID, err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := b.wd.FindElement(selenium.ByXPATH, boxCellXPath(boxID)); err != nil {
			return // row is gone
		}
		if time.Now().After(deadline) {
			src, _ := b.wd.PageSource()
			e.t.Fatalf("box %q row never disappeared after Remove; page was:\n%s", boxID, src)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// boxSpoke returns the spoke a box is listed under in the dashboard, or "" when
// the box has no row.
//
// @arg b The browser session to read from.
// @arg boxID The box ID to look up.
// @return string The spoke cell's text, or "" when the box is absent.
func (e *clusterBrowserEnv) boxSpoke(b *browser, boxID string) string {
	cell, err := b.wd.FindElement(selenium.ByXPATH, fmt.Sprintf(
		`//div[@id='boxes-card']//tr[td[contains(@class,'mono') and normalize-space(.)=%q]]/td[contains(@class,'mono')][2]`,
		boxID))
	if err != nil {
		return ""
	}
	txt, _ := cell.Text()
	return strings.TrimSpace(txt)
}

// boxCellXPath builds the XPath of the box-ID cell for a box row in the dashboard.
//
// @arg boxID The box ID the cell holds.
// @return string The XPath selecting that cell.
func boxCellXPath(boxID string) string {
	return fmt.Sprintf(`//div[@id='boxes-card']//td[contains(@class,'mono') and normalize-space(.)=%q]`, boxID)
}

// randContainerID returns a random hex string for a fake container ID.
//
// @return string A 40-character hex string.
func randContainerID() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// TestAdminCreateAndRemoveBoxOnSpokeInBrowser drives the full create-then-remove
// flow on a remote spoke through a real headless Chrome: it fills the create form,
// picks the spoke and clicks Create, checks the box lands on the spoke and shows
// up under it in the dashboard, then clicks Remove and checks it is gone.
//
// @arg t The test, failed on any setup, navigation, or assertion error.
func TestAdminCreateAndRemoveBoxOnSpokeInBrowser(t *testing.T) {
	env := newClusterBrowserEnv(t)
	edge := env.connectSpoke("edge")

	b := newBrowser(t)
	t.Cleanup(b.close)
	env.openAdmin(b)
	env.waitSpokeStatus(b, "edge", true) // also leaves the page with the edge option rendered

	env.createBox(b, "b1", "edge")
	if !edge.mgr.hasBox("b1") {
		t.Fatal("box b1 not present on the edge spoke after create in the browser")
	}
	if got := env.boxSpoke(b, "b1"); got != "edge" {
		t.Fatalf("dashboard shows box b1 on spoke %q, want edge", got)
	}

	env.removeBox(b, "b1")
	if edge.mgr.hasBox("b1") {
		t.Error("box b1 still on the edge spoke after Remove in the browser")
	}
}

// TestAdminSpokeDisconnectReconnectInBrowser checks the dashboard reflects a
// spoke dropping and re-establishing its connection: it creates a box on the
// spoke, drops the connection and checks the status pill reads offline, reconnects
// and checks it reads connected again, then removes the box via the Remove button.
//
// @arg t The test, failed on any setup, navigation, or assertion error.
func TestAdminSpokeDisconnectReconnectInBrowser(t *testing.T) {
	env := newClusterBrowserEnv(t)
	edge := env.connectSpoke("edge")

	b := newBrowser(t)
	t.Cleanup(b.close)
	env.openAdmin(b)
	env.waitSpokeStatus(b, "edge", true)

	env.createBox(b, "b1", "edge")

	// Drop the spoke; the dashboard must show it offline.
	edge.disconnect()
	env.waitSpokeStatus(b, "edge", false)

	// Reconnect with saved credentials; the dashboard must show it connected.
	edge.reconnect()
	env.waitSpokeStatus(b, "edge", true)

	// The box survived the reconnect; remove it through the UI.
	env.removeBox(b, "b1")
	if edge.mgr.hasBox("b1") {
		t.Error("box b1 still on the edge spoke after Remove in the browser")
	}
}

// TestAdminRemoveHumanDestroyedBoxOnSpokeInBrowser creates a box on a remote
// spoke, simulates a human destroying its container out of band, then clicks
// Remove in the browser. The click must succeed (idempotent removal) and the box
// row must disappear even though the container no longer exists on the spoke.
//
// @arg t The test, failed on any setup, navigation, or assertion error.
func TestAdminRemoveHumanDestroyedBoxOnSpokeInBrowser(t *testing.T) {
	env := newClusterBrowserEnv(t)
	edge := env.connectSpoke("edge")

	b := newBrowser(t)
	t.Cleanup(b.close)
	env.openAdmin(b)
	env.waitSpokeStatus(b, "edge", true)

	env.createBox(b, "b1", "edge")

	// A human removes the container directly on the host, out of band. We do NOT
	// reload: the admin's page is now STALE — it still shows the box's row (the
	// dashboard only refreshes on an action), reproducing the real race where an
	// operator clicks Remove on a box whose container is already gone.
	edge.mgr.humanDestroy("b1")
	if edge.mgr.hasBox("b1") {
		t.Fatal("box b1 should be gone from the spoke after the human removed it")
	}

	// Clicking Remove on the stale row must still succeed (idempotent removal) and
	// the row must then disappear.
	env.removeBox(b, "b1")
}
