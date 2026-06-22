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
	"path/filepath"
	"sort"
	"strings"
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

	var listConfigPath string
	listCmd := &cobra.Command{
		Use:           "list",
		Short:         "List outstanding spoke join tokens",
		SilenceUsage:  true,
		SilenceErrors: false,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(listConfigPath, cmd.Flags().Changed("config"))
			if err != nil {
				return err
			}
			return listJoinTokens(cmd.OutOrStdout(), cfg, time.Now())
		},
	}
	listCmd.Flags().StringVarP(&listConfigPath, "config", "c", "llmbox.yaml", "path to the YAML configuration file (for the hub state file)")
	tokenCmd.AddCommand(listCmd)

	var (
		revokeConfigPath string
		revokeID         string
		revokeName       string
	)
	revokeCmd := &cobra.Command{
		Use:           "revoke",
		Short:         "Revoke spoke join tokens by ID or spoke name",
		SilenceUsage:  true,
		SilenceErrors: false,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if revokeID == "" && revokeName == "" {
				return errors.New("one of --id or --name is required")
			}
			cfg, err := loadConfig(revokeConfigPath, cmd.Flags().Changed("config"))
			if err != nil {
				return err
			}
			return revokeJoinTokens(cmd.OutOrStdout(), cfg, revokeID, revokeName)
		},
	}
	revokeCmd.Flags().StringVarP(&revokeConfigPath, "config", "c", "llmbox.yaml", "path to the YAML configuration file (for the hub state file)")
	revokeCmd.Flags().StringVar(&revokeID, "id", "", "revoke the single token whose ID has this prefix")
	revokeCmd.Flags().StringVar(&revokeName, "name", "", "revoke every token issued for this spoke name")
	tokenCmd.AddCommand(revokeCmd)

	return tokenCmd
}

// joinTokenIDLen is how many leading hash characters the CLI shows (and accepts
// as an --id prefix) for a join token — enough to be unambiguous in practice
// without dumping the full hash.
const joinTokenIDLen = 12

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
	// Show the state file the token landed in: a token is only honored by a hub
	// reading this exact same store, so a mismatch here (e.g. defaults used
	// because no config was found) is the usual cause of "enrollment rejected".
	fmt.Fprintf(out, "Join token for spoke %q (valid %s, one-time use):\n\n  %s\n\nWritten to state file: %s\n(the running hub must use this same state_file, or it will reject the token)\n\nStart the spoke with:\n\n  llmbox spoke --hub wss://<hub>/spoke/connect --token %s\n", spokeName, ttl, token, cfg.StateFile, token)
	return nil
}

// listJoinTokens prints the outstanding join tokens (short ID, spoke name, and
// expiry/expired marker) from the hub's store.
//
// @arg out The writer the listing is printed to.
// @arg cfg The hub configuration (its StateFile holds the cluster store).
// @arg now The current time, used to flag expired tokens.
// @error error if the store cannot be opened or read.
//
// @testcase TestListJoinTokensCmd lists outstanding tokens with their spoke and expiry.
func listJoinTokens(out io.Writer, cfg *config.Config, now time.Time) error {
	store, err := server.OpenStore(cfg.StateFile)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	tokens, err := store.ListJoinTokens()
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		fmt.Fprintln(out, "No outstanding join tokens.")
		return nil
	}
	sort.Slice(tokens, func(i, j int) bool { return tokens[i].Name < tokens[j].Name })
	fmt.Fprintf(out, "%-*s  %-20s  %s\n", joinTokenIDLen, "ID", "SPOKE", "EXPIRES")
	for _, t := range tokens {
		status := t.ExpiresAt.Format(time.RFC3339)
		if now.After(t.ExpiresAt) {
			status += " (expired)"
		}
		fmt.Fprintf(out, "%-*s  %-20s  %s\n", joinTokenIDLen, shortID(t.ID), t.Name, status)
	}
	return nil
}

// revokeJoinTokens deletes join tokens by ID prefix or by spoke name. With idPrefix
// set it revokes the single token whose ID starts with it (erroring if none or
// more than one match); with name set it revokes every token for that spoke.
//
// @arg out The writer revocation results are printed to.
// @arg cfg The hub configuration (its StateFile holds the cluster store).
// @arg idPrefix The ID prefix selecting a single token; empty to select by name.
// @arg name The spoke name selecting all its tokens; empty to select by ID.
// @error error if the store cannot be opened, no token matches, or an ID prefix is ambiguous.
//
// @testcase TestRevokeJoinTokenByID revokes the single token matching an ID prefix.
// @testcase TestRevokeJoinTokenByName revokes every token for a spoke name.
// @testcase TestRevokeJoinTokenNoMatch errors when nothing matches.
func revokeJoinTokens(out io.Writer, cfg *config.Config, idPrefix, name string) error {
	store, err := server.OpenStore(cfg.StateFile)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	tokens, err := store.ListJoinTokens()
	if err != nil {
		return err
	}

	var matched []cluster.JoinTokenInfo
	for _, t := range tokens {
		if idPrefix != "" && strings.HasPrefix(t.ID, idPrefix) {
			matched = append(matched, t)
		} else if name != "" && t.Name == name {
			matched = append(matched, t)
		}
	}
	if len(matched) == 0 {
		return errors.New("no join token matches")
	}
	if idPrefix != "" && len(matched) > 1 {
		return fmt.Errorf("ID prefix %q is ambiguous (%d tokens match); use more characters", idPrefix, len(matched))
	}
	for _, t := range matched {
		if err := store.DeleteJoinToken(t.ID); err != nil {
			return err
		}
		fmt.Fprintf(out, "Revoked join token %s for spoke %q.\n", shortID(t.ID), t.Name)
	}
	return nil
}

// shortID truncates a join token hash ID to the display length.
//
// @arg id The full hash ID.
// @return string The leading joinTokenIDLen characters (or the whole id if shorter).
//
// @testcase TestListJoinTokensCmd shows shortened token IDs.
func shortID(id string) string {
	if len(id) <= joinTokenIDLen {
		return id
	}
	return id[:joinTokenIDLen]
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
// @error error if the Docker manager cannot be built, no credential or token is available, the state path is not writable for a first enrollment, or enrollment is rejected.
//
// @testcase TestRunSpokeRequiresTokenOrCreds errors when neither a token nor saved credentials are available.
func runSpoke(parent context.Context, cfg *config.Config, hubURL, token, statePath string) error {
	mgr, err := docker.NewManager(cfg.ClaudeImage, cfg.RemoteArgs, cfg.ClaudeBin, cfg.BoxPeers)
	if err != nil {
		return err
	}
	mgr.SetBoxLimits(boxLimits(cfg.Box))
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
	// First enrollment consumes the one-time join token and the hub then mints a
	// credential the spoke MUST persist. If the state path is not writable we would
	// burn the token, fail to save, and reconnect-loop presenting the now-dead
	// token. Verify writability up front so a misconfigured state location fails
	// fast without spending the token.
	if creds == nil {
		if err := checkStateWritable(statePath); err != nil {
			return err
		}
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
	policy := cluster.ValidationPolicy{AllowedImages: cfg.Spoke.AllowedImages}

	backoff := time.Second
	for {
		err := cluster.Run(ctx, dial, mgr, token, creds, save, policy)
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

// checkStateWritable verifies the spoke can persist its credential at path before
// it enrolls. A first enrollment consumes the one-time join token, so a state
// location the process cannot write (e.g. a container volume owned by root while
// llmbox runs as the distroless nonroot user) must fail here rather than after the
// token is already spent. It creates the parent directory and probes it with a
// temporary file, removing the probe before returning.
//
// @arg path The credential file path the spoke will write on enrollment.
// @error error if the parent directory cannot be created or is not writable.
//
// @testcase TestCheckStateWritable accepts a writable directory and rejects a read-only one.
func checkStateWritable(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating spoke state directory %s: %w", dir, err)
	}
	probe, err := os.CreateTemp(dir, ".llmbox-spoke-*.probe")
	if err != nil {
		return fmt.Errorf("spoke state path %s is not writable (if running in a container, ensure the mounted state volume is writable by the llmbox user): %w", path, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
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
