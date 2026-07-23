//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tebeka/selenium"

	"github.com/clems4ever/llmbox/internal/hub/auth"
)

// TestNetworkAllowlistInBrowser drives the network-isolation UI (the Network view
// with its allowlist groups and assignments, plus the create-workspace group
// picker) through a real headless Chrome, and — when
// $LLMBOX_E2E_SCREENSHOT_DIR is set — captures the screens on desktop and mobile
// for the PR/README. It seeds groups, boxes, and one per-workspace assignment
// through the real box-control API so the page renders from live state.
//
// Opt-in via `-tags e2e` (needs Chrome + ChromeDriver), like the rest of the
// suite.
//
// @arg t The test, failed on any setup, navigation, or assertion error.
func TestNetworkAllowlistInBrowser(t *testing.T) {
	s, _, st := newAdminServer(t)
	cookie := signIn(t, st, true)

	httpSrv := httptest.NewServer(s.APIHandler())
	t.Cleanup(httpSrv.Close)

	post := func(path string, body any) {
		t.Helper()
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal %s body: %v", path, err)
		}
		req, err := http.NewRequest(http.MethodPost, httpSrv.URL+path, bytes.NewReader(buf))
		if err != nil {
			t.Fatalf("build %s request: %v", path, err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", "CSRF")
		req.AddCookie(cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %s, want 200", path, resp.Status)
		}
	}

	// Seed allowlist groups (two global, two optional).
	post("/api/v1/save-allowlist-group", map[string]any{
		"name": "core-ai", "description": "LLM provider APIs available to every workspace.",
		"is_global": true, "domains": []string{"api.anthropic.com", "api.openai.com", "api.mistral.ai"},
	})
	post("/api/v1/save-allowlist-group", map[string]any{
		"name": "github", "description": "Git operations and package registries.",
		"is_global": true, "domains": []string{"github.com", "api.github.com", "codeload.github.com", "ghcr.io"},
	})
	post("/api/v1/save-allowlist-group", map[string]any{
		"name": "python-pkgs", "description": "PyPI and conda mirrors for builds.",
		"domains": []string{"pypi.org", "files.pythonhosted.org"},
	})
	post("/api/v1/save-allowlist-group", map[string]any{
		"name": "observability", "description": "Metrics & error reporting endpoints.",
		"domains": []string{"sentry.io", "api.datadoghq.com"},
	})

	// Seed workspaces and one per-workspace assignment.
	for _, id := range []string{"web-scraper", "data-agent", "pr-reviewer"} {
		post("/api/v1/create-box", map[string]any{"opts": map[string]any{"BoxID": id}})
	}
	post("/api/v1/set-box-groups", map[string]any{"box_id": "web-scraper", "group_ids": []string{"python-pkgs"}})
	post("/api/v1/set-box-groups", map[string]any{"box_id": "data-agent", "group_ids": []string{"observability"}})

	// Seed DNS-audit rows directly in the store (enforcement is a runner feature;
	// here we only need rows for the audit view to render).
	now := time.Now()
	audit := []struct {
		box, domain, verdict string
		hits                 int
	}{
		{"web-scraper", "registry.npmjs.org", "blocked", 14},
		{"web-scraper", "github.com", "allowed", 6},
		{"data-agent", "telemetry.example.net", "blocked", 3},
		{"pr-reviewer", "api.anthropic.com", "allowed", 21},
		{"data-agent", "s3.amazonaws.com", "allowed", 9},
		{"web-scraper", "pypi.org", "allowed", 4},
		{"pr-reviewer", "unknown-cdn.ru", "blocked", 1},
	}
	for i, a := range audit {
		for h := 0; h < a.hits; h++ {
			if err := st.RecordDNSLookup(a.box, a.domain, a.verdict, now.Add(time.Duration(i)*time.Second)); err != nil {
				t.Fatalf("seed audit: %v", err)
			}
		}
	}

	b := newBrowser(t)
	t.Cleanup(b.close)

	// Establish the admin session by injecting the login cookie (skipping OIDC).
	if err := b.wd.Get(httpSrv.URL + "/admin"); err != nil {
		t.Fatalf("loading origin: %v", err)
	}
	if _, err := b.wd.ExecuteScript("document.cookie = \""+auth.LoginCookie+"="+cookie.Value+"; path=/\";", nil); err != nil {
		t.Fatalf("setting login cookie: %v", err)
	}
	if err := b.wd.Get(httpSrv.URL + "/admin"); err != nil {
		t.Fatalf("loading /admin: %v", err)
	}

	dir := screenshotDir(t)
	reload := func() {
		if err := b.wd.Get(httpSrv.URL + "/admin"); err != nil {
			t.Fatalf("reloading /admin: %v", err)
		}
	}
	openNetwork := func() {
		b.waitFor(t, selenium.ByXPATH, `//a[.//text()[contains(., 'Network')]]`).Click()
		b.waitFor(t, selenium.ByXPATH, `//*[text()='core-ai']`)
	}

	// --- Desktop ---
	b.resizeForScreenshot(t)
	openNetwork()
	settle()
	maybeShot(t, b, dir, "network-groups.png")

	b.waitFor(t, selenium.ByXPATH, `//button[.//text()[contains(., 'Assignments')]]`).Click()
	b.waitFor(t, selenium.ByXPATH, `//*[text()='Applied to all workspaces']`)
	settle()
	maybeShot(t, b, dir, "network-assignments.png")

	// DNS audit tab + the add-to-group modal on a blocked domain.
	b.waitFor(t, selenium.ByXPATH, `//button[.//text()[contains(., 'DNS audit')]]`).Click()
	b.waitFor(t, selenium.ByXPATH, `//*[text()='registry.npmjs.org']`)
	settle()
	maybeShot(t, b, dir, "network-audit.png")
	b.waitFor(t, selenium.ByXPATH, `(//button[.//text()[contains(., 'Add to group')]])[1]`).Click()
	b.waitFor(t, selenium.ByXPATH, `//*[text()='Add domain to group']`)
	settle()
	maybeShot(t, b, dir, "network-audit-add.png")
	reload()
	openNetwork() // back to the Network view after the modal reload

	// Group editor modal (settle so the open animation finishes and the overlay
	// is fully opaque before capture). A page reload afterwards clears the modal,
	// which is more robust than matching the close control.
	b.waitFor(t, selenium.ByXPATH, `//button[.//text()[contains(., 'New group')]]`).Click()
	b.waitFor(t, selenium.ByXPATH, `//*[text()='New allowlist group']`)
	settle()
	maybeShot(t, b, dir, "network-group-editor.png")
	reload()

	// Create-workspace modal with the allowlist group picker.
	b.waitFor(t, selenium.ByXPATH, `//button[.//text()[contains(., 'New workspace')]]`).Click()
	b.waitFor(t, selenium.ByXPATH, `//*[text()='Allowlist groups']`)
	settle()
	maybeShot(t, b, dir, "network-create-workspace.png")
	reload()

	// --- Mobile ---
	b.resizeForMobileScreenshot(t)
	// Open the burger menu, then the Network item.
	b.waitFor(t, selenium.ByXPATH, `//header//button[contains(@class,'Burger')]`).Click()
	settle()
	openNetwork()
	settle()
	maybeShot(t, b, dir, "network-groups-mobile.png")

	b.waitFor(t, selenium.ByXPATH, `//button[.//text()[contains(., 'Assignments')]]`).Click()
	b.waitFor(t, selenium.ByXPATH, `//*[text()='Applied to all workspaces']`)
	settle()
	maybeShot(t, b, dir, "network-assignments-mobile.png")

	b.waitFor(t, selenium.ByXPATH, `//button[.//text()[contains(., 'DNS audit')]]`).Click()
	b.waitFor(t, selenium.ByXPATH, `//*[text()='registry.npmjs.org']`)
	settle()
	maybeShot(t, b, dir, "network-audit-mobile.png")
}

// settle waits for a modal open/tab-switch animation to finish before a capture,
// so screenshots are fully rendered rather than caught mid-transition.
func settle() { time.Sleep(600 * time.Millisecond) }

// maybeShot captures a screenshot only when a directory is configured.
//
// @arg t The test.
// @arg b The browser to capture.
// @arg dir The output directory, or "" to skip.
// @arg name The screenshot file name.
func maybeShot(t *testing.T, b *browser, dir, name string) {
	t.Helper()
	if dir == "" {
		return
	}
	b.saveScreenshot(t, dir, name)
}
