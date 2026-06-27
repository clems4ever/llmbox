//go:build integration

package docker

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestCreateIntegration drives a real container: it creates a box from a box
// image with the standalone Claude binary baked in (and tini as PID 1), and
// verifies that a genuine OAuth authorize URL is captured from the live `claude
// auth login` flow. It then destroys the box.
//
// Set LLMBOX_IT_IMAGE to override the box image (it must bake in Claude, tini,
// util-linux, and a CA bundle — see Dockerfile.box); it defaults to the built-in
// box image:
//
//	LLMBOX_IT_IMAGE=ghcr.io/clems4ever/llmbox-box:latest go test -tags=integration -run Integration -v ./internal/docker/
func TestCreateIntegration(t *testing.T) {
	image := os.Getenv("LLMBOX_IT_IMAGE")
	if image == "" {
		image = DefaultImage
	}

	m, err := NewManager(image, "", nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	id, url, err := m.Create(ctx, CreateOptions{})
	if id != "" {
		// Always clean up the container we started.
		defer func() {
			if derr := m.Destroy(context.Background(), id); derr != nil {
				t.Logf("cleanup: %v", derr)
			}
		}()
	}
	if err != nil {
		t.Fatalf("Create: %v", err)
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
