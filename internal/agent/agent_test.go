package agent

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// TestListenVsockReturns checks ListenVsockAndServe does not hang: on a host
// without an AF_VSOCK transport it returns the listen error, and if a vsock
// listener can be created it returns cleanly once the context is cancelled. The
// microVM path is exercised end-to-end by the Firecracker integration test; this
// only guards the entrypoint against blocking forever.
func TestListenVsockReturns(t *testing.T) {
	a := New(Options{ClaudeCmd: "/bin/true"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel up front so a successful listen still unblocks serve

	done := make(chan error, 1)
	go func() { done <- a.ListenVsockAndServe(ctx, 5005) }()

	select {
	case <-done:
		// Either a listen error (no vsock on this host) or nil (listen ok,
		// serve returned on the cancelled context) — both are non-hanging.
	case <-time.After(3 * time.Second):
		t.Fatal("ListenVsockAndServe hung instead of returning")
	}
}

// mockClaude mimics the standalone `claude` binary closely enough to exercise the
// agent's PTY handling and URL scanning: `auth login` prints an authorize URL and
// blocks reading the OAuth code, then `remote-control` prints a session URL and
// stays alive reading stdin until the PTY closes.
const mockClaude = `#!/bin/sh
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

// writeMockClaude writes the mock claude script to a temp file and returns its
// path.
func writeMockClaude(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(p, []byte(mockClaude), 0o755); err != nil {
		t.Fatalf("writing mock claude: %v", err)
	}
	return p
}

// startAgent starts an agent serving a unix socket and returns it with a client.
func startAgent(t *testing.T, opts Options) (*Agent, *Client) {
	t.Helper()
	a := New(opts)
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
			t.Fatal("agent socket did not appear")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() {
		a.Shutdown()
		cancel()
		<-errc
	})
	return a, NewUnixClient(sock)
}

// boxEnv returns an environment that points HOME at an empty temp dir (so the box
// looks unauthenticated) unless seedCreds is true, in which case a credentials
// file is planted so the box looks already authenticated.
func boxEnv(t *testing.T, seedCreds bool) []string {
	t.Helper()
	home := t.TempDir()
	if seedCreds {
		dir := filepath.Join(home, ".claude")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"token":"x"}`), 0o600); err != nil {
			t.Fatalf("seed creds: %v", err)
		}
	}
	return []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
}

// TestAgentLifecycle drives a box through Init, Start (authorize URL), SubmitCode
// (session URL), Exec, and Logs over the unix-socket client, then Shutdown.
func TestAgentLifecycle(t *testing.T) {
	_, c := startAgent(t, Options{ClaudeCmd: writeMockClaude(t)})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := c.Init(ctx, InitReq{BoxID: "my-box", Env: boxEnv(t, false)}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	start, err := c.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.Contains(start.AuthorizeURL, "oauth/authorize") {
		t.Fatalf("AuthorizeURL = %q, want an authorize URL", start.AuthorizeURL)
	}
	if start.SessionURL != "" {
		t.Fatalf("SessionURL should be empty before login, got %q", start.SessionURL)
	}

	session, err := c.SubmitCode(ctx, "the-oauth-code")
	if err != nil {
		t.Fatalf("SubmitCode: %v", err)
	}
	if !strings.HasPrefix(session, "https://claude.ai/") {
		t.Fatalf("session URL = %q, want a claude.ai session URL", session)
	}

	res, err := c.Exec(ctx, []string{"echo", "hello-exec"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "hello-exec" || res.ExitCode != 0 {
		t.Fatalf("Exec result = %+v", res)
	}

	logs, err := c.Logs(ctx, 0)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !strings.Contains(logs, "Remote control session ready") {
		t.Fatalf("logs missing remote-control banner:\n%s", logs)
	}
}

// TestListenAndServeSocketPerms checks the control socket lives inside an
// owner-only (0700) directory — the access gate that stops a non-owner local
// user from reaching it — while the socket itself is group/other-accessible
// (0666) so the host process can connect to it across a container bind mount
// where the in-box agent runs as a different uid.
func TestListenAndServeSocketPerms(t *testing.T) {
	a := New(Options{ClaudeCmd: writeMockClaude(t)})
	sock := filepath.Join(t.TempDir(), "sockdir", "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- a.ListenAndServe(ctx, sock) }()
	t.Cleanup(func() { a.Shutdown(); cancel(); <-errc })

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("control socket did not appear")
		}
		time.Sleep(5 * time.Millisecond)
	}

	si, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if si.Mode().Perm() != 0o666 {
		t.Fatalf("socket mode = %v, want 0666", si.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(sock))
	if err != nil {
		t.Fatalf("stat socket dir: %v", err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir mode = %v, want 0700", di.Mode().Perm())
	}
}

// TestAgentStartAlreadyAuthenticated returns a session URL (not an authorize URL)
// when the box already has credentials on disk.
func TestAgentStartAlreadyAuthenticated(t *testing.T) {
	_, c := startAgent(t, Options{ClaudeCmd: writeMockClaude(t)})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := c.Init(ctx, InitReq{BoxID: "authed", Env: boxEnv(t, true)}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	start, err := c.Start(ctx)
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

// TestClientOverUnixSocket is the lifecycle exercised end to end through the unix
// client (an alias assertion that the public client path works).
func TestClientOverUnixSocket(t *testing.T) {
	TestAgentLifecycle(t)
}

// TestAgentInitWritesFiles writes a file with the requested mode and owner.
func TestAgentInitWritesFiles(t *testing.T) {
	_, c := startAgent(t, Options{ClaudeCmd: writeMockClaude(t)})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	target := filepath.Join(t.TempDir(), "nested", "seed.json")
	if err := c.Init(ctx, InitReq{
		Env:   boxEnv(t, false),
		Files: []sandbox.InjectFile{{Path: target, Content: []byte(`{"ok":true}`), Mode: 0o600}},
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat injected file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	b, _ := os.ReadFile(target)
	if string(b) != `{"ok":true}` {
		t.Fatalf("content = %q", b)
	}
}

// TestAgentExecNonZeroExit reports a non-zero exit code without erroring.
func TestAgentExecNonZeroExit(t *testing.T) {
	_, c := startAgent(t, Options{ClaudeCmd: writeMockClaude(t)})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Init(ctx, InitReq{Env: boxEnv(t, false)}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	res, err := c.Exec(ctx, []string{"/bin/sh", "-c", "echo out; echo err 1>&2; exit 3"})
	if err != nil {
		t.Fatalf("Exec returned a transport error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", res.ExitCode)
	}
	if strings.TrimSpace(res.Stdout) != "out" || strings.TrimSpace(res.Stderr) != "err" {
		t.Fatalf("result = %+v", res)
	}
}

// TestAgentUnknownVerb returns an error for an unrecognised verb.
func TestAgentUnknownVerb(t *testing.T) {
	_, c := startAgent(t, Options{ClaudeCmd: writeMockClaude(t)})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.call(ctx, "bogus", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown verb") {
		t.Fatalf("err = %v, want unknown verb", err)
	}
}

// TestAgentSubmitCodeBeforeStart errors when called before Start.
func TestAgentSubmitCodeBeforeStart(t *testing.T) {
	_, c := startAgent(t, Options{ClaudeCmd: writeMockClaude(t)})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Init(ctx, InitReq{Env: boxEnv(t, false)}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := c.SubmitCode(ctx, "x"); err == nil || !strings.Contains(err.Error(), "not started") {
		t.Fatalf("err = %v, want 'not started'", err)
	}
}

// TestAgentEntryEnvFillsHomeAndPath fills HOME (from Options.Home) and PATH when
// the Init env omits them, without inheriting other ambient variables.
func TestAgentEntryEnvFillsHomeAndPath(t *testing.T) {
	a := New(Options{Home: "/box/home"})
	env := a.entryEnv()
	if !hasEnvKey(env, "HOME") || !hasEnvKey(env, "PATH") {
		t.Fatalf("entryEnv = %v, want HOME and PATH present", env)
	}
	var home string
	for _, e := range env {
		if strings.HasPrefix(e, "HOME=") {
			home = strings.TrimPrefix(e, "HOME=")
		}
	}
	if home != "/box/home" {
		t.Fatalf("HOME = %q, want /box/home", home)
	}
}

// TestAgentEntryEnvKeepsInitValues keeps an Init-supplied HOME in preference to
// Options.Home.
func TestAgentEntryEnvKeepsInitValues(t *testing.T) {
	a := New(Options{Home: "/box/home"})
	a.initReq = InitReq{Env: []string{"HOME=/init/home", "PATH=/usr/bin"}}
	env := a.entryEnv()
	count := 0
	for _, e := range env {
		if strings.HasPrefix(e, "HOME=") {
			count++
			if e != "HOME=/init/home" {
				t.Fatalf("HOME = %q, want the Init value", e)
			}
		}
	}
	if count != 1 {
		t.Fatalf("want exactly one HOME assignment, got %d in %v", count, env)
	}
}

// TestAgentEntrypointNamesDefaultSession adds a --name for the box's default
// session when a box ID is set, and omits it otherwise.
func TestAgentEntrypointNamesDefaultSession(t *testing.T) {
	a := New(Options{ClaudeCmd: "claude"})
	if got := a.entrypoint(InitReq{BoxID: "mybox"}); !strings.Contains(got, "--name mybox-default") {
		t.Fatalf("entrypoint = %q, want a --name for the default session", got)
	}
	if got := a.entrypoint(InitReq{}); strings.Contains(got, "--name") {
		t.Fatalf("entrypoint = %q, want no --name when box ID is empty", got)
	}
}

// TestClientDialPort reaches a listener inside the box through the agent, and
// TestAgentDialRejectsBadPort rejects an out-of-range port.
func TestClientDialPort(t *testing.T) {
	// A local echo server stands in for an in-box service on 127.0.0.1.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 256)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						_, _ = conn.Write(buf[:n])
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	_, c := startAgent(t, Options{ClaudeCmd: writeMockClaude(t)})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := c.DialPort(ctx, port)
	if err != nil {
		t.Fatalf("DialPort: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo = %q, want ping", buf)
	}
}

// TestAgentDialRejectsBadPort writes an error response for an out-of-range port.
func TestAgentDialRejectsBadPort(t *testing.T) {
	_, c := startAgent(t, Options{ClaudeCmd: writeMockClaude(t)})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.DialPort(ctx, 70000); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("err = %v, want out of range", err)
	}
}
