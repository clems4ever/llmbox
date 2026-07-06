package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/sandbox"
)

// recvWithin reads one frame from tr or fails the test on timeout.
func recvWithin(t *testing.T, tr transport) frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	f, err := tr.Recv(ctx)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	return f
}

// TestSpokeRunEnrollsAndServes is a package test.
func TestSpokeRunEnrollsAndServes(t *testing.T) {
	spokeEnd, hubEnd := newPipe()
	dial := func(context.Context) (transport, error) { return spokeEnd, nil }
	fake := &fakeManager{boxes: []sandbox.Box{{InstanceID: "c1"}}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	saved := make(chan Credentials, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, dial, fake, "tok", nil, func(c Credentials) error {
			saved <- c
			return nil
		}, ValidationPolicy{})
	}()

	// Hub side: receive the enroll request carrying the join token.
	enroll := recvWithin(t, hubEnd)
	if enroll.Type != frameEnroll {
		t.Fatalf("first frame type = %q, want enroll", enroll.Type)
	}
	var req enrollReq
	if err := decodePayload(enroll.Payload, &req); err != nil {
		t.Fatalf("decode enroll: %v", err)
	}
	if req.JoinToken != "tok" || req.Credential != "" {
		t.Errorf("enroll req = %+v, want join token only", req)
	}

	// Hub side: welcome with a minted credential.
	wp, _ := encodePayload(welcomeResp{Name: "s1", Credential: "cred-1"})
	if err := hubEnd.Send(ctx, frame{Type: frameWelcome, Payload: wp}); err != nil {
		t.Fatalf("send welcome: %v", err)
	}

	select {
	case c := <-saved:
		if c.Name != "s1" || c.Credential != "cred-1" {
			t.Errorf("saved = %+v", c)
		}
	case <-time.After(time.Second):
		t.Fatal("save callback was not invoked")
	}

	// Hub side: drive a verb to confirm the serve loop is running.
	lp, _ := encodePayload(struct{}{})
	if err := hubEnd.Send(ctx, frame{Type: frameReq, ID: 1, Method: methodList, Payload: lp}); err != nil {
		t.Fatalf("send list req: %v", err)
	}
	resp := recvWithin(t, hubEnd)
	if resp.Type != frameResp || resp.ID != 1 || resp.Error != "" {
		t.Fatalf("list resp = %+v", resp)
	}
}

// TestSpokeRunReconnectsWithCreds is a package test.
func TestSpokeRunReconnectsWithCreds(t *testing.T) {
	spokeEnd, hubEnd := newPipe()
	dial := func(context.Context) (transport, error) { return spokeEnd, nil }
	creds := &Credentials{Name: "s1", Credential: "cred-1"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	savedCount := 0
	go func() {
		_ = Run(ctx, dial, &fakeManager{}, "", creds, func(Credentials) error {
			savedCount++
			return nil
		}, ValidationPolicy{})
	}()

	enroll := recvWithin(t, hubEnd)
	var req enrollReq
	if err := decodePayload(enroll.Payload, &req); err != nil {
		t.Fatalf("decode enroll: %v", err)
	}
	if req.JoinToken != "" || req.Name != "s1" || req.Credential != "cred-1" {
		t.Errorf("reconnect enroll = %+v, want name+credential", req)
	}

	wp, _ := encodePayload(welcomeResp{Name: "s1"})
	if err := hubEnd.Send(ctx, frame{Type: frameWelcome, Payload: wp}); err != nil {
		t.Fatalf("send welcome: %v", err)
	}

	// Drive a verb so we know enrollment finished, then assert save was skipped.
	lp, _ := encodePayload(struct{}{})
	_ = hubEnd.Send(ctx, frame{Type: frameReq, ID: 1, Method: methodList, Payload: lp})
	_ = recvWithin(t, hubEnd)
	if savedCount != 0 {
		t.Errorf("save called %d times on reconnect, want 0", savedCount)
	}
}

// TestSpokeRunEnrollRejected is a package test.
func TestSpokeRunEnrollRejected(t *testing.T) {
	spokeEnd, hubEnd := newPipe()
	dial := func(context.Context) (transport, error) { return spokeEnd, nil }

	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), dial, &fakeManager{}, "tok", nil, nil, ValidationPolicy{})
	}()

	_ = recvWithin(t, hubEnd) // consume enroll
	if err := hubEnd.Send(context.Background(), frame{Type: frameErr, Error: "enrollment rejected"}); err != nil {
		t.Fatalf("send err frame: %v", err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, ErrEnrollRejected) {
			t.Fatalf("Run err = %v, want ErrEnrollRejected", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after rejection")
	}
}
