// Package spoke implements the llmbox-spoke command tree: running a spoke that
// joins a hub and serves boxes against the local Docker daemon. The one-time join
// tokens a hub issues to enroll spokes are managed by the hub instead (see the
// llmbox-server `token` command). The cmd/llmbox-spoke binary is a thin main that
// builds this command via NewRootCmd and executes it.
package spoke

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/clems4ever/llmbox/internal/shared/boxconfig"
	"github.com/clems4ever/llmbox/internal/shared/cluster"
	"github.com/clems4ever/llmbox/internal/spoke/box"
	"github.com/clems4ever/llmbox/internal/spoke/box/backend"
	_ "github.com/clems4ever/llmbox/internal/spoke/docker"      // registers the "docker" box backend
	_ "github.com/clems4ever/llmbox/internal/spoke/firecracker" // registers the "firecracker" box backend
)

const (
	// defaultSpokeStateFile is where a spoke persists the credential it is issued
	// at first enrollment, so it can reconnect without the (one-time) join token.
	defaultSpokeStateFile = "llmbox-spoke.json"

	// spokeReconnectMax bounds the exponential backoff between reconnect attempts.
	spokeReconnectMax = 30 * time.Second
)

// spokeOptions holds everything a running spoke needs, sourced entirely from
// command-line flags. A spoke reads no config file: a spoke host runs a single
// copy-pasteable command (the one the admin UI generates), and every knob the
// hub's config would have supplied is instead an optional flag with the same
// built-in default.
type spokeOptions struct {
	hubURL    string
	token     string
	statePath string
	// tlsCAFile trusts a PEM CA bundle for a wss:// hub with a private-CA or
	// self-signed certificate; tlsInsecure skips certificate verification entirely
	// (testing only). Both apply to the hub connection on any backend.
	tlsCAFile   string
	tlsInsecure bool
	boxGPUs     string
	remoteArgs  string
	// backend is the box isolation backend this spoke runs ("docker" or
	// "firecracker"); it is set from the chosen run subcommand, not a flag.
	backend string
	// fcKernelImage/fcRootfsImage/fcStateDir are the Firecracker backend's guest
	// kernel, default rootfs image, and state directory; unused for Docker.
	fcKernelImage string
	fcRootfsImage string
	// fcPayloadImage is an optional read-only ext4 carrying the guest (plus
	// claude and its trust seed), attached to every box as a shared second drive so
	// the guest updates without rebuilding the base rootfs; unused for Docker.
	fcPayloadImage  string
	fcStateDir      string
	fcDisableEgress bool
	fcPoolSize      int
	// box carries the per-box resource caps, socket dir, and namespace, reusing
	// the config type so boxconfig.BoxLimits does the unit conversion.
	box      boxconfig.BoxConfig
	boxPeers []string
	// image is the container image every box on this spoke launches (Docker
	// backend); empty uses the backend's built-in default. The spoke owns the
	// image outright — the hub never names one — so nothing about the image
	// crosses the hub/spoke boundary.
	image    string
	registry registryFlags
}

// registryFlags are the single optional registry credential a spoke may use to
// pull box images from an authenticated registry. Empty host means anonymous
// pulls (the default).
type registryFlags struct {
	host         string
	username     string
	passwordFile string
}

// registries resolves the registry flags into the config type boxconfig.RegistryAuths
// consumes, reading the password from its file. It returns nil (anonymous pulls)
// when no registry host is given.
//
// @return []boxconfig.RegistryConfig The single resolved registry entry, or nil when none is configured.
// @error error if a host is given without a readable password file.
//
// @testcase TestSpokeRegistriesFromFlags resolves a registry password from its file and errors on a missing file.
func (o spokeOptions) registries() ([]boxconfig.RegistryConfig, error) {
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
	return []boxconfig.RegistryConfig{{
		Registry: o.registry.host,
		Username: o.registry.username,
		Password: strings.TrimSpace(string(data)),
	}}, nil
}

// hubTLS builds the TLS client config for the hub WebSocket dial. It returns nil
// (use the system trust store) when neither TLS flag is set, a config trusting the
// --tls-ca bundle when given, and/or one skipping verification when --tls-insecure
// is set.
//
// @return *tls.Config The TLS config for the hub dial, or nil for the system default.
// @error error if the CA bundle cannot be read or contains no certificates.
//
// @testcase TestSpokeHubTLS builds a config from the CA/insecure flags and errors on a bad CA file.
func (o spokeOptions) hubTLS() (*tls.Config, error) {
	if o.tlsCAFile == "" && !o.tlsInsecure {
		return nil, nil
	}
	cfg := &tls.Config{InsecureSkipVerify: o.tlsInsecure} //nolint:gosec // opt-in via --tls-insecure, documented as testing-only
	if o.tlsCAFile != "" {
		pem, err := os.ReadFile(o.tlsCAFile)
		if err != nil {
			return nil, fmt.Errorf("reading --tls-ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("--tls-ca %q contains no valid certificates", o.tlsCAFile)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// NewRootCmd builds the llmbox-spoke command tree: a `docker` and a `firecracker`
// subcommand each run a spoke that joins a hub and serves boxes on that backend —
// each carrying only that backend's flags, never a mix. The spoke reads no config
// file — every setting is a flag — so a host can run the single command the admin
// UI generates. Join tokens are managed on the hub (the llmbox-server `token`
// command), not here. The binary name and version shown by the command are passed
// in by the cmd/llmbox-spoke main.
//
// @arg name The command name shown in usage (the binary name).
// @arg version The version string reported by --version.
// @return *cobra.Command The configured root command with its docker and firecracker subcommands.
//
// @testcase TestNewRootCmd checks the command wiring (per-backend flags and subcommands).
func NewRootCmd(name, version string) *cobra.Command {
	root := &cobra.Command{
		Use:           name,
		Short:         "Join a hub and run boxes on this host (choose the docker or firecracker backend)",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: false,
		Args:          cobra.NoArgs,
	}
	root.AddCommand(newSpokeRunCmd("docker", "Run a spoke that runs boxes as Docker containers", addDockerSpokeFlags))
	root.AddCommand(newSpokeRunCmd("firecracker", "Run a spoke that runs boxes as Firecracker microVMs", addFirecrackerSpokeFlags))
	return root
}

// newSpokeRunCmd builds a backend-specific run subcommand: it registers the flags
// common to every spoke plus the given backend's own flags, then runs a spoke on
// that backend. backendName is passed straight to backend.New.
//
// @arg backendName The box backend this subcommand runs ("docker" or "firecracker").
// @arg short The one-line command summary shown in usage.
// @arg addBackendFlags Registers the backend-specific flags on the command.
// @return *cobra.Command The configured run subcommand.
//
// @testcase TestNewRootCmd checks each backend subcommand runs on its own backend with its own flags.
func newSpokeRunCmd(backendName, short string, addBackendFlags func(*pflag.FlagSet, *spokeOptions)) *cobra.Command {
	o := &spokeOptions{}
	cmd := &cobra.Command{
		Use:           backendName,
		Short:         short,
		SilenceUsage:  true,
		SilenceErrors: false,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if o.hubURL == "" {
				return errors.New("--hub is required (e.g. wss://hub.example.com/spoke/connect)")
			}
			o.backend = backendName
			return runSpoke(cmd.Context(), *o)
		},
	}
	f := cmd.Flags()
	addCommonSpokeFlags(f, o)
	addBackendFlags(f, o)
	return cmd
}

// addCommonSpokeFlags registers the flags every spoke shares regardless of backend.
//
// @arg f The flag set to register the flags on.
// @arg o The options struct the flags bind into.
//
// @testcase TestNewRootCmd checks the common flags are present on each backend subcommand.
func addCommonSpokeFlags(f *pflag.FlagSet, o *spokeOptions) {
	f.StringVar(&o.hubURL, "hub", "", "hub spoke-connect URL, e.g. wss://hub.example.com/spoke/connect")
	f.StringVar(&o.token, "token", "", "one-time join token (only needed for first enrollment)")
	f.StringVar(&o.statePath, "state", defaultSpokeStateFile, "file storing this spoke's issued credential")
	f.StringVar(&o.tlsCAFile, "tls-ca", "", "PEM CA bundle to trust for a wss:// hub with a private-CA or self-signed certificate")
	f.BoolVar(&o.tlsInsecure, "tls-insecure", false, "skip TLS certificate verification when dialing a wss:// hub (testing only; prefer --tls-ca)")
	f.StringVar(&o.box.Namespace, "namespace", "", "scope this spoke's boxes to a namespace so two spokes can share one host without collapsing each other's boxes; empty is unscoped")
	f.StringVar(&o.remoteArgs, "remote-args", "", "args passed to `claude remote-control` in every box; empty uses the built-in default")
	f.IntVar(&o.box.MemoryMB, "box-memory-mb", boxconfig.DefaultBoxMemoryMB, "hard memory limit per box in MiB (0 = unlimited)")
	f.Float64Var(&o.box.CPUs, "box-cpus", boxconfig.DefaultBoxCPUs, "CPU quota per box, fractional allowed (0 = unlimited)")
	f.Int64Var(&o.box.PidsLimit, "box-pids-limit", boxconfig.DefaultBoxPidsLimit, "max processes/threads per box, blunts fork bombs (0 = unlimited)")
	f.IntVar(&o.box.MaxBoxes, "max-boxes", 0, "max concurrent boxes on this spoke (0 = unlimited)")
	f.StringVar(&o.box.SocketDir, "box-socket-dir", "", "host directory holding each box's control socket; empty uses the provisioner default")
	f.StringArrayVar(&o.boxPeers, "box-peer", nil, "container name connected into every box's network so boxes can reach it (repeatable)")
	f.StringVar(&o.registry.host, "registry", "", `registry host to authenticate to when pulling box images, e.g. "ghcr.io" (empty pulls anonymously)`)
	f.StringVar(&o.registry.username, "registry-username", "", "username for --registry")
	f.StringVar(&o.registry.passwordFile, "registry-password-file", "", "file holding the password or token for --registry")
}

// addDockerSpokeFlags registers the flags specific to the Docker backend.
//
// @arg f The flag set to register the flags on.
// @arg o The options struct the flags bind into.
//
// @testcase TestNewRootCmd checks the docker subcommand exposes --box-gpus and no firecracker flags.
func addDockerSpokeFlags(f *pflag.FlagSet, o *spokeOptions) {
	f.StringVar(&o.image, "image", "", "container image every box on this spoke launches; empty uses the built-in default")
	f.StringVar(&o.boxGPUs, "box-gpus", "", `GPUs to attach to every box on this spoke, like docker run --gpus (e.g. "all", "2", or "device=0,1"); empty attaches none`)
}

// addFirecrackerSpokeFlags registers the flags specific to the Firecracker backend.
// Any image path left empty is auto-resolved from the published registry images.
//
// @arg f The flag set to register the flags on.
// @arg o The options struct the flags bind into.
//
// @testcase TestNewRootCmd checks the firecracker subcommand exposes its image flags and no docker flags.
func addFirecrackerSpokeFlags(f *pflag.FlagSet, o *spokeOptions) {
	f.StringVar(&o.fcKernelImage, "kernel", "", "host path to the guest kernel (vmlinux); empty pulls the published kernel from the registry")
	f.StringVar(&o.fcRootfsImage, "rootfs", "", "host path to the default guest rootfs; empty pulls the published base rootfs from the registry")
	f.StringVar(&o.fcPayloadImage, "payload", "", "host path to a read-only ext4 carrying the guest (+claude), attached as a shared second drive so the guest updates without rebuilding the rootfs; empty pulls the published payload (unless --rootfs is a custom all-in-one image)")
	f.StringVar(&o.fcStateDir, "state-dir", "", "directory for per-box state; empty uses the backend default")
	f.BoolVar(&o.fcDisableEgress, "disable-egress", false, "boot control-only boxes (no TAP/NAT egress), so the spoke needs no CAP_NET_ADMIN; boxes then have no outbound network")
	f.IntVar(&o.fcPoolSize, "pool-size", 0, "number of egress TAP devices provisioned at startup (caps concurrent networked boxes); 0 uses the default")
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
	// The spoke owns the box image: every box it runs launches o.image (empty uses
	// the backend's built-in default). The hub never names an image, so nothing
	// about the image crosses the hub/spoke boundary.
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
	log.Printf("initializing %s box backend (first run may fetch guest images and set up networking)...", o.backend)
	// One HubCaller for the spoke's lifetime: the backend serves each box's
	// port-publishing API against it, and every (re)connection to the hub
	// attaches to it below, so box-port requests always ride the live link.
	portCaller := cluster.NewHubCaller()
	prov, err := backend.New(o.backend, backend.Options{
		DefaultImage:     o.image,
		SocketDir:        o.box.SocketDir,
		Peers:            o.boxPeers,
		Limits:           BoxLimits(o.box),
		Namespace:        o.box.Namespace,
		BoxPorts:         portCaller,
		GPUs:             o.boxGPUs,
		RegistryAuths:    boxconfig.RegistryAuths(regs),
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

	tlsConf, err := o.hubTLS()
	if err != nil {
		return err
	}
	dial := cluster.WebSocketDialerTLS(hubURL, tlsConf)
	save := func(c cluster.Credentials) error {
		if err := saveSpokeCreds(statePath, c); err != nil {
			return err
		}
		creds = &c // reconnect with the saved credential from now on
		log.Printf("enrolled as spoke %q; credential saved to %s", c.Name, statePath)
		return nil
	}

	log.Printf("connecting to hub %s ...", hubURL)
	backoff := time.Second
	for {
		err := cluster.RunWithCaller(ctx, dial, mgr, token, creds, save, portCaller)
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
