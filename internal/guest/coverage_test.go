package guest

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// TestWriteInjectFileDefaultMode writes a file with the default 0644 when Mode is
// zero, creating parent directories.
func TestWriteInjectFileDefaultMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a", "b", "f.txt")
	if err := writeInjectFile(sandbox.InjectFile{Path: p, Content: []byte("hi")}); err != nil {
		t.Fatalf("writeInjectFile: %v", err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644", info.Mode().Perm())
	}
}

// TestHandleInitTwiceErrors rejects a second Init.
func TestHandleInitTwiceErrors(t *testing.T) {
	a := New(Options{})
	if _, err := a.handleInit(context.Background(), InitReq{}); err != nil {
		t.Fatalf("first Init: %v", err)
	}
	if _, err := a.handleInit(context.Background(), InitReq{}); err == nil {
		t.Fatal("second Init should fail")
	}
}

// TestDialPortUnreachable surfaces an error when the in-box port has no listener.
func TestDialPortUnreachable(t *testing.T) {
	_, c := startGuest(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 9 is the discard port; nothing listens, so the guest's dial is refused.
	if _, err := c.DialPort(ctx, 9); err == nil {
		t.Fatal("DialPort to an unused port should fail")
	}
}

// TestWriteInjectFileChowns exercises the owner-setting branch (chown to the
// current uid/gid is a no-op but covers the path).
func TestWriteInjectFileChowns(t *testing.T) {
	p := filepath.Join(t.TempDir(), "owned.txt")
	if err := writeInjectFile(sandbox.InjectFile{Path: p, Content: []byte("x"), Mode: 0o600, UID: os.Getuid(), GID: os.Getgid()}); err != nil {
		t.Fatalf("writeInjectFile: %v", err)
	}
}

// TestGuestExecEmptyCommand rejects an empty command.
func TestGuestExecEmptyCommand(t *testing.T) {
	_, c := startGuest(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Exec(ctx, nil); err == nil {
		t.Fatal("Exec with an empty command should fail")
	}
}

// TestClientDialError surfaces a connection error from a verb when the control
// channel cannot be opened.
func TestClientDialError(t *testing.T) {
	c := &Client{Dial: func(context.Context) (net.Conn, error) { return nil, errBoom }}
	if _, err := c.Init(context.Background(), InitReq{}); err == nil {
		t.Fatal("Init should fail when the control channel cannot be opened")
	}
	if _, err := c.DialPort(context.Background(), 80); err == nil {
		t.Fatal("DialPort should fail when the control channel cannot be opened")
	}
}

var errBoom = errorString("dial boom")

// errorString is a minimal error type for tests.
type errorString string

// Error implements error for the test error type.
func (e errorString) Error() string { return string(e) }

// TestGuestMalformedPayload returns an error response for a verb whose payload is
// not valid JSON.
func TestGuestMalformedPayload(t *testing.T) {
	_, c := startGuest(t, Options{})
	conn, err := c.Dial(context.Background())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := writeFrame(conn, req{Verb: verbExec, Data: jsonRaw(`"not-an-exec-request"`)}); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	var r resp
	if err := readFrame(conn, &r); err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if r.Err == "" {
		t.Fatal("want an error response for a malformed payload")
	}
}

// jsonRaw is a json.RawMessage literal helper for tests.
func jsonRaw(s string) []byte { return []byte(s) }

// TestGuestDialDecodeError returns an error response for a dial whose payload is
// malformed.
func TestGuestDialDecodeError(t *testing.T) {
	_, c := startGuest(t, Options{})
	conn, err := c.Dial(context.Background())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := writeFrame(conn, req{Verb: verbDial, Data: jsonRaw(`"not-a-dial-request"`)}); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	var r resp
	if err := readFrame(conn, &r); err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if r.Err == "" {
		t.Fatal("want an error response for a malformed dial payload")
	}
}
