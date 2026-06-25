//go:build e2e

package e2e

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tebeka/selenium"

	"github.com/clems4ever/llmbox/internal/docker"
	"github.com/clems4ever/llmbox/internal/server"
)

// TestAdminRemoveBoxInBrowser drives the cluster-admin "Remove" button through a
// real headless Chrome, reproducing the exact path a user takes: admin.js
// intercepts the form and submits it over fetch() as urlencoded, and the server
// answers with JSON. This guards the AJAX-only admin flow end-to-end through the
// page's JavaScript (the regression where the click was rejected with "Invalid
// or missing form token"); the unit test TestAdminDeleteBox pins the same path
// at the HTTP layer.
//
// It is opt-in via `-tags e2e` (it needs Chrome + ChromeDriver), like the rest
// of the e2e suite, so a missing browser is a fatal failure rather than a skip.
//
// @arg t The test, failed on any setup, navigation, or assertion error.
func TestAdminRemoveBoxInBrowser(t *testing.T) {
	s, f, st := newAdminServer(t)
	// Seed one box so the dashboard renders a Remove button; the fake manager's
	// Destroy just records the id, so a successful click flashes "removed box foo".
	f.ListResult = []docker.Box{{
		BoxID: "foo", Spoke: "local", Image: "img", State: "running", Phase: "ready",
	}}
	cookie := signIn(t, st, true, false) // admin session "SID" with CSRF "CSRF"

	httpSrv := httptest.NewServer(s.Handler(s.MCPServer("t", "v")))
	t.Cleanup(httpSrv.Close)

	b := newBrowser(t)
	t.Cleanup(b.close)

	// Load the origin first to establish the document, then inject the admin login
	// cookie via document.cookie (skipping the real OIDC redirect dance). Setting
	// it through the page is reliable on an httptest IP host, where WebDriver's
	// AddCookie is not; the server only reads the cookie value, so HttpOnly does
	// not matter here.
	if err := b.wd.Get(httpSrv.URL + "/admin"); err != nil {
		t.Fatalf("loading origin: %v", err)
	}
	setCookie := fmt.Sprintf("document.cookie = %q;", server.LoginCookie+"="+cookie.Value+"; path=/")
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
