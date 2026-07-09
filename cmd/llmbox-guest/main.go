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
	"log/slog"
	"os"
	"os/signal"
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
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := run(ctx, *socket, uint32(*vsockPort), *boxapiSocket, uint32(*boxapiPort), *claudeCmd, log); err != nil {
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
// @arg log The logger the guest uses.
// @error error if the guest cannot serve the selected transport.
//
// @testcase TestRunServesAndStops serves a socket then stops cleanly on cancel.
// @testcase TestRunStartsBoxAPIBridge serves the box API bridge alongside the vsock control channel.
func run(ctx context.Context, socket string, vsockPort uint32, boxapiSocket string, boxapiPort uint32, claudeCmd string, log *slog.Logger) error {
	a := guest.New(guest.Options{ClaudeCmd: claudeCmd, Log: log})
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
