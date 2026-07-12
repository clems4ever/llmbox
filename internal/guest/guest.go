package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mdlayher/vsock"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

const (
	// defaultInitScriptTimeout bounds an init script that the host did not give an
	// explicit timeout for, so a hung provisioning script cannot wedge Create.
	defaultInitScriptTimeout = 5 * time.Minute
	// defaultInitScriptPath is where handleInit writes the host-provided init script
	// inside the box before running it. It sits directly under /run — deliberately
	// NOT under the 0700 control-socket dir (/run/llmbox): the script runs as the
	// unprivileged box user, which must be able to traverse to and exec it, whereas
	// the socket dir is locked to the guest's own (root) user. Putting the script
	// under the 0700 dir made execve fail with EACCES on backends that drop to a box
	// user (Firecracker), while Docker slipped through only because it runs the guest
	// as root. Tests override this via Options.InitScriptPath so they need no
	// privileged location.
	defaultInitScriptPath = "/run/llmbox-init-script"
)

// Options configure a Guest.
type Options struct {
	// Home overrides $HOME for the box's Exec commands and init script. Empty
	// inherits the home the guest itself was started with (a real container sets
	// HOME=/root); the in-process test fake sets a per-box home so concurrent boxes
	// stay isolated.
	Home string
	// InitScriptPath is where the host-provided init script is written inside the
	// box before it is run. Empty uses defaultInitScriptPath; tests override it to a
	// writable temp path so they need no privileged location.
	InitScriptPath string
	// Credential, when non-nil, is the OS credential (uid/gid and supplementary
	// groups) the guest drops to when running the box's init script and Exec
	// commands. The guest itself keeps its own privileges (root, to serve the
	// control channel and inject files); only those child processes run as this
	// credential — so the box's own workload runs unprivileged yet can still
	// escalate via sudo. Nil runs children as the guest's own user. Pair it with
	// Home so the dropped processes get that user's home.
	Credential *syscall.Credential
	// Log records best-effort failures; nil falls back to slog.Default().
	Log *slog.Logger
}

// Guest is the in-box guest: it serves the box-operation verbs (Init, Exec, Dial)
// over a control channel so the host can provision and reach the box without any
// host→box bridge networking. A single Guest handles one box for its lifetime;
// Init runs once, while Exec and Dial may run concurrently.
type Guest struct {
	home           string
	initScriptPath string
	cred           *syscall.Credential
	log            *slog.Logger

	mu      sync.Mutex // guards the init lifecycle fields below
	initReq InitReq
	inited  bool
}

// New returns a Guest configured by opts, applying defaults for any zero field.
//
// @arg opts The guest options; zero fields take their defaults.
// @return *Guest A ready-to-serve guest.
//
// @testcase TestGuestLifecycle drives a guest built by New through its verbs.
func New(opts Options) *Guest {
	a := &Guest{
		home:           opts.Home,
		initScriptPath: opts.InitScriptPath,
		cred:           opts.Credential,
		log:            opts.Log,
	}
	if a.initScriptPath == "" {
		a.initScriptPath = defaultInitScriptPath
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	return a
}

// entryEnv builds the environment for the box's Exec commands and init script. It
// starts from the host-supplied Init env, then fills in HOME (preferring an
// explicit Init HOME, else the configured Options.Home, else the ambient HOME the
// box was started with) and PATH (from the ambient env) only when absent. It
// deliberately does not inherit the rest of the guest's ambient environment, so a
// stray host variable cannot leak into the box.
//
// @return []string The environment for the box's processes.
//
// @testcase TestGuestEntryEnvFillsHomeAndPath fills HOME/PATH when Init omits them.
// @testcase TestGuestEntryEnvKeepsInitValues keeps an Init-supplied HOME over Options.Home.
func (a *Guest) entryEnv() []string {
	env := append([]string(nil), a.initReq.Env...)
	if !hasEnvKey(env, "HOME") {
		home := a.home
		if home == "" {
			home = os.Getenv("HOME")
		}
		if home != "" {
			env = append(env, "HOME="+home)
		}
	}
	if !hasEnvKey(env, "PATH") {
		if p := os.Getenv("PATH"); p != "" {
			env = append(env, "PATH="+p)
		}
	}
	return env
}

// hasEnvKey reports whether env contains an assignment for key (a "key=" prefix).
//
// @arg env The environment slice to search.
// @arg key The variable name to look for.
// @return bool True when env assigns key.
//
// @testcase TestGuestEntryEnvKeepsInitValues relies on hasEnvKey to detect an Init HOME.
func hasEnvKey(env []string, key string) bool {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// ListenAndServe creates the control socket at path (replacing any stale socket),
// restricts it to the owner, and serves connections until ctx is cancelled or the
// listener fails.
//
// @arg ctx Context whose cancellation stops the accept loop and removes the socket.
// @arg path The filesystem path of the Unix control socket to create.
// @error error if the socket cannot be created or the accept loop fails for a reason other than ctx cancellation.
//
// @testcase TestGuestLifecycle serves over a socket created by ListenAndServe.
func (a *Guest) ListenAndServe(ctx context.Context, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating socket dir: %w", err)
	}
	// Remove a stale socket left by a previous run so bind succeeds on restart.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing stale socket: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listening on control socket: %w", err)
	}
	defer ln.Close()
	// The access gate is the 0700 parent directory, owned by the host process
	// (the spoke): only it (and root) can traverse into it, so no other local user
	// can reach the socket. The socket itself is made group/other-accessible
	// because, across the container bind mount, the in-box guest runs as a
	// different uid (root) than the host spoke, and a connect() needs write
	// permission on the socket. The pre-chmod mode is only ever more restrictive
	// than this, so there is no window where the socket is wider than intended.
	if err := os.Chmod(path, 0o666); err != nil {
		return fmt.Errorf("setting control socket mode: %w", err)
	}

	return a.serve(ctx, ln)
}

// ListenVsockAndServe listens on the guest AF_VSOCK port and serves control
// connections until ctx is cancelled or the listener fails. It is the microVM
// transport: the host reaches this listener over the hypervisor's vsock, so no
// filesystem socket crosses a bind mount. The control protocol served is
// identical to the Unix-socket transport.
//
// @arg ctx Context whose cancellation stops the accept loop and closes the listener.
// @arg port The guest AF_VSOCK port to listen on.
// @error error if the vsock listener cannot be created or the accept loop fails for a reason other than ctx cancellation.
//
// @testcase TestListenVsockReturns returns promptly (an error when AF_VSOCK is unavailable, or nil once ctx is cancelled) rather than hanging.
func (a *Guest) ListenVsockAndServe(ctx context.Context, port uint32) error {
	ln, err := vsock.Listen(port, nil)
	if err != nil {
		return fmt.Errorf("listening on vsock port %d: %w", port, err)
	}
	defer ln.Close()
	return a.serve(ctx, ln)
}

// serve runs the accept loop on ln, dispatching each connection to handleConn,
// until ctx is cancelled (a clean stop) or Accept fails for another reason. It is
// transport-agnostic so the Unix-socket and vsock entrypoints share it.
//
// @arg ctx Context whose cancellation closes the listener and ends the loop.
// @arg ln The listener to accept control connections on.
// @error error if the accept loop fails for a reason other than ctx cancellation.
//
// @testcase TestGuestLifecycle serves over a listener via serve.
func (a *Guest) serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accepting control connection: %w", err)
		}
		go a.handleConn(ctx, conn)
	}
}

// handleConn reads framed verb requests from one control connection and
// dispatches each, replying with a framed response. The Dial verb is terminal:
// after its response the connection becomes a raw byte pipe and the loop ends.
//
// @arg ctx Context for the verbs (Exec honours its cancellation).
// @arg conn The control connection to serve.
//
// @testcase TestGuestLifecycle issues each verb over a connection handled here.
func (a *Guest) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	for {
		var r req
		if err := readFrame(conn, &r); err != nil {
			return
		}
		if r.Verb == verbDial {
			a.handleDial(conn, r.Data)
			return
		}
		data, err := a.dispatch(ctx, r)
		if err != nil {
			_ = writeFrame(conn, resp{Err: err.Error()})
			continue
		}
		if err := writeFrame(conn, resp{Data: data}); err != nil {
			return
		}
	}
}

// dispatch runs one non-Dial verb and returns its JSON-encoded response payload.
//
// @arg ctx Context for the verb.
// @arg r The decoded request envelope.
// @return json.RawMessage The verb's response payload (nil for verbs that return none).
// @error error if the verb is unknown, its payload is malformed, or it fails.
//
// @testcase TestGuestLifecycle exercises each verb through dispatch.
// @testcase TestGuestUnknownVerb returns an error for an unrecognised verb.
func (a *Guest) dispatch(ctx context.Context, r req) (json.RawMessage, error) {
	switch r.Verb {
	case verbInit:
		var in InitReq
		if err := json.Unmarshal(r.Data, &in); err != nil {
			return nil, fmt.Errorf("decoding init: %w", err)
		}
		out, err := a.handleInit(ctx, in)
		if err != nil {
			return nil, err
		}
		return json.Marshal(out)
	case verbExec:
		var in execReq
		if err := json.Unmarshal(r.Data, &in); err != nil {
			return nil, fmt.Errorf("decoding exec: %w", err)
		}
		out, err := a.handleExec(ctx, in)
		if err != nil {
			return nil, err
		}
		return json.Marshal(out)
	default:
		return nil, fmt.Errorf("unknown verb %q", r.Verb)
	}
}

// handleInit records the box parameters, writes the injected files, and runs the
// host-provided init script (if any). It must be called once; it is the box's
// only provisioning step. A failing init script is not a transport error: it is
// reported in the returned InitResp so the host can keep the box as a broken one
// the operator can inspect, with the script's output, rather than tearing it down
// silently.
//
// @arg ctx Context for the init script (its cancellation stops the script).
// @arg in The init request carrying the files, env, and init script.
// @return InitResp The init outcome; ScriptFailed is set (with the reason and output) when the init script fails.
// @error error if a file cannot be written or Init was already called.
//
// @testcase TestGuestLifecycle injects files via Init.
// @testcase TestGuestInitWritesFiles writes each file with its mode and owner.
// @testcase TestGuestInitRunsScript runs the init script as the box user.
// @testcase TestGuestInitScriptFailureReportsBroken reports a non-zero init script as a broken box.
func (a *Guest) handleInit(ctx context.Context, in InitReq) (InitResp, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inited {
		return InitResp{}, errors.New("already initialised")
	}
	for _, f := range in.Files {
		if err := writeInjectFile(f); err != nil {
			return InitResp{}, err
		}
	}
	// Spoke --copy files are written owned by the box user (the same credential the
	// init script and workload run as), overriding whatever UID/GID they carry, so
	// the workload can read and write what the spoke staged into the box. Files
	// above keep their caller-set owner (root-owned secrets stay root-owned).
	uid, gid := 0, 0
	if a.cred != nil {
		uid, gid = int(a.cred.Uid), int(a.cred.Gid)
	}
	for _, f := range in.CopyFiles {
		f.UID, f.GID = uid, gid
		if err := writeInjectFile(f); err != nil {
			return InitResp{}, err
		}
	}
	// Record the params before running the init script so it (via entryEnv) sees
	// the box's HOME/PATH/env. inited stays false until the script succeeds, so a
	// failing script leaves the box reported as broken.
	a.initReq = in
	if len(in.InitScript) > 0 {
		if output, err := a.runInitScript(ctx, in); err != nil {
			// Report the failure as data, not a transport error, so the host keeps the
			// box and surfaces the script output as a broken box instead of destroying it.
			return InitResp{ScriptFailed: true, ScriptError: err.Error(), ScriptOutput: output}, nil
		}
	}
	a.inited = true
	return InitResp{}, nil
}

// runInitScript writes the host-provided init script into the box and runs it to
// completion as the same (unprivileged) credential the box's workload runs as,
// with the box environment and the box user's home as the working directory. Its
// combined output is captured and returned (as a bounded tail) alongside a
// descriptive error on a non-zero exit, launch failure, or timeout, so the caller
// can surface both the reason and the script's output; a successful run returns an
// empty output and a nil error. The run is bounded by in.InitScriptTimeout (or
// defaultInitScriptTimeout when unset), so a hung script cannot wedge box
// creation. Callers hold a.mu.
//
// @arg ctx Context whose cancellation stops the script.
// @arg in The init request carrying the script bytes and its timeout.
// @return string The script's captured output tail (empty on success).
// @error error if the script cannot be written or launched, or exits non-zero.
//
// @testcase TestGuestInitRunsScript runs the script as the box user.
// @testcase TestGuestInitScriptFailureReportsBroken surfaces a non-zero exit with its output.
func (a *Guest) runInitScript(ctx context.Context, in InitReq) (string, error) {
	uid, gid := 0, 0
	if a.cred != nil {
		uid, gid = int(a.cred.Uid), int(a.cred.Gid)
	}
	if err := writeInjectFile(sandbox.InjectFile{
		Path: a.initScriptPath, Content: in.InitScript, Mode: 0o755, UID: uid, GID: gid,
	}); err != nil {
		return "", fmt.Errorf("writing init script: %w", err)
	}

	timeout := in.InitScriptTimeout
	if timeout <= 0 {
		timeout = defaultInitScriptTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, a.initScriptPath)
	cmd.Env = a.entryEnv()
	if home := homeFromEnv(cmd.Env); home != "" {
		cmd.Dir = home
	}
	if a.cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: a.cred}
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	a.log.Info("box init script finished", "err", err, "output_bytes", out.Len())
	if err != nil {
		tail := strings.TrimSpace(capOutput(out.Bytes()))
		if runCtx.Err() != nil {
			return tail, fmt.Errorf("init script did not finish within %s", timeout)
		}
		return tail, fmt.Errorf("init script failed: %w", err)
	}
	return "", nil
}

// homeFromEnv returns the value of the HOME assignment in env, or empty when none
// is present, so the init script runs from the box user's home directory.
//
// @arg env The environment slice to search.
// @return string The HOME value, or empty when env sets no HOME.
//
// @testcase TestGuestInitRunsScript relies on homeFromEnv to run the script from HOME.
func homeFromEnv(env []string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "HOME="); ok {
			return v
		}
	}
	return ""
}

// handleExec runs cmd inside the box as a separate process and returns its
// captured, length-capped output and exit code.
//
// @arg ctx Context whose cancellation kills the command.
// @arg in The exec request carrying the command and its arguments.
// @return sandbox.ExecResult The command's stdout, stderr, and exit code.
// @error error if no command is given or the command cannot be started.
//
// @testcase TestGuestLifecycle runs a command via Exec and captures its output.
// @testcase TestGuestExecNonZeroExit reports a non-zero exit code without erroring.
// @testcase TestGuestRunsAsCredential runs an Exec command under a configured credential.
func (a *Guest) handleExec(ctx context.Context, in execReq) (sandbox.ExecResult, error) {
	if len(in.Cmd) == 0 {
		return sandbox.ExecResult{}, errors.New("empty command")
	}
	cmd := exec.CommandContext(ctx, in.Cmd[0], in.Cmd[1:]...)
	cmd.Env = a.entryEnv()
	if a.cred != nil {
		// Run Exec as the same unprivileged box user the box's workload runs as, so
		// `llmbox exec` sees the box exactly as its own processes do (and can sudo).
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: a.cred}
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	exit := cmd.ProcessState.ExitCode()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return sandbox.ExecResult{}, fmt.Errorf("running command: %w", err)
	}
	return sandbox.ExecResult{
		Stdout:   capOutput(stdout.Bytes()),
		Stderr:   capOutput(stderr.Bytes()),
		ExitCode: exit,
	}, nil
}

// handleDial connects to the requested localhost port inside the box and, on
// success, splices the control connection to it so the host can reach an in-box
// service through the same socket. On failure it writes an error response and
// returns without splicing.
//
// @arg conn The control connection to splice to the dialled port.
// @arg data The JSON-encoded dialReq naming the port.
//
// @testcase TestClientDialPort forwards bytes between the conn and a localhost listener.
// @testcase TestGuestDialRejectsBadPort writes an error response for an out-of-range port.
func (a *Guest) handleDial(conn net.Conn, data json.RawMessage) {
	var in dialReq
	if err := json.Unmarshal(data, &in); err != nil {
		_ = writeFrame(conn, resp{Err: fmt.Sprintf("decoding dial: %v", err)})
		return
	}
	if in.Port < 1 || in.Port > 65535 {
		_ = writeFrame(conn, resp{Err: fmt.Sprintf("port %d out of range", in.Port)})
		return
	}
	backend, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", in.Port), 10*time.Second)
	if err != nil {
		_ = writeFrame(conn, resp{Err: fmt.Sprintf("dialing localhost:%d: %v", in.Port, err)})
		return
	}
	defer backend.Close()
	if err := writeFrame(conn, resp{}); err != nil {
		return
	}
	splice(conn, backend)
}

// writeInjectFile writes one injected file, creating its parent directories and
// applying its mode and owner.
//
// @arg f The file to write.
// @error error if the directory or file cannot be created or chowned.
//
// @testcase TestGuestInitWritesFiles writes a file with the requested mode and owner.
func writeInjectFile(f sandbox.InjectFile) error {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return fmt.Errorf("creating dir for %s: %w", f.Path, err)
	}
	mode := os.FileMode(f.Mode)
	if mode == 0 {
		mode = 0o644
	}
	if err := os.WriteFile(f.Path, f.Content, mode); err != nil {
		return fmt.Errorf("writing %s: %w", f.Path, err)
	}
	// Re-apply the mode in case umask narrowed WriteFile's, then set the owner.
	if err := os.Chmod(f.Path, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", f.Path, err)
	}
	if f.UID != 0 || f.GID != 0 {
		if err := os.Chown(f.Path, f.UID, f.GID); err != nil {
			return fmt.Errorf("chown %s: %w", f.Path, err)
		}
	}
	return nil
}

// splice copies bytes in both directions between a and b until either side
// closes, then returns.
//
// @arg a One end of the proxied connection.
// @arg b The other end of the proxied connection.
//
// @testcase TestClientDialPort moves bytes both ways through splice.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		// Unblock the other direction's Read by closing both ends.
		_ = dst.Close()
		_ = src.Close()
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}
