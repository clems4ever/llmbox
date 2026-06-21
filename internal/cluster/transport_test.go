package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestWSTransportRoundTrip sends a frame over a real loopback WebSocket and
// reads it back, exercising the wsTransport wrapper end to end.
func TestWSTransportRoundTrip(t *testing.T) {
	got := make(chan frame, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		tr := newWSTransport(conn)
		f, err := tr.Recv(r.Context())
		if err != nil {
			t.Errorf("server recv: %v", err)
			return
		}
		got <- f
		_ = tr.Send(r.Context(), frame{Type: frameResp, ID: f.ID, Method: "echo"})
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	url := "ws" + srv.URL[len("http"):] + "/"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	tr := newWSTransport(conn)
	defer func() { _ = tr.Close() }()

	if err := tr.Send(ctx, frame{Type: frameReq, ID: 7, Method: "ping"}); err != nil {
		t.Fatalf("client send: %v", err)
	}
	select {
	case f := <-got:
		if f.ID != 7 || f.Method != "ping" {
			t.Errorf("server got %+v", f)
		}
	case <-ctx.Done():
		t.Fatal("server did not receive frame")
	}

	reply, err := tr.Recv(ctx)
	if err != nil || reply.ID != 7 || reply.Method != "echo" {
		t.Fatalf("client reply = (%+v,%v)", reply, err)
	}
}

// TestWSTransportLargeFrame round-trips a frame whose payload exceeds the old
// 8 MiB read limit. A create request injects CLI binaries (base64-encoded, so
// tens of megabytes in one frame); the spoke used to drop the connection with
// "message too big" and reconnect-loop forever, never receiving the create.
func TestWSTransportLargeFrame(t *testing.T) {
	// 16 MiB of payload: comfortably above the previous 8 MiB cap, below 64 MiB.
	const payloadBytes = 16 << 20
	bigPayload, err := json.Marshal(bytes.Repeat([]byte{'a'}, payloadBytes))
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	want := frame{Type: frameReq, ID: 9, Method: methodCreate, Payload: bigPayload}

	got := make(chan frame, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		tr := newWSTransport(conn)
		f, err := tr.Recv(r.Context())
		if err != nil {
			t.Errorf("server recv: %v", err)
			return
		}
		got <- f
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	url := "ws" + srv.URL[len("http"):] + "/"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	tr := newWSTransport(conn)
	defer func() { _ = tr.Close() }()

	if err := tr.Send(ctx, want); err != nil {
		t.Fatalf("client send: %v", err)
	}
	select {
	case f := <-got:
		if f.ID != want.ID || f.Method != want.Method || !bytes.Equal(f.Payload, want.Payload) {
			t.Errorf("server got type=%q id=%d method=%q payloadLen=%d, want payloadLen=%d",
				f.Type, f.ID, f.Method, len(f.Payload), len(want.Payload))
		}
	case <-ctx.Done():
		t.Fatal("server did not receive large frame")
	}
}
