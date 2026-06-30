package conformance_test

import (
	"testing"

	"github.com/clems4ever/llmbox/internal/box"
	"github.com/clems4ever/llmbox/internal/box/conformance"
)

// TestConformanceFake runs the backend-neutral box contract against the
// in-process Fake provisioner, validating the Manager + agent-protocol stack with
// no Docker. The Docker backend reuses conformance.Run with its own provisioner.
func TestConformanceFake(t *testing.T) {
	conformance.Run(t, func(t testing.TB) box.Provisioner {
		return conformance.NewFake(t)
	})
}
