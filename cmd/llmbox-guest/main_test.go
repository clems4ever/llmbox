package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunServesAndStops checks that run serves the control socket and returns
// cleanly once its context is cancelled.
func TestRunServesAndStops(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	errc := make(chan error, 1)
	go func() { errc <- run(ctx, sock, 0, "", 0, "/bin/true", "", log) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("control socket did not appear")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("run returned %v, want nil on cancel", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not return after cancel")
	}
}

// TestRunStartsBoxAPIBridge checks vsock mode with a box API port serves the
// in-guest bridge socket. The control channel itself may fail (AF_VSOCK is not
// available on every test host) — the bridge must come up regardless.
func TestRunStartsBoxAPIBridge(t *testing.T) {
	boxapiSock := filepath.Join(t.TempDir(), "boxapi.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	go func() { _ = run(ctx, "", 1, boxapiSock, 5001, "/bin/true", "", log) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(boxapiSock); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("box API bridge socket did not appear")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRunRejectsUnknownUser checks run fails fast (before serving) when --user
// names an account the box does not have, naming the offending user.
func TestRunRejectsUnknownUser(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sock := filepath.Join(t.TempDir(), "control.sock")
	err := run(context.Background(), sock, 0, "", 0, "/bin/true", "no-such-box-user-xyz", log)
	if err == nil || !strings.Contains(err.Error(), "no-such-box-user-xyz") {
		t.Fatalf("run err = %v, want a lookup failure naming the user", err)
	}
	if _, serr := os.Stat(sock); serr == nil {
		t.Fatal("run created the control socket despite the bad user")
	}
}

// TestLookupUserResolvesCurrentUser resolves the running user to its own
// uid/gid/home, and an empty name to a nil (no-drop) credential.
func TestLookupUserResolvesCurrentUser(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}
	cred, home, err := lookupUser(u.Username)
	if err != nil {
		t.Fatalf("lookupUser: %v", err)
	}
	if cred == nil {
		t.Fatal("cred = nil, want a credential")
	}
	if cred.Uid != uint32(os.Getuid()) || cred.Gid != uint32(os.Getgid()) {
		t.Fatalf("cred = %+v, want uid %d gid %d", cred, os.Getuid(), os.Getgid())
	}
	if home != u.HomeDir {
		t.Fatalf("home = %q, want %q", home, u.HomeDir)
	}

	cred, home, err = lookupUser("")
	if err != nil || cred != nil || home != "" {
		t.Fatalf(`lookupUser("") = %+v, %q, %v; want nil, "", nil`, cred, home, err)
	}
}
