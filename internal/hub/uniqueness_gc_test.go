package hub

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/hub/store"
	"github.com/clems4ever/llmbox/internal/shared/cluster"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/testutils"
)

// seedSession injects a ready session for boxID on spoke directly into the
// registry and the store, bypassing createBox so a test can place a box on a
// spoke that is not connected (e.g. an offline-but-enrolled or departed spoke).
func seedSession(t *testing.T, s *Server, token, boxID, spoke string) {
	t.Helper()
	sess := &session{
		BoxID:      boxID,
		SpokeName:  spoke,
		Generation: "container-" + token,
		CreatedAt:  time.Now(),
		Phase:      "ready",
	}
	s.regSession(token, sess)
	if err := s.store.PutBox(sess.persist()); err != nil {
		t.Fatalf("seed session %q: %v", boxID, err)
	}
}

// mustSaveProxy stores a proxy record on the given spoke for a test.
func mustSaveProxy(t *testing.T, st Store, slug, boxID, spoke string) {
	t.Helper()
	if err := st.SaveProxy(store.ProxyRecord{
		Slug:       slug,
		BoxID:      boxID,
		InstanceID: "container-" + boxID,
		Port:       8000,
		Spoke:      spoke,
	}); err != nil {
		t.Fatalf("save proxy %q: %v", slug, err)
	}
}

// TestCreateBoxRejectsDuplicateBoxIDSameSpoke checks the hub rejects a second box
// reusing a box ID already taken on the same (local) spoke, independently of the
// per-spoke docker check.
func TestCreateBoxRejectsDuplicateBoxIDSameSpoke(t *testing.T) {
	s, _ := newProxyServer(t, &testutils.FakeMgr{CreateID: "c1"}, nil)
	if _, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "dup"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "dup"}); err == nil {
		t.Fatal("second create with the same box ID succeeded, want rejection")
	}
}

// TestCreateBoxRejectsDuplicateBoxIDAcrossSpokes checks box IDs are unique
// hub-wide: a box ID taken on one spoke cannot be reused on another (the per-spoke
// docker check would never catch this, since each spoke only sees its own boxes).
func TestCreateBoxRejectsDuplicateBoxIDAcrossSpokes(t *testing.T) {
	s, _ := newProxyServer(t, &testutils.FakeMgr{}, nil)
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{
		"one": &testutils.FakeMgr{CreateID: "c-one"},
		"two": &testutils.FakeMgr{CreateID: "c-two"},
	}})

	if _, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "dup", SpokeName: "one"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "dup", SpokeName: "two"})
	if err == nil {
		t.Fatal("duplicate box ID on another spoke succeeded, want hub-wide rejection")
	}
}

// TestCreateBoxConcurrentSameBoxIDOnlyOneWins checks the box-ID reservation is
// atomic: many concurrent creates of the same ID yield exactly one success, with
// no conflicting duplicate sessions. Run with -race to exercise the locking.
func TestCreateBoxConcurrentSameBoxIDOnlyOneWins(t *testing.T) {
	s, _ := newProxyServer(t, &testutils.FakeMgr{CreateID: "c1"}, nil)
	const n = 24
	var wg sync.WaitGroup
	var success atomic.Int64
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "race"}); err == nil {
				success.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := success.Load(); got != 1 {
		t.Fatalf("concurrent creates of one box ID: %d succeeded, want exactly 1", got)
	}
}

// TestLookupByBoxIDPrefersReachableSpoke checks that, when a duplicate session
// lingers across spokes, lookup deterministically prefers the one on a reachable
// spoke — even when the unreachable one is newer — and never resolves at random.
func TestLookupByBoxIDPrefersReachableSpoke(t *testing.T) {
	hub := &testutils.FakeHub{Connected: map[string]boxManager{"remote1": &testutils.FakeMgr{}}}
	s, _ := newProxyServer(t, &testutils.FakeMgr{}, nil)
	s.SetHub(hub)

	live := &session{Token: "tok-live", BoxID: "dup", SpokeName: "remote1", Generation: "cl", CreatedAt: time.Unix(100, 0), Phase: "ready"}
	// dead is on a disconnected spoke and is NEWER — reachability must still win.
	dead := &session{Token: "tok-dead", BoxID: "dup", SpokeName: "ghost", Generation: "cd", CreatedAt: time.Unix(200, 0), Phase: "ready"}
	s.regSession("tok-live", live)
	s.regSession("tok-dead", dead)

	for i := 0; i < 50; i++ {
		got := s.lookupByBoxID("dup")
		if got != live {
			t.Fatalf("iteration %d: resolved to spoke %q, want reachable spoke remote1", i, got.SpokeName)
		}
	}
}

// TestPruneDepartedSpokesRemovesStaleObjects checks the purge drops sessions and
// proxies pinned to a spoke that has been de-enrolled, while keeping those on a
// connected spoke and on an enrolled-but-offline spoke.
func TestPruneDepartedSpokesRemovesStaleObjects(t *testing.T) {
	s, st := newProxyServer(t, &testutils.FakeMgr{}, nil)
	// remote1 is enrolled and connected; kept1 is enrolled but offline; ghost is
	// neither (departed).
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{"remote1": &testutils.FakeMgr{}}})
	if err := st.PutSpoke("remote1", cluster.SpokeRecord{Name: "remote1", EnrolledAt: time.Now()}); err != nil {
		t.Fatalf("PutSpoke: %v", err)
	}
	if err := st.PutSpoke("kept1", cluster.SpokeRecord{Name: "kept1", EnrolledAt: time.Now()}); err != nil {
		t.Fatalf("PutSpoke: %v", err)
	}

	seedSession(t, s, "tok-kept", "box-kept", "kept1")
	seedSession(t, s, "tok-remote", "box-remote", "remote1")
	seedSession(t, s, "tok-ghost", "box-ghost", "ghost") // departed: not enrolled
	mustSaveProxy(t, st, "slug-kept", "box-kept", "kept1")
	mustSaveProxy(t, st, "slug-remote", "box-remote", "remote1")
	mustSaveProxy(t, st, "slug-ghost", "box-ghost", "ghost") // departed

	purged, err := s.PruneDepartedSpokes()
	if err != nil {
		t.Fatalf("PruneDepartedSpokes: %v", err)
	}
	if len(purged) != 1 || purged[0] != "box-ghost" {
		t.Fatalf("purged = %v, want [box-ghost]", purged)
	}

	// Sessions: ghost gone; enrolled-offline and enrolled-remote kept.
	if s.lookupByBoxID("box-ghost") != nil {
		t.Error("departed spoke's session was not purged")
	}
	if s.lookupByBoxID("box-kept") == nil {
		t.Error("enrolled-offline session was wrongly purged")
	}
	if s.lookupByBoxID("box-remote") == nil {
		t.Error("enrolled remote session was wrongly purged")
	}

	// Store: ghost session token removed.
	all, err := st.ListBoxes()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	for _, ps := range all {
		if ps.Token == "tok-ghost" {
			t.Error("departed spoke's session still in the store")
		}
	}

	// Proxies: ghost deleted; the others kept.
	for slug, wantPresent := range map[string]bool{"slug-ghost": false, "slug-kept": true, "slug-remote": true} {
		_, ok, err := st.GetProxy(slug)
		if err != nil {
			t.Fatalf("GetProxy %q: %v", slug, err)
		}
		if ok != wantPresent {
			t.Errorf("proxy %q present=%v, want %v", slug, ok, wantPresent)
		}
	}
}

// TestPruneDepartedSpokesKeepsOfflineEnrolledSpoke checks an enrolled spoke that
// is merely offline (not currently connected) is NOT treated as departed — its
// box may still be alive and the spoke may reconnect, so its objects are kept.
func TestPruneDepartedSpokesKeepsOfflineEnrolledSpoke(t *testing.T) {
	s, st := newProxyServer(t, &testutils.FakeMgr{}, nil)
	// Clustering enabled, but offline1 is enrolled in the store with no live hub
	// connection => offline, not departed.
	s.SetHub(&testutils.FakeHub{Connected: map[string]boxManager{}})
	if err := st.PutSpoke("offline1", cluster.SpokeRecord{Name: "offline1", EnrolledAt: time.Now()}); err != nil {
		t.Fatalf("PutSpoke: %v", err)
	}
	seedSession(t, s, "tok-off", "box-off", "offline1")
	mustSaveProxy(t, st, "slug-off", "box-off", "offline1")

	purged, err := s.PruneDepartedSpokes()
	if err != nil {
		t.Fatalf("PruneDepartedSpokes: %v", err)
	}
	if len(purged) != 0 {
		t.Fatalf("purged %v, want none (an offline but enrolled spoke must be kept)", purged)
	}
	if s.lookupByBoxID("box-off") == nil {
		t.Error("offline enrolled spoke's session was wrongly purged")
	}
	if _, ok, _ := st.GetProxy("slug-off"); !ok {
		t.Error("offline enrolled spoke's proxy was wrongly deleted")
	}
}
