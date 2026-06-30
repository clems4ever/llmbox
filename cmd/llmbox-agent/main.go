// Command llmbox-agent is the in-box guest agent. It is the box entrypoint (run
// under tini, which reaps zombies and forwards signals) and serves the box
// control verbs over a Unix-domain socket the host bind-mounts in. See
// internal/agent for the protocol and behaviour.
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

// main parses flags, then serves the control socket until a termination signal
// arrives.
//
// @testcase TestRunServesAndStops covers the serve loop main delegates to.
func main() {
	socket := flag.String("socket", "/run/llmbox/control.sock", "path of the control socket to serve")
	claudeCmd := flag.String("claude", "claude", "the claude command used in the box entrypoint")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := run(ctx, *socket, *claudeCmd, log); err != nil {
		log.Error("agent exited", "err", err)
		os.Exit(1)
	}
}

// run builds the agent and serves the control socket at the given path until ctx
// is cancelled. It is the testable core of main.
//
// @arg ctx Context whose cancellation stops the agent.
// @arg socket The control-socket path to serve.
// @arg claudeCmd The claude command used in the box entrypoint.
// @arg log The logger the agent uses.
// @error error if the agent cannot serve the socket.
//
// @testcase TestRunServesAndStops serves a socket then stops cleanly on cancel.
func run(ctx context.Context, socket, claudeCmd string, log *slog.Logger) error {
	a := agent.New(agent.Options{ClaudeCmd: claudeCmd, Log: log})
	return a.ListenAndServe(ctx, socket)
}
