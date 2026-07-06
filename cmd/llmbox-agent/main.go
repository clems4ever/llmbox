// Command llmbox-agent is the in-box guest agent. It is the box entrypoint and
// serves the box control verbs over one of two transports: a Unix-domain socket
// the host bind-mounts in (the Docker backend, run under tini), or a guest
// AF_VSOCK port the hypervisor forwards to the host (the Firecracker backend,
// run as the microVM's init). When --vsock-port is non-zero the agent serves over
// vsock; otherwise it serves the --socket path. See internal/agent for the
// protocol and behaviour.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/clems4ever/llmbox/internal/agent"
)

// main parses flags, then serves the control channel until a termination signal
// arrives.
//
// @testcase TestRunServesAndStops covers the serve loop main delegates to.
func main() {
	socket := flag.String("socket", "/run/llmbox/control.sock", "path of the Unix control socket to serve (used when --vsock-port is 0)")
	vsockPort := flag.Uint("vsock-port", 0, "guest AF_VSOCK port to serve on; when non-zero the agent serves over vsock instead of --socket")
	claudeCmd := flag.String("claude", "claude", "the claude command used in the box entrypoint")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := run(ctx, *socket, uint32(*vsockPort), *claudeCmd, log); err != nil {
		log.Error("agent exited", "err", err)
		os.Exit(1)
	}
}

// run builds the agent and serves the control channel until ctx is cancelled: on
// the vsock port when vsockPort is non-zero, otherwise on the Unix socket path.
// It is the testable core of main.
//
// @arg ctx Context whose cancellation stops the agent.
// @arg socket The Unix control-socket path to serve when vsockPort is 0.
// @arg vsockPort The guest AF_VSOCK port to serve on; 0 selects the Unix socket.
// @arg claudeCmd The claude command used in the box entrypoint.
// @arg log The logger the agent uses.
// @error error if the agent cannot serve the selected transport.
//
// @testcase TestRunServesAndStops serves a socket then stops cleanly on cancel.
func run(ctx context.Context, socket string, vsockPort uint32, claudeCmd string, log *slog.Logger) error {
	a := agent.New(agent.Options{ClaudeCmd: claudeCmd, Log: log})
	if vsockPort != 0 {
		return a.ListenVsockAndServe(ctx, vsockPort)
	}
	return a.ListenAndServe(ctx, socket)
}
