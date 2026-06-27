package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/cluster"
	"github.com/clems4ever/llmbox/internal/docker"
	"github.com/clems4ever/llmbox/testutils"
)

// TestCreateBoxRoutesToSpoke checks a box with a spoke name is created on that
// connected remote spoke, not the local one, and the session records the spoke.
func TestCreateBoxRoutesToSpoke(t *testing.T) {
	local := &testutils.FakeMgr{CreateID: "local-id", CreateURL: "https://local"}
	edge := &testutils.FakeMgr{CreateID: "edge-id", CreateURL: "https://edge"}
	s := newTestServer(local)
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{"edge": edge}})

	sess, err := s.createBox(context.Background(), docker.CreateOptions{BoxID: "b1", SpokeName: "edge"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if sess.ContainerID != "edge-id" {
		t.Errorf("box created on wrong spoke: container = %q", sess.ContainerID)
	}
	if sess.SpokeName != "edge" {
		t.Errorf("session spoke = %q, want edge", sess.SpokeName)
	}
	if edge.GotOpts.BoxID != "b1" {
		t.Errorf("edge spoke did not receive the create (%+v)", edge.GotOpts)
	}
	if local.GotOpts.BoxID != "" {
		t.Errorf("local spoke wrongly received the create (%+v)", local.GotOpts)
	}
}

// TestCreateBoxUnknownSpoke checks creating a box on an unconnected spoke errors.
func TestCreateBoxUnknownSpoke(t *testing.T) {
	s := newTestServer(&testutils.FakeMgr{})
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{}})
	if _, err := s.createBox(context.Background(), docker.CreateOptions{BoxID: "b1", SpokeName: "ghost"}); err == nil {
		t.Fatal("expected error for unconnected spoke")
	}
}

// TestCreateBoxDefaultsToLocalSpoke checks a box with no spoke runs on the local spoke.
func TestCreateBoxDefaultsToLocalSpoke(t *testing.T) {
	local := &testutils.FakeMgr{CreateID: "local-id"}
	s := newTestServer(local)
	sess, err := s.createBox(context.Background(), docker.CreateOptions{BoxID: "b1"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if sess.SpokeName != localSpokeName {
		t.Errorf("session spoke = %q, want %q", sess.SpokeName, localSpokeName)
	}
	if local.GotOpts.BoxID != "b1" {
		t.Error("local spoke did not receive the create")
	}
}

// TestListFansOutAcrossSpokes checks list aggregates boxes from every spoke, each tagged.
func TestListFansOutAcrossSpokes(t *testing.T) {
	local := &testutils.FakeMgr{ListResult: []docker.Box{{ContainerID: "L", BoxID: "lbox"}}}
	edge := &testutils.FakeMgr{ListResult: []docker.Box{{ContainerID: "E", BoxID: "ebox"}}}
	s := newTestServer(local)
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{"edge": edge}})

	boxes, err := s.listBoxes(context.Background())
	if err != nil {
		t.Fatalf("ListBoxes: %v", err)
	}
	bySpoke := map[string]string{} // spoke -> box id
	for _, b := range boxes {
		bySpoke[b.Spoke] = b.BoxID
	}
	if bySpoke[localSpokeName] != "lbox" {
		t.Errorf("local box missing or mistagged: %v", bySpoke)
	}
	if bySpoke["edge"] != "ebox" {
		t.Errorf("edge box missing or mistagged: %v", bySpoke)
	}
}

// TestReapFansOutAcrossSpokes checks reaping fans out across local and remote spokes.
func TestReapFansOutAcrossSpokes(t *testing.T) {
	local := &testutils.FakeMgr{Reaped: []string{"l1"}}
	edge := &testutils.FakeMgr{Reaped: []string{"e1"}}
	s := newTestServer(local)
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{"edge": edge}})

	got := map[string]bool{}
	for _, id := range s.reapAllSpokes(context.Background(), nil) {
		got[id] = true
	}
	if !got["l1"] || !got["e1"] {
		t.Errorf("reaped across spokes = %v, want l1 and e1", got)
	}
}

// TestDestroyRoutesToSpoke checks a box is destroyed on the spoke its session names.
func TestDestroyRoutesToSpoke(t *testing.T) {
	local := &testutils.FakeMgr{}
	edge := &testutils.FakeMgr{CreateID: "edge-id"}
	s := newTestServer(local)
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{"edge": edge}})

	sess, err := s.createBox(context.Background(), docker.CreateOptions{BoxID: "b1", SpokeName: "edge"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if err := s.destroyBox(context.Background(), sess.ContainerID); err != nil {
		t.Fatalf("DestroyBox: %v", err)
	}
	if len(edge.Destroyed) != 1 || edge.Destroyed[0] != "edge-id" {
		t.Errorf("edge.Destroyed = %v, want [edge-id]", edge.Destroyed)
	}
	if len(local.Destroyed) != 0 {
		t.Errorf("local.Destroyed = %v, want none", local.Destroyed)
	}
}

// TestDestroyBoxByBoxIDRoutesToSpoke checks that destroying by BOX ID (what the
// admin Remove button sends, not the container ID) routes to the box's spoke and
// cleans up its session — it must not fall back to the local spoke.
func TestDestroyBoxByBoxIDRoutesToSpoke(t *testing.T) {
	local := &testutils.FakeMgr{}
	edge := &testutils.FakeMgr{CreateID: "edge-id"}
	s := newTestServer(local)
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{"edge": edge}})

	sess, err := s.createBox(context.Background(), docker.CreateOptions{BoxID: "b1", SpokeName: "edge"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}

	if err := s.destroyBox(context.Background(), "b1"); err != nil {
		t.Fatalf("DestroyBox by box id: %v", err)
	}
	if len(edge.Destroyed) != 1 || edge.Destroyed[0] != "b1" {
		t.Errorf("edge.Destroyed = %v, want [b1] (routed to the box's spoke)", edge.Destroyed)
	}
	if len(local.Destroyed) != 0 {
		t.Errorf("local.Destroyed = %v, want none (must not fall back to local)", local.Destroyed)
	}
	if s.lookup(sess.Token) != nil {
		t.Error("session not removed after destroy by box id")
	}
}

// TestDestroySessionlessBoxFindsSpoke checks that a box which appears in the
// admin list (built straight from each spoke) but has NO tracked session is
// destroyed on the spoke that actually hosts it, instead of failing because the
// destroy fell back to the local spoke. This is the bug behind the admin UI's
// "no managed box matches" error when removing a box on a remote spoke.
func TestDestroySessionlessBoxFindsSpoke(t *testing.T) {
	local := &testutils.FakeMgr{}
	// The edge spoke reports a box "test" with no session tracking it.
	edge := &testutils.FakeMgr{ListResult: []docker.Box{{BoxID: "test", ContainerID: "edgecid"}}}
	s := newTestServer(local)
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{"edge": edge}})

	if err := s.destroyBox(context.Background(), "test"); err != nil {
		t.Fatalf("DestroyBox sessionless: %v", err)
	}
	if len(edge.Destroyed) != 1 || edge.Destroyed[0] != "test" {
		t.Errorf("edge.Destroyed = %v, want [test] (routed to the hosting spoke)", edge.Destroyed)
	}
	if len(local.Destroyed) != 0 {
		t.Errorf("local.Destroyed = %v, want none (must not fall back to local)", local.Destroyed)
	}
}

// TestSpokeStatusesReportsHealth checks SpokeStatuses returns the local spoke
// plus each enrolled spoke, marking which are currently connected.
func TestSpokeStatusesReportsHealth(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_ = store.PutSpoke("edge", cluster.SpokeRecord{Name: "edge"})
	_ = store.PutSpoke("offline", cluster.SpokeRecord{Name: "offline"})

	s := New(&testutils.FakeMgr{}, nil, "https://h", time.Minute, store, nil)
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{"edge": &testutils.FakeMgr{}}}) // only edge connected

	got, err := s.SpokeStatuses(context.Background())
	if err != nil {
		t.Fatalf("SpokeStatuses: %v", err)
	}
	byName := map[string]SpokeStatus{}
	for _, st := range got {
		byName[st.Name] = st
	}
	if !byName[localSpokeName].Connected || !byName[localSpokeName].Local {
		t.Errorf("local spoke status = %+v, want connected+local", byName[localSpokeName])
	}
	if !byName["edge"].Connected {
		t.Errorf("edge should be connected: %+v", byName["edge"])
	}
	if byName["offline"].Connected {
		t.Errorf("offline should not be connected: %+v", byName["offline"])
	}
}

// TestSpokeStatusesLocalOnly checks that without a hub only the local spoke is reported.
func TestSpokeStatusesLocalOnly(t *testing.T) {
	s := newTestServer(&testutils.FakeMgr{})
	got, err := s.SpokeStatuses(context.Background())
	if err != nil {
		t.Fatalf("SpokeStatuses: %v", err)
	}
	if len(got) != 1 || got[0].Name != localSpokeName || !got[0].Connected {
		t.Fatalf("SpokeStatuses without hub = %+v, want only local", got)
	}
}

// TestListSpokesTool checks the MCP backend reports the spoke statuses.
func TestListSpokesTool(t *testing.T) {
	s := newTestServer(&testutils.FakeMgr{})
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{}})
	spokes, err := s.MCPBackend().SpokeStatuses(context.Background())
	if err != nil {
		t.Fatalf("SpokeStatuses: %v", err)
	}
	if len(spokes) != 1 || spokes[0].Name != localSpokeName {
		t.Fatalf("spokes = %+v, want the local spoke", spokes)
	}
}

// TestRestoreKeepsDisconnectedSpokeSessions checks a session on an offline spoke is kept while a dead local one is dropped.
func TestRestoreKeepsDisconnectedSpokeSessions(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// A live local session and a session on a spoke that isn't connected.
	if err := store.Save(persistedSession{Token: "dead-local", ContainerID: "deadbeef", SpokeName: localSpokeName, Status: "pending"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store.Save(persistedSession{Token: "edge-sess", ContainerID: "edgecid", SpokeName: "edge", Status: "ready"}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Local spoke lists no boxes (so the local session is dead); no hub means the
	// "edge" spoke is unreachable, so its session must be kept.
	local := &testutils.FakeMgr{ListResult: nil}
	s := New(local, nil, "https://boxes.example.com", time.Minute, store, nil)

	n, err := s.Restore(context.Background())
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if n != 1 {
		t.Errorf("restored %d sessions, want 1", n)
	}
	if s.lookup("dead-local") != nil {
		t.Error("dead local session should have been dropped")
	}
	if s.lookup("edge-sess") == nil {
		t.Error("session on a Disconnected spoke should have been kept")
	}
}
