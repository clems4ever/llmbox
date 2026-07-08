package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
	go func() { errc <- run(ctx, sock, 0, "", 0, "/bin/true", log) }()

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

	go func() { _ = run(ctx, "", 1, boxapiSock, 5001, "/bin/true", log) }()

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
