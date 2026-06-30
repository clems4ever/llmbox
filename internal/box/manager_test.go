package box_test

import (
	"testing"

	"github.com/clems4ever/llmbox/internal/box"
	"github.com/clems4ever/llmbox/internal/box/conformance"
)

// TestBoxManager runs the backend-neutral box contract against the in-process
// Fake provisioner, validating the Manager and the agent-protocol path it drives
// without Docker. The Docker backend reuses the same conformance.Run.
func TestBoxManager(t *testing.T) {
	conformance.Run(t, func(t testing.TB) box.Provisioner {
		return conformance.NewFake(t)
	})
}
