// Command llmbox-guest is the in-box guest. It is the box entrypoint and
// serves the box control verbs over one of two transports: a Unix-domain socket
// the host bind-mounts in (the Docker backend, run under tini), or a guest
// AF_VSOCK port the hypervisor forwards to the host (the Firecracker backend,
// run as the microVM's init). When --vsock-port is non-zero the guest serves over
// vsock; otherwise it serves the --socket path. See internal/guest for the
// protocol and behaviour.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"syscall"

	"github.com/clems4ever/llmbox/internal/guest"
)

// main parses flags, then serves the control channel until a termination signal
// arrives.
//
// @testcase TestRunServesAndStops covers the serve loop main delegates to.
func main() {
	socket := flag.String("socket", "/run/llmbox/control.sock", "path of the Unix control socket to serve (used when --vsock-port is 0)")
	vsockPort := flag.Uint("vsock-port", 0, "guest AF_VSOCK port to serve on; when non-zero the guest serves over vsock instead of --socket")
	boxapiSocket := flag.String("boxapi-socket", "/run/llmbox/boxapi.sock", "in-guest Unix socket bridged to the host box-port API (vsock mode only)")
	boxapiPort := flag.Uint("boxapi-port", 0, "host vsock port the box-port API socket is bridged to; 0 disables the bridge (vsock mode only)")
	claudeCmd := flag.String("claude", "claude", "the claude command used in the box entrypoint")
	runAsUser := flag.String("user", "", "unprivileged box account to run claude and Exec commands as (must exist in the box's /etc/passwd); empty runs them as the guest's own user (root)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := run(ctx, *socket, uint32(*vsockPort), *boxapiSocket, uint32(*boxapiPort), *claudeCmd, *runAsUser, log); err != nil {
		log.Error("guest exited", "err", err)
		os.Exit(1)
	}
}

// run builds the guest and serves the control channel until ctx is cancelled: on
// the vsock port when vsockPort is non-zero, otherwise on the Unix socket path.
// In vsock mode it also bridges the in-guest box-port API socket to the host
// vsock port when one is configured; a bridge failure is logged but never takes
// the control channel down with it. It is the testable core of main.
//
// @arg ctx Context whose cancellation stops the guest.
// @arg socket The Unix control-socket path to serve when vsockPort is 0.
// @arg vsockPort The guest AF_VSOCK port to serve on; 0 selects the Unix socket.
// @arg boxapiSocket The in-guest Unix socket bridged to the host box-port API (vsock mode only).
// @arg boxapiPort The host vsock port the box-port API bridges to; 0 disables the bridge.
// @arg claudeCmd The claude command used in the box entrypoint.
// @arg runAsUser The unprivileged box account claude and Exec commands run as; empty runs them as the guest's own user.
// @arg log The logger the guest uses.
// @error error if runAsUser is set but absent from the box, or the guest cannot serve the selected transport.
//
// @testcase TestRunServesAndStops serves a socket then stops cleanly on cancel.
// @testcase TestRunStartsBoxAPIBridge serves the box API bridge alongside the vsock control channel.
// @testcase TestRunRejectsUnknownUser fails fast when runAsUser names no account.
func run(ctx context.Context, socket string, vsockPort uint32, boxapiSocket string, boxapiPort uint32, claudeCmd, runAsUser string, log *slog.Logger) error {
	cred, home, err := lookupUser(runAsUser)
	if err != nil {
		return err
	}
	a := guest.New(guest.Options{ClaudeCmd: claudeCmd, Credential: cred, Home: home, Log: log})
	if vsockPort != 0 {
		if boxapiPort != 0 {
			// The bridge is best-effort: a box without its port API is degraded,
			// but one without its control channel is dead — so bridge failures
			// are logged, never returned.
			go func() {
				if err := guest.RunBoxAPIBridge(ctx, boxapiSocket, guest.DialHostVsock(boxapiPort), log); err != nil {
					log.Error("box API bridge exited", "err", err)
				}
			}()
		}
		return a.ListenVsockAndServe(ctx, vsockPort)
	}
	return a.ListenAndServe(ctx, socket)
}

// lookupUser resolves an unprivileged box account name to the OS credential and
// home directory the guest drops box processes to. An empty name means "do not
// drop privileges" and yields a nil credential, preserving the run-as-root
// behaviour. The credential carries the account's supplementary groups (so the
// box user keeps memberships like docker) when they can be read. A missing
// account is an error, so a misconfigured --user aborts the guest loudly rather
// than silently running claude as root.
//
// @arg name The box account name, or empty to keep running as the guest's user.
// @return *syscall.Credential The uid/gid/groups to run box processes as, or nil when name is empty.
// @return string The account's home directory, or empty when name is empty.
// @error error if name is non-empty but names no account, or its ids are unparseable.
//
// @testcase TestRunRejectsUnknownUser drives lookupUser through run with a bogus name.
// @testcase TestLookupUserResolvesCurrentUser resolves the running user to its own uid/gid and home.
func lookupUser(name string) (*syscall.Credential, string, error) {
	if name == "" {
		return nil, "", nil
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil, "", fmt.Errorf("looking up box user %q: %w", name, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return nil, "", fmt.Errorf("parsing uid %q for box user %q: %w", u.Uid, name, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return nil, "", fmt.Errorf("parsing gid %q for box user %q: %w", u.Gid, name, err)
	}
	cred := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	// Preserve supplementary group membership (e.g. docker, sudo) when it can be
	// read; a failure here only costs those extra groups, not the privilege drop.
	if gids, err := u.GroupIds(); err == nil {
		groups := make([]uint32, 0, len(gids))
		for _, g := range gids {
			if n, aerr := strconv.Atoi(g); aerr == nil {
				groups = append(groups, uint32(n))
			}
		}
		cred.Groups = groups
	}
	return cred, u.HomeDir, nil
}
