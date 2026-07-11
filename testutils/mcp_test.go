package testutils

import (
	"context"
	"testing"

	"github.com/clems4ever/llmbox/internal/mcpserver"
	"github.com/clems4ever/llmbox/internal/shared/api"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// TestFakeBackend checks the fake records its inputs and returns the canned
// results across the Backend methods.
func TestFakeBackend(t *testing.T) {
	f := &FakeBackend{
		CreateSess: mcpserver.BoxSession{BoxID: "web", Generation: "cid"},
		Sessions:   map[string]mcpserver.BoxSession{"web": {BoxID: "web", Description: "ready"}},
		Boxes:      []api.BoxView{{Box: sandbox.Box{BoxID: "b1"}}},
		ExecResult: sandbox.ExecResult{ExitCode: 3},
		ProxyOn:    true,
	}
	ctx := context.Background()

	if _, err := f.CreateBox(ctx, sandbox.CreateOptions{BoxID: "web", Description: "d"}); err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if f.GotCreate.BoxID != "web" || f.GotCreate.Description != "d" {
		t.Errorf("GotCreate = %+v", f.GotCreate)
	}

	if s, ok := f.LookupByBoxID("WEB"); !ok || s.Description != "ready" {
		t.Errorf("LookupByBoxID case-insensitive miss: %+v ok=%v", s, ok)
	}
	if _, ok := f.LookupByBoxID("nope"); ok {
		t.Error("LookupByBoxID(nope) should miss")
	}

	if boxes, _ := f.ListBoxes(ctx); len(boxes) != 1 {
		t.Errorf("ListBoxes = %v", boxes)
	}
	if err := f.DestroyBox(ctx, "cid"); err != nil || f.GotDestroyID != "cid" {
		t.Errorf("DestroyBox err=%v id=%q", err, f.GotDestroyID)
	}
	if err := f.PauseBox(ctx, "pz"); err != nil || f.GotPauseID != "pz" {
		t.Errorf("PauseBox err=%v id=%q", err, f.GotPauseID)
	}
	if err := f.ResumeBox(ctx, "pz"); err != nil || f.GotResumeID != "pz" {
		t.Errorf("ResumeBox err=%v id=%q", err, f.GotResumeID)
	}
	if res, _ := f.BoxExec(ctx, "web", "ls"); res.ExitCode != 3 || f.GotExecCmd != "ls" {
		t.Errorf("BoxExec = %+v cmd=%q", res, f.GotExecCmd)
	}
	if !f.ProxyEnabled() {
		t.Error("ProxyEnabled = false")
	}
	if _, _ = f.CreateProxy(ctx, "web", 8000, "app"); f.GotProxyPort != 8000 || f.GotProxyDesc != "app" {
		t.Errorf("CreateProxy recorded %d/%q", f.GotProxyPort, f.GotProxyDesc)
	}
	if err := f.DeleteProxy(ctx, "web", 8000); err != nil || f.GotDeleteBoxID != "web" {
		t.Errorf("DeleteProxy err=%v box=%q", err, f.GotDeleteBoxID)
	}
	if _, _ = f.ListProxies(ctx, "b1"); f.GotListBoxID != "b1" {
		t.Errorf("ListProxies filter = %q", f.GotListBoxID)
	}
}

// TestConnectMCP checks the helper stands up an MCP server over a FakeBackend and
// returns a session that can list the registered tools.
func TestConnectMCP(t *testing.T) {
	cs := ConnectMCP(t, &FakeBackend{}, "test", "v0")
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	found := false
	for _, tl := range tools.Tools {
		if tl.Name == "create_llmbox" {
			found = true
		}
	}
	if !found {
		t.Fatal("create_llmbox tool not registered via ConnectMCP")
	}
}
