package hooks

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeHook writes an executable shell script to a temp file and returns its
// path, so tests can drive the real subprocess path of the Runner.
func writeHook(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hook.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writing hook: %v", err)
	}
	return p
}

// TestNewEmptyIsNil checks New returns a nil Runner for an empty list.
func TestNewEmptyIsNil(t *testing.T) {
	if r := New(nil); r != nil {
		t.Errorf("New(nil) = %v, want nil", r)
	}
}

// TestOnCreateNilIsNoop checks a nil Runner returns no files, no state, no error.
func TestOnCreateNilIsNoop(t *testing.T) {
	var r *Runner
	files, state, err := r.OnCreate(context.Background(), BoxInfo{})
	if files != nil || state != nil || err != nil {
		t.Errorf("nil OnCreate = (%v, %v, %v), want all nil", files, state, err)
	}
}

// TestOnDestroyNilIsNoop checks OnDestroy is a no-op for a nil Runner and for an
// empty state map.
func TestOnDestroyNilIsNoop(t *testing.T) {
	var r *Runner
	if err := r.OnDestroy(context.Background(), BoxInfo{}, map[string]string{"h": "s"}); err != nil {
		t.Errorf("nil OnDestroy: %v", err)
	}
	if err := New([]string{"/nope"}).OnDestroy(context.Background(), BoxInfo{}, nil); err != nil {
		t.Errorf("empty-state OnDestroy: %v", err)
	}
}

// TestOnCreateInjectsFilesAndState checks OnCreate runs the hook with the request
// on stdin, returns the files it emits (with base64 decoded and the octal mode
// parsed), and keys the returned state by the hook executable.
func TestOnCreateInjectsFilesAndState(t *testing.T) {
	// Emits one base64 file at mode 0755 and a state string. "aGk=" is "hi".
	hook := writeHook(t, `cat >/dev/null
printf '%s' '{"state":"tok-1","files":[{"path":"/usr/local/bin/x","content_base64":"aGk=","mode":"0755","uid":0,"gid":0}]}'`)
	r := New([]string{hook})

	files, state, err := r.OnCreate(context.Background(), BoxInfo{BoxID: "h"})
	if err != nil {
		t.Fatalf("OnCreate: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1", len(files))
	}
	f := files[0]
	if f.Path != "/usr/local/bin/x" || string(f.Content) != "hi" || f.Mode != 0o755 {
		t.Errorf("file = %+v (content %q)", f, f.Content)
	}
	if state[hook] != "tok-1" {
		t.Errorf("state[hook] = %q, want tok-1", state[hook])
	}
}

// TestOnCreateFailingHookErrors checks a hook that exits non-zero surfaces its
// stderr in the error and returns the state gathered before it failed.
func TestOnCreateFailingHookErrors(t *testing.T) {
	ok := writeHook(t, `cat >/dev/null; printf '%s' '{"state":"first"}'`)
	bad := writeHook(t, `cat >/dev/null; echo "mint failed" >&2; exit 1`)
	r := New([]string{ok, bad})

	_, state, err := r.OnCreate(context.Background(), BoxInfo{})
	if err == nil {
		t.Fatal("expected error from failing hook")
	}
	if state[ok] != "first" {
		t.Errorf("partial state = %v, want first hook's state retained", state)
	}
}

// TestOnDestroyReplaysState checks OnDestroy hands each hook back the state it
// returned at create time, via the request's state field.
func TestOnDestroyReplaysState(t *testing.T) {
	// The hook writes the request it received to a file so the test can inspect it.
	out := filepath.Join(t.TempDir(), "seen.json")
	hook := writeHook(t, `cat >`+out+`; printf '{}'`)
	r := New([]string{hook})

	if err := r.OnDestroy(context.Background(), BoxInfo{BoxID: "h"}, map[string]string{hook: "tok-9"}); err != nil {
		t.Fatalf("OnDestroy: %v", err)
	}
	seen, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading seen request: %v", err)
	}
	if got := string(seen); !contains(got, `"state":"tok-9"`) || !contains(got, `"event":"box.destroy"`) {
		t.Errorf("hook saw request %s", got)
	}
}

// contains reports whether s contains sub (a tiny helper to keep the test free of
// an extra import).
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
