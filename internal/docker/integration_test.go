//go:build integration

package docker

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestCreateLLMBoxIntegration drives a real container: it creates a box from a
// plain glibc image with the standalone Claude binary injected, and verifies
// that a genuine OAuth authorize URL is captured from the live `claude auth
// login` flow. It then destroys the box.
//
// Set LLMBOX_IT_IMAGE to override the base image and LLMBOX_IT_CLAUDE_BIN to
// point at a Claude binary on the test host:
//
//	LLMBOX_IT_CLAUDE_BIN=$HOME/.local/bin/claude go test -tags=integration -run Integration -v ./internal/docker/
func TestCreateLLMBoxIntegration(t *testing.T) {
	image := os.Getenv("LLMBOX_IT_IMAGE")
	if image == "" {
		image = "debian:bookworm-slim"
	}
	claudeBin := os.Getenv("LLMBOX_IT_CLAUDE_BIN")
	if claudeBin == "" {
		t.Skip("set LLMBOX_IT_CLAUDE_BIN to the path of a standalone Claude binary to run this test")
	}

	m, err := NewManager(image, "", claudeBin, nil)
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
