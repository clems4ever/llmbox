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
	go func() { errc <- run(ctx, sock, "/bin/true", log) }()

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
