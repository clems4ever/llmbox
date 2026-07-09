package hub

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/testutils"
)

// TestServerWithoutStore checks the server functions with a no-op store.
func TestServerWithoutStore(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "u"}
	s := wireSpoke(New(nil, "https://boxes.example.com", time.Minute, newTestStore(), nil), f)
	sess, err := s.createBox(context.Background(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if s.lookup(sess.Token) == nil {
		t.Error("session not registered with no-op store")
	}
}

// TestCreateBoxPersistsSession checks CreateBox writes the session to the store
// and SubmitCode persists the updated status.
func TestCreateBoxPersistsSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer st.Close()

	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "https://claude.com/cai/oauth/authorize?z=1", SubmitURL: "https://claude.ai/code/s/1"}
	s := wireSpoke(New(nil, "https://boxes.example.com", time.Minute, st, nil), f)

	sess, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "h", Description: "d"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}

	saved, err := st.ListBoxes()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(saved) != 1 || saved[0].Token != sess.Token || saved[0].Status != "pending" {
		t.Fatalf("create not persisted as pending: %+v", saved)
	}
	if saved[0].BoxID != "h" || saved[0].Description != "d" {
		t.Errorf("box ID/description not persisted: %+v", saved[0])
	}

	if err := s.submitCode(context.Background(), sess.Token, "CODE"); err != nil {
		t.Fatalf("SubmitCode: %v", err)
	}
	saved, _ = st.ListBoxes()
	if len(saved) != 1 || saved[0].Status != "ready" || saved[0].SessionURL != "https://claude.ai/code/s/1" {
		t.Errorf("ready status not persisted: %+v", saved)
	}
}

// TestRestoreLoadsWithoutSpokes checks Restore rehydrates every persisted record
// from the store alone — no spoke is contacted and no record is dropped, even
// for a box the spoke no longer reports (the sync pass owns that conclusion).
func TestRestoreLoadsWithoutSpokes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer st.Close()

	// Two saved sessions: one box still exists, one is gone from its spoke.
	if err := st.PutBox(boxRecord{Token: "live", InstanceID: "aaaaaaaaaaaa1111", Status: "pending"}); err != nil {
		t.Fatalf("Save live: %v", err)
	}
	if err := st.PutBox(boxRecord{Token: "dead", InstanceID: "bbbbbbbbbbbb2222", Status: "pending"}); err != nil {
		t.Fatalf("Save dead: %v", err)
	}

	// The spoke only reports the live box, but Restore must not care: it never
	// lists a spoke.
	f := &testutils.FakeMgr{ListResult: []sandbox.Box{{InstanceID: "aaaaaaaaaaaa"}}}
	s := wireSpoke(New(nil, "https://boxes.example.com", time.Minute, st, nil), f)

	n, err := s.Restore()
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if n != 2 {
		t.Errorf("restored %d sessions, want 2", n)
	}
	if s.lookup("live") == nil || s.lookup("dead") == nil {
		t.Error("Restore should rehydrate every record, spoke state notwithstanding")
	}
	if f.ListCalls() != 0 {
		t.Errorf("Restore listed a spoke %d time(s); startup must not contact spokes", f.ListCalls())
	}
}

// TestSyncMarksVanishedBoxTerminated checks the sync pass tombstones a record
// whose box is gone from its reachable spoke (keeping it listed) and refreshes
// the record whose box is still there.
func TestSyncMarksVanishedBoxTerminated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer st.Close()

	if err := st.PutBox(boxRecord{Token: "live", InstanceID: "aaaaaaaaaaaa1111", Status: "pending", BoxID: "live-box"}); err != nil {
		t.Fatalf("Save live: %v", err)
	}
	if err := st.PutBox(boxRecord{Token: "dead", InstanceID: "bbbbbbbbbbbb2222", Status: "pending", BoxID: "dead-box"}); err != nil {
		t.Fatalf("Save dead: %v", err)
	}

	// The spoke reports only the live box (short 12-char ID).
	f := &testutils.FakeMgr{ListResult: []sandbox.Box{{InstanceID: "aaaaaaaaaaaa", Name: "n1", Image: "img:1", State: "running"}}}
	s := wireSpoke(New(nil, "https://boxes.example.com", time.Minute, st, nil), f)
	if _, err := s.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	s.syncSpokes(context.Background())

	// Both records survive; the vanished one is a terminated tombstone.
	if sess := s.lookup("dead"); sess == nil {
		t.Fatal("terminated record should be kept as a tombstone")
	} else if !sess.terminated() {
		t.Error("vanished box's record should be marked terminated")
	}
	if sess := s.lookup("live"); sess == nil || sess.terminated() {
		t.Error("live box's record should stay running")
	}
	// The transition is persisted, and the live record carries observed metadata.
	saved, _ := st.ListBoxes()
	byToken := map[string]boxRecord{}
	for _, ps := range saved {
		byToken[ps.Token] = ps
	}
	if got := string(byToken["dead"].Lifecycle); got != boxStateTerminated {
		t.Errorf("dead record box state = %q, want terminated", got)
	}
	live := byToken["live"]
	if live.ObservedName != "n1" || live.ObservedImage != "img:1" || live.ObservedState != "running" || live.ObservedAt.IsZero() {
		t.Errorf("live record metadata not synced: %+v", live)
	}
}

// TestSyncGraceKeepsFreshRecord checks a record younger than the create grace is
// not tombstoned when it is absent from a (possibly stale) spoke listing.
func TestSyncGraceKeepsFreshRecord(t *testing.T) {
	f := &testutils.FakeMgr{ListResult: nil}
	s := newTestServer(f)
	s.mu.Lock()
	s.byToken["fresh"] = &session{Token: "fresh", ContainerID: "cccccccccccc3333", CreatedAt: time.Now(), SpokeName: testSpoke, Status: "pending"}
	s.mu.Unlock()

	s.syncSpokes(context.Background())

	if sess := s.lookup("fresh"); sess == nil || sess.terminated() {
		t.Error("a just-created record must not be tombstoned by a stale listing")
	}
}

// TestSyncRefreshesObservedMetadata checks the sync pass records a live box's
// observed name, image, backend state, and last-seen on its record, and that
// the listing then renders the backend state.
func TestSyncRefreshesObservedMetadata(t *testing.T) {
	f := &testutils.FakeMgr{ListResult: []sandbox.Box{{InstanceID: "aaaaaaaaaaaa", Name: "n1", Image: "img:2", State: "exited"}}}
	s := newTestServer(f)
	s.mu.Lock()
	s.byToken["tok"] = &session{Token: "tok", ContainerID: "aaaaaaaaaaaa1111", SpokeName: testSpoke, Status: "pending"}
	s.mu.Unlock()

	s.syncSpokes(context.Background())

	ps := s.lookup("tok").persist()
	if ps.ObservedName != "n1" || ps.ObservedImage != "img:2" || ps.ObservedState != "exited" || ps.ObservedAt.IsZero() {
		t.Errorf("observed metadata not recorded: %+v", ps)
	}
	if string(ps.Lifecycle) != boxStateRunning {
		t.Errorf("box state = %q, want running", ps.Lifecycle)
	}
	boxes, _ := s.listBoxes(context.Background())
	if len(boxes) != 1 || boxes[0].State != "exited" || boxes[0].Image != "img:2" {
		t.Errorf("listing should render the observed backend state: %+v", boxes)
	}
}

// TestSyncSkipsUnreachableSpoke checks the sync pass draws no conclusion about a
// record whose spoke is not connected: the record stays running, untouched.
func TestSyncSkipsUnreachableSpoke(t *testing.T) {
	f := &testutils.FakeMgr{} // the connected spoke (testSpoke) reports no boxes
	s := newTestServer(f)
	s.mu.Lock()
	s.byToken["tok"] = &session{Token: "tok", ContainerID: "aaaaaaaaaaaa1111", SpokeName: "offline-spoke", Status: "pending"}
	s.mu.Unlock()

	s.syncSpokes(context.Background())

	if sess := s.lookup("tok"); sess == nil || sess.terminated() {
		t.Error("a record on an unreachable spoke must be left untouched by sync")
	}
}

// TestSyncRevivesReappearedBox checks a tombstone whose box shows up again on
// its spoke is re-marked running (the spoke is the authority on what exists).
func TestSyncRevivesReappearedBox(t *testing.T) {
	f := &testutils.FakeMgr{ListResult: []sandbox.Box{{InstanceID: "dddddddddddd", State: "running"}}}
	s := newTestServer(f)
	s.mu.Lock()
	s.byToken["back"] = &session{Token: "back", ContainerID: "dddddddddddd4444", SpokeName: testSpoke, Status: "pending", BoxState: boxStateTerminated}
	s.mu.Unlock()

	s.syncSpokes(context.Background())

	if sess := s.lookup("back"); sess == nil || sess.terminated() {
		t.Error("a reappeared box's tombstone should be revived to running")
	}
}
