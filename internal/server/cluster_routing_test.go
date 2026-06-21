package server

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/cluster"
	"github.com/clems4ever/llmbox/internal/docker"
)

// fakeHub is a stand-in for *cluster.Hub: a fixed set of connected spokes.
type fakeHub struct {
	spokes       map[string]boxManager
	disconnected []string // names passed to Disconnect, for assertions
}

// Spoke returns the connected spoke with the given name.
func (h *fakeHub) Spoke(name string) (boxManager, bool) {
	bm, ok := h.spokes[name]
	return bm, ok
}

// Spokes returns the connected spokes.
func (h *fakeHub) Spokes() map[string]boxManager { return h.spokes }

// ConnectHandler is a no-op; tests inject spokes directly.
func (h *fakeHub) ConnectHandler(http.ResponseWriter, *http.Request) {}

// Disconnect records the name so tests can assert a spoke was kicked.
func (h *fakeHub) Disconnect(name string) { h.disconnected = append(h.disconnected, name) }

// TestCreateBoxRoutesToSpoke checks a box with a spoke name is created on that
// connected remote spoke, not the local one, and the session records the spoke.
func TestCreateBoxRoutesToSpoke(t *testing.T) {
	local := &fakeMgr{createID: "local-id", createURL: "https://local"}
	edge := &fakeMgr{createID: "edge-id", createURL: "https://edge"}
	s := newTestServer(local)
	s.SetHub(&fakeHub{spokes: map[string]boxManager{"edge": edge}})

	sess, err := s.CreateBox(context.Background(), docker.CreateOptions{BoxID: "b1", SpokeName: "edge"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if sess.ContainerID != "edge-id" {
		t.Errorf("box created on wrong spoke: container = %q", sess.ContainerID)
	}
	if sess.SpokeName != "edge" {
		t.Errorf("session spoke = %q, want edge", sess.SpokeName)
	}
	if edge.gotOpts.BoxID != "b1" {
		t.Errorf("edge spoke did not receive the create (%+v)", edge.gotOpts)
	}
	if local.gotOpts.BoxID != "" {
		t.Errorf("local spoke wrongly received the create (%+v)", local.gotOpts)
	}
}

// TestCreateBoxUnknownSpoke checks creating a box on an unconnected spoke errors.
func TestCreateBoxUnknownSpoke(t *testing.T) {
	s := newTestServer(&fakeMgr{})
	s.SetHub(&fakeHub{spokes: map[string]boxManager{}})
	if _, err := s.CreateBox(context.Background(), docker.CreateOptions{BoxID: "b1", SpokeName: "ghost"}); err == nil {
		t.Fatal("expected error for unconnected spoke")
	}
}

// TestCreateBoxDefaultsToLocalSpoke checks a box with no spoke runs on the local spoke.
func TestCreateBoxDefaultsToLocalSpoke(t *testing.T) {
	local := &fakeMgr{createID: "local-id"}
	s := newTestServer(local)
	sess, err := s.CreateBox(context.Background(), docker.CreateOptions{BoxID: "b1"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if sess.SpokeName != localSpokeName {
		t.Errorf("session spoke = %q, want %q", sess.SpokeName, localSpokeName)
	}
	if local.gotOpts.BoxID != "b1" {
		t.Error("local spoke did not receive the create")
	}
}

// TestListFansOutAcrossSpokes checks list aggregates boxes from every spoke, each tagged.
func TestListFansOutAcrossSpokes(t *testing.T) {
	local := &fakeMgr{listResult: []docker.Box{{ContainerID: "L", BoxID: "lbox"}}}
	edge := &fakeMgr{listResult: []docker.Box{{ContainerID: "E", BoxID: "ebox"}}}
	s := newTestServer(local)
	s.SetHub(&fakeHub{spokes: map[string]boxManager{"edge": edge}})

	boxes, err := s.ListBoxes(context.Background())
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
	local := &fakeMgr{reaped: []string{"l1"}}
	edge := &fakeMgr{reaped: []string{"e1"}}
	s := newTestServer(local)
	s.SetHub(&fakeHub{spokes: map[string]boxManager{"edge": edge}})

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
	local := &fakeMgr{}
	edge := &fakeMgr{createID: "edge-id"}
	s := newTestServer(local)
	s.SetHub(&fakeHub{spokes: map[string]boxManager{"edge": edge}})

	sess, err := s.CreateBox(context.Background(), docker.CreateOptions{BoxID: "b1", SpokeName: "edge"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if err := s.DestroyBox(context.Background(), sess.ContainerID); err != nil {
		t.Fatalf("DestroyBox: %v", err)
	}
	if len(edge.destroyed) != 1 || edge.destroyed[0] != "edge-id" {
		t.Errorf("edge.destroyed = %v, want [edge-id]", edge.destroyed)
	}
	if len(local.destroyed) != 0 {
		t.Errorf("local.destroyed = %v, want none", local.destroyed)
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

	s := New(&fakeMgr{}, nil, "https://h", time.Minute, store, nil)
	s.SetHub(&fakeHub{spokes: map[string]boxManager{"edge": &fakeMgr{}}}) // only edge connected

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
	s := newTestServer(&fakeMgr{})
	got, err := s.SpokeStatuses(context.Background())
	if err != nil {
		t.Fatalf("SpokeStatuses: %v", err)
	}
	if len(got) != 1 || got[0].Name != localSpokeName || !got[0].Connected {
		t.Fatalf("SpokeStatuses without hub = %+v, want only local", got)
	}
}

// TestListSpokesTool checks the list_spokes MCP handler returns the spoke statuses.
func TestListSpokesTool(t *testing.T) {
	s := newTestServer(&fakeMgr{})
	s.SetHub(&fakeHub{spokes: map[string]boxManager{}})
	_, out, err := s.toolListSpokes(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatalf("toolListSpokes: %v", err)
	}
	if len(out.Spokes) != 1 || out.Spokes[0].Name != localSpokeName {
		t.Fatalf("list_spokes output = %+v, want the local spoke", out.Spokes)
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
	local := &fakeMgr{listResult: nil}
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
		t.Error("session on a disconnected spoke should have been kept")
	}
}
