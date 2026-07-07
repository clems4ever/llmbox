package cluster

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/sandbox"
)

// startSpoke wires a remoteSpoke (hub side) to a serve loop (spoke side) backed
// by mgr over an in-memory pipe, and returns the remoteSpoke. It cancels the
// serve loop on test cleanup.
func startSpoke(t *testing.T, mgr BoxManager) *remoteSpoke {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	hubEnd, spokeEnd := newPipe()
	go func() { _ = serve(ctx, spokeEnd, mgr, ValidationPolicy{}) }()
	rs := newRemoteSpoke("s", hubEnd)
	t.Cleanup(func() { _ = rs.Close() })
	return rs
}

// TestRemoteSpokeRoundTrip is a package test.
func TestRemoteSpokeRoundTrip(t *testing.T) {
	fake := &fakeManager{
		createID:   "cid",
		createURL:  "https://auth",
		sessionURL: "https://session",
		boxes:      []sandbox.Box{{InstanceID: "c1", BoxID: "b1"}},
		logsOut:    "log output",
		execResult: sandbox.ExecResult{Stdout: "out", Stderr: "err", ExitCode: 3},
		reaped:     []string{"r1", "r2"},
	}
	rs := startSpoke(t, fake)
	ctx := context.Background()

	id, url, err := rs.Create(ctx, sandbox.CreateOptions{BoxID: "b1", Image: "img:1", SpokeName: "s"})
	if err != nil || id != "cid" || url != "https://auth" {
		t.Fatalf("Create = (%q,%q,%v)", id, url, err)
	}
	if fake.lastCreate.BoxID != "b1" {
		t.Errorf("spoke saw create box id %q", fake.lastCreate.BoxID)
	}

	if url, err := rs.SubmitCode(ctx, "cid", "code-1"); err != nil || url != "https://session" {
		t.Fatalf("SubmitCode = (%q,%v)", url, err)
	}
	if fake.lastSubmit != [2]string{"cid", "code-1"} {
		t.Errorf("spoke saw submit %v", fake.lastSubmit)
	}

	boxes, err := rs.List(ctx)
	if err != nil || !reflect.DeepEqual(boxes, fake.boxes) {
		t.Fatalf("List = (%v,%v)", boxes, err)
	}

	if err := rs.Destroy(ctx, "b1"); err != nil || fake.lastDestroy != "b1" {
		t.Fatalf("Destroy err=%v lastDestroy=%q", err, fake.lastDestroy)
	}

	logs, err := rs.Logs(ctx, "b1", 42)
	if err != nil || logs != "log output" {
		t.Fatalf("Logs = (%q,%v)", logs, err)
	}
	if fake.lastLogs != [2]any{"b1", 42} {
		t.Errorf("spoke saw logs %v", fake.lastLogs)
	}

	res, err := rs.Exec(ctx, "b1", []string{"/bin/sh", "-c", "echo hi"})
	if err != nil || !reflect.DeepEqual(res, fake.execResult) {
		t.Fatalf("Exec = (%+v,%v)", res, err)
	}
	if !reflect.DeepEqual(fake.lastExec.cmd, []string{"/bin/sh", "-c", "echo hi"}) {
		t.Errorf("spoke saw exec cmd %v", fake.lastExec.cmd)
	}

	reaped, err := rs.ReapOrphans(ctx, 5*time.Second)
	if err != nil || !reflect.DeepEqual(reaped, []string{"r1", "r2"}) {
		t.Fatalf("ReapOrphans = (%v,%v)", reaped, err)
	}
	if fake.lastReap != 5*time.Second {
		t.Errorf("spoke saw reap ttl %v", fake.lastReap)
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
	rs := newRemoteSpoke("s", hubEnd)

	// A call in flight (no serve loop answers) fails once the connection drops.
	errc := make(chan error, 1)
	go func() {
		_, _, err := rs.Create(context.Background(), sandbox.CreateOptions{})
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
	rs := newRemoteSpoke("s", hubEnd)
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
		createURL:  "https://auth",
		sessionURL: "https://session",
		boxes:      []sandbox.Box{{InstanceID: "c1"}},
		logsOut:    "logz",
		execResult: sandbox.ExecResult{Stdout: "o", ExitCode: 1},
		reaped:     []string{"x"},
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
	p, err := dispatch(ctx, fake, mustReq(methodDestroy, destroyReq{IDOrName: "b1"}), ValidationPolicy{})
	if err != nil || p != nil {
		t.Fatalf("destroy dispatch = (%s,%v)", p, err)
	}
	if fake.lastDestroy != "b1" {
		t.Errorf("destroy reached fake with %q", fake.lastDestroy)
	}

	// List returns boxes.
	p, err = dispatch(ctx, fake, mustReq(methodList, struct{}{}), ValidationPolicy{})
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
	_, err := dispatch(context.Background(), &fakeManager{}, frame{Type: frameReq, Method: "bogus"}, ValidationPolicy{})
	if err == nil || !strings.Contains(err.Error(), "unknown method") {
		t.Fatalf("err = %v, want unknown method", err)
	}
}

// TestDispatchBadPayload is a package test.
func TestDispatchBadPayload(t *testing.T) {
	req := frame{Type: frameReq, Method: methodCreate, Payload: []byte("{not json")}
	if _, err := dispatch(context.Background(), &fakeManager{}, req, ValidationPolicy{}); err == nil {
		t.Fatal("expected error for malformed payload")
	}
}
