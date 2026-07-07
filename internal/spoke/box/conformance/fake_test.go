package conformance_test

import (
	"context"
	"errors"
	"testing"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/box/conformance"
)

// TestFakeProvisionerDirect exercises the Fake provisioner's resolve/teardown
// branches directly: Find by id and box id, the not-found path, MarkReady, and
// idempotent Destroy.
func TestFakeProvisionerDirect(t *testing.T) {
	f := conformance.NewFake(t)
	ctx := context.Background()

	inst, err := f.Provision(ctx, sandbox.CreateOptions{BoxID: "direct-box"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	id := inst.Meta().InstanceID

	if _, err := f.Find(ctx, id); err != nil {
		t.Fatalf("Find by id: %v", err)
	}
	if _, err := f.Find(ctx, "direct-box"); err != nil {
		t.Fatalf("Find by box id: %v", err)
	}
	if _, err := f.Find(ctx, "nope"); !errors.Is(err, sandbox.ErrBoxNotFound) {
		t.Fatalf("Find unknown = %v, want ErrBoxNotFound", err)
	}

	conn, err := inst.Control(ctx)
	if err != nil {
		t.Fatalf("Control: %v", err)
	}
	_ = conn.Close()

	if err := inst.MarkReady(ctx); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	if inst.Meta().Phase != "ready" {
		t.Fatalf("phase = %q, want ready", inst.Meta().Phase)
	}

	if err := inst.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if err := inst.Destroy(ctx); !errors.Is(err, sandbox.ErrBoxNotFound) {
		t.Fatalf("second Destroy = %v, want ErrBoxNotFound", err)
	}
}
