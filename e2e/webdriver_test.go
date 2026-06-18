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

// newBrowser starts ChromeDriver and a headless Chrome session. When no
// chromedriver is available it skips the test rather than failing, so the suite
// is runnable locally without a browser while still exercising the UI in CI,
// where Chrome and ChromeDriver are installed.
//
// @arg t The test, used for fatal errors, skipping, and cleanup.
// @return *browser A ready browser whose session drives the auth UI.
func newBrowser(t *testing.T) *browser {
	t.Helper()
	driver := findChromeDriver()
	if driver == "" {
		t.Skip("chromedriver not found; set CHROMEWEBDRIVER or install chromedriver to run the e2e UI test")
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
