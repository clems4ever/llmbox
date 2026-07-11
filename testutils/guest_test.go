package testutils

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/guest"
)

// TestGuestFixtureDrivesLifecycle uses the GuestFixture to drive a box through
// Init and Exec over a real control socket.
func TestGuestFixtureDrivesLifecycle(t *testing.T) {
	f := NewGuestFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := f.Client.Init(ctx, guest.InitReq{Env: f.BoxEnv(t)}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	res, err := f.Client.Exec(ctx, []string{"echo", "from-fixture"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "from-fixture" || res.ExitCode != 0 {
		t.Fatalf("Exec = %+v", res)
	}

	// Close is idempotent: the explicit call here and the t.Cleanup call must not
	// double-drain the serve loop.
	f.Close()
}
