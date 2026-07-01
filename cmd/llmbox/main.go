// Command llmbox runs a server that manages sandboxed Claude containers
// ("llmboxes") and lets an end user authenticate each one via OAuth in their
// browser — never routing the OAuth secret through the chatbot.
//
// One process serves two things on two separate HTTP ports:
//
//	mcp_addr   /              MCP (streamable HTTP) — a chatbot creates/lists/destroys boxes
//	http_addr  /auth/{token}  web page where the user pastes their OAuth code (+ admin UI, health)
//
// The MCP port is split out so it can sit behind its own authenticating reverse
// proxy (e.g. oauth2-proxy), independently of the public UI/API port.
//
// Boxes that are never authenticated are destroyed after a TTL.
//
// Configuration is a YAML file (default ./llmbox.yaml, override with --config).
// Every field is optional; unset fields fall back to built-in defaults:
//
//	http_addr:    ":8080"                  # UI/API listen address
//	mcp_addr:     ":8081"                  # MCP listen address (put behind an auth proxy)
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

	"github.com/docker/docker/api/types/registry"
	"github.com/spf13/cobra"

	"github.com/clems4ever/llmbox/internal/auth"
	"github.com/clems4ever/llmbox/internal/box"
	"github.com/clems4ever/llmbox/internal/cluster"
	"github.com/clems4ever/llmbox/internal/config"
	"github.com/clems4ever/llmbox/internal/docker"
	"github.com/clems4ever/llmbox/internal/hooks"
	"github.com/clems4ever/llmbox/internal/sandbox"
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

// boxLimits converts the YAML box block into the per-box sandbox.Limits,
// translating the operator-friendly units (mebibytes, fractional CPUs) into the
// raw byte / nano-CPU counts the Docker API expects. A zero field stays zero
// (unlimited) so the conversion preserves "no limit" semantics.
//
// @arg b The box resource configuration from the YAML config.
// @return sandbox.Limits The equivalent per-box caps and max-box ceiling.
//
// @testcase TestBoxLimitsConvertsUnits converts mebibytes and CPUs to bytes and nano-CPUs.
func boxLimits(b config.BoxConfig) sandbox.Limits {
	return sandbox.Limits{
		MemoryBytes: int64(b.MemoryMB) * 1024 * 1024,
		NanoCPUs:    int64(b.CPUs * 1e9),
		PidsLimit:   b.PidsLimit,
		MaxBoxes:    b.MaxBoxes,
	}
}

// registryAuths turns the configured registry credentials into the per-host
// auth map the Docker provisioner consumes, keyed by registry host. It returns nil
// when no registries are configured, which leaves every image pull anonymous.
//
// @arg regs The configured registry credentials (each carrying a resolved password).
// @return map[string]registry.AuthConfig Pull credentials keyed by registry host, or nil when none are configured.
//
// @testcase TestRegistryAuthsKeyedByHost maps each entry by its registry host and returns nil when empty.
func registryAuths(regs []config.RegistryConfig) map[string]registry.AuthConfig {
	if len(regs) == 0 {
		return nil
	}
	auths := make(map[string]registry.AuthConfig, len(regs))
	for _, r := range regs {
		auths[r.Registry] = registry.AuthConfig{
			Username:      r.Username,
			Password:      r.Password,
			ServerAddress: r.Registry,
		}
	}
	return auths
}

// run assembles and serves the llmbox hub from cfg: it builds the Docker manager
// (applying the configured per-box resource limits), opens the session store,
// sets up optional lifecycle hooks and activation auth, optionally enables
// hub-and-spoke clustering and HTTP proxying of box ports, restores persisted
// sessions, starts the orphan reaper, and serves the MCP endpoint and the UI/API
// on their two separate ports until the parent context is cancelled
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
	prov.SetPerBoxLimits(boxLimits(cfg.Box))
	prov.SetRegistryAuths(registryAuths(cfg.Registries))
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

	// Two listeners: the MCP endpoint on its own port (meant to sit behind an auth
	// proxy) and the UI/API (auth pages, admin, health) on the public port.
	mcpSrv := &http.Server{
		Addr:    cfg.MCPAddr,
		Handler: srv.MCPHandler(srv.MCPServer(name, version)),
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

	log.Printf("%s %s listening: API on %s, MCP on %s (public URL %s, auth TTL %s)", name, version, cfg.HTTPAddr, cfg.MCPAddr, cfg.PublicURL, authTTL)

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
