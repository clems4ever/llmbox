package testutils

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/guest"
)

// MockClaudeScript is a stand-in for the standalone `claude` binary, close enough
// to exercise the guest's PTY handling and URL scanning without a real
// Claude: `auth login` prints an authorize URL then blocks reading the OAuth
// code; `remote-control` prints a session URL then stays alive reading stdin
// until the PTY closes. The GuestFixture installs it as the box's claude command.
const MockClaudeScript = `#!/bin/sh
case "$1" in
auth)
  echo "To authenticate, visit https://claude.com/cai/oauth/authorize?a=1&code_challenge=chal&state=st8 then paste the code"
  IFS= read -r code
  echo "submitted code: $code"
  exit 0
  ;;
remote-control)
  echo "Remote control session ready: https://claude.ai/s/mock-session-xyz"
  while IFS= read -r _; do : ; done
  exit 0
  ;;
*)
  echo "mock claude: unexpected args: $*" >&2
  exit 2
  ;;
esac
`

// GuestFixture is a running guest backed by a mock claude, with a connected
// host-side client. It lets any package drive the full box-control surface (Init,
// Start, SubmitCode, Exec, Logs, Dial) over a real Unix socket without Docker or a
// real Claude. Use NewGuestFixture to build one; its teardown is registered on
// the test automatically.
type GuestFixture struct {
	// Client is the host-side client connected to the guest's control socket.
	Client *guest.Client
	// SocketPath is the filesystem path of the guest's control socket.
	SocketPath string

	guest     *guest.Guest
	cancel    context.CancelFunc
	errc      chan error
	closeOnce sync.Once
}

// NewGuestFixture starts a guest serving a temporary Unix control socket,
// backed by the bundled mock claude, and returns it with a connected client. The
// guest is shut down and the serve loop drained via t.Cleanup, so callers need no
// manual teardown.
//
// @arg t The test the fixture's lifetime and temp files are scoped to.
// @return *GuestFixture A running guest with a connected client.
//
// @testcase TestGuestFixtureDrivesLifecycle drives a box through a fixture built here.
func NewGuestFixture(t testing.TB) *GuestFixture {
	t.Helper()
	claude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claude, []byte(MockClaudeScript), 0o755); err != nil {
		t.Fatalf("writing mock claude: %v", err)
	}

	a := guest.New(guest.Options{ClaudeCmd: claude})
	sock := filepath.Join(t.TempDir(), "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- a.ListenAndServe(ctx, sock) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("guest control socket did not appear")
		}
		time.Sleep(5 * time.Millisecond)
	}

	f := &GuestFixture{
		Client:     guest.NewUnixClient(sock),
		SocketPath: sock,
		guest:      a,
		cancel:     cancel,
		errc:       errc,
	}
	t.Cleanup(f.Close)
	return f
}

// BoxEnv returns an environment to pass in InitReq.Env that points HOME at a
// fresh temp dir, so the box looks unauthenticated. When seedCreds is true a
// credentials file is planted under that HOME so the box instead looks already
// authenticated (Start then yields a session URL rather than an authorize URL).
//
// @arg t The test the temp HOME is scoped to.
// @arg seedCreds Whether to plant a credentials file so the box looks authenticated.
// @return []string The HOME (and PATH) environment for the box.
//
// @testcase TestGuestFixtureDrivesLifecycle uses an unauthenticated BoxEnv.
// @testcase TestGuestFixtureSeedsCredentials uses a credentialed BoxEnv to skip login.
func (f *GuestFixture) BoxEnv(t testing.TB, seedCreds bool) []string {
	t.Helper()
	home := t.TempDir()
	if seedCreds {
		dir := filepath.Join(home, ".claude")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir creds dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"token":"x"}`), 0o600); err != nil {
			t.Fatalf("seed creds: %v", err)
		}
	}
	return []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
}

// Close shuts the guest down and waits for its serve loop to return. It is
// registered with t.Cleanup by NewGuestFixture, so tests rarely call it directly;
// it is safe to call more than once.
//
// @testcase TestGuestFixtureDrivesLifecycle tears the fixture down via Close.
func (f *GuestFixture) Close() {
	f.closeOnce.Do(func() {
		f.guest.Shutdown()
		f.cancel()
		<-f.errc
	})
}
