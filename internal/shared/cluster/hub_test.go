package cluster

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// waitForSpoke polls the hub until a spoke with name connects, or fails the test.
func waitForSpoke(t *testing.T, hub *Hub, name string) BoxManager {
	t.Helper()
	for i := 0; i < 200; i++ {
		if bm, ok := hub.Spoke(name); ok {
			return bm
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("spoke %q never connected", name)
	return nil
}

// TestHubEnrollAndRoute is a package test.
func TestHubEnrollAndRoute(t *testing.T) {
	store := newMemStore()
	now := time.Unix(10_000, 0)
	tok, err := CreateJoinToken(store, "edge", time.Hour, now)
	if err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}

	hubCtx, hubCancel := context.WithCancel(context.Background())
	defer hubCancel()
	hub := NewHub(hubCtx, store, func() time.Time { return now }, nil)
	srv := httptest.NewServer(http.HandlerFunc(hub.ConnectHandler))
	defer srv.Close()
	url := "ws" + srv.URL[len("http"):] + "/"

	fake := &fakeManager{boxes: []sandbox.Box{{InstanceID: "c1", BoxID: "b1"}}}
	spokeCtx, spokeCancel := context.WithCancel(context.Background())
	defer spokeCancel()
	saved := make(chan Credentials, 1)
	go func() {
		_ = Run(spokeCtx, WebSocketDialer(url), fake, tok, nil, func(c Credentials) error {
			saved <- c
			return nil
		})
	}()

	bm := waitForSpoke(t, hub, "edge")
	boxes, err := bm.List(context.Background())
	if err != nil || len(boxes) != 1 || boxes[0].BoxID != "b1" {
		t.Fatalf("List through hub = (%v,%v)", boxes, err)
	}
	if names := hub.Spokes(); len(names) != 1 {
		t.Errorf("Spokes() = %v, want one entry", names)
	}

	select {
	case c := <-saved:
		if c.Name != "edge" || len(c.Credential) != 64 {
			t.Errorf("saved credentials = %+v", c)
		}
	case <-time.After(time.Second):
		t.Error("spoke never saved its credentials")
	}
}

// TestHubRejectsBadEnrollment is a package test.
func TestHubRejectsBadEnrollment(t *testing.T) {
	hubCtx, hubCancel := context.WithCancel(context.Background())
	defer hubCancel()
	hub := NewHub(hubCtx, newMemStore(), nil, nil)
	srv := httptest.NewServer(http.HandlerFunc(hub.ConnectHandler))
	defer srv.Close()
	url := "ws" + srv.URL[len("http"):] + "/"

	err := Run(context.Background(), WebSocketDialer(url), &fakeManager{}, "bad-token", nil, nil)
	if !errors.Is(err, ErrEnrollRejected) {
		t.Fatalf("Run err = %v, want ErrEnrollRejected", err)
	}
	if _, ok := hub.Spoke("edge"); ok {
		t.Error("rejected spoke should not be registered")
	}
}

// TestHubDisconnectClosesConnection checks Disconnect force-closes a connected
// spoke's link, and is a no-op for an unknown spoke.
func TestHubDisconnectClosesConnection(t *testing.T) {
	hub := NewHub(context.Background(), newMemStore(), nil, nil)

	hubEnd, _ := newPipe()
	rs := newRemoteSpoke("edge", hubEnd)
	hub.register("edge", rs)

	hub.Disconnect("ghost") // unknown: must not panic or affect edge
	if _, ok := hub.Spoke("edge"); !ok {
		t.Fatal("disconnecting an unknown spoke evicted edge")
	}

	hub.Disconnect("edge")
	select {
	case <-rs.Done():
	case <-time.After(time.Second):
		t.Fatal("Disconnect did not close the connection")
	}
}

// TestHubReconnectSupersedes is a package test.
func TestHubReconnectSupersedes(t *testing.T) {
	hub := NewHub(context.Background(), newMemStore(), nil, nil)

	hubEnd1, _ := newPipe()
	rs1 := newRemoteSpoke("x", hubEnd1)
	hub.register("x", rs1)

	hubEnd2, _ := newPipe()
	rs2 := newRemoteSpoke("x", hubEnd2)
	hub.register("x", rs2) // supersedes rs1

	// rs1's connection is closed by the supersede.
	select {
	case <-rs1.Done():
	case <-time.After(time.Second):
		t.Fatal("superseded connection was not closed")
	}

	if bm, ok := hub.Spoke("x"); !ok || bm != rs2 {
		t.Fatalf("Spoke(x) = (%v,%v), want rs2", bm, ok)
	}

	// The old connection tearing down must not evict the newer one.
	hub.unregister("x", rs1)
	if _, ok := hub.Spoke("x"); !ok {
		t.Error("newer connection was wrongly evicted by the old one's teardown")
	}
}
