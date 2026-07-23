package cluster

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// startSpoke wires a remoteSpoke (hub side) to a serve loop (spoke side) backed
// by mgr over an in-memory pipe, and returns the remoteSpoke. It cancels the
// serve loop on test cleanup.
func startSpoke(t *testing.T, mgr BoxManager) *remoteSpoke {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	hubEnd, spokeEnd := newPipe()
	go func() { _ = serve(ctx, spokeEnd, mgr, nil) }()
	rs := newRemoteSpoke("s", hubEnd, nil)
	t.Cleanup(func() { _ = rs.Close() })
	return rs
}

// TestRemoteSpokeRoundTrip is a package test.
func TestRemoteSpokeRoundTrip(t *testing.T) {
	fake := &fakeManager{
		createID:   "cid",
		boxes:      []sandbox.Box{{InstanceID: "c1", BoxID: "b1"}},
		execResult: sandbox.ExecResult{Stdout: "out", Stderr: "err", ExitCode: 3},
	}
	rs := startSpoke(t, fake)
	ctx := context.Background()

	created, err := rs.Create(ctx, sandbox.CreateOptions{BoxID: "b1", SpokeName: "s"})
	if err != nil || created.InstanceID != "cid" {
		t.Fatalf("Create = (%+v,%v)", created, err)
	}
	if fake.lastCreate.BoxID != "b1" {
		t.Errorf("spoke saw create box id %q", fake.lastCreate.BoxID)
	}

	boxes, err := rs.List(ctx)
	if err != nil || !reflect.DeepEqual(boxes, fake.boxes) {
		t.Fatalf("List = (%v,%v)", boxes, err)
	}

	if err := rs.Destroy(ctx, "b1"); err != nil || fake.lastDestroy != "b1" {
		t.Fatalf("Destroy err=%v lastDestroy=%q", err, fake.lastDestroy)
	}

	if err := rs.Pause(ctx, "b1"); err != nil || fake.lastPause != "b1" {
		t.Fatalf("Pause err=%v lastPause=%q", err, fake.lastPause)
	}

	if err := rs.Resume(ctx, "b1"); err != nil || fake.lastResume != "b1" {
		t.Fatalf("Resume = %v lastResume=%q", err, fake.lastResume)
	}

	res, err := rs.Exec(ctx, "b1", []string{"/bin/sh", "-c", "echo hi"})
	if err != nil || !reflect.DeepEqual(res, fake.execResult) {
		t.Fatalf("Exec = (%+v,%v)", res, err)
	}
	if !reflect.DeepEqual(fake.lastExec.cmd, []string{"/bin/sh", "-c", "echo hi"}) {
		t.Errorf("spoke saw exec cmd %v", fake.lastExec.cmd)
	}

	policy := sandbox.NetworkPolicy{Enabled: true, Rules: []sandbox.DomainRule{{Pattern: "github.com", TTLSeconds: 30}}}
	if err := rs.SetNetworkPolicy(ctx, "b1", policy); err != nil {
		t.Fatalf("SetNetworkPolicy: %v", err)
	}
	if fake.lastPolicy.boxID != "b1" || !reflect.DeepEqual(fake.lastPolicy.policy, policy) {
		t.Errorf("spoke saw policy box=%q %+v", fake.lastPolicy.boxID, fake.lastPolicy.policy)
	}
}

// TestRemoteSpokeVerbError is a package test.
func TestRemoteSpokeVerbError(t *testing.T) {
	rs := startSpoke(t, &fakeManager{err: errors.New("boom")})
	_, err := rs.List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("List err = %v, want one containing boom", err)
	}
}

// TestRemoteSpokeDisconnect is a package test.
func TestRemoteSpokeDisconnect(t *testing.T) {
	hubEnd, spokeEnd := newPipe()
	rs := newRemoteSpoke("s", hubEnd, nil)

	// A call in flight (no serve loop answers) fails once the connection drops.
	errc := make(chan error, 1)
	go func() {
		_, err := rs.Create(context.Background(), sandbox.CreateOptions{})
		errc <- err
	}()
	// Let the call register, then drop the connection.
	time.Sleep(20 * time.Millisecond)
	_ = spokeEnd.Close()

	select {
	case err := <-errc:
		if !errors.Is(err, errSpokeDisconnected) {
			t.Fatalf("in-flight call err = %v, want errSpokeDisconnected", err)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight call did not return after disconnect")
	}

	<-rs.Done()
	// A call after disconnect fails immediately.
	if _, err := rs.List(context.Background()); err == nil {
		t.Fatal("call after disconnect should fail")
	}
}

// TestRemoteSpokeContextCancel is a package test.
func TestRemoteSpokeContextCancel(t *testing.T) {
	hubEnd, _ := newPipe()
	rs := newRemoteSpoke("s", hubEnd, nil)
	t.Cleanup(func() { _ = rs.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := rs.List(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestDispatchHandlesVerbs is a package test.
func TestDispatchHandlesVerbs(t *testing.T) {
	fake := &fakeManager{
		createID:   "cid",
		boxes:      []sandbox.Box{{InstanceID: "c1"}},
		execResult: sandbox.ExecResult{Stdout: "o", ExitCode: 1},
	}
	ctx := context.Background()

	mustReq := func(method string, v any) frame {
		p, err := encodePayload(v)
		if err != nil {
			t.Fatalf("encode %s: %v", method, err)
		}
		return frame{Type: frameReq, Method: method, Payload: p}
	}

	// Destroy returns a nil payload.
	p, err := dispatch(ctx, fake, mustReq(methodDestroy, destroyReq{IDOrName: "b1"}))
	if err != nil || p != nil {
		t.Fatalf("destroy dispatch = (%s,%v)", p, err)
	}
	if fake.lastDestroy != "b1" {
		t.Errorf("destroy reached fake with %q", fake.lastDestroy)
	}

	// Pause returns a nil payload.
	p, err = dispatch(ctx, fake, mustReq(methodPause, pauseReq{IDOrName: "b1"}))
	if err != nil || p != nil {
		t.Fatalf("pause dispatch = (%s,%v)", p, err)
	}
	if fake.lastPause != "b1" {
		t.Errorf("pause reached fake with %q", fake.lastPause)
	}

	// Resume returns a nil payload.
	p, err = dispatch(ctx, fake, mustReq(methodResume, resumeReq{IDOrName: "b1"}))
	if err != nil || p != nil {
		t.Fatalf("resume dispatch = (%s,%v)", p, err)
	}
	if fake.lastResume != "b1" {
		t.Errorf("resume reached fake with %q", fake.lastResume)
	}

	// List returns boxes.
	p, err = dispatch(ctx, fake, mustReq(methodList, struct{}{}))
	if err != nil {
		t.Fatalf("list dispatch: %v", err)
	}
	var lr listResp
	if err := decodePayload(p, &lr); err != nil || len(lr.Boxes) != 1 {
		t.Fatalf("list resp = %+v (%v)", lr, err)
	}
}

// TestDispatchUnknownMethod is a package test.
func TestDispatchUnknownMethod(t *testing.T) {
	_, err := dispatch(context.Background(), &fakeManager{}, frame{Type: frameReq, Method: "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown method") {
		t.Fatalf("err = %v, want unknown method", err)
	}
}

// TestDispatchBadPayload is a package test.
func TestDispatchBadPayload(t *testing.T) {
	req := frame{Type: frameReq, Method: methodCreate, Payload: []byte("{not json")}
	if _, err := dispatch(context.Background(), &fakeManager{}, req); err == nil {
		t.Fatal("expected error for malformed payload")
	}
}

// TestDispatchRejectsInvalidCreate is a package test: a create whose box id is
// malformed is rejected at the wire boundary before it reaches the manager.
func TestDispatchRejectsInvalidCreate(t *testing.T) {
	p, err := encodePayload(createReq{Opts: sandbox.CreateOptions{BoxID: "Bad_ID"}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	fake := &fakeManager{}
	req := frame{Type: frameReq, Method: methodCreate, Payload: p}
	if _, err := dispatch(context.Background(), fake, req); err == nil || !strings.Contains(err.Error(), "invalid box id") {
		t.Fatalf("err = %v, want invalid box id", err)
	}
	if fake.lastCreate.BoxID != "" {
		t.Error("manager.Create was called despite the malformed box id")
	}
}
