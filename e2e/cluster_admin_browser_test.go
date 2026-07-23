//go:build e2e

package e2e

// These end-to-end tests drive the hub-and-spoke box lifecycle through the admin
// single-page app in a REAL headless Chrome, the way a human operates it: they
// load /admin, let the SPA bootstrap its session from /api/v1/me, type into the
// create form, pick the spoke, and click the Create / Remove buttons — each
// action travelling over the authenticated box-control API with the CSRF
// header. They are the slow, full-stack counterpart to the fast API-level tests
// in package clustere2e (e2e/cluster/spoke_lifecycle_e2e_test.go): same
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

	"github.com/clems4ever/llmbox/internal/hub"
	"github.com/clems4ever/llmbox/internal/hub/auth"
	"github.com/clems4ever/llmbox/internal/shared/cluster"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
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

// Create records a box and returns a fake container ID. A box is ready as soon
// as it is created; there is no activation step.
//
// @arg ctx Context (unused by the fake).
// @arg opts The create options; only the box ID is recorded.
// @return sandbox.CreateResult The fake container ID.
// @error error Always nil.
func (m *browserSpokeMgr) Create(ctx context.Context, opts sandbox.CreateOptions) (sandbox.CreateResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := randGeneration()
	m.boxes[id] = opts.BoxID
	return sandbox.CreateResult{InstanceID: id}, nil
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
		out = append(out, sandbox.Box{InstanceID: id, BoxID: boxID, State: "running", Phase: "ready"})
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

// Pause is a no-op in the simulation and always succeeds.
//
// @arg ctx Context (unused by the fake).
// @arg idOrName The box identifier (ignored).
// @error error Always nil.
func (m *browserSpokeMgr) Pause(ctx context.Context, idOrName string) error {
	return nil
}

// Resume is a no-op in the simulation and always succeeds.
//
// @arg ctx Context (unused by the fake).
// @arg idOrName The box identifier (ignored).
// @error error Always nil.
func (m *browserSpokeMgr) Resume(ctx context.Context, idOrName string) error {
	return nil
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

func (m *browserSpokeMgr) SetNetworkPolicy(context.Context, string, sandbox.NetworkPolicy) error {
	return nil
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
	store   hub.Store
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

	st, err := hub.OpenStore(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	a := auth.NewTestAuthenticator("admin@corp.com")
	clusterHub := cluster.NewHub(ctx, st, nil, nil, nil)
	srv := hub.New(nil, "https://boxes.example.com", st, a)
	srv.SetHub(clusterHub)

	httpSrv := httptest.NewServer(srv.APIHandler())
	t.Cleanup(httpSrv.Close)

	return &clusterBrowserEnv{
		t:       t,
		ctx:     ctx,
		cancel:  cancel,
		store:   st,
		httpSrv: httpSrv,
		wsURL:   "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/spoke/connect",
		cookie:  signIn(t, st, true),
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
	joinToken, err := cluster.CreateJoinToken(e.store, name, "docker", time.Hour, time.Now())
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
		_ = cluster.Run(ctx, cluster.WebSocketDialer(r.env.wsURL), r.mgr, joinToken, creds, save)
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
// and reloads so the SPA boots with the session.
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

// reload navigates to /admin afresh (a navigation resets the page's JS and
// refetches the dashboard), so the page reflects current server state. The SPA
// opens on the Workspaces view. Destructive actions are now guarded by an
// in-app confirmation modal rather than window.confirm, so nothing to stub.
//
// @arg b The browser session to drive.
func (e *clusterBrowserEnv) reload(b *browser) {
	e.t.Helper()
	if err := b.wd.Get(e.httpSrv.URL + "/admin"); err != nil {
		e.t.Fatalf("loading /admin: %v", err)
	}
}

// showView clicks the named section in the SPA navbar (Workspaces or
// Infrastructure). Unlike reload it switches views WITHOUT refetching, so a
// stale list stays stale — which TestAdminRemoveHumanDestroyedBoxOnSpokeInBrowser
// depends on. It waits for the nav item, which also waits out the SPA boot.
//
// @arg b The browser session to drive.
// @arg label The nav item label to click ("Workspaces" or "Infrastructure").
func (e *clusterBrowserEnv) showView(b *browser, label string) {
	e.t.Helper()
	nav := b.waitFor(e.t, selenium.ByXPATH,
		fmt.Sprintf(`//*[contains(@class,'mantine-NavLink-label') and normalize-space(.)=%q]`, label))
	if err := nav.Click(); err != nil {
		e.t.Fatalf("clicking %q nav: %v", label, err)
	}
}

// waitSpokeStatus polls until the named spoke's status badge shows the wanted
// state (connected or offline), failing the test if it never does. Each iteration
// reloads /admin afresh — which re-establishes the session from the login cookie
// and refetches the spoke list — then opens the Infrastructure view and waits a
// short beat for the async fetch to render before checking. Reloading per poll
// (rather than nursing one long-lived page) avoids a mid-poll session refresh
// redirecting to sign-in, and naturally re-observes a flapping connection.
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
	// Match on the row's data-spoke-status attribute, NOT on rendered badge text:
	// WebDriver's Text() returns text as rendered, and Mantine badges are
	// uppercased by CSS, so a text comparison against "connected" can never match.
	// The SPA stamps the raw status on the row exactly so tests stay independent
	// of styling.
	row := fmt.Sprintf(`//tr[@data-spoke-row=%q and @data-spoke-status=%q]`, name, want)
	deadline := time.Now().Add(40 * time.Second)
	for {
		e.reload(b)
		e.showView(b, "Infrastructure")
		// Let the just-loaded Infrastructure view finish its async fetch and
		// render the spokes table before judging (skip the skeleton frame).
		settle := time.Now().Add(3 * time.Second)
		for time.Now().Before(settle) {
			if _, err := b.wd.FindElement(selenium.ByXPATH, row); err == nil {
				return
			}
			time.Sleep(150 * time.Millisecond)
		}
		if time.Now().After(deadline) {
			src, _ := b.wd.PageSource()
			e.t.Fatalf("spoke %q never showed %q in the dashboard; page was:\n%s", name, want, src)
		}
	}
}

// createBox opens the "New workspace" modal, fills the create-box form for the
// given spoke and submits, then waits for the box's row to appear in the
// Workspaces list. It reloads first so the modal's spoke picker reflects the
// freshly-connected spoke.
//
// @arg b The browser session to drive.
// @arg boxID The box ID to create.
// @arg spoke The spoke to create the box on (must be a connected option).
func (e *clusterBrowserEnv) createBox(b *browser, boxID, spoke string) {
	e.t.Helper()
	e.reload(b) // fresh Workspaces view with up-to-date spoke options
	newBtn := b.waitFor(e.t, selenium.ByXPATH, `//button[normalize-space()='New workspace']`)
	if err := newBtn.Click(); err != nil {
		e.t.Fatalf("clicking New workspace: %v", err)
	}
	idInput := b.waitFor(e.t, selenium.ByXPATH, `//form[@id='create-box-form']//input[@name='box_id']`)
	if err := idInput.SendKeys(boxID); err != nil {
		e.t.Fatalf("typing box id: %v", err)
	}
	opt := b.waitFor(e.t, selenium.ByXPATH,
		fmt.Sprintf(`//form[@id='create-box-form']//select[@name='spoke']/option[@value=%q]`, spoke))
	if err := opt.Click(); err != nil {
		e.t.Fatalf("selecting spoke %q: %v", spoke, err)
	}
	btn := b.waitFor(e.t, selenium.ByXPATH, `//form[@id='create-box-form']//button[@type='submit']`)
	if err := btn.Click(); err != nil {
		e.t.Fatalf("clicking Create: %v", err)
	}
	// Creation closes the modal and refreshes the list, where the new row appears.
	b.waitFor(e.t, selenium.ByXPATH, boxCellXPath(boxID))
}

// removeBox clicks the Remove button on the given box's row, confirms in the
// modal, and waits for the row to disappear. It switches to the Workspaces view
// via the navbar (never a reload) so a deliberately-stale row is preserved.
//
// @arg b The browser session to drive.
// @arg boxID The box ID to remove.
func (e *clusterBrowserEnv) removeBox(b *browser, boxID string) {
	e.t.Helper()
	e.showView(b, "Workspaces")
	btn := b.waitFor(e.t, selenium.ByXPATH, fmt.Sprintf(`//button[@data-box=%q]`, boxID))
	if err := btn.Click(); err != nil {
		e.t.Fatalf("clicking Remove for %q: %v", boxID, err)
	}
	confirm := b.waitFor(e.t, selenium.ByXPATH, `//button[normalize-space()='Remove']`)
	if err := confirm.Click(); err != nil {
		e.t.Fatalf("confirming Remove for %q: %v", boxID, err)
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

// boxSpoke returns the spoke a box is listed under in the Workspaces list, or ""
// when the box has no row.
//
// @arg b The browser session to read from.
// @arg boxID The box ID to look up.
// @return string The spoke cell's text, or "" when the box is absent.
func (e *clusterBrowserEnv) boxSpoke(b *browser, boxID string) string {
	cell, err := b.wd.FindElement(selenium.ByXPATH,
		fmt.Sprintf(`//*[@data-box-spoke=%q]`, boxID))
	if err != nil {
		return ""
	}
	txt, _ := cell.Text()
	return strings.TrimSpace(txt)
}

// boxCellXPath builds the XPath of the box row for a box in the Workspaces list.
//
// @arg boxID The box ID the row holds.
// @return string The XPath selecting that row.
func boxCellXPath(boxID string) string {
	return fmt.Sprintf(`//*[@data-box-row=%q]`, boxID)
}

// randGeneration returns a random hex string for a fake container ID.
//
// @return string A 40-character hex string.
func randGeneration() string {
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
