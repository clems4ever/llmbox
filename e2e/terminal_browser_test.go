//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tebeka/selenium"

	"github.com/clems4ever/llmbox/internal/hub"
	"github.com/clems4ever/llmbox/internal/hub/auth"
	"github.com/clems4ever/llmbox/internal/spoke/box"
	"github.com/clems4ever/llmbox/internal/spoke/box/conformance"
)

// TestAdminTerminalInBrowser drives the in-browser terminal end to end through a
// real headless Chrome: it wires the hub to a real *box.Manager (the in-process
// Fake provisioner, whose guest spawns an actual PTY-backed shell), creates a box,
// opens its details drawer, clicks "Open terminal", and types a command whose
// output must appear in the xterm.js terminal. This exercises the whole new path —
// the admin SPA's WebSocket, the hub's terminal endpoint, the PTY tunnel, and the
// guest's shell — the way a user does. When $LLMBOX_E2E_SCREENSHOT_DIR is set it
// also captures the terminal for the docs/PR.
//
// It is opt-in via `-tags e2e` (it needs Chrome + ChromeDriver), like the rest of
// the e2e suite, so a missing browser is a fatal failure rather than a skip.
//
// @arg t The test, failed on any setup, navigation, or assertion error.
func TestAdminTerminalInBrowser(t *testing.T) {
	// A real box manager (not the fake): its guest opens an actual PTY shell, so
	// the terminal shows real output rather than a stub.
	boxMgr := box.NewManager(conformance.NewFake(t), box.Config{})
	srv, st := newTerminalServer(t, boxMgr)
	cookie := signIn(t, st, true) // admin session "SID" with CSRF "CSRF"

	httpSrv := httptest.NewServer(srv.APIHandler())
	t.Cleanup(httpSrv.Close)

	// Seed a box through the box-control API so the hub records it and the manager
	// actually creates it (the terminal attaches to this live box).
	createBox(t, httpSrv.URL, cookie, "demo")

	b := newBrowser(t)
	t.Cleanup(b.close)

	if err := b.wd.Get(httpSrv.URL + "/admin"); err != nil {
		t.Fatalf("loading origin: %v", err)
	}
	setCookie := fmt.Sprintf("document.cookie = %q;", auth.LoginCookie+"="+cookie.Value+"; path=/")
	if _, err := b.wd.ExecuteScript(setCookie, nil); err != nil {
		t.Fatalf("setting login cookie: %v", err)
	}
	if err := b.wd.Get(httpSrv.URL + "/admin"); err != nil {
		t.Fatalf("loading /admin: %v", err)
	}

	// Open the box's details drawer, then the terminal.
	row := b.waitFor(t, selenium.ByCSSSelector, `[data-box-row='demo']`)
	clickWhenReady(t, row, "box row")
	openBtn := b.waitFor(t, selenium.ByCSSSelector, `[data-testid='open-terminal']`)

	// Capture the details drawer with the new "Open terminal" action for the docs.
	drawerShotDir, err := resolveScreenshotDir(os.Getenv("LLMBOX_E2E_SCREENSHOT_DIR"))
	if err != nil {
		t.Fatalf("resolving screenshot dir: %v", err)
	}
	if drawerShotDir != "" {
		if err := b.wd.ResizeWindow("", 1440, 900); err != nil {
			t.Logf("resizing for screenshot (continuing): %v", err)
		}
		time.Sleep(600 * time.Millisecond) // let the drawer slide-in animation settle
		b.saveScreenshot(t, drawerShotDir, "workspace-drawer.png")
	}

	clickWhenReady(t, openBtn, "Open terminal")

	// Wait for the socket to connect, then type a command with a unique marker and
	// assert its output streams back into the terminal.
	b.waitFor(t, selenium.ByCSSSelector, `[data-terminal-state='connected']`)
	textarea := b.waitFor(t, selenium.ByCSSSelector, `.xterm-helper-textarea`)
	// Focus xterm's hidden input via JS rather than a click: the terminal canvas
	// overlays the helper textarea, so it is not pointer-interactable (a click
	// errors "element not interactable"), but it is keyboard-interactable, so
	// SendKeys delivers the keystrokes once it is focused.
	if _, err := b.wd.ExecuteScript(
		`document.querySelector('.xterm-helper-textarea').focus();`, nil); err != nil {
		t.Fatalf("focusing terminal: %v", err)
	}
	const marker = "llmbox-terminal-demo-42"
	if err := textarea.SendKeys("echo " + marker + "\n"); err != nil {
		t.Fatalf("typing into terminal: %v", err)
	}

	// The marker appears twice in the terminal (the echoed keystrokes and the
	// command's own output); wait until the shell's output line has rendered.
	waitForTerminalText(t, b, marker, 2)

	shotDir, err := resolveScreenshotDir(os.Getenv("LLMBOX_E2E_SCREENSHOT_DIR"))
	if err != nil {
		t.Fatalf("resolving screenshot dir: %v", err)
	}
	if shotDir != "" {
		// Desktop (centered dialog), then tablet and mobile (full-screen): resizing
		// the window flips the modal to full-screen and the terminal refits, so each
		// capture shows the terminal adapted to that screen size.
		for _, shot := range []struct {
			name          string
			width, height int
		}{
			{"terminal.png", 1440, 900},
			{"terminal-tablet.png", 834, 1112},
			{"terminal-mobile.png", 390, 844},
		} {
			if err := b.wd.ResizeWindow("", shot.width, shot.height); err != nil {
				t.Logf("resizing for screenshot (continuing): %v", err)
			}
			time.Sleep(600 * time.Millisecond) // let the terminal reflow before capture
			b.saveScreenshot(t, shotDir, shot.name)
		}
	}
}

// newTerminalServer builds an admin-enabled hub wired to mgr as its default spoke,
// backed by a real store. It mirrors newAdminServer but takes the box manager so a
// test can supply a real one whose guest opens a live PTY.
//
// @arg t The test, failed if the store cannot be opened.
// @arg mgr The box manager to serve as the default spoke.
// @return *hub.Server The admin-enabled server.
// @return hub.Store The backing store, for seeding login sessions.
func newTerminalServer(t *testing.T, mgr *box.Manager) (*hub.Server, hub.Store) {
	t.Helper()
	st, err := hub.OpenStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := auth.NewTestAuthenticator("admin@corp.com")
	srv := hub.New(nil, "https://boxes.example.com", st, a)
	wireDefaultSpoke(t, srv, st, mgr)
	return srv, st
}

// createBox creates a box through the box-control API with the admin cookie and
// CSRF header, failing the test on a non-200.
//
// @arg t The test, failed on any request or status error.
// @arg baseURL The server's base URL.
// @arg cookie The admin login cookie.
// @arg boxID The box ID to create.
func createBox(t *testing.T, baseURL string, cookie *http.Cookie, boxID string) {
	t.Helper()
	body := fmt.Sprintf(`{"opts":{"BoxID":%q}}`, boxID)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/create-box", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build create-box request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", "CSRF")
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create-box: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create-box status = %s, want 200", resp.Status)
	}
}

// clickWhenReady clicks el, retrying until it succeeds or a short deadline
// passes. A drawer or modal that is still animating in reports its controls as
// "not interactable"; retrying rides out the transition rather than racing it.
//
// @arg t The test, failed when the element never becomes clickable.
// @arg el The element to click.
// @arg desc A human label for the element, used in the failure message.
func clickWhenReady(t *testing.T, el selenium.WebElement, desc string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := el.Click(); err == nil {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("clicking %s: never became interactable: %v", desc, err)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// waitForTerminalText polls the rendered page until want appears at least min
// times in the terminal's text, so the spec waits for the shell's streamed output
// rather than a fixed sleep.
//
// @arg t The test, failed when the text never appears enough times.
// @arg b The browser session.
// @arg want The substring to wait for.
// @arg min The minimum number of occurrences required.
func waitForTerminalText(t *testing.T, b *browser, want string, min int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if src, err := b.wd.PageSource(); err == nil && strings.Count(src, want) >= min {
			return
		}
		if time.Now().After(deadline) {
			src, _ := b.wd.PageSource()
			t.Fatalf("terminal never showed %q %d× within timeout; page was:\n%s", want, min, src)
		}
		time.Sleep(150 * time.Millisecond)
	}
}
