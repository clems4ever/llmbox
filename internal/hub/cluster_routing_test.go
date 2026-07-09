package hub

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/cluster"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/testutils"
)

// serverWithSpokes builds a hub-backed Server whose connected spokes are the given
// managers, using a settings-capable in-memory store.
func serverWithSpokes(spokes map[string]boxManager) *Server {
	s := New(nil, "https://boxes.example.com", time.Minute, newTestStore(), nil)
	s.SetHub(&testutils.FakeHub{Connected: spokes})
	return s
}

// TestCreateBoxRoutesToSpoke checks a box with a spoke name is created on that
// connected remote spoke, not another, and the session records the spoke.
func TestCreateBoxRoutesToSpoke(t *testing.T) {
	other := &testutils.FakeMgr{CreateID: "other-id"}
	edge := &testutils.FakeMgr{CreateID: "edge-id", CreateURL: "https://edge"}
	s := serverWithSpokes(map[string]boxManager{"edge": edge, "other": other})

	sess, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "b1", SpokeName: "edge"})
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
	if other.GotOpts.BoxID != "" {
		t.Errorf("other spoke wrongly received the create (%+v)", other.GotOpts)
	}
}

// TestDefaultSpokeRoundTrip checks the default spoke persists through the store and
// clears back to empty.
func TestDefaultSpokeRoundTrip(t *testing.T) {
	s := New(nil, "https://h", time.Minute, newTestStore(), nil)
	if def, err := s.DefaultSpoke(); err != nil || def != "" {
		t.Fatalf("initial default = %q, err %v; want empty", def, err)
	}
	if err := s.SetDefaultSpoke("edge-7"); err != nil {
		t.Fatalf("SetDefaultSpoke: %v", err)
	}
	if def, _ := s.DefaultSpoke(); def != "edge-7" {
		t.Errorf("default = %q, want edge-7", def)
	}
	if err := s.SetDefaultSpoke(""); err != nil {
		t.Fatalf("clear default: %v", err)
	}
	if def, _ := s.DefaultSpoke(); def != "" {
		t.Errorf("default after clear = %q, want empty", def)
	}
}

// TestCreateBoxUnknownSpoke checks creating a box on an unconnected spoke errors.
func TestCreateBoxUnknownSpoke(t *testing.T) {
	s := serverWithSpokes(map[string]boxManager{})
	if _, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "b1", SpokeName: "ghost"}); err == nil {
		t.Fatal("expected error for unconnected spoke")
	}
}

// TestCreateBoxDefaultsToDefaultSpoke checks a box with no spoke runs on the
// admin-chosen default spoke and the session records it.
func TestCreateBoxDefaultsToDefaultSpoke(t *testing.T) {
	edge := &testutils.FakeMgr{CreateID: "edge-id"}
	s := serverWithSpokes(map[string]boxManager{"edge": edge})
	if err := s.SetDefaultSpoke("edge"); err != nil {
		t.Fatalf("SetDefaultSpoke: %v", err)
	}
	sess, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "b1"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if sess.SpokeName != "edge" {
		t.Errorf("session spoke = %q, want edge (the default)", sess.SpokeName)
	}
	if edge.GotOpts.BoxID != "b1" {
		t.Error("default spoke did not receive the create")
	}
}

// TestCreateBoxNoDefaultSpoke checks a box with no spoke errors when no default is set.
func TestCreateBoxNoDefaultSpoke(t *testing.T) {
	s := serverWithSpokes(map[string]boxManager{"edge": &testutils.FakeMgr{}})
	if _, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "b1"}); err == nil {
		t.Fatal("expected error creating a box with no spoke and no default set")
	}
}

// TestListBoxesFromRecords checks the box list is rendered from the hub's
// records — one entry per tracked box, tagged with its spoke and carrying the
// observed metadata — without any live spoke fan-out at read time.
func TestListBoxesFromRecords(t *testing.T) {
	one := &testutils.FakeMgr{CreateID: "aaaaaaaaaaaa1111"}
	edge := &testutils.FakeMgr{CreateID: "eeeeeeeeeeee2222"}
	s := serverWithSpokes(map[string]boxManager{"one": one, "edge": edge})
	if _, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "onebox", SpokeName: "one"}); err != nil {
		t.Fatalf("CreateBox one: %v", err)
	}
	if _, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "ebox", SpokeName: "edge"}); err != nil {
		t.Fatalf("CreateBox edge: %v", err)
	}

	oneLists, edgeLists := one.ListCalls(), edge.ListCalls()
	boxes, err := s.listBoxes(context.Background())
	if err != nil {
		t.Fatalf("ListBoxes: %v", err)
	}
	bySpoke := map[string]sandbox.Box{}
	for _, b := range boxes {
		bySpoke[b.Spoke] = b
	}
	if bySpoke["one"].BoxID != "onebox" {
		t.Errorf("box missing or mistagged: %v", bySpoke)
	}
	if bySpoke["edge"].BoxID != "ebox" {
		t.Errorf("edge box missing or mistagged: %v", bySpoke)
	}
	if got := bySpoke["one"].InstanceID; got != "aaaaaaaaaaaa" {
		t.Errorf("instance ID = %q, want the short 12-char form", got)
	}
	if bySpoke["one"].State != "running" || bySpoke["edge"].State != "running" {
		t.Errorf("connected spokes' boxes should list as running: %v", bySpoke)
	}
	// The listing itself must not have fanned out to the spokes.
	if one.ListCalls() != oneLists || edge.ListCalls() != edgeLists {
		t.Error("listBoxes contacted a spoke; the records are the source of truth")
	}
}

// TestListBoxesMarksUnreachable checks a box whose spoke has no live connection
// stays listed, rendered as unreachable rather than dropped.
func TestListBoxesMarksUnreachable(t *testing.T) {
	edge := &testutils.FakeMgr{CreateID: "eeeeeeeeeeee2222"}
	s := serverWithSpokes(map[string]boxManager{"edge": edge})
	if _, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "ebox", SpokeName: "edge"}); err != nil {
		t.Fatalf("CreateBox: %v", err)
	}

	// The spoke disconnects: the hub no longer holds a live connection to it.
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{}})

	boxes, err := s.listBoxes(context.Background())
	if err != nil {
		t.Fatalf("ListBoxes: %v", err)
	}
	if len(boxes) != 1 {
		t.Fatalf("got %d boxes, want the offline spoke's box to stay listed", len(boxes))
	}
	if boxes[0].State != sandbox.StateUnreachable {
		t.Errorf("state = %q, want unreachable", boxes[0].State)
	}
	if boxes[0].LastSeen == 0 {
		t.Error("an unreachable box should carry its last-seen timestamp")
	}

	// The spoke reconnects: the same record renders running again, instantly —
	// reachability is computed at read time, never stored.
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{"edge": edge}})
	boxes, _ = s.listBoxes(context.Background())
	if len(boxes) != 1 || boxes[0].State == sandbox.StateUnreachable {
		t.Errorf("reconnected spoke's box should not be unreachable: %+v", boxes)
	}
}

// TestReapFansOutAcrossSpokes checks reaping fans out across every connected spoke.
func TestReapFansOutAcrossSpokes(t *testing.T) {
	one := &testutils.FakeMgr{Reaped: []string{"o1"}}
	edge := &testutils.FakeMgr{Reaped: []string{"e1"}}
	s := serverWithSpokes(map[string]boxManager{"one": one, "edge": edge})

	got := map[string]bool{}
	for _, id := range s.reapAllSpokes(context.Background(), nil) {
		got[id] = true
	}
	if !got["o1"] || !got["e1"] {
		t.Errorf("reaped across spokes = %v, want o1 and e1", got)
	}
}

// TestDestroyRoutesToSpoke checks a box is destroyed on the spoke its session names.
func TestDestroyRoutesToSpoke(t *testing.T) {
	other := &testutils.FakeMgr{}
	edge := &testutils.FakeMgr{CreateID: "edge-id"}
	s := serverWithSpokes(map[string]boxManager{"edge": edge, "other": other})

	sess, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "b1", SpokeName: "edge"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if err := s.destroyBox(context.Background(), sess.ContainerID); err != nil {
		t.Fatalf("DestroyBox: %v", err)
	}
	if len(edge.Destroyed) != 1 || edge.Destroyed[0] != "edge-id" {
		t.Errorf("edge.Destroyed = %v, want [edge-id]", edge.Destroyed)
	}
	if len(other.Destroyed) != 0 {
		t.Errorf("other.Destroyed = %v, want none", other.Destroyed)
	}
}

// TestDestroyBoxByBoxIDRoutesToSpoke checks that destroying by BOX ID (what the
// admin Remove button sends, not the container ID) routes to the box's spoke and
// cleans up its session.
func TestDestroyBoxByBoxIDRoutesToSpoke(t *testing.T) {
	other := &testutils.FakeMgr{}
	edge := &testutils.FakeMgr{CreateID: "edge-id"}
	s := serverWithSpokes(map[string]boxManager{"edge": edge, "other": other})

	sess, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "b1", SpokeName: "edge"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}

	if err := s.destroyBox(context.Background(), "b1"); err != nil {
		t.Fatalf("DestroyBox by box id: %v", err)
	}
	if len(edge.Destroyed) != 1 || edge.Destroyed[0] != "b1" {
		t.Errorf("edge.Destroyed = %v, want [b1] (routed to the box's spoke)", edge.Destroyed)
	}
	if len(other.Destroyed) != 0 {
		t.Errorf("other.Destroyed = %v, want none", other.Destroyed)
	}
	if s.lookup(sess.Token) != nil {
		t.Error("session not removed after destroy by box id")
	}
}

// TestDestroySessionlessBoxFindsSpoke checks that a box which appears in the admin
// list (built straight from each spoke) but has NO tracked session is destroyed on
// the spoke that actually hosts it.
func TestDestroySessionlessBoxFindsSpoke(t *testing.T) {
	other := &testutils.FakeMgr{}
	// The edge spoke reports a box "test" with no session tracking it.
	edge := &testutils.FakeMgr{ListResult: []sandbox.Box{{BoxID: "test", InstanceID: "edgecid"}}}
	s := serverWithSpokes(map[string]boxManager{"edge": edge, "other": other})

	if err := s.destroyBox(context.Background(), "test"); err != nil {
		t.Fatalf("DestroyBox sessionless: %v", err)
	}
	if len(edge.Destroyed) != 1 || edge.Destroyed[0] != "test" {
		t.Errorf("edge.Destroyed = %v, want [test] (routed to the hosting spoke)", edge.Destroyed)
	}
	if len(other.Destroyed) != 0 {
		t.Errorf("other.Destroyed = %v, want none", other.Destroyed)
	}
}

// TestDestroyAlreadyGoneBoxSucceeds checks that removing a box whose container is
// already gone on its spoke (the spoke returns a not-found error) is treated as a
// successful, idempotent removal: no error, and the session is still forgotten.
func TestDestroyAlreadyGoneBoxSucceeds(t *testing.T) {
	edge := &testutils.FakeMgr{CreateID: "edge-id", DestroyErr: sandbox.ErrBoxNotFound}
	s := serverWithSpokes(map[string]boxManager{"edge": edge})

	sess, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "b1", SpokeName: "edge"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}

	if err := s.destroyBox(context.Background(), "b1"); err != nil {
		t.Fatalf("destroyBox of an already-gone box should succeed, got: %v", err)
	}
	if len(edge.Destroyed) != 1 || edge.Destroyed[0] != "b1" {
		t.Errorf("edge.Destroyed = %v, want [b1] (destroy still routed to the spoke)", edge.Destroyed)
	}
	if s.lookup(sess.Token) != nil {
		t.Error("session not forgotten after destroying an already-gone box")
	}
}

// TestDestroyUnknownBoxIsIdempotent checks that destroying a box no session tracks
// and no connected spoke reports succeeds as a no-op (the box is already gone
// everywhere) rather than erroring, and touches no spoke.
func TestDestroyUnknownBoxIsIdempotent(t *testing.T) {
	edge := &testutils.FakeMgr{} // lists no boxes
	s := serverWithSpokes(map[string]boxManager{"edge": edge})

	if err := s.destroyBox(context.Background(), "ghost-box"); err != nil {
		t.Fatalf("destroying an unknown box should be a no-op success, got: %v", err)
	}
	if len(edge.Destroyed) != 0 {
		t.Errorf("edge.Destroyed = %v, want none (no spoke hosts the box)", edge.Destroyed)
	}
}

// TestSpokeStatusesReportsHealth checks SpokeStatuses returns each enrolled spoke,
// marking which are connected and which is the default.
func TestSpokeStatusesReportsHealth(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_ = store.PutSpoke("edge", cluster.SpokeRecord{Name: "edge"})
	_ = store.PutSpoke("offline", cluster.SpokeRecord{Name: "offline"})

	s := New(nil, "https://h", time.Minute, store, nil)
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{"edge": &testutils.FakeMgr{}}}) // only edge connected
	if err := s.SetDefaultSpoke("edge"); err != nil {
		t.Fatalf("SetDefaultSpoke: %v", err)
	}

	got, err := s.SpokeStatuses(context.Background())
	if err != nil {
		t.Fatalf("SpokeStatuses: %v", err)
	}
	byName := map[string]SpokeStatus{}
	for _, st := range got {
		byName[st.Name] = st
	}
	if len(got) != 2 {
		t.Fatalf("SpokeStatuses = %+v, want 2 (no synthetic local)", got)
	}
	if !byName["edge"].Connected {
		t.Errorf("edge should be connected: %+v", byName["edge"])
	}
	if byName["offline"].Connected {
		t.Errorf("offline should not be connected: %+v", byName["offline"])
	}
}

// TestSpokeStatusesMarksDefault checks the default spoke is flagged in the report.
func TestSpokeStatusesMarksDefault(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_ = store.PutSpoke("edge", cluster.SpokeRecord{Name: "edge"})
	_ = store.PutSpoke("edge2", cluster.SpokeRecord{Name: "edge2"})

	s := New(nil, "https://h", time.Minute, store, nil)
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{"edge": &testutils.FakeMgr{}, "edge2": &testutils.FakeMgr{}}})
	if err := s.SetDefaultSpoke("edge2"); err != nil {
		t.Fatalf("SetDefaultSpoke: %v", err)
	}

	got, err := s.SpokeStatuses(context.Background())
	if err != nil {
		t.Fatalf("SpokeStatuses: %v", err)
	}
	for _, st := range got {
		if st.Name == "edge2" && !st.Default {
			t.Errorf("edge2 should be marked default: %+v", st)
		}
		if st.Name == "edge" && st.Default {
			t.Errorf("edge should not be marked default: %+v", st)
		}
	}
}

// TestListSpokesTool checks the MCP backend reports the enrolled spoke statuses.
func TestListSpokesTool(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_ = store.PutSpoke("edge", cluster.SpokeRecord{Name: "edge"})

	s := New(nil, "https://h", time.Minute, store, nil)
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{"edge": &testutils.FakeMgr{}}})

	spokes, err := s.boxBackend().SpokeStatuses(context.Background())
	if err != nil {
		t.Fatalf("SpokeStatuses: %v", err)
	}
	if len(spokes) != 1 || spokes[0].Name != "edge" {
		t.Fatalf("spokes = %+v, want the enrolled edge spoke", spokes)
	}
}

// TestRestoreKeepsDisconnectedSpokeSessions checks every record survives a
// restart — restore never contacts a spoke — and the subsequent sync pass only
// draws conclusions about reachable spokes: a box gone from a connected spoke is
// tombstoned while a record on a disconnected spoke is left untouched.
func TestRestoreKeepsDisconnectedSpokeSessions(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Both spokes are enrolled (so neither is treated as departed); only edge is
	// connected below.
	_ = store.PutSpoke("edge", cluster.SpokeRecord{Name: "edge"})
	_ = store.PutSpoke("offline", cluster.SpokeRecord{Name: "offline"})

	// A dead session on a connected spoke and a session on a spoke that isn't connected.
	if err := store.Save(persistedSession{Token: "dead-edge", ContainerID: "deadbeef", SpokeName: "edge", Status: "pending"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store.Save(persistedSession{Token: "off-sess", ContainerID: "offcid", SpokeName: "offline", Status: "ready"}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// The edge spoke lists no boxes (so its session is dead); the "offline" spoke is
	// not connected, so its session must be left untouched.
	edge := &testutils.FakeMgr{ListResult: nil}
	s := New(nil, "https://boxes.example.com", time.Minute, store, nil)
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{"edge": edge}})

	n, err := s.Restore()
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if n != 2 {
		t.Errorf("restored %d sessions, want 2 (restore drops nothing)", n)
	}

	s.syncSpokes(context.Background())

	if sess := s.lookup("dead-edge"); sess == nil {
		t.Error("dead session should be kept as a tombstone, not dropped")
	} else if !sess.terminated() {
		t.Error("dead session on a connected spoke should be marked terminated")
	}
	if sess := s.lookup("off-sess"); sess == nil || sess.terminated() {
		t.Error("session on a disconnected spoke should be kept untouched")
	}
}
