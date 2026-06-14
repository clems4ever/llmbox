//go:build integration

package docker

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestCreateLLMBoxIntegration drives a real container: it creates a box from the
// configured image and verifies that a genuine OAuth authorize URL is captured
// from the live `claude auth login` flow. It then destroys the box.
//
// Run with a built claude image:
//
//	LLMBOX_IT_IMAGE=claude-remote:test go test -tags=integration -run Integration -v ./internal/docker/
func TestCreateLLMBoxIntegration(t *testing.T) {
	image := "claude-remote:test"

	m, err := NewManager(image, "", "", "")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	id, url, err := m.CreateLLMBox(ctx, CreateOptions{})
	if id != "" {
		// Always clean up the container we started.
		defer func() {
			if derr := m.Destroy(context.Background(), id); derr != nil {
				t.Logf("cleanup: %v", derr)
			}
		}()
	}
	if err != nil {
		t.Fatalf("CreateLLMBox: %v", err)
	}

	t.Logf("captured authorize URL: %s", url)
	if !strings.HasPrefix(url, "https://claude.com/cai/oauth/authorize?") {
		t.Errorf("unexpected authorize URL: %q", url)
	}
	if !strings.Contains(url, "code_challenge=") || !strings.Contains(url, "state=") {
		t.Errorf("authorize URL missing PKCE/state params: %q", url)
	}
	if !strings.Contains(url, "platform.claude.com%2Foauth%2Fcode%2Fcallback") {
		t.Errorf("authorize URL is not the out-of-band code flow: %q", url)
	}
}
