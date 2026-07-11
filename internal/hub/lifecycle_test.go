package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/testutils"
)

// TestDestroyTerminatedRecordSkipsSpoke checks removing a terminated record (a
// tombstone) needs no spoke at all — it works while the spoke is offline — and
// does not re-run the destroy hooks, which already fired when the sync pass
// marked the box terminated.
func TestDestroyTerminatedRecordSkipsSpoke(t *testing.T) {
	fh := &fakeHooks{}
	f := &testutils.FakeMgr{}
	s := wireSpoke(New(fh, "https://boxes.example.com", newTestStore(), nil), f)
	s.regSession("tok", &session{
		BoxID: "dead-box", Generation: "cccccccccccc1111", SpokeName: testSpoke,
		Phase: "ready", BoxState: boxStateTerminated, HookState: map[string]string{"hook": "state"},
	})
	// The spoke goes offline: a tombstone must still be removable.
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{}})

	if err := s.destroyBox(context.Background(), "dead-box"); err != nil {
		t.Fatalf("destroyBox: %v", err)
	}
	if s.lookupTok("tok") != nil {
		t.Error("tombstone record should be forgotten")
	}
	if len(f.Destroyed) != 0 {
		t.Errorf("removing a tombstone contacted the spoke: %v", f.Destroyed)
	}
	if len(fh.destroyed) != 0 {
		t.Error("destroy hooks must not re-run for a tombstone (they ran at termination)")
	}
}

// TestDestroyUnreachableSpokeRefused checks destroying a live box whose spoke is
// offline is refused with a clear error and the record is kept: the hub does not
// guess about a box it cannot observe.
func TestDestroyUnreachableSpokeRefused(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789"}
	s := newTestServer(f)
	sess, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "b1"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}

	// The spoke disconnects before the destroy.
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{}})

	err = s.destroyBox(context.Background(), "b1")
	if err == nil {
		t.Fatal("destroying a box on an offline spoke should be refused")
	}
	if !strings.Contains(err.Error(), "not connected") {
		t.Errorf("error should explain the spoke is offline, got: %v", err)
	}
	if s.lookupByBoxID(sess.BoxID) == nil {
		t.Error("the refused destroy must keep the box record")
	}
}

// TestCreateBoxReplacesTerminatedTombstone checks a terminated record does not
// block reuse of its box ID, and creating the new box clears the tombstone so
// one name never lists two boxes.
func TestCreateBoxReplacesTerminatedTombstone(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789"}
	s := newTestServer(f)
	s.regSession("old", &session{
		BoxID: "web", Generation: "000000000000aaaa", SpokeName: testSpoke,
		Phase: "ready", BoxState: boxStateTerminated,
	})

	sess, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "web"})
	if err != nil {
		t.Fatalf("CreateBox reusing a tombstone's box ID: %v", err)
	}
	if s.lookupTok("old") != nil {
		t.Error("the tombstone should be replaced by the new box's record")
	}
	if got := s.lookupByBoxID("web"); got == nil || got.Token != sess.Token {
		t.Errorf("box ID should resolve to the new box, got %+v", got)
	}
}

// TestCreateBoxSyncsObservedMetadata checks a create folds the spoke's inventory
// in right away, so the new record carries the observed name/image/state without
// waiting for the next periodic sync.
func TestCreateBoxSyncsObservedMetadata(t *testing.T) {
	f := &testutils.FakeMgr{
		CreateID: "abcdef0123456789",
		ListResult: []sandbox.Box{{
			InstanceID: "abcdef0123456789", Name: "cname", Image: "img:9", State: "running",
		}},
	}
	s := newTestServer(f)

	sess, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "m1"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	ps := sess.persist()
	if ps.ObservedName != "cname" || ps.ObservedImage != "img:9" || ps.ObservedState != "running" {
		t.Errorf("create did not sync the observed metadata: %+v", ps)
	}
	if string(ps.Lifecycle) != boxStateRunning || ps.ObservedAt.IsZero() {
		t.Errorf("new record should be running with a last-seen time: %+v", ps)
	}
}

// TestLookupByBoxIDPrefersAliveOverTerminated checks a duplicate box ID resolves
// to the live box even when the tombstone is newer and on a reachable spoke —
// alive outranks everything else in the ordering.
func TestLookupByBoxIDPrefersAliveOverTerminated(t *testing.T) {
	f := &testutils.FakeMgr{}
	s := newTestServer(f)
	// alive is OLDER and on an unreachable spoke; the tombstone is newer and on
	// the connected spoke. Alive must still win.
	alive := &session{Token: "tok-alive", BoxID: "dup", SpokeName: "ghost", Generation: "ca", CreatedAt: time.Unix(100, 0), Phase: "ready"}
	tomb := &session{Token: "tok-tomb", BoxID: "dup", SpokeName: testSpoke, Generation: "ct", CreatedAt: time.Unix(200, 0), Phase: "ready", BoxState: boxStateTerminated}
	s.regSession("tok-alive", alive)
	s.regSession("tok-tomb", tomb)

	for i := 0; i < 50; i++ {
		if got := s.lookupByBoxID("dup"); got != alive {
			t.Fatalf("iteration %d: resolved to %q, want the live session", i, got.Token)
		}
	}
}
