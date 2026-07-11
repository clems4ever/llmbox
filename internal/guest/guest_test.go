package guest

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
	a := New(Options{})
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

// startGuest starts a guest serving a unix socket and returns it with a client.
func startGuest(t *testing.T, opts Options) (*Guest, *Client) {
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
			t.Fatal("guest socket did not appear")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		<-errc
	})
	return a, NewUnixClient(sock)
}

// boxEnv returns an environment that points HOME at an empty temp dir and carries
// the ambient PATH, isolating each box's home directory.
func boxEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	return []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
}

// TestGuestLifecycle drives a box through Init and Exec over the unix-socket
// client, the two control verbs the guest still serves.
func TestGuestLifecycle(t *testing.T) {
	_, c := startGuest(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := c.Init(ctx, InitReq{Env: boxEnv(t)}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	res, err := c.Exec(ctx, []string{"echo", "hello-exec"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "hello-exec" || res.ExitCode != 0 {
		t.Fatalf("Exec result = %+v", res)
	}
}

// TestGuestRunsAsCredential launches the box under an explicit OS credential and
// checks Exec (handleExec) runs as that uid. It targets the running user's own
// uid/gid — a no-op drop any non-root process may perform — so the credential
// plumbing is exercised without root; NoSetGroups skips the privileged setgroups
// call a non-root test cannot make.
func TestGuestRunsAsCredential(t *testing.T) {
	cred := &syscall.Credential{
		Uid:         uint32(os.Getuid()),
		Gid:         uint32(os.Getgid()),
		NoSetGroups: true,
	}
	_, c := startGuest(t, Options{Credential: cred})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := c.Init(ctx, InitReq{Env: boxEnv(t)}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// handleExec must run the command as the configured uid.
	res, err := c.Exec(ctx, []string{"id", "-u"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != strconv.Itoa(os.Getuid()) {
		t.Fatalf("id -u = %q, want %d", got, os.Getuid())
	}
}

// TestListenAndServeSocketPerms checks the control socket lives inside an
// owner-only (0700) directory — the access gate that stops a non-owner local
// user from reaching it — while the socket itself is group/other-accessible
// (0666) so the host process can connect to it across a container bind mount
// where the in-box guest runs as a different uid.
func TestListenAndServeSocketPerms(t *testing.T) {
	a := New(Options{})
	sock := filepath.Join(t.TempDir(), "sockdir", "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- a.ListenAndServe(ctx, sock) }()
	t.Cleanup(func() { cancel(); <-errc })

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

// TestClientOverUnixSocket is the lifecycle exercised end to end through the unix
// client (an alias assertion that the public client path works).
func TestClientOverUnixSocket(t *testing.T) {
	TestGuestLifecycle(t)
}

// TestGuestInitWritesFiles writes a file with the requested mode and owner.
func TestGuestInitWritesFiles(t *testing.T) {
	_, c := startGuest(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	target := filepath.Join(t.TempDir(), "nested", "seed.json")
	if _, err := c.Init(ctx, InitReq{
		Env:   boxEnv(t),
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

// TestGuestInitRunsScript runs a host-provided init script during Init and checks
// its side effect landed (a sentinel written into the box home).
func TestGuestInitRunsScript(t *testing.T) {
	_, c := startGuest(t, Options{
		InitScriptPath: filepath.Join(t.TempDir(), "init-script"),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	home := t.TempDir()
	sentinel := filepath.Join(home, "provisioned")
	script := "#!/bin/sh\necho customising box\ntouch \"$HOME/provisioned\"\n"
	if _, err := c.Init(ctx, InitReq{
		Env:        []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")},
		InitScript: []byte(script),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("init script did not run (no sentinel): %v", err)
	}
}

// TestGuestInitScriptFailureReportsBroken checks a non-zero init script does not
// fail Init at the transport level but reports a broken box in the InitResp
// (carrying the reason and its output) — the host keeps the box for inspection
// rather than tearing it down.
func TestGuestInitScriptFailureReportsBroken(t *testing.T) {
	_, c := startGuest(t, Options{
		InitScriptPath: filepath.Join(t.TempDir(), "init-script"),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	script := "#!/bin/sh\necho boom-provisioning-error 1>&2\nexit 7\n"
	resp, err := c.Init(ctx, InitReq{
		Env:        boxEnv(t),
		InitScript: []byte(script),
	})
	if err != nil {
		t.Fatalf("Init should report a broken box as data, not error: %v", err)
	}
	if !resp.ScriptFailed {
		t.Fatal("InitResp.ScriptFailed should be set for a non-zero init script")
	}
	if !strings.Contains(resp.ScriptError, "exit status 7") {
		t.Fatalf("ScriptError = %q, want the exit reason", resp.ScriptError)
	}
	if !strings.Contains(resp.ScriptOutput, "boom-provisioning-error") {
		t.Fatalf("ScriptOutput missing the script's output: %q", resp.ScriptOutput)
	}
}

// TestDefaultInitScriptPathOutsideSocketDir guards against a Firecracker EACCES
// regression: the init script must not be written under the 0700 control-socket
// dir (/run/llmbox), because it is exec'd as the unprivileged box user, which
// cannot traverse a directory locked to the guest's own (root) user. It must live
// in a world-traversable location so execve as the box user succeeds on every
// backend (Docker runs the guest as root and slipped through; Firecracker drops
// to the box user and did not).
func TestDefaultInitScriptPathOutsideSocketDir(t *testing.T) {
	const socketDir = "/run/llmbox" // matches docker.socketMountTarget
	dir := filepath.Dir(defaultInitScriptPath)
	if dir == socketDir || strings.HasPrefix(dir, socketDir+"/") {
		t.Fatalf("init script %q lives under the 0700 socket dir %q; the box user cannot traverse it (EACCES on execve)", defaultInitScriptPath, socketDir)
	}
}

// TestGuestExecNonZeroExit reports a non-zero exit code without erroring.
func TestGuestExecNonZeroExit(t *testing.T) {
	_, c := startGuest(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Init(ctx, InitReq{Env: boxEnv(t)}); err != nil {
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

// TestGuestUnknownVerb returns an error for an unrecognised verb.
func TestGuestUnknownVerb(t *testing.T) {
	_, c := startGuest(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.call(ctx, "bogus", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown verb") {
		t.Fatalf("err = %v, want unknown verb", err)
	}
}

// TestGuestEntryEnvFillsHomeAndPath fills HOME (from Options.Home) and PATH when
// the Init env omits them, without inheriting other ambient variables.
func TestGuestEntryEnvFillsHomeAndPath(t *testing.T) {
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

// TestGuestEntryEnvKeepsInitValues keeps an Init-supplied HOME in preference to
// Options.Home.
func TestGuestEntryEnvKeepsInitValues(t *testing.T) {
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

// TestClientDialPort reaches a listener inside the box through the guest, and
// TestGuestDialRejectsBadPort rejects an out-of-range port.
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

	_, c := startGuest(t, Options{})
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

// TestGuestDialRejectsBadPort writes an error response for an out-of-range port.
func TestGuestDialRejectsBadPort(t *testing.T) {
	_, c := startGuest(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.DialPort(ctx, 70000); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("err = %v, want out of range", err)
	}
}
