//go:build e2e

// These end-to-end tests exercise the hub-and-spoke box lifecycle entirely
// through the ADMIN UI (the admin HTTP endpoints a human clicks), not the MCP
// API. They share clusterFixture, which stands up a real hub, a real HTTP server
// and a signed-in admin, and attach real spokes that dial in over WebSockets.
package clustere2e

import "testing"

// TestUIStartThenDestroyBoxOnRemote starts a box on a remote spoke through the
// admin UI, then removes it through the admin UI, checking the box lands on the
// spoke, shows up in the dashboard under that spoke, and is gone from both the
// spoke and the dashboard after removal.
func TestUIStartThenDestroyBoxOnRemote(t *testing.T) {
	f := newClusterFixture(t)
	edge := f.connectSpoke("edge")

	// Create the box on the remote spoke through the UI.
	f.createBoxUI("b1", "edge")
	if edge.mgr.creates() != 1 {
		t.Errorf("edge spoke creates = %d, want 1", edge.mgr.creates())
	}
	if f.localMgr.creates() != 0 {
		t.Errorf("local spoke creates = %d, want 0 (box must not run locally)", f.localMgr.creates())
	}
	if !edge.mgr.hasBox("b1") {
		t.Fatal("box b1 not present on the edge spoke after create")
	}
	if spoke, present := f.boxOnSpokeUI("b1"); !present || spoke != "edge" {
		t.Fatalf("dashboard shows box b1 on spoke %q (present=%v), want edge", spoke, present)
	}

	// Remove the box through the UI.
	if res := f.deleteBoxUI("b1"); !res.OK {
		t.Fatalf("removing box b1 failed: %s", res.Err)
	}
	if edge.mgr.live() != 0 {
		t.Errorf("edge spoke still has %d live box(es) after removal", edge.mgr.live())
	}
	if _, present := f.boxOnSpokeUI("b1"); present {
		t.Error("dashboard still lists box b1 after removal")
	}
}

// TestUIDisconnectReconnectThenDestroy starts a box on a remote spoke, drops the
// spoke's connection and checks the dashboard reports it offline, reconnects it
// and checks the dashboard reports it connected again, then removes the box
// through the UI.
func TestUIDisconnectReconnectThenDestroy(t *testing.T) {
	f := newClusterFixture(t)
	edge := f.connectSpoke("edge")

	f.createBoxUI("b1", "edge")
	if !edge.mgr.hasBox("b1") {
		t.Fatal("box b1 not present on the edge spoke after create")
	}

	// Disconnect the spoke and confirm the UI shows it offline.
	edge.disconnect()
	if connected, present := f.spokeConnectedUI("edge"); !present || connected {
		t.Fatalf("after disconnect, dashboard shows edge connected=%v present=%v, want offline", connected, present)
	}

	// Reconnect the spoke (with its saved credentials) and confirm the UI shows it
	// connected again.
	edge.reconnect()
	if connected, present := f.spokeConnectedUI("edge"); !present || !connected {
		t.Fatalf("after reconnect, dashboard shows edge connected=%v present=%v, want connected", connected, present)
	}

	// The box still lives on the spoke across the reconnect; remove it via the UI.
	if res := f.deleteBoxUI("b1"); !res.OK {
		t.Fatalf("removing box b1 after reconnect failed: %s", res.Err)
	}
	if edge.mgr.live() != 0 {
		t.Errorf("edge spoke still has %d live box(es) after removal", edge.mgr.live())
	}
}

// TestUIRemoveBoxAfterHumanDestroyedIt creates a box on a remote spoke, simulates
// a human removing its container directly on the host (out of band), then removes
// it through the admin UI. The removal must succeed without error even though the
// container no longer exists, and the box must be cleared from the dashboard.
func TestUIRemoveBoxAfterHumanDestroyedIt(t *testing.T) {
	f := newClusterFixture(t)
	edge := f.connectSpoke("edge")

	f.createBoxUI("b1", "edge")
	if !edge.mgr.hasBox("b1") {
		t.Fatal("box b1 not present on the edge spoke after create")
	}

	// A human destroys the container directly on the host, out of band: the box
	// vanishes from the spoke without going through the cluster Destroy path.
	edge.mgr.humanDestroy("b1")
	if edge.mgr.hasBox("b1") {
		t.Fatal("box b1 should be gone from the spoke after the human removed it")
	}

	// The admin clicks Remove in the UI. It must succeed despite the container
	// already being gone (removal is idempotent).
	if res := f.deleteBoxUI("b1"); !res.OK {
		t.Fatalf("removing an already-gone box should succeed, got error: %s", res.Err)
	}
	if _, present := f.boxOnSpokeUI("b1"); present {
		t.Error("dashboard still lists box b1 after removal")
	}
}
