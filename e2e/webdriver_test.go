//go:build e2e

package e2e

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tebeka/selenium"
	"github.com/tebeka/selenium/chrome"
)

// browser bundles a running ChromeDriver service with a WebDriver session so the
// test can tear both down together.
type browser struct {
	service *selenium.Service
	wd      selenium.WebDriver
}

// newBrowser starts ChromeDriver and a headless Chrome session. A missing
// chromedriver is fatal, never skipped: the e2e suite is opt-in (it only builds
// under `-tags e2e`, i.e. `make test-e2e`), so if you asked to run it and the
// browser is not there, that is a failure to surface — not a green no-op that
// silently leaves the README screenshots stale.
//
// @arg t The test, used for fatal errors and cleanup.
// @return *browser A ready browser whose session drives the auth UI.
func newBrowser(t *testing.T) *browser {
	t.Helper()
	driver := findChromeDriver()
	if driver == "" {
		t.Fatal("chromedriver not found: the e2e suite needs Chrome + ChromeDriver. " +
			"Set $CHROMEWEBDRIVER (or put chromedriver on $PATH) and ensure Chrome is " +
			"installed. This suite is opt-in via `-tags e2e` / `make test-e2e`, so a " +
			"missing browser is a failure, not a skip.")
	}

	port, err := freePort()
	if err != nil {
		t.Fatalf("picking a free port for chromedriver: %v", err)
	}
	service, err := selenium.NewChromeDriverService(driver, port)
	if err != nil {
		t.Fatalf("starting chromedriver (%s): %v", driver, err)
	}

	caps := selenium.Capabilities{"browserName": "chrome"}
	caps.AddChrome(chrome.Capabilities{
		Path: findChromeBinary(),
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
// timeout elapses, returning it. It is the WebDriver counterpart of waiting for
// a navigation or a form-POST response to render.
//
// @arg t The test, failed when the element never appears.
// @arg by The WebDriver selector strategy (e.g. selenium.ByCSSSelector).
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

// shotWidth and shotHeight frame the auth card (max-width 30rem, centered) with
// a little margin when capturing a screenshot, so the saved image is a tidy
// portrait of the page rather than the card lost in a wide viewport.
//
// mobileShotWidth and mobileShotHeight frame the same page at a typical phone
// viewport (portrait, iPhone-class), so the README can show the responsive
// mobile layout alongside the desktop capture.
const (
	shotWidth  = 820
	shotHeight = 1000

	mobileShotWidth  = 390
	mobileShotHeight = 844
)

// resizeForScreenshot sizes the window to frame the auth card for a screenshot.
// A failure is logged rather than fatal — the screenshot is documentation, not
// an assertion.
//
// @arg t The test, used for logging.
func (b *browser) resizeForScreenshot(t *testing.T) {
	t.Helper()
	if err := b.wd.ResizeWindow("", shotWidth, shotHeight); err != nil {
		t.Logf("resizing window for screenshot (continuing): %v", err)
	}
}

// resizeForMobileScreenshot sizes the window to a phone-sized viewport for a
// mobile screenshot. Like resizeForScreenshot, a failure is logged rather than
// fatal — the screenshot is documentation, not an assertion.
//
// @arg t The test, used for logging.
func (b *browser) resizeForMobileScreenshot(t *testing.T) {
	t.Helper()
	if err := b.wd.ResizeWindow("", mobileShotWidth, mobileShotHeight); err != nil {
		t.Logf("resizing window for mobile screenshot (continuing): %v", err)
	}
}

// saveScreenshot captures the current page as a PNG and writes it to dir/name,
// creating dir if needed. It is a no-op-friendly helper gated by the caller on
// $LLMBOX_E2E_SCREENSHOT_DIR, so screenshots are only produced when asked for.
//
// @arg t The test, failed if the capture or write fails.
// @arg dir The directory to write the screenshot into.
// @arg name The screenshot file name.
func (b *browser) saveScreenshot(t *testing.T, dir, name string) {
	t.Helper()
	png, err := b.wd.Screenshot()
	if err != nil {
		t.Fatalf("capturing screenshot %s: %v", name, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating screenshot dir %s: %v", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, png, 0o644); err != nil {
		t.Fatalf("writing screenshot %s: %v", path, err)
	}
	t.Logf("wrote screenshot %s (%d bytes)", path, len(png))
}

// findChromeDriver locates a chromedriver binary, honouring $CHROMEWEBDRIVER (the
// directory GitHub-hosted runners expose) and falling back to the PATH. It
// returns "" when none is found.
//
// @return string The chromedriver path, or "" if not found.
func findChromeDriver() string {
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

// findChromeBinary locates a Chrome/Chromium binary, honouring $CHROME_BIN and
// falling back to common names on the PATH. "" lets ChromeDriver pick its own
// default.
//
// @return string The browser binary path, or "" to use ChromeDriver's default.
func findChromeBinary() string {
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

// freePort returns an unused TCP port for the ChromeDriver service.
//
// @return int A currently-free localhost TCP port.
// @error error if a listener cannot be opened.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
