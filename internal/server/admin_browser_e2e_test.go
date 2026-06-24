//go:build e2e

package server

import (
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tebeka/selenium"
	"github.com/tebeka/selenium/chrome"

	"github.com/clems4ever/llmbox/internal/docker"
)

// TestAdminRemoveBoxInBrowser drives the cluster-admin "Remove" button through a
// real headless Chrome, reproducing the exact path a user takes: admin.js
// intercepts the form and submits it over fetch() as urlencoded, and the server
// answers with JSON. This guards the AJAX-only admin flow end-to-end through the
// page's JavaScript (the regression where the click was rejected with "Invalid
// or missing form token"); the unit test TestAdminDeleteBox pins the same path
// at the HTTP layer.
//
// It is opt-in via `-tags e2e` (it needs Chrome + ChromeDriver), like the
// workflow e2e, so a missing browser is a fatal failure rather than a skip.
//
// @arg t The test, failed on any setup, navigation, or assertion error.
func TestAdminRemoveBoxInBrowser(t *testing.T) {
	s, f, st := newAdminServer(t)
	// Seed one box so the dashboard renders a Remove button; the fake manager's
	// Destroy just records the id, so a successful click flashes "removed box foo".
	f.listResult = []docker.Box{{
		BoxID: "foo", Spoke: "local", Image: "img", State: "running", Phase: "ready",
	}}
	cookie := signIn(t, st, true, false) // admin session "SID" with CSRF "CSRF"

	httpSrv := httptest.NewServer(s.Handler(s.MCPServer("t", "v")))
	t.Cleanup(httpSrv.Close)

	b := newAdminBrowser(t)
	t.Cleanup(b.close)

	// Load the origin first to establish the document, then inject the admin login
	// cookie via document.cookie (skipping the real OIDC redirect dance). Setting
	// it through the page is reliable on an httptest IP host, where WebDriver's
	// AddCookie is not; the server only reads the cookie value, so HttpOnly does
	// not matter here.
	if err := b.wd.Get(httpSrv.URL + "/admin"); err != nil {
		t.Fatalf("loading origin: %v", err)
	}
	setCookie := fmt.Sprintf("document.cookie = %q;", loginCookie+"="+cookie.Value+"; path=/")
	if _, err := b.wd.ExecuteScript(setCookie, nil); err != nil {
		t.Fatalf("setting login cookie: %v", err)
	}

	// Reload the dashboard now that the cookie is set; it must render our box and
	// its Remove button (a sign-in page here means the cookie was not accepted).
	if err := b.wd.Get(httpSrv.URL + "/admin"); err != nil {
		t.Fatalf("loading /admin: %v", err)
	}
	removeBtn := b.waitFor(t, selenium.ByXPATH, `//form[@action='/admin/boxes/delete']//button`)

	// The Remove form guards on confirm(); accept it unconditionally so the
	// headless click proceeds to the real fetch() submit admin.js makes.
	if _, err := b.wd.ExecuteScript("window.confirm = function () { return true; };", nil); err != nil {
		t.Fatalf("stubbing confirm(): %v", err)
	}
	if err := removeBtn.Click(); err != nil {
		t.Fatalf("clicking Remove: %v", err)
	}

	// On success admin.js flashes "removed box foo" and refreshes the cards in
	// place. Under the bug the same banner would instead carry the CSRF error, so
	// waiting for the success banner is what distinguishes fixed from broken.
	b.waitFor(t, selenium.ByXPATH,
		`//div[contains(@class,'banner')][contains(normalize-space(.),'removed box foo')]`)

	src, _ := b.wd.PageSource()
	if strings.Contains(src, "Invalid or missing form token") {
		t.Fatalf("Remove hit the CSRF error path:\n%s", src)
	}
}

// browser bundles a running ChromeDriver service with a WebDriver session so the
// admin e2e can tear both down together. It mirrors the workflow suite's helper
// but lives in package server so the test can reach the unexported login/admin
// test seams (signIn, newAdminServer, loginCookie).
type browser struct {
	service *selenium.Service
	wd      selenium.WebDriver
}

// newAdminBrowser starts ChromeDriver and a headless Chrome session. A missing
// chromedriver is fatal, never skipped: the e2e suite is opt-in (it only builds
// under `-tags e2e`), so a missing browser is a failure to surface, not a green
// no-op.
//
// @arg t The test, used for fatal errors.
// @return *browser A ready browser whose session drives the admin UI.
func newAdminBrowser(t *testing.T) *browser {
	t.Helper()
	driver := chromeDriverPath()
	if driver == "" {
		t.Fatal("chromedriver not found: the e2e suite needs Chrome + ChromeDriver. " +
			"Set $CHROMEWEBDRIVER (or put chromedriver on $PATH) and ensure Chrome is " +
			"installed. This suite is opt-in via `-tags e2e`, so a missing browser is a " +
			"failure, not a skip.")
	}

	port, err := freeLocalPort()
	if err != nil {
		t.Fatalf("picking a free port for chromedriver: %v", err)
	}
	service, err := selenium.NewChromeDriverService(driver, port)
	if err != nil {
		t.Fatalf("starting chromedriver (%s): %v", driver, err)
	}

	caps := selenium.Capabilities{"browserName": "chrome"}
	caps.AddChrome(chrome.Capabilities{
		Path: chromeBinaryPath(),
		Args: []string{
			"--headless=new",
			"--no-sandbox",
			"--disable-dev-shm-usage",
			"--disable-gpu",
			"--window-size=1280,1024",
		},
	})
	wd, err := selenium.NewRemote(caps, fmt.Sprintf("http://127.0.0.1:%d/wd/hub", port))
	if err != nil {
		_ = service.Stop()
		t.Fatalf("starting headless chrome session: %v", err)
	}
	return &browser{service: service, wd: wd}
}

// close quits the WebDriver session and stops ChromeDriver.
func (b *browser) close() {
	if b.wd != nil {
		_ = b.wd.Quit()
	}
	if b.service != nil {
		_ = b.service.Stop()
	}
}

// waitFor polls for the element matched by (by, value) until it appears or the
// timeout elapses, returning it.
//
// @arg t The test, failed when the element never appears.
// @arg by The WebDriver selector strategy (e.g. selenium.ByXPATH).
// @arg value The selector value.
// @return selenium.WebElement The matched element.
func (b *browser) waitFor(t *testing.T, by, value string) selenium.WebElement {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if el, err := b.wd.FindElement(by, value); err == nil {
			return el
		}
		if time.Now().After(deadline) {
			src, _ := b.wd.PageSource()
			t.Fatalf("timed out waiting for element %s=%q; page was:\n%s", by, value, src)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// chromeDriverPath locates a chromedriver binary, honouring $CHROMEWEBDRIVER (the
// directory GitHub-hosted runners expose) and falling back to the PATH.
//
// @return string The chromedriver path, or "" if not found.
func chromeDriverPath() string {
	if dir := os.Getenv("CHROMEWEBDRIVER"); dir != "" {
		p := filepath.Join(dir, "chromedriver")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("chromedriver"); err == nil {
		return p
	}
	return ""
}

// chromeBinaryPath locates a Chrome/Chromium binary, honouring $CHROME_BIN and
// falling back to common names on the PATH.
//
// @return string The browser binary path, or "" to use ChromeDriver's default.
func chromeBinaryPath() string {
	if b := os.Getenv("CHROME_BIN"); b != "" {
		return b
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// freeLocalPort returns an unused localhost TCP port for the ChromeDriver service.
//
// @return int A currently-free localhost TCP port.
// @error error if a listener cannot be opened.
func freeLocalPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
