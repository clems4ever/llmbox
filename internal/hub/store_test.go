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

	saved, err := st.LoadAll()
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
	if err := st.Save(persistedSession{Token: "live", ContainerID: "aaaaaaaaaaaa1111", Status: "pending"}); err != nil {
		t.Fatalf("Save live: %v", err)
	}
	if err := st.Save(persistedSession{Token: "dead", ContainerID: "bbbbbbbbbbbb2222", Status: "pending"}); err != nil {
		t.Fatalf("Save dead: %v", err)
	}

	// Docker only reports the live box (short 12-char ID).
	f := &testutils.FakeMgr{ListResult: []sandbox.Box{{InstanceID: "aaaaaaaaaaaa"}}}
	s := wireSpoke(New(nil, "https://boxes.example.com", time.Minute, st, nil), f)

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
