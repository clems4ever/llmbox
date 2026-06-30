package testutils

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/agent"
)

// MockClaudeScript is a stand-in for the standalone `claude` binary, close enough
// to exercise the guest agent's PTY handling and URL scanning without a real
// Claude: `auth login` prints an authorize URL then blocks reading the OAuth
// code; `remote-control` prints a session URL then stays alive reading stdin
// until the PTY closes. The AgentFixture installs it as the box's claude command.
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

// AgentFixture is a running guest agent backed by a mock claude, with a connected
// host-side client. It lets any package drive the full box-control surface (Init,
// Start, SubmitCode, Exec, Logs, Dial) over a real Unix socket without Docker or a
// real Claude. Use NewAgentFixture to build one; its teardown is registered on
// the test automatically.
type AgentFixture struct {
	// Client is the host-side client connected to the agent's control socket.
	Client *agent.Client
	// SocketPath is the filesystem path of the agent's control socket.
	SocketPath string

	agent     *agent.Agent
	cancel    context.CancelFunc
	errc      chan error
	closeOnce sync.Once
}

// NewAgentFixture starts a guest agent serving a temporary Unix control socket,
// backed by the bundled mock claude, and returns it with a connected client. The
// agent is shut down and the serve loop drained via t.Cleanup, so callers need no
// manual teardown.
//
// @arg t The test the fixture's lifetime and temp files are scoped to.
// @return *AgentFixture A running agent with a connected client.
//
// @testcase TestAgentFixtureDrivesLifecycle drives a box through a fixture built here.
func NewAgentFixture(t testing.TB) *AgentFixture {
	t.Helper()
	claude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claude, []byte(MockClaudeScript), 0o755); err != nil {
		t.Fatalf("writing mock claude: %v", err)
	}

	a := agent.New(agent.Options{ClaudeCmd: claude})
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
			t.Fatal("agent control socket did not appear")
		}
		time.Sleep(5 * time.Millisecond)
	}

	f := &AgentFixture{
		Client:     agent.NewUnixClient(sock),
		SocketPath: sock,
		agent:      a,
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
// @testcase TestAgentFixtureDrivesLifecycle uses an unauthenticated BoxEnv.
// @testcase TestAgentFixtureSeedsCredentials uses a credentialed BoxEnv to skip login.
func (f *AgentFixture) BoxEnv(t testing.TB, seedCreds bool) []string {
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

// Close shuts the agent down and waits for its serve loop to return. It is
// registered with t.Cleanup by NewAgentFixture, so tests rarely call it directly;
// it is safe to call more than once.
//
// @testcase TestAgentFixtureDrivesLifecycle tears the fixture down via Close.
func (f *AgentFixture) Close() {
	f.closeOnce.Do(func() {
		f.agent.Shutdown()
		f.cancel()
		<-f.errc
	})
}
