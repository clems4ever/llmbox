package testutils

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/guest"
)

// TestGuestFixtureDrivesLifecycle uses the GuestFixture to drive a box through
// Init, Start (authorize URL), SubmitCode (session URL), Exec, and Logs.
func TestGuestFixtureDrivesLifecycle(t *testing.T) {
	f := NewGuestFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := f.Client.Init(ctx, guest.InitReq{BoxID: "fix-box", Env: f.BoxEnv(t, false)}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	start, err := f.Client.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.Contains(start.AuthorizeURL, "oauth/authorize") || start.SessionURL != "" {
		t.Fatalf("Start = %+v, want an authorize URL and no session URL", start)
	}

	session, err := f.Client.SubmitCode(ctx, "the-code")
	if err != nil {
		t.Fatalf("SubmitCode: %v", err)
	}
	if !strings.HasPrefix(session, "https://claude.ai/") {
		t.Fatalf("session URL = %q", session)
	}

	res, err := f.Client.Exec(ctx, []string{"echo", "from-fixture"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "from-fixture" || res.ExitCode != 0 {
		t.Fatalf("Exec = %+v", res)
	}

	logs, err := f.Client.Logs(ctx, 0)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !strings.Contains(logs, "Remote control session ready") {
		t.Fatalf("logs missing remote-control banner:\n%s", logs)
	}

	// Close is idempotent: the explicit call here and the t.Cleanup call must not
	// double-drain the serve loop.
	f.Close()
}

// TestGuestFixtureSeedsCredentials checks that a credentialed BoxEnv makes Start
// skip login and return a session URL directly.
func TestGuestFixtureSeedsCredentials(t *testing.T) {
	f := NewGuestFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := f.Client.Init(ctx, guest.InitReq{BoxID: "authed-box", Env: f.BoxEnv(t, true)}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	start, err := f.Client.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if start.AuthorizeURL != "" {
		t.Fatalf("AuthorizeURL = %q, want none for an authenticated box", start.AuthorizeURL)
	}
	if !strings.HasPrefix(start.SessionURL, "https://claude.ai/") {
		t.Fatalf("SessionURL = %q, want a session URL", start.SessionURL)
	}
}
