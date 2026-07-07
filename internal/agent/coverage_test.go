package agent

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// TestTranscriptAppendTrims bounds the retained transcript and keeps scanning
// after the buffer is trimmed.
func TestTranscriptAppendTrims(t *testing.T) {
	tr := newTranscript()
	tr.append(make([]byte, transcriptCap+4096))
	if len(tr.raw) > transcriptCap {
		t.Fatalf("raw not trimmed: %d > %d", len(tr.raw), transcriptCap)
	}
	tr.append([]byte("ready https://claude.ai/s/x"))
	match, _, _, err := tr.waitForAny([]*regexp.Regexp{sessionURLRe}, time.Second)
	if err != nil || !strings.Contains(match, "claude.ai") {
		t.Fatalf("scan after trim: match=%q err=%v", match, err)
	}
}

// TestLastLinesTruncates keeps only the trailing n bytes.
func TestLastLinesTruncates(t *testing.T) {
	got := lastLines([]byte(strings.Repeat("x", 1000)), 100)
	if len(got) > 100 {
		t.Fatalf("lastLines kept %d bytes, want <= 100", len(got))
	}
}

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
	if err := a.handleInit(InitReq{}); err != nil {
		t.Fatalf("first Init: %v", err)
	}
	if err := a.handleInit(InitReq{}); err == nil {
		t.Fatal("second Init should fail")
	}
}

// TestHandleStartBeforeInit rejects Start before Init.
func TestHandleStartBeforeInit(t *testing.T) {
	a := New(Options{})
	if _, err := a.handleStart(); err == nil {
		t.Fatal("Start before Init should fail")
	}
}

// TestHandleLogsBeforeStart returns empty logs before the box starts.
func TestHandleLogsBeforeStart(t *testing.T) {
	a := New(Options{})
	if out := a.handleLogs(logsReq{}); out != "" {
		t.Fatalf("logs before start = %q, want empty", out)
	}
}

// TestDialPortUnreachable surfaces an error when the in-box port has no listener.
func TestDialPortUnreachable(t *testing.T) {
	_, c := startAgent(t, Options{ClaudeCmd: writeMockClaude(t)})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 9 is the discard port; nothing listens, so the agent's dial is refused.
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

// TestAgentStartTwice rejects a second Start.
func TestAgentStartTwice(t *testing.T) {
	_, c := startAgent(t, Options{ClaudeCmd: writeMockClaude(t)})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.Init(ctx, InitReq{Env: boxEnv(t, false)}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := c.Start(ctx); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if _, err := c.Start(ctx); err == nil {
		t.Fatal("second Start should fail")
	}
}

// TestAgentExecEmptyCommand rejects an empty command.
func TestAgentExecEmptyCommand(t *testing.T) {
	_, c := startAgent(t, Options{ClaudeCmd: writeMockClaude(t)})
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
	if err := c.Init(context.Background(), InitReq{}); err == nil {
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

// TestAgentMalformedPayload returns an error response for a verb whose payload is
// not valid JSON.
func TestAgentMalformedPayload(t *testing.T) {
	_, c := startAgent(t, Options{ClaudeCmd: writeMockClaude(t)})
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

// noURLClaude exits without ever printing an authorize/session URL, so Start
// observes the stream end before a match.
const noURLClaude = `#!/bin/sh
echo "login failed: invalid request"
exit 1
`

// TestAgentStartNoURL surfaces the box's tail output when no URL appears before
// the box process exits.
func TestAgentStartNoURL(t *testing.T) {
	mock := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(mock, []byte(noURLClaude), 0o755); err != nil {
		t.Fatalf("write mock: %v", err)
	}
	_, c := startAgent(t, Options{ClaudeCmd: mock})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.Init(ctx, InitReq{Env: boxEnv(t, false)}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := c.Start(ctx); err == nil {
		t.Fatal("Start should fail when no URL appears")
	}
}

// TestAgentDialDecodeError returns an error response for a dial whose payload is
// malformed.
func TestAgentDialDecodeError(t *testing.T) {
	_, c := startAgent(t, Options{ClaudeCmd: writeMockClaude(t)})
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
