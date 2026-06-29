package cluster

import (
	"context"
	"strings"
	"testing"

	"github.com/clems4ever/llmbox/internal/sandbox"
)

// TestValidateCreateRejectsBadBoxID is a package test.
func TestValidateCreateRejectsBadBoxID(t *testing.T) {
	p := ValidationPolicy{}
	bad := []string{"", "-leading", "trailing-", "Upper", "has space", "under_score", strings.Repeat("a", 64)}
	for _, id := range bad {
		if err := p.validateCreate(sandbox.CreateOptions{BoxID: id}); err == nil {
			t.Errorf("box id %q should be rejected", id)
		}
	}
	good := []string{"b1", "refactor-auth", "a", strings.Repeat("a", 63)}
	for _, id := range good {
		if err := p.validateCreate(sandbox.CreateOptions{BoxID: id, Image: "img:1"}); err != nil {
			t.Errorf("box id %q should be accepted, got %v", id, err)
		}
	}
}

// TestValidateCreateImageAllowlist is a package test.
func TestValidateCreateImageAllowlist(t *testing.T) {
	p := ValidationPolicy{AllowedImages: []string{"good:1", "good:2"}}
	if err := p.validateCreate(sandbox.CreateOptions{BoxID: "b1", Image: "good:2"}); err != nil {
		t.Errorf("allowed image rejected: %v", err)
	}
	if err := p.validateCreate(sandbox.CreateOptions{BoxID: "b1", Image: "evil:latest"}); err == nil {
		t.Error("image not in allowlist should be rejected")
	}
}

// TestValidateCreateRejectsEmptyImage is a package test.
func TestValidateCreateRejectsEmptyImage(t *testing.T) {
	// A spoke has no default image of its own, so a create that names none is
	// rejected — the hub must always send an explicit image.
	p := ValidationPolicy{}
	if err := p.validateCreate(sandbox.CreateOptions{BoxID: "b1", Image: ""}); err == nil {
		t.Error("empty image should be rejected (spokes have no default)")
	}
}

// TestDispatchRejectsInvalidCreate is a package test.
func TestDispatchRejectsInvalidCreate(t *testing.T) {
	fake := &fakeManager{createID: "cid"}
	policy := ValidationPolicy{AllowedImages: []string{"good:1"}}

	// Bad box id is rejected before reaching the manager.
	req := func(opts sandbox.CreateOptions) frame {
		p, _ := encodePayload(createReq{Opts: opts})
		return frame{Type: frameReq, Method: methodCreate, Payload: p}
	}
	if _, err := dispatch(context.Background(), fake, req(sandbox.CreateOptions{BoxID: "Bad ID"}), policy); err == nil {
		t.Error("invalid box id should be rejected by dispatch")
	}
	// Disallowed image is rejected.
	if _, err := dispatch(context.Background(), fake, req(sandbox.CreateOptions{BoxID: "b1", Image: "evil"}), policy); err == nil {
		t.Error("disallowed image should be rejected by dispatch")
	}
	// The manager was never called for the rejected requests.
	if fake.lastCreate.BoxID != "" {
		t.Errorf("manager should not have been reached, saw %+v", fake.lastCreate)
	}
	// A valid request passes through.
	if _, err := dispatch(context.Background(), fake, req(sandbox.CreateOptions{BoxID: "b1", Image: "good:1"}), policy); err != nil {
		t.Errorf("valid create rejected: %v", err)
	}
	if fake.lastCreate.BoxID != "b1" {
		t.Errorf("manager should have received the valid create, saw %+v", fake.lastCreate)
	}
}
