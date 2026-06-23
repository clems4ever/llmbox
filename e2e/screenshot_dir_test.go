//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveScreenshotDir guards the screenshot-path resolution that keeps CI's
// auto-refresh working. `go test` runs this package with e2e/ as the working
// directory, so a relative LLMBOX_E2E_SCREENSHOT_DIR=.github/screenshots used
// verbatim would write under e2e/.github/screenshots — which the workflow's
// commit/upload steps (rooted at the repo) never see, silently leaving the
// committed README screenshots stale. resolveScreenshotDir must anchor a relative
// dir at the module root instead. This test fails against the old behaviour of
// using the env var verbatim.
func TestResolveScreenshotDir(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Fatalf("moduleRoot: %v", err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if wd == root {
		t.Fatalf("test working directory %s is the module root; this test relies on "+
			"go test running it from the package dir to be meaningful", wd)
	}

	t.Run("relative resolves against the module root, not the working dir", func(t *testing.T) {
		got, err := resolveScreenshotDir(".github/screenshots")
		if err != nil {
			t.Fatalf("resolveScreenshotDir: %v", err)
		}
		want := filepath.Join(root, ".github/screenshots")
		if got != want {
			t.Fatalf("resolveScreenshotDir(%q) = %q, want %q", ".github/screenshots", got, want)
		}
		// The bug it guards against: resolving against the package working dir.
		if buggy := filepath.Join(wd, ".github/screenshots"); got == buggy {
			t.Fatalf("resolveScreenshotDir resolved under the package dir %q", buggy)
		}
	})

	t.Run("absolute is returned unchanged", func(t *testing.T) {
		abs := filepath.Join(root, "elsewhere")
		got, err := resolveScreenshotDir(abs)
		if err != nil {
			t.Fatalf("resolveScreenshotDir: %v", err)
		}
		if got != abs {
			t.Fatalf("resolveScreenshotDir(%q) = %q, want it unchanged", abs, got)
		}
	})

	t.Run("empty is returned unchanged", func(t *testing.T) {
		got, err := resolveScreenshotDir("")
		if err != nil {
			t.Fatalf("resolveScreenshotDir: %v", err)
		}
		if got != "" {
			t.Fatalf("resolveScreenshotDir(\"\") = %q, want empty", got)
		}
	})
}
