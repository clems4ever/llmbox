//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tebeka/selenium"

	"github.com/clems4ever/llmbox/internal/hub/auth"
)

// TestAdminRemoveBoxInBrowser drives the workspace "Remove" button through a real
// headless Chrome, reproducing the exact path a user takes: the admin SPA
// bootstraps its session from /api/v1/me, and removing a workspace clicks the
// row's remove button, confirms in the Mantine confirmation modal, and submits
// the destroy over the authenticated box-control API with the CSRF header. This
// guards the whole browser flow (cookie → /me → confirm → CSRF header → API
// action) end-to-end through the page's JavaScript.
//
// It is opt-in via `-tags e2e` (it needs Chrome + ChromeDriver), like the rest
// of the e2e suite, so a missing browser is a fatal failure rather than a skip.
//
// @arg t The test, failed on any setup, navigation, or assertion error.
func TestAdminRemoveBoxInBrowser(t *testing.T) {
	s, _, st := newAdminServer(t)
	cookie := signIn(t, st, true, false) // admin session "SID" with CSRF "CSRF"

	httpSrv := httptest.NewServer(s.APIHandler())
	t.Cleanup(httpSrv.Close)

	// Seed one box through the box-control API so the hub holds a record for it
	// (the dashboard renders from the hub's records, not from a live spoke
	// listing); the fake manager's Destroy just records the id, so a confirmed
	// removal shows a "removed box foo" success notification.
	createReq, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/create-box",
		strings.NewReader(`{"opts":{"BoxID":"foo"}}`))
	if err != nil {
		t.Fatalf("build create-box request: %v", err)
	}
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("X-CSRF-Token", "CSRF")
	createReq.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(createReq)
	if err != nil {
		t.Fatalf("create-box: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create-box status = %s, want 200", resp.Status)
	}

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
	setCookie := fmt.Sprintf("document.cookie = %q;", auth.LoginCookie+"="+cookie.Value+"; path=/")
	if _, err := b.wd.ExecuteScript(setCookie, nil); err != nil {
		t.Fatalf("setting login cookie: %v", err)
	}

	// Reload the dashboard now that the cookie is set; it must render our box and
	// its Remove button (a sign-in page here means the cookie was not accepted).
	if err := b.wd.Get(httpSrv.URL + "/admin"); err != nil {
		t.Fatalf("loading /admin: %v", err)
	}
	removeBtn := b.waitFor(t, selenium.ByXPATH, `//button[@data-box='foo']`)
	if err := removeBtn.Click(); err != nil {
		t.Fatalf("clicking Remove: %v", err)
	}

	// Removal is guarded by a confirmation modal (not window.confirm); click its
	// red confirm button, whose only visible text is "Remove", to proceed to the
	// real fetch() submit the SPA makes.
	confirmBtn := b.waitFor(t, selenium.ByXPATH, `//button[normalize-space()='Remove']`)
	if err := confirmBtn.Click(); err != nil {
		t.Fatalf("confirming Remove: %v", err)
	}

	// On success the SPA shows a "removed box foo" notification and refreshes the
	// list in place; a broken CSRF path would show the server's error instead, so
	// waiting for the success notification is what distinguishes fixed from broken.
	b.waitFor(t, selenium.ByXPATH,
		`//*[contains(normalize-space(text()),'removed box foo')]`)

	src, _ := b.wd.PageSource()
	if strings.Contains(src, "invalid or missing X-CSRF-Token") {
		t.Fatalf("Remove hit the CSRF error path:\n%s", src)
	}
}
