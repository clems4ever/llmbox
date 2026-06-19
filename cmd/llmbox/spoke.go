package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/clems4ever/llmbox/internal/cluster"
	"github.com/clems4ever/llmbox/internal/config"
	"github.com/clems4ever/llmbox/internal/docker"
	"github.com/clems4ever/llmbox/internal/server"
)

const (
	// defaultSpokeStateFile is where a spoke persists the credential it is issued
	// at first enrollment, so it can reconnect without the (one-time) join token.
	defaultSpokeStateFile = "llmbox-spoke.json"

	// defaultJoinTokenTTL is how long a generated join token stays valid when
	// --ttl is not given.
	defaultJoinTokenTTL = time.Hour

	// spokeReconnectMax bounds the exponential backoff between reconnect attempts.
	spokeReconnectMax = 30 * time.Second
)

// newSpokeCmd builds the `spoke` command tree: `llmbox spoke` runs a spoke that
// joins a hub and serves boxes against the local Docker daemon, and
// `llmbox spoke token create` mints a one-time join token on the hub.
//
// @return *cobra.Command The configured spoke command with its token subcommand.
//
// @testcase TestNewSpokeCmd checks the spoke command wiring (flags and subcommands).
func newSpokeCmd() *cobra.Command {
	var (
		configPath string
		hubURL     string
		token      string
		statePath  string
	)
	spokeCmd := &cobra.Command{
		Use:           "spoke",
		Short:         "Run a spoke that joins a hub and runs boxes on the local Docker daemon",
		SilenceUsage:  true,
		SilenceErrors: false,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if hubURL == "" {
				return errors.New("--hub is required (e.g. wss://hub.example.com/spoke/connect)")
			}
			cfg, err := loadConfig(configPath, cmd.Flags().Changed("config"))
			if err != nil {
				return err
			}
			return runSpoke(cmd.Context(), cfg, hubURL, token, statePath)
		},
	}
	spokeCmd.Flags().StringVarP(&configPath, "config", "c", "llmbox.yaml", "path to the YAML configuration file (for Docker settings)")
	spokeCmd.Flags().StringVar(&hubURL, "hub", "", "hub spoke-connect URL, e.g. wss://hub.example.com/spoke/connect")
	spokeCmd.Flags().StringVar(&token, "token", "", "one-time join token (only needed for first enrollment)")
	spokeCmd.Flags().StringVar(&statePath, "state", defaultSpokeStateFile, "file storing this spoke's issued credential")

	spokeCmd.AddCommand(newSpokeTokenCmd())
	return spokeCmd
}

// newSpokeTokenCmd builds the `spoke token` command tree (currently just
// `create`). Token management runs on the hub and operates on its state file.
//
// @return *cobra.Command The configured `spoke token` command.
//
// @testcase TestNewSpokeCmd checks the token subcommand is registered.
func newSpokeTokenCmd() *cobra.Command {
	tokenCmd := &cobra.Command{
		Use:   "token",
		Short: "Manage spoke join tokens (run on the hub)",
		Args:  cobra.NoArgs,
	}

	var (
		configPath string
		spokeName  string
		ttl        time.Duration
	)
	createCmd := &cobra.Command{
		Use:           "create",
		Short:         "Mint a one-time join token for a named spoke",
		SilenceUsage:  true,
		SilenceErrors: false,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if spokeName == "" {
				return errors.New("--name is required (the spoke name baked into the token)")
			}
			cfg, err := loadConfig(configPath, cmd.Flags().Changed("config"))
			if err != nil {
				return err
			}
			return createJoinToken(cmd.OutOrStdout(), cfg, spokeName, ttl)
		},
	}
	createCmd.Flags().StringVarP(&configPath, "config", "c", "llmbox.yaml", "path to the YAML configuration file (for the hub state file)")
	createCmd.Flags().StringVar(&spokeName, "name", "", "name of the spoke this token enrolls")
	createCmd.Flags().DurationVar(&ttl, "ttl", defaultJoinTokenTTL, "how long the token stays valid")

	tokenCmd.AddCommand(createCmd)
	return tokenCmd
}

// createJoinToken opens the hub's store, mints a one-time join token for the
// named spoke, and prints it once to out.
//
// @arg out The writer the token is printed to.
// @arg cfg The hub configuration (its StateFile holds the cluster store).
// @arg spokeName The spoke name baked into the token.
// @arg ttl How long the token stays valid.
// @error error if the store cannot be opened or the token cannot be minted.
//
// @testcase TestCreateJoinTokenCmdPrintsToken mints a token and prints it once.
func createJoinToken(out io.Writer, cfg *config.Config, spokeName string, ttl time.Duration) error {
	store, err := server.OpenStore(cfg.StateFile)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	token, err := cluster.CreateJoinToken(store, spokeName, ttl, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Join token for spoke %q (valid %s, one-time use):\n\n  %s\n\nStart the spoke with:\n\n  llmbox spoke --hub wss://<hub>/spoke/connect --token %s\n", spokeName, ttl, token, token)
	return nil
}

// runSpoke connects a spoke to the hub and serves boxes against the local
// Docker daemon, reconnecting with exponential backoff until interrupted. On
// first enrollment it uses the join token and saves the issued credential to
// statePath; subsequent connections reconnect with that saved credential.
//
// @arg parent Base context; serving stops when it (or SIGINT/SIGTERM) fires.
// @arg cfg The configuration supplying Docker settings for the local manager.
// @arg hubURL The hub's spoke-connect URL.
// @arg token The one-time join token (required only for first enrollment).
// @arg statePath The file storing this spoke's issued credential.
// @error error if the Docker manager cannot be built, no credential or token is available, or enrollment is rejected.
//
// @testcase TestRunSpokeRequiresTokenOrCreds errors when neither a token nor saved credentials are available.
func runSpoke(parent context.Context, cfg *config.Config, hubURL, token, statePath string) error {
	mgr, err := docker.NewManager(cfg.ClaudeImage, cfg.RemoteArgs, cfg.ClaudeBin, cfg.BoxPeers)
	if err != nil {
		return err
	}
	defer func() {
		if err := mgr.Close(); err != nil {
			log.Printf("closing docker manager: %v", err)
		}
	}()

	creds, err := loadSpokeCreds(statePath)
	if err != nil {
		return err
	}
	if creds == nil && token == "" {
		return fmt.Errorf("a --token is required for first enrollment (no saved credentials at %s)", statePath)
	}

	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dial := cluster.WebSocketDialer(hubURL)
	save := func(c cluster.Credentials) error {
		if err := saveSpokeCreds(statePath, c); err != nil {
			return err
		}
		creds = &c // reconnect with the saved credential from now on
		log.Printf("enrolled as spoke %q; credential saved to %s", c.Name, statePath)
		return nil
	}

	backoff := time.Second
	for {
		err := cluster.Run(ctx, dial, mgr, token, creds, save)
		if ctx.Err() != nil {
			return nil
		}
		// A rejected enrollment is terminal: retrying the same token won't help.
		if creds == nil && errors.Is(err, cluster.ErrEnrollRejected) {
			return err
		}
		log.Printf("spoke connection ended: %v; reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < spokeReconnectMax {
			backoff *= 2
		}
	}
}

// loadSpokeCreds reads the spoke's saved credentials from path. A missing file
// returns (nil, nil), meaning the spoke has not enrolled yet.
//
// @arg path The credential file path.
// @return *cluster.Credentials The saved credentials, or nil when the file is absent.
// @error error if the file exists but cannot be read or parsed.
//
// @testcase TestSpokeCredsRoundTrip reads back saved credentials and returns nil for a missing file.
func loadSpokeCreds(path string) (*cluster.Credentials, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading spoke credentials %s: %w", path, err)
	}
	var c cluster.Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing spoke credentials %s: %w", path, err)
	}
	return &c, nil
}

// saveSpokeCreds writes the spoke's credentials to path with owner-only
// permissions (the credential is a bearer secret).
//
// @arg path The credential file path.
// @arg c The credentials to persist.
// @error error if the credentials cannot be encoded or written.
//
// @testcase TestSpokeCredsRoundTrip writes credentials this reads back.
func saveSpokeCreds(path string, c cluster.Credentials) error {
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encoding spoke credentials: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing spoke credentials %s: %w", path, err)
	}
	return nil
}
