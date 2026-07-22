// Command llmbox-server runs the llmbox server that manages sandboxed boxes
// ("llmboxes") — the box infrastructure; each box's workload is installed by the
// spoke's init script.
//
// One process serves everything on a single HTTP port (http_addr):
//
//	/api/v1/...     box-control JSON API — the UI and any programmatic caller drive boxes through it
//	/admin, /signin admin web UI and the OIDC sign-in that gates it and the per-box proxies (+ health)
//
// The box-control API is authenticated: callers present an API key as a bearer
// token (minted with `llmbox-server apikey add`), and the admin web app uses the
// signed-in admin's login cookie plus a CSRF header.
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
	"github.com/clems4ever/llmbox/internal/hub/apikey"
	"github.com/clems4ever/llmbox/internal/hub/config"
	"github.com/clems4ever/llmbox/internal/hub/token"
)

const name = "llmbox-server"

// version is the binary's reported version. It defaults to "dev" for local
// builds and is overridden at release time by GoReleaser via the linker flag
// -X main.version=<tag> (see .goreleaser.yaml).
var version = "dev"

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
// rather than on the spoke). The spoke is a separate binary (llmbox-spoke). The
// --config/-c flag selects the config file
// (default ./llmbox.yaml); when that default is absent, built-in defaults are used.
//
// @return *cobra.Command The configured root command, ready to Execute.
//
// @testcase TestNewRootCmd checks the command wiring (use, subcommands, flag).
func newRootCmd() *cobra.Command {
	var configPath string

	rootCmd := &cobra.Command{
		Use:           name,
		Short:         "Run the llmbox server that manages sandboxed boxes",
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
	// Join-token and API-key management run on the hub (they operate on the hub's
	// state file), so the `token` and `apikey` commands live here rather than on
	// the spoke.
	rootCmd.AddCommand(token.NewCmd())
	rootCmd.AddCommand(apikey.NewCmd())

	return rootCmd
}
