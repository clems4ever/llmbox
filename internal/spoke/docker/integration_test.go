//go:build integration

package docker_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/spoke/box"
	"github.com/clems4ever/llmbox/internal/spoke/box/conformance"
	"github.com/clems4ever/llmbox/internal/spoke/docker"
	"github.com/clems4ever/llmbox/testutils"
)

// mockDockerfile builds a minimal box image: tini, the llmbox-agent entrypoint,
// and a mock `claude` so the box's login flow is deterministic (real claude would
// need a real OAuth code). It lets the full conformance contract run against real
// Docker containers, exercising the provisioner, the socket bind-mount across
// uids, agent reachability, exec, dial, rename, and destroy.
const mockDockerfile = `FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends tini && rm -rf /var/lib/apt/lists/*
COPY llmbox-agent /usr/local/bin/llmbox-agent
COPY claude /usr/local/bin/claude
ENTRYPOINT ["tini","-g","--","llmbox-agent"]
`

// TestDockerConformance runs the backend-neutral box contract against a real
// Docker daemon, using a mock-claude box image. Run it with:
//
//	go test -tags=integration -run TestDockerConformance -v ./internal/docker/
func TestDockerConformance(t *testing.T) {
	requireDocker(t)
	image := buildMockImage(t)

	socketDir := t.TempDir()
	conformance.Run(t, func(t testing.TB) box.Provisioner {
		prov, err := docker.NewProvisioner(image, socketDir, nil, nil)
		if err != nil {
			t.Fatalf("NewProvisioner: %v", err)
		}
		// Subtests share one daemon and the contract makes exact List-count
		// assertions, so start each from a clean slate (destroy any managed boxes
		// left by a previous subtest or run) and clean up again when it ends.
		destroyAll := func() {
			insts, _ := prov.List(context.Background())
			for _, inst := range insts {
				_ = inst.Destroy(context.Background())
			}
		}
		destroyAll()
		t.Cleanup(func() {
			destroyAll()
			_ = prov.Close()
		})
		return prov
	})
}

// requireDocker skips the test when no Docker daemon is reachable.
func requireDocker(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
}

// buildMockImage builds (the agent binary, the mock claude, then) the mock box
// image and returns its tag, removing it on cleanup.
func buildMockImage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// Build the agent binary for the container (linux/amd64, static).
	agentBin := filepath.Join(dir, "llmbox-agent")
	build := exec.Command("go", "build", "-trimpath", "-o", agentBin, "github.com/clems4ever/llmbox/cmd/llmbox-agent")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building agent: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(testutils.MockClaudeScript), 0o755); err != nil {
		t.Fatalf("writing mock claude: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(mockDockerfile), 0o644); err != nil {
		t.Fatalf("writing Dockerfile: %v", err)
	}

	tag := "llmbox-it-mock:latest"
	if out, err := exec.Command("docker", "build", "-t", tag, dir).CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", tag).Run() })
	return tag
}
