// Package spoke implements the llmbox-spoke command tree: running a spoke that
// joins a hub and serves boxes against the local Docker daemon, and the `token`
// subcommand that mints, lists, and revokes the one-time join tokens a hub issues
// to enroll spokes. The cmd/llmbox-spoke binary is a thin main that builds this
// command via NewRootCmd and executes it.
package spoke

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

	"github.com/clems4ever/llmbox/internal/box"
	"github.com/clems4ever/llmbox/internal/box/backend"
	"github.com/clems4ever/llmbox/internal/cli"
	"github.com/clems4ever/llmbox/internal/cluster"
	"github.com/clems4ever/llmbox/internal/config"
	_ "github.com/clems4ever/llmbox/internal/docker"      // registers the "docker" box backend
	_ "github.com/clems4ever/llmbox/internal/firecracker" // registers the "firecracker" box backend
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

// spokeOptions holds everything a running spoke needs, sourced entirely from
// command-line flags. A spoke reads no config file: a spoke host runs a single
// copy-pasteable command (the one the admin UI generates), and every knob the
// hub's config would have supplied is instead an optional flag with the same
// built-in default.
type spokeOptions struct {
	hubURL     string
	token      string
	statePath  string
	boxGPUs    string
	remoteArgs string
	// backend selects the box isolation backend by name ("docker" or
	// "firecracker"); empty defaults to Docker.
	backend string
	// fcKernelImage/fcRootfsImage/fcStateDir are the Firecracker backend's guest
	// kernel, default rootfs image, and state directory; unused for Docker.
	fcKernelImage string
	fcRootfsImage string
	// fcPayloadImage is an optional read-only ext4 carrying the guest agent (plus
	// claude and its trust seed), attached to every box as a shared second drive so
	// the agent updates without rebuilding the base rootfs; unused for Docker.
	fcPayloadImage  string
	fcStateDir      string
	fcDisableEgress bool
	fcPoolSize      int
	// box carries the per-box resource caps, socket dir, and namespace, reusing
	// the config type so cli.BoxLimits does the unit conversion.
	box           config.BoxConfig
	boxPeers      []string
	allowedImages []string
	registry      registryFlags
}

// registryFlags are the single optional registry credential a spoke may use to
// pull box images from an authenticated registry. Empty host means anonymous
// pulls (the default).
type registryFlags struct {
	host         string
	username     string
	passwordFile string
}

// registries resolves the registry flags into the config type cli.RegistryAuths
// consumes, reading the password from its file. It returns nil (anonymous pulls)
// when no registry host is given.
//
// @return []config.RegistryConfig The single resolved registry entry, or nil when none is configured.
// @error error if a host is given without a readable password file.
//
// @testcase TestSpokeRegistriesFromFlags resolves a registry password from its file and errors on a missing file.
func (o spokeOptions) registries() ([]config.RegistryConfig, error) {
	if o.registry.host == "" {
		return nil, nil
	}
	if o.registry.passwordFile == "" {
		return nil, errors.New("--registry-password-file is required with --registry")
	}
	data, err := os.ReadFile(o.registry.passwordFile)
	if err != nil {
		return nil, fmt.Errorf("reading --registry-password-file: %w", err)
	}
	return []config.RegistryConfig{{
		Registry: o.registry.host,
		Username: o.registry.username,
		Password: strings.TrimSpace(string(data)),
	}}, nil
}

// NewRootCmd builds the llmbox-spoke command tree: the root command runs a spoke
// that joins a hub and serves boxes against the local Docker daemon, and a
// `token` subcommand (create/list/revoke) manages the one-time join tokens on the
// hub. The spoke reads no config file — every setting is a flag — so a host can
// run the single command the admin UI generates. The binary name and version
// shown by the command are passed in by the cmd/llmbox-spoke main.
//
// @arg name The command name shown in usage (the binary name).
// @arg version The version string reported by --version.
// @return *cobra.Command The configured root command with its token subcommand.
//
// @testcase TestNewRootCmd checks the command wiring (flags and subcommands).
func NewRootCmd(name, version string) *cobra.Command {
	var o spokeOptions
	spokeCmd := &cobra.Command{
		Use:           name,
		Short:         "Run a spoke that joins a hub and runs boxes on the local Docker daemon",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: false,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if o.hubURL == "" {
				return errors.New("--hub is required (e.g. wss://hub.example.com/spoke/connect)")
			}
			return runSpoke(cmd.Context(), o)
		},
	}
	f := spokeCmd.Flags()
	f.StringVar(&o.hubURL, "hub", "", "hub spoke-connect URL, e.g. wss://hub.example.com/spoke/connect")
	f.StringVar(&o.token, "token", "", "one-time join token (only needed for first enrollment)")
	f.StringVar(&o.statePath, "state", defaultSpokeStateFile, "file storing this spoke's issued credential")
	f.StringVar(&o.box.Namespace, "namespace", "", "scope this spoke's boxes to a namespace so two spokes can share one Docker daemon without collapsing each other's containers; empty is unscoped")
	f.StringVar(&o.boxGPUs, "box-gpus", "", `GPUs to attach to every box on this spoke, like docker run --gpus (e.g. "all", "2", or "device=0,1"); empty attaches none`)
	f.StringVar(&o.remoteArgs, "remote-args", "", "args passed to `claude remote-control` in every box; empty uses the built-in default")
	f.IntVar(&o.box.MemoryMB, "box-memory-mb", config.DefaultBoxMemoryMB, "hard memory limit per box in MiB (0 = unlimited)")
	f.Float64Var(&o.box.CPUs, "box-cpus", config.DefaultBoxCPUs, "CPU quota per box, fractional allowed (0 = unlimited)")
	f.Int64Var(&o.box.PidsLimit, "box-pids-limit", config.DefaultBoxPidsLimit, "max processes/threads per box, blunts fork bombs (0 = unlimited)")
	f.IntVar(&o.box.MaxBoxes, "max-boxes", 0, "max concurrent boxes on this spoke (0 = unlimited)")
	f.StringVar(&o.box.SocketDir, "box-socket-dir", "", "host directory holding each box's control socket; empty uses the provisioner default")
	f.StringVar(&o.backend, "backend", "", `box isolation backend: "docker" (default) or "firecracker"`)
	f.StringVar(&o.fcKernelImage, "fc-kernel", "", "firecracker backend: host path to the guest kernel (vmlinux) every box boots")
	f.StringVar(&o.fcRootfsImage, "fc-rootfs", "", "firecracker backend: host path to the default guest rootfs image booted when a create supplies none")
	f.StringVar(&o.fcPayloadImage, "fc-payload", "", "firecracker backend: host path to a read-only ext4 carrying the guest agent (+claude), attached as a shared second drive so the agent updates without rebuilding the rootfs; empty bakes the agent into the rootfs")
	f.StringVar(&o.fcStateDir, "fc-state-dir", "", "firecracker backend: directory for per-box state; empty uses the backend default")
	f.BoolVar(&o.fcDisableEgress, "fc-disable-egress", false, "firecracker backend: boot control-only boxes (no TAP/NAT egress), so the spoke needs no CAP_NET_ADMIN; boxes then have no outbound network")
	f.IntVar(&o.fcPoolSize, "fc-pool-size", 0, "firecracker backend: number of egress TAP devices provisioned at startup (caps concurrent networked boxes); 0 uses the default")
	f.StringArrayVar(&o.boxPeers, "box-peer", nil, "container name connected into every box's network so boxes can reach it (repeatable)")
	f.StringArrayVar(&o.allowedImages, "allowed-image", nil, "restrict which box images this spoke will launch (repeatable; empty places no restriction)")
	f.StringVar(&o.registry.host, "registry", "", `registry host to authenticate to when pulling box images, e.g. "ghcr.io" (empty pulls anonymously)`)
	f.StringVar(&o.registry.username, "registry-username", "", "username for --registry")
	f.StringVar(&o.registry.passwordFile, "registry-password-file", "", "file holding the password or token for --registry")

	spokeCmd.AddCommand(newSpokeTokenCmd())
	return spokeCmd
}

// newSpokeTokenCmd builds the `token` command tree (create/list/revoke). Token
// management runs against the hub's state file.
//
// @return *cobra.Command The configured `token` command.
//
// @testcase TestNewRootCmd checks the token subcommand is registered.
func newSpokeTokenCmd() *cobra.Command {
	tokenCmd := &cobra.Command{
		Use:   "token",
		Short: "Manage spoke join tokens (run on the hub)",
		Args:  cobra.NoArgs,
	}

	var (
		stateFile string
		spokeName string
		ttl       time.Duration
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
			return createJoinToken(cmd.OutOrStdout(), stateFile, spokeName, ttl)
		},
	}
	createCmd.Flags().StringVar(&stateFile, "state-file", config.DefaultStateFile, "the hub's state file the token is written to (must match the running hub's state_file)")
	createCmd.Flags().StringVar(&spokeName, "name", "", "name of the spoke this token enrolls")
	createCmd.Flags().DurationVar(&ttl, "ttl", defaultJoinTokenTTL, "how long the token stays valid")
	tokenCmd.AddCommand(createCmd)

	var listStateFile string
	listCmd := &cobra.Command{
		Use:           "list",
		Short:         "List outstanding spoke join tokens",
		SilenceUsage:  true,
		SilenceErrors: false,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return listJoinTokens(cmd.OutOrStdout(), listStateFile, time.Now())
		},
	}
	listCmd.Flags().StringVar(&listStateFile, "state-file", config.DefaultStateFile, "the hub's state file to read tokens from")
	tokenCmd.AddCommand(listCmd)

	var (
		revokeStateFile string
		revokeID        string
		revokeName      string
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
			return revokeJoinTokens(cmd.OutOrStdout(), revokeStateFile, revokeID, revokeName)
		},
	}
	revokeCmd.Flags().StringVar(&revokeStateFile, "state-file", config.DefaultStateFile, "the hub's state file to revoke tokens from")
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
// @arg stateFile The hub's state file holding the cluster store.
// @arg spokeName The spoke name baked into the token.
// @arg ttl How long the token stays valid.
// @error error if the store cannot be opened or the token cannot be minted.
//
// @testcase TestCreateJoinTokenCmdPrintsToken mints a token and prints it once.
func createJoinToken(out io.Writer, stateFile, spokeName string, ttl time.Duration) error {
	store, err := server.OpenStore(stateFile)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	token, err := cluster.CreateJoinToken(store, spokeName, ttl, time.Now())
	if err != nil {
		return err
	}
	// Show the state file the token landed in: a token is only honored by a hub
	// reading this exact same store, so a mismatch here (e.g. the wrong
	// --state-file) is the usual cause of "enrollment rejected".
	fmt.Fprintf(out, "Join token for spoke %q (valid %s, one-time use):\n\n  %s\n\nWritten to state file: %s\n(the running hub must use this same state_file, or it will reject the token)\n\nStart the spoke with:\n\n  llmbox spoke --hub wss://<hub>/spoke/connect --token %s\n", spokeName, ttl, token, stateFile, token)
	return nil
}

// listJoinTokens prints the outstanding join tokens (short ID, spoke name, and
// expiry/expired marker) from the hub's store.
//
// @arg out The writer the listing is printed to.
// @arg stateFile The hub's state file holding the cluster store.
// @arg now The current time, used to flag expired tokens.
// @error error if the store cannot be opened or read.
//
// @testcase TestListJoinTokensCmd lists outstanding tokens with their spoke and expiry.
func listJoinTokens(out io.Writer, stateFile string, now time.Time) error {
	store, err := server.OpenStore(stateFile)
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
// @arg stateFile The hub's state file holding the cluster store.
// @arg idPrefix The ID prefix selecting a single token; empty to select by name.
// @arg name The spoke name selecting all its tokens; empty to select by ID.
// @error error if the store cannot be opened, no token matches, or an ID prefix is ambiguous.
//
// @testcase TestRevokeJoinTokenByID revokes the single token matching an ID prefix.
// @testcase TestRevokeJoinTokenByName revokes every token for a spoke name.
// @testcase TestRevokeJoinTokenNoMatch errors when nothing matches.
func revokeJoinTokens(out io.Writer, stateFile, idPrefix, name string) error {
	store, err := server.OpenStore(stateFile)
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
// o.statePath; subsequent connections reconnect with that saved credential.
// Every Docker setting comes from o (the flags), never a config file.
//
// @arg parent Base context; serving stops when it (or SIGINT/SIGTERM) fires.
// @arg o The flag-sourced options: hub URL, token, state path, and per-box Docker settings.
// @error error if the Docker manager cannot be built, the GPU spec is malformed, a registry password file cannot be read, no credential or token is available, the state path is not writable for a first enrollment, or enrollment is rejected.
//
// @testcase TestRunSpokeRequiresTokenOrCreds errors when neither a token nor saved credentials are available.
// @testcase TestRunSpokeRejectsBadGPUs errors when the GPU spec is malformed.
func runSpoke(parent context.Context, o spokeOptions) error {
	statePath, token, hubURL := o.statePath, o.token, o.hubURL
	// A spoke holds no box image of its own: the hub resolves the image (its own
	// default included) and sends it with every create, and validateCreate rejects
	// any create that arrives without one. Pass no default here so nothing local
	// can stand in for what the hub sends.
	regs, err := o.registries()
	if err != nil {
		return err
	}
	// Select the box backend by name (Docker by default). Both Docker-specific
	// (GPUs, registry auths) and microVM-specific (kernel/rootfs/state) fields are
	// passed; each backend uses only the ones that apply. GPUs are machine-local,
	// so they are attached to every box this spoke runs. The namespace scopes this
	// spoke's boxes so two spokes can share one host without listing, reaping, or
	// destroying each other's boxes.
	prov, err := backend.New(o.backend, backend.Options{
		SocketDir:        o.box.SocketDir,
		Peers:            o.boxPeers,
		Limits:           cli.BoxLimits(o.box),
		Namespace:        o.box.Namespace,
		GPUs:             o.boxGPUs,
		RegistryAuths:    cli.RegistryAuths(regs),
		KernelImagePath:  o.fcKernelImage,
		RootfsImagePath:  o.fcRootfsImage,
		PayloadImagePath: o.fcPayloadImage,
		StateDir:         o.fcStateDir,
		DisableEgress:    o.fcDisableEgress,
		PoolSize:         o.fcPoolSize,
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := prov.Close(); err != nil {
			log.Printf("closing box backend: %v", err)
		}
	}()
	mgr := box.NewManager(prov, box.Config{RemoteArgs: o.remoteArgs, MaxBoxes: o.box.MaxBoxes})

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
	policy := cluster.ValidationPolicy{AllowedImages: o.allowedImages}

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
