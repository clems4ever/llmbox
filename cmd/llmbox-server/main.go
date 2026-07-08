// Command llmbox-server runs the llmbox server that manages sandboxed Claude
// containers ("llmboxes") and lets an end user authenticate each one via OAuth in
// their browser — never routing the OAuth secret through the chatbot.
//
// One process serves everything on a single HTTP port (http_addr):
//
//	/api/v1/...     box-control JSON API — the UI and the stand-alone llmbox-mcp binary call it
//	/auth/{token}   web page where the user pastes their OAuth code (+ admin UI, health)
//
// The MCP protocol itself is served by a separate binary (llmbox-mcp), which
// forwards every call to the box-control API over HTTP. The box-control API is
// currently unauthenticated (API-key / UI-session auth is planned), so run llmbox
// behind an authenticating reverse proxy in front of trusted callers.
//
// Boxes that are never authenticated are destroyed after a TTL.
//
// Configuration is a YAML file (default ./llmbox.yaml, override with --config).
// Every field is optional; unset fields fall back to built-in defaults:
//
//	http_addr:    ":8080"                  # HTTP listen address (UI + box-control API)
//	public_url:   "http://localhost:8080"  # external base URL for auth links
//	auth_ttl:     "5m"                      # how long a box may stay un-authenticated (Go duration)
//	state_file:   "llmbox-sessions.db"     # SQLite file persisting the session registry
//
// The hub runs no box backend of its own, so it holds no box-provisioning
// config: the box image, resource caps, and backend are the spoke's concern and
// are set with llmbox-spoke flags (e.g. --image).
//
// By default the single HTTP server is served in the clear and a loud startup
// warning is logged, since it is meant to sit behind a TLS-terminating reverse
// proxy. To terminate TLS in-process instead, enable the tls block:
//
//	tls:
//	  enabled:   true
//	  cert_file: "/etc/llmbox/tls-cert.pem"  # PEM certificate (full chain, leaf first)
//	  key_file:  "/etc/llmbox/tls-key.pem"   # PEM private key matching cert_file
//
// Box lifecycle hooks (optional). hooks is a list of external executables llmbox
// runs at box.create and box.destroy, exchanging JSON per the hookproto
// contract. A hook may inject files into each box and persist opaque state; this
// is how integrations like granular plug in without llmbox depending on them:
//
//	hooks:
//	  - /opt/granular-llmbox/hook
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/clems4ever/llmbox/internal/hub"
	"github.com/clems4ever/llmbox/internal/hub/config"
	"github.com/clems4ever/llmbox/internal/hub/token"
)

const (
	name    = "llmbox-server"
	version = "v0.1.0"
)

// main executes the root command and exits non-zero on a fatal error.
//
// @testcase TestNewRootCmd covers the command wiring main relies on.
func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// newRootCmd builds the Cobra command tree: the root command loads the YAML
// config and runs the server (the hub), a "version" subcommand prints the build
// version, and a "token" subcommand manages the one-time join tokens the hub
// issues to enroll spokes (it operates on the hub's state file, so it runs here
// rather than on the spoke). The spoke and the MCP front-end are separate binaries
// (llmbox-spoke, llmbox-mcp). The --config/-c flag selects the config file
// (default ./llmbox.yaml); when that default is absent, built-in defaults are used.
//
// @return *cobra.Command The configured root command, ready to Execute.
//
// @testcase TestNewRootCmd checks the command wiring (use, subcommands, flag).
func newRootCmd() *cobra.Command {
	var configPath string

	rootCmd := &cobra.Command{
		Use:           name,
		Short:         "Run the llmbox server that manages sandboxed Claude containers",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadConfig(configPath, cmd.Flags().Changed("config"))
			if err != nil {
				return err
			}
			return hub.Serve(cmd.Context(), cfg, name, version)
		},
	}
	rootCmd.Flags().StringVarP(&configPath, "config", "c", "llmbox.yaml", "path to the YAML configuration file")

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print the llmbox-server version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", name, version)
		},
	}
	rootCmd.AddCommand(versionCmd)
	// Join-token management runs on the hub (it operates on the hub's state file),
	// so the `token` command lives here rather than on the spoke.
	rootCmd.AddCommand(token.NewCmd())

	return rootCmd
}
