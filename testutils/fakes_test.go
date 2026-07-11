package testutils

import (
	"context"
	"errors"
	"testing"

	"github.com/clems4ever/llmbox/internal/shared/cluster"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// TestFakeMgr checks FakeMgr satisfies cluster.BoxManager and that each verb
// records its inputs and returns the canned results/errors it was configured
// with.
func TestFakeMgr(t *testing.T) {
	var m cluster.BoxManager = &FakeMgr{}
	f := m.(*FakeMgr)
	f.CreateID = "cid"
	f.ListResult = []sandbox.Box{{BoxID: "b1"}}
	f.ExecResult = sandbox.ExecResult{ExitCode: 0}

	if res, err := m.Create(context.Background(), sandbox.CreateOptions{BoxID: "b1"}); err != nil || res.InstanceID != "cid" {
		t.Errorf("Create = %+v, %v", res, err)
	}
	if f.GotOpts.BoxID != "b1" {
		t.Errorf("GotOpts.BoxID = %q, want b1", f.GotOpts.BoxID)
	}
	// ListResult seeded one box and the Create above added another, so List returns
	// both — created boxes track through the fake like a real spoke.
	if got, err := m.List(context.Background()); err != nil || len(got) != 2 {
		t.Errorf("List = %v, %v", got, err)
	}
	if f.ListCalls() != 1 {
		t.Errorf("ListCalls = %d, want 1", f.ListCalls())
	}
	if _, err := m.Exec(context.Background(), "b1", []string{"echo"}); err != nil {
		t.Errorf("Exec: %v", err)
	}

	// Pause records the ID; Resume records the ID.
	if err := m.Pause(context.Background(), "b1"); err != nil {
		t.Errorf("Pause: %v", err)
	}
	if len(f.Paused) != 1 || f.Paused[0] != "b1" {
		t.Errorf("Paused = %v, want [b1]", f.Paused)
	}
	if err := m.Resume(context.Background(), "b1"); err != nil {
		t.Errorf("Resume: %v", err)
	}
	if len(f.Resumed) != 1 || f.Resumed[0] != "b1" {
		t.Errorf("Resumed = %v, want [b1]", f.Resumed)
	}

	// Destroy records the ID and surfaces the canned DestroyErr.
	sentinel := errors.New("gone")
	f.DestroyErr = sentinel
	if err := m.Destroy(context.Background(), "b1"); !errors.Is(err, sentinel) {
		t.Errorf("Destroy err = %v, want sentinel", err)
	}
	if len(f.Destroyed) != 1 || f.Destroyed[0] != "b1" {
		t.Errorf("Destroyed = %v, want [b1]", f.Destroyed)
	}
}

// TestFakeHub checks FakeHub satisfies the server's hub surface: it returns the
// spokes injected into it and records the names passed to Disconnect.
func TestFakeHub(t *testing.T) {
	mgr := &FakeMgr{}
	h := &FakeHub{Connected: map[string]cluster.BoxManager{"edge": mgr}}

	if bm, ok := h.Spoke("edge"); !ok || bm != mgr {
		t.Errorf("Spoke(edge) = %v, %v", bm, ok)
	}
	if _, ok := h.Spoke("missing"); ok {
		t.Error("Spoke(missing) should report not found")
	}
	if got := h.Spokes(); len(got) != 1 {
		t.Errorf("Spokes = %v, want one entry", got)
	}
	// ConnectHandler is inert (tests inject spokes directly).
	h.ConnectHandler(nil, nil)
	h.Disconnect("edge")
	if len(h.Disconnected) != 1 || h.Disconnected[0] != "edge" {
		t.Errorf("Disconnected = %v, want [edge]", h.Disconnected)
	}
}
