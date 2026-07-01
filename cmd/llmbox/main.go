// Command llmbox runs a server that manages sandboxed Claude containers
// ("llmboxes") and lets an end user authenticate each one via OAuth in their
// browser — never routing the OAuth secret through the chatbot.
//
// One process serves two things on two separate HTTP ports:
//
//	mcp_addr   /api/v1/...     box-control JSON API — the stand-alone llmbox-mcp binary forwards MCP tool calls here
//	http_addr  /auth/{token}   web page where the user pastes their OAuth code (+ admin UI, health)
//
// The MCP protocol itself is served by a separate binary (llmbox-mcp), which
// forwards every call to the box-control API over HTTP. That API port is split
// out so it can sit behind its own authenticating reverse proxy (e.g.
// oauth2-proxy), independently of the public UI/API port.
//
// Boxes that are never authenticated are destroyed after a TTL.
//
// Configuration is a YAML file (default ./llmbox.yaml, override with --config).
// Every field is optional; unset fields fall back to built-in defaults:
//
//	http_addr:    ":8080"                  # UI/API listen address
//	mcp_addr:     ":8081"                  # box-control API listen address (put behind an auth proxy; llmbox-mcp forwards here)
//	public_url:   "http://localhost:8080"  # external base URL for auth links
//	claude_image: "ghcr.io/clems4ever/llmbox-box:latest"  # base image per box; must bake in Claude, tini, util-linux, and a CA bundle (see Dockerfile.box)
//	remote_args:  "--spawn same-dir"       # args passed to `claude remote-control`
//	auth_ttl:     "5m"                      # how long a box may stay un-authenticated (Go duration)
//	state_file:   "llmbox-sessions.db"     # SQLite file persisting the session registry
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

	"github.com/clems4ever/llmbox/internal/auth"
	"github.com/clems4ever/llmbox/internal/box"
	"github.com/clems4ever/llmbox/internal/cli"
	"github.com/clems4ever/llmbox/internal/cluster"
	"github.com/clems4ever/llmbox/internal/config"
	"github.com/clems4ever/llmbox/internal/docker"
	"github.com/clems4ever/llmbox/internal/hooks"
	"github.com/clems4ever/llmbox/internal/mcpapi"
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
// config and runs the server (the hub), and a "version" subcommand prints the
// build version. The spoke and the MCP front-end are separate binaries
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
		Short:         "Run the llmbox MCP server that manages sandboxed Claude containers",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := cli.LoadConfig(configPath, cmd.Flags().Changed("config"))
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

	return rootCmd
}

// run assembles and serves the llmbox hub from cfg: it builds the Docker manager
// (applying the configured per-box resource limits), opens the session store,
// sets up optional lifecycle hooks and activation auth, optionally enables
// hub-and-spoke clustering and HTTP proxying of box ports, restores persisted
// sessions, starts the orphan reaper, and serves the box-control API and the
// UI/API on their two separate ports until the parent context is cancelled
// (SIGINT/SIGTERM) at which point both shut down gracefully.
//
// @arg parent The parent context whose cancellation (or a termination signal) triggers graceful shutdown.
// @arg cfg The resolved configuration driving the manager, store, auth, clustering, and HTTP servers.
// @error error if the manager, store, or authenticator cannot be built, a state directory cannot be created, or either HTTP server fails for a reason other than a clean shutdown.
//
// @testcase TestNewRootCmd covers the command that loads cfg and calls run.
func run(parent context.Context, cfg *config.Config) error {
	authTTL := time.Duration(cfg.AuthTTL)

	// Resolve the per-box image once, here on the hub: an unset claude_image falls
	// back to the built-in default at this single point, so every box-creation
	// request carries an explicit image. Spokes are config-free and supply none of
	// their own — the hub is the only source of the box image, default included.
	boxImage := cfg.ClaudeImage
	if boxImage == "" {
		boxImage = docker.DefaultImage
	}

	prov, err := docker.NewProvisioner(boxImage, cfg.Box.SocketDir, cfg.BoxPeers)
	if err != nil {
		return err
	}
	prov.SetPerBoxLimits(cli.BoxLimits(cfg.Box))
	prov.SetRegistryAuths(cli.RegistryAuths(cfg.Registries))
	// Scope this hub's local boxes to its configured namespace so it never lists,
	// reaps, or destroys boxes owned by another spoke sharing the Docker daemon.
	prov.SetNamespace(cfg.Box.Namespace)
	defer func() {
		if err := prov.Close(); err != nil {
			log.Printf("closing docker provisioner: %v", err)
		}
	}()
	mgr := box.NewManager(prov, box.Config{RemoteArgs: cfg.RemoteArgs, MaxBoxes: cfg.Box.MaxBoxes})

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
	authr, err := auth.New(parent, cfg.Auth)
	if err != nil {
		return err
	}
	if authr == nil {
		log.Print("activation auth is DISABLED: anyone with a box's auth-page URL can activate it; configure auth.google to require sign-in")
	}

	// Cancel background work on signal (or when the parent context fires).
	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := server.New(mgr, hookRunner, cfg.PublicURL, authTTL, store, authr)
	srv.SetSpokeImage(cfg.Cluster.SpokeImage)
	srv.SetBoxImage(boxImage)
	srv.SetProxyBaseDomain(cfg.Proxy.BaseDomain)
	if cfg.Proxy.BaseDomain != "" {
		log.Printf("box HTTP proxying enabled at *.%s", cfg.Proxy.BaseDomain)
	}

	// Hub-and-spoke clustering: when enabled, accept spoke connections and let
	// boxes be placed on remote spokes (boxes still default to the local spoke).
	if cfg.Cluster.Enabled {
		srv.SetHub(cluster.NewHub(ctx, store, nil, nil))
		log.Printf("clustering enabled: spokes may join at %s/spoke/connect", cfg.PublicURL)
	}

	// Two listeners: the box-control API on its own port (meant to sit behind an
	// auth proxy) and the UI/API (auth pages, admin, health) on the public port.
	// The box-control port serves the mcpapi JSON API; the MCP protocol itself is
	// served by the stand-alone llmbox-mcp binary, which forwards to this API.
	mcpSrv := &http.Server{
		Addr:    cfg.MCPAddr,
		Handler: mcpapi.NewHandler(srv.MCPBackend()),
		// SubmitCode blocks while the box logs in, so allow long requests.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      90 * time.Second,
	}
	apiSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.APIHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      90 * time.Second,
	}
	servers := []*http.Server{mcpSrv, apiSrv}

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
		for _, hs := range servers {
			if err := hs.Shutdown(shutCtx); err != nil {
				log.Printf("graceful shutdown failed for %s: %v", hs.Addr, err)
			}
		}
	}()

	log.Printf("%s %s listening: UI/API on %s, box-control API on %s (public URL %s, auth TTL %s)", name, version, cfg.HTTPAddr, cfg.MCPAddr, cfg.PublicURL, authTTL)

	// Serve both ports. If either listener fails, cancel the context so the other
	// is shut down too, and surface the first real error.
	errCh := make(chan error, len(servers))
	for _, hs := range servers {
		go func(hs *http.Server) {
			if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("listening on %s: %w", hs.Addr, err)
				return
			}
			errCh <- nil
		}(hs)
	}

	var firstErr error
	for range servers {
		if err := <-errCh; err != nil && firstErr == nil {
			firstErr = err
			stop() // cancel ctx -> the goroutine above shuts both servers down
		}
	}
	return firstErr
}
