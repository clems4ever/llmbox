// Command llmbox runs a server that manages sandboxed Claude containers
// ("llmboxes") and lets an end user authenticate each one via OAuth in their
// browser — never routing the OAuth secret through the chatbot.
//
// One process serves two things on the same HTTP port:
//
//	/              MCP (streamable HTTP) — a chatbot creates/lists/destroys boxes
//	/auth/{token}  web page where the user pastes their OAuth code
//
// Boxes that are never authenticated are destroyed after a TTL.
//
// Configuration is a YAML file (default ./llmbox.yaml, override with --config).
// Every field is optional; unset fields fall back to built-in defaults:
//
//	http_addr:    ":8080"                  # listen address
//	public_url:   "http://localhost:8080"  # external base URL for auth links
//	claude_image: "ghcr.io/clems4ever/llmbox-box:latest"  # base image per box; any glibc image with a CA bundle works — Claude is injected, not baked in
//	claude_bin:   "/opt/llmbox/claude"     # standalone Claude binary injected into each box
//	remote_args:  "--spawn same-dir"       # args passed to `claude remote-control`
//	auth_ttl:     "5m"                      # how long a box may stay un-authenticated (Go duration)
//	state_file:   "llmbox-sessions.db"     # bbolt file persisting the session registry
//
// Box lifecycle hooks (optional). hooks is a list of external executables llmbox
// runs at box.create and box.destroy, exchanging JSON per the hookproto
// contract. A hook may inject files into each box and persist opaque state; this
// is how integrations like granular plug in without llmbox depending on them.
// box_peers is a list of container names connected into every box's network so
// boxes can reach them (e.g. a hook's resource servers):
//
//	hooks:
//	  - /opt/granular-llmbox/hook
//	box_peers:
//	  - granular-github
//	  - granular-as
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/clems4ever/llmbox/internal/cluster"
	"github.com/clems4ever/llmbox/internal/config"
	"github.com/clems4ever/llmbox/internal/docker"
	"github.com/clems4ever/llmbox/internal/hooks"
	"github.com/clems4ever/llmbox/internal/server"
)

const (
	name    = "llmbox"
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
// version, and a "spoke" subcommand runs a hub-and-spoke spoke (with a
// "spoke token create" child for minting join tokens). The --config/-c flag
// selects the config file (default ./llmbox.yaml); when that default is absent,
// built-in defaults are used.
//
// @return *cobra.Command The configured root command, ready to Execute.
//
// @testcase TestNewRootCmd checks the command wiring (use, subcommands, flag).
func newRootCmd() *cobra.Command {
	var configPath string

	rootCmd := &cobra.Command{
		Use:           name,
		Short:         "Run the llmbox MCP server that manages sandboxed Claude containers",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(configPath, cmd.Flags().Changed("config"))
			if err != nil {
				return err
			}
			return run(cmd.Context(), cfg)
		},
	}
	rootCmd.Flags().StringVarP(&configPath, "config", "c", "llmbox.yaml", "path to the YAML configuration file")

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print the llmbox version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", name, version)
		},
	}
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(newSpokeCmd())

	return rootCmd
}

// loadConfig loads the YAML config at path. When the path was not given
// explicitly on the command line and the default file is absent, it prints a
// warning to stderr and returns the built-in defaults so llmbox runs without a
// config file; an explicitly named missing or invalid file is an error. The
// warning exists because a silent default state_file is a common cause of a
// command (e.g. `spoke token create`) writing to a different store than the
// running hub reads.
//
// @arg path The config file path.
// @arg explicit Whether --config was set on the command line.
// @return *config.Config The loaded (or default) configuration.
// @error error if an explicitly named file is missing, or any named file is invalid.
//
// @testcase TestLoadConfigDefaultsWhenAbsent returns defaults for a missing implicit file.
// @testcase TestLoadConfigErrorsWhenExplicitMissing errors for a missing explicit file.
// @testcase TestLoadConfigReadsFile parses an existing config file.
func loadConfig(path string, explicit bool) (*config.Config, error) {
	if !explicit {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			// Falling back to built-in defaults silently has bitten operators:
			// a command run without the hub's config (e.g. `spoke token create`
			// via docker exec) ends up using DefaultStateFile, a different store
			// than the running hub, so its tokens are never seen. Make the
			// fallback visible.
			fmt.Fprintf(os.Stderr, "warning: no config file at %q; using built-in defaults (state_file %q)\n", path, config.DefaultStateFile)
			return config.Default(), nil
		}
	}
	return config.Load(path)
}

// run wires up the Docker manager, session store, and HTTP server from the given
// configuration, then serves until interrupted. The SIGINT/SIGTERM context is
// established before the server is built so that, when cfg.Cluster.Enabled is
// set, the cluster hub created via cluster.NewHub and attached with srv.SetHub
// shares the server's lifetime; remote spokes then join at /spoke/connect while
// boxes still default to the in-process "local" spoke.
//
// @arg parent Base context; serving stops when it (or a SIGINT/SIGTERM) fires.
// @arg cfg The loaded configuration.
// @error error if the manager, the store, or the HTTP server fails.
//
// @testcase TestNewRootCmd covers the command that loads cfg and calls run.
func run(parent context.Context, cfg *config.Config) error {
	authTTL := time.Duration(cfg.AuthTTL)

	mgr, err := docker.NewManager(cfg.ClaudeImage, cfg.RemoteArgs, cfg.ClaudeBin, cfg.BoxPeers)
	if err != nil {
		return err
	}
	defer func() {
		if err := mgr.Close(); err != nil {
			log.Printf("closing docker manager: %v", err)
		}
	}()

	// Optional box lifecycle hooks: external programs run at box.create/destroy.
	// New returns nil (no hooks) when the list is empty.
	hookRunner := hooks.New(cfg.Hooks)

	// Persist the session registry so auth links survive a server restart.
	if dir := filepath.Dir(cfg.StateFile); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	store, err := server.OpenStore(cfg.StateFile)
	if err != nil {
		return err
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Printf("closing session store: %v", err)
		}
	}()

	// Activation auth (OIDC). Returns nil when no provider is configured, which
	// leaves box activation unauthenticated.
	auth, err := server.NewAuthenticator(parent, cfg.Auth)
	if err != nil {
		return err
	}
	if auth == nil {
		log.Print("activation auth is DISABLED: anyone with a box's auth-page URL can activate it; configure auth.google to require sign-in")
	}

	// Cancel background work on signal (or when the parent context fires).
	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := server.New(mgr, hookRunner, cfg.PublicURL, authTTL, store, auth)

	// Hub-and-spoke clustering: when enabled, accept spoke connections and let
	// boxes be placed on remote spokes (boxes still default to the local spoke).
	if cfg.Cluster.Enabled {
		srv.SetHub(cluster.NewHub(ctx, store, nil, nil))
		log.Printf("clustering enabled: spokes may join at %s/spoke/connect", cfg.PublicURL)
	}

	httpSrv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: srv.Handler(srv.MCPServer(name, version)),
		// SubmitCode blocks while the box logs in, so allow long requests.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      90 * time.Second,
	}

	// Reload sessions saved before a restart, dropping any whose box is gone.
	if n, err := srv.Restore(ctx); err != nil {
		log.Printf("restore: %v", err)
	} else if n > 0 {
		log.Printf("restored %d session(s) from %s", n, cfg.StateFile)
	}

	go srv.ReapLoop(ctx, 30*time.Second, func(msg string) { log.Print(msg) })

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutCtx); err != nil {
			log.Printf("graceful shutdown failed: %v", err)
		}
	}()

	log.Printf("%s %s listening on %s (public URL %s, auth TTL %s)", name, version, cfg.HTTPAddr, cfg.PublicURL, authTTL)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
