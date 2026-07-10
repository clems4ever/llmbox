// Command llmbox-spoke runs a hub-and-spoke spoke: it connects to a hub over a
// WebSocket and serves box operations against the local Docker daemon, so boxes
// can be placed on this host from a central hub. Its `token` subcommand mints,
// lists, and revokes the one-time join tokens a hub issues to enroll spokes.
//
// It is a separate binary from the llmbox server (the hub) so a spoke host needs
// only this thin command and a Docker daemon, not the full server.
package main

import (
	"os"

	"github.com/clems4ever/llmbox/internal/spoke"
)

const name = "llmbox-spoke"

// version is the binary's reported version. It defaults to "dev" for local
// builds and is overridden at release time by GoReleaser via the linker flag
// -X main.version=<tag> (see .goreleaser.yaml).
var version = "dev"

// main builds the spoke command tree (from the spoke package) with this binary's
// name and version, executes it, and exits non-zero on a fatal error.
//
// @testcase TestMainBuildsRootCmd covers the command wiring main relies on.
func main() {
	if err := spoke.NewRootCmd(name, version).Execute(); err != nil {
		os.Exit(1)
	}
}
