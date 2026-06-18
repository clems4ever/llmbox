package server

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/docker"
)

// TestBoltStoreRoundTrip checks a session survives save, reload, and close.
func TestBoltStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	ps := persistedSession{
		Token:        "tok1",
		BoxID:        "abcdef0123456789",
		AuthorizeURL: "https://claude.com/cai/oauth/authorize?x=1",
		CreatedAt:    time.Unix(1700000000, 0).UTC(),
		HookState:    map[string]string{"granular-hook": "subj-1"},
		Hostname:     "web-box",
		Description:  "front-end",
		Status:       "pending",
	}
	if err := st.Save(ps); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen to prove data is on disk, not just in memory.
	st2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()

	got, err := st2.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 session, got %d", len(got))
	}
	if !reflect.DeepEqual(got[0], ps) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got[0], ps)
	}
}

// TestBoltStoreDelete checks a deleted session is gone and deleting a missing
// token is a harmless no-op.
func TestBoltStoreDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer st.Close()

	if err := st.Save(persistedSession{Token: "a", BoxID: "id-a", Status: "pending"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := st.Delete("a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := st.Delete("missing"); err != nil {
		t.Errorf("Delete of missing token should be a no-op, got %v", err)
	}
	got, err := st.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 sessions after delete, got %d", len(got))
	}
}

// TestServerWithoutStore checks the server functions with a no-op store.
func TestServerWithoutStore(t *testing.T) {
	f := &fakeMgr{createID: "abcdef0123456789", createURL: "u"}
	s := New(f, nil, "https://boxes.example.com", time.Minute, noopStore{})
	sess, err := s.CreateBox(context.Background(), docker.CreateOptions{})
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

	f := &fakeMgr{createID: "abcdef0123456789", createURL: "https://claude.com/cai/oauth/authorize?z=1", submitURL: "https://claude.ai/code/s/1"}
	s := New(f, nil, "https://boxes.example.com", time.Minute, st)

	sess, err := s.CreateBox(context.Background(), docker.CreateOptions{Hostname: "h", Description: "d"})
	if err != nil {
		t.Fatalf("CreateBox: %v", err)
	}

	saved, err := st.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(saved) != 1 || saved[0].Token != sess.Token || saved[0].Status != "pending" {
		t.Fatalf("create not persisted as pending: %+v", saved)
	}
	if saved[0].Hostname != "h" || saved[0].Description != "d" {
		t.Errorf("hostname/description not persisted: %+v", saved[0])
	}

	if err := s.SubmitCode(context.Background(), sess.Token, "CODE"); err != nil {
		t.Fatalf("SubmitCode: %v", err)
	}
	saved, _ = st.LoadAll()
	if len(saved) != 1 || saved[0].Status != "ready" || saved[0].SessionURL != "https://claude.ai/code/s/1" {
		t.Errorf("ready status not persisted: %+v", saved)
	}
}

// TestRestoreLoadsAndReconciles checks Restore rehydrates sessions whose box is
// still alive and drops (and deletes) sessions whose box is gone.
func TestRestoreLoadsAndReconciles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer st.Close()

	// Two saved sessions: one box still exists, one is gone.
	if err := st.Save(persistedSession{Token: "live", BoxID: "aaaaaaaaaaaa1111", Status: "pending"}); err != nil {
		t.Fatalf("Save live: %v", err)
	}
	if err := st.Save(persistedSession{Token: "dead", BoxID: "bbbbbbbbbbbb2222", Status: "pending"}); err != nil {
		t.Fatalf("Save dead: %v", err)
	}

	// Docker only reports the live box (short 12-char ID).
	f := &fakeMgr{listResult: []docker.Box{{ID: "aaaaaaaaaaaa"}}}
	s := New(f, nil, "https://boxes.example.com", time.Minute, st)

	n, err := s.Restore(context.Background())
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if n != 1 {
		t.Errorf("restored %d sessions, want 1", n)
	}
	if s.lookup("live") == nil {
		t.Error("live session not restored")
	}
	if s.lookup("dead") != nil {
		t.Error("dead session should not be restored")
	}
	// The dead session should also be removed from the store.
	saved, _ := st.LoadAll()
	if len(saved) != 1 || saved[0].Token != "live" {
		t.Errorf("dead session not pruned from store: %+v", saved)
	}
}
