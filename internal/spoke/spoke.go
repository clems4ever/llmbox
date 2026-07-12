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
	"io"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/clems4ever/llmbox/internal/shared/boxconfig"
	"github.com/clems4ever/llmbox/internal/shared/cluster"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/box"
	"github.com/clems4ever/llmbox/internal/spoke/box/backend"
	_ "github.com/clems4ever/llmbox/internal/spoke/docker" // registers the "docker" box backend
	"github.com/clems4ever/llmbox/internal/spoke/firecracker"
)

const (
	// spokeStateFileName is the file a spoke persists the credential it is issued
	// at first enrollment in, so it can reconnect without the (one-time) join
	// token. It lives in spokeStateDirName under the user's home by default (see
	// defaultSpokeStatePath); --state overrides the full path.
	spokeStateFileName = "llmbox-spoke.json"

	// spokeStateDirName is the hidden directory under the user's home holding the
	// spoke's state by default.
	spokeStateDirName = ".llmbox"

	// spokeReconnectMax bounds the exponential backoff between reconnect attempts.
	spokeReconnectMax = 30 * time.Second
)

// defaultSpokeStatePath is the default credential file location:
// ~/.llmbox/llmbox-spoke.json, a hidden directory under the user's home, so the
// enrollment command the hub generates needs no --state flag and the credential
// lands in a predictable per-user spot regardless of the working directory. When
// the home directory cannot be resolved (e.g. a bare container user) it falls
// back to the bare filename in the working directory, the historical default.
// The --state flag overrides it either way.
//
// @return string The default credential file path.
//
// @testcase TestDefaultSpokeStatePath puts the default under the home directory and names the state file.
func defaultSpokeStatePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return spokeStateFileName
	}
	return filepath.Join(home, spokeStateDirName, spokeStateFileName)
}

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
	// initScriptPath is a host file run inside every box this spoke spawns, once at
	// creation, before the box's workload starts, so a spoke can install and start
	// that workload without rebuilding the image; empty runs nothing.
	// initScriptTimeout bounds each run.
	initScriptPath    string
	initScriptTimeout time.Duration
	// copyPaths are the raw --copy flag values ("HOST_SRC[:BOX_DEST]", repeatable):
	// host files or directories copied into every box this spoke creates, at
	// creation before the init script runs. Resolved by copyFiles.
	copyPaths []string
	// publishPorts are the raw --publish-port flag values ("PORT[:DESCRIPTION]",
	// repeatable): in-box ports the hub should expose as an HTTP proxy for every box
	// this spoke creates. Parsed by parsePublishPorts.
	publishPorts []string
	// backend is the box isolation backend this spoke runs ("docker" or
	// "firecracker"); it is set from the chosen run subcommand, not a flag.
	backend string
	// fcKernelImage/fcRootfsImage/fcStateDir are the Firecracker backend's guest
	// kernel, default rootfs image, and state directory; unused for Docker.
	fcKernelImage string
	fcRootfsImage string
	// fcPayloadImage is an optional read-only ext4 carrying the guest binary,
	// attached to every box as a shared second drive so the guest updates without
	// rebuilding the base rootfs; unused for Docker.
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

// initScript reads the spoke's --init-script file into the bytes the box manager
// runs inside every box. It returns nil (no script) when the flag is unset, and
// errors when the path is set but unreadable or empty, so a misconfigured init
// script fails the spoke at startup rather than silently on every box create.
//
// @return []byte The init script bytes, or nil when --init-script is unset.
// @error error if the configured path cannot be read or holds an empty script.
//
// @testcase TestSpokeInitScriptFromFlag reads a script file and returns nil when unset.
// @testcase TestSpokeInitScriptErrors errors on a missing file and on an empty script.
func (o spokeOptions) initScript() ([]byte, error) {
	if o.initScriptPath == "" {
		return nil, nil
	}
	data, err := os.ReadFile(o.initScriptPath)
	if err != nil {
		return nil, fmt.Errorf("reading --init-script: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("--init-script %q is empty", o.initScriptPath)
	}
	return data, nil
}

// maxCopyBytes caps the total content the --copy flag stages into a box. Every
// copied file rides the single Init control frame alongside the injected files
// and init script, and that frame is bounded (guest.maxFrame, 16 MiB) — and JSON
// base64-inflates the bytes by ~1/3 — so a generous-but-safe raw ceiling keeps a
// large --copy from silently overflowing the frame at box-create time. It is meant
// for config, credentials, and seed data, not bulk data; a directory that needs
// more belongs in the image or an init-script download.
const maxCopyBytes = 10 << 20

// copyFiles resolves the spoke's --copy specs into the files the box manager
// stages into every box during Init. Each spec is "HOST_SRC[:BOX_DEST]": HOST_SRC
// is a host file or directory, BOX_DEST the absolute in-box path it lands at
// (defaulting to HOST_SRC's absolute path when omitted). A directory is copied
// recursively, each regular file preserving its mode; non-regular entries
// (symlinks, sockets, devices) are skipped. It returns nil (nothing to copy) when
// the flag is unset, and errors — failing the spoke at startup rather than on
// every box create — when a spec is malformed, a source is missing or unreadable,
// or the total content would overflow the Init frame.
//
// @return []sandbox.InjectFile The files to copy into every box, or nil when --copy is unset.
// @error error if a spec is malformed, a source path is missing/unreadable/an unsupported type, or the total content exceeds maxCopyBytes.
//
// @testcase TestSpokeCopyFiles copies a file and a directory tree, preserves modes, defaults the destination, and returns nil when unset.
// @testcase TestSpokeCopyFilesErrors rejects a malformed spec, a relative destination, a missing source, and an oversize copy.
func (o spokeOptions) copyFiles() ([]sandbox.InjectFile, error) {
	if len(o.copyPaths) == 0 {
		return nil, nil
	}
	var out []sandbox.InjectFile
	var total int64
	for _, spec := range o.copyPaths {
		src, dest, err := parseCopySpec(spec)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(src)
		if err != nil {
			return nil, fmt.Errorf("--copy %q: %w", spec, err)
		}
		if info.IsDir() {
			err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.Type().IsRegular() {
					return nil // skip directories themselves and non-regular entries
				}
				rel, err := filepath.Rel(src, p)
				if err != nil {
					return err
				}
				fi, err := d.Info()
				if err != nil {
					return err
				}
				f, ferr := readCopyFile(p, path.Join(dest, filepath.ToSlash(rel)), fi)
				if ferr != nil {
					return ferr
				}
				total += int64(len(f.Content))
				out = append(out, f)
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("--copy %q: %w", spec, err)
			}
		} else if info.Mode().IsRegular() {
			f, err := readCopyFile(src, dest, info)
			if err != nil {
				return nil, fmt.Errorf("--copy %q: %w", spec, err)
			}
			total += int64(len(f.Content))
			out = append(out, f)
		} else {
			return nil, fmt.Errorf("--copy %q: %s is not a regular file or directory", spec, src)
		}
		if total > maxCopyBytes {
			return nil, fmt.Errorf("--copy total exceeds %d bytes; --copy is for config and seed data, not bulk data (bake large files into the image or fetch them from the init script)", int64(maxCopyBytes))
		}
	}
	return out, nil
}

// parseCopySpec splits a --copy value into its host source and absolute in-box
// destination. It splits on the first ':' so a "HOST_SRC:BOX_DEST" form works;
// with no ':' the destination defaults to the source's absolute path. The
// destination must be an absolute (in-box) path so a box file always lands
// somewhere predictable.
//
// @arg spec The raw --copy value.
// @return string The host source path.
// @return string The cleaned absolute in-box destination path.
// @error error if the source is empty, or the destination is empty or not absolute.
//
// @testcase TestSpokeCopyFiles defaults the destination to the source's absolute path.
// @testcase TestSpokeCopyFilesErrors rejects an empty source and a relative destination.
func parseCopySpec(spec string) (string, string, error) {
	src, dest := spec, ""
	if i := strings.IndexByte(spec, ':'); i >= 0 {
		src, dest = spec[:i], spec[i+1:]
	}
	if src == "" {
		return "", "", fmt.Errorf("--copy %q: empty source path", spec)
	}
	if dest == "" {
		abs, err := filepath.Abs(src)
		if err != nil {
			return "", "", fmt.Errorf("--copy %q: resolving default destination: %w", spec, err)
		}
		dest = abs
	}
	if !path.IsAbs(dest) {
		return "", "", fmt.Errorf("--copy %q: destination %q must be an absolute in-box path", spec, dest)
	}
	return src, path.Clean(dest), nil
}

// readCopyFile reads a host file into an InjectFile bound for dest, preserving its
// permission bits. The owner is left zero: the guest writes --copy files owned by
// the box user regardless, so the workload can use them.
//
// @arg src The host file to read.
// @arg dest The absolute in-box path it lands at.
// @arg info The host file's stat, for its mode.
// @return sandbox.InjectFile The file to inject.
// @error error if the file cannot be read.
//
// @testcase TestSpokeCopyFiles preserves an executable file's mode.
func readCopyFile(src, dest string, info fs.FileInfo) (sandbox.InjectFile, error) {
	content, err := os.ReadFile(src)
	if err != nil {
		return sandbox.InjectFile{}, err
	}
	return sandbox.InjectFile{Path: dest, Content: content, Mode: int64(info.Mode().Perm())}, nil
}

// parsePublishPorts turns the raw --publish-port flag values into the port specs
// the box manager returns on every create, validating each as "PORT[:DESCRIPTION]"
// with a 1-65535 port and rejecting duplicates, so a typo fails the spoke at
// startup rather than silently on every box. Returns nil when the flag is unset.
//
// @return []sandbox.PublishPort The parsed ports to publish, or nil when none are configured.
// @error error if a value is malformed, out of range, or names a duplicate port.
//
// @testcase TestSpokeParsePublishPorts parses port and port:description forms and rejects bad input.
func (o spokeOptions) parsePublishPorts() ([]sandbox.PublishPort, error) {
	if len(o.publishPorts) == 0 {
		return nil, nil
	}
	out := make([]sandbox.PublishPort, 0, len(o.publishPorts))
	seen := make(map[int]bool, len(o.publishPorts))
	for _, spec := range o.publishPorts {
		portStr, desc := spec, ""
		if i := strings.IndexByte(spec, ':'); i >= 0 {
			portStr, desc = spec[:i], spec[i+1:]
		}
		port, err := strconv.Atoi(strings.TrimSpace(portStr))
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid --publish-port %q: want PORT[:DESCRIPTION] with PORT in 1-65535", spec)
		}
		if seen[port] {
			return nil, fmt.Errorf("--publish-port %d is given more than once", port)
		}
		seen[port] = true
		out = append(out, sandbox.PublishPort{Port: port, Description: strings.TrimSpace(desc)})
	}
	return out, nil
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
	fc := newSpokeRunCmd("firecracker", "Run a spoke that runs boxes as Firecracker microVMs", addFirecrackerSpokeFlags)
	fc.AddCommand(newFirecrackerFetchCmd())
	fc.AddCommand(newFirecrackerVMCmd())
	root.AddCommand(fc)
	return root
}

// newFirecrackerFetchCmd builds the `fetch` subcommand of the firecracker spoke: it
// downloads the published guest images (kernel, base rootfs, payload) into the
// on-disk cache the backend reads from and exits, without joining a hub or setting
// up networking. The download is resumable, so a slow or flaky link that interrupts
// a multi-GiB transfer picks up where it left off on the next run instead of
// restarting — letting an operator pre-seed a spoke's images, or warm the cache on
// a faster host, separately from running the spoke.
//
// @return *cobra.Command The configured fetch subcommand.
//
// @testcase TestFirecrackerFetchCmd exposes the state-dir and registry flags and no run flags.
func newFirecrackerFetchCmd() *cobra.Command {
	var stateDir string
	var reg registryFlags
	cmd := &cobra.Command{
		Use:           "fetch",
		Short:         "Download the Firecracker guest images into the local cache (resumable), then exit",
		SilenceUsage:  true,
		SilenceErrors: false,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runFirecrackerFetch(cmd.Context(), stateDir, reg)
		},
	}
	f := cmd.Flags()
	f.StringVar(&stateDir, "state-dir", "", "spoke state directory whose assets/ subdir caches the images (empty uses the backend default; LLMBOX_FC_ASSET_CACHE overrides)")
	f.StringVar(&reg.host, "registry", "", `registry host to authenticate to when pulling the images, e.g. "ghcr.io" (empty pulls anonymously)`)
	f.StringVar(&reg.username, "registry-username", "", "username for --registry")
	f.StringVar(&reg.passwordFile, "registry-password-file", "", "file holding the password or token for --registry")
	return cmd
}

// runFirecrackerFetch downloads the published Firecracker guest images into the
// backend's on-disk cache and returns once they are all present, so the fetch
// completes and exits rather than running a spoke. It resolves the optional
// registry credential the same way a run does and stops promptly on SIGINT/SIGTERM,
// leaving the partial downloads on disk for the next invocation to resume.
//
// @arg parent Base context; the fetch stops when it (or SIGINT/SIGTERM) fires.
// @arg stateDir The spoke's --state-dir, selecting the cache location.
// @arg reg The optional registry credential flags for an authenticated pull.
// @error error if the registry password file cannot be read or an image cannot be fetched.
//
// @testcase TestRunFirecrackerFetchBadRegistry errors when a registry is set without a readable password file.
func runFirecrackerFetch(parent context.Context, stateDir string, reg registryFlags) error {
	regs, err := spokeOptions{registry: reg}.registries()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cacheDir, paths, err := firecracker.FetchAssets(ctx, stateDir, boxconfig.RegistryAuths(regs))
	if err != nil {
		return err
	}
	for _, p := range paths {
		log.Printf("fetched %s", p)
	}
	log.Printf("firecracker: guest images ready in %s", cacheDir)
	return nil
}

// newFirecrackerVMCmd builds the `vm` subcommand group of the firecracker spoke: an
// operator escape hatch to `list` and `destroy` microVM boxes directly from the
// host's on-disk state, without a running spoke or a hub. Normal box lifecycle is
// driven by the hub; these exist only to inspect or reap boxes a crashed or detached
// spoke left running on the host.
//
// @return *cobra.Command The `vm` command with its list and destroy subcommands.
//
// @testcase TestFirecrackerVMCmd wires the list and destroy subcommands with a --state-dir flag.
func newFirecrackerVMCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "vm",
		Short:         "Operator tools to list and destroy Firecracker microVM boxes on this host",
		SilenceUsage:  true,
		SilenceErrors: false,
		Args:          cobra.NoArgs,
	}
	cmd.AddCommand(newFirecrackerVMListCmd(), newFirecrackerVMDestroyCmd(), newFirecrackerVMDestroyAllCmd())
	return cmd
}

// newFirecrackerVMListCmd builds `vm list`: it prints every microVM box persisted
// under the state dir with its id, phase, and probed running state, then exits.
//
// @return *cobra.Command The configured list subcommand.
//
// @testcase TestFirecrackerVMCmd exposes the --state-dir flag on the list subcommand.
func newFirecrackerVMListCmd() *cobra.Command {
	var stateDir string
	cmd := &cobra.Command{
		Use:           "list",
		Short:         "List the Firecracker microVM boxes persisted on this host",
		SilenceUsage:  true,
		SilenceErrors: false,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runFirecrackerVMList(cmd.OutOrStdout(), stateDir)
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "spoke state directory holding per-box state (empty uses the backend default)")
	return cmd
}

// newFirecrackerVMDestroyCmd builds `vm destroy <box-id|token>`: it stops and removes
// a single box (halting a live VMM first) directly against the state dir, then exits.
//
// @return *cobra.Command The configured destroy subcommand.
//
// @testcase TestFirecrackerVMCmd exposes the --state-dir flag and requires one argument on destroy.
func newFirecrackerVMDestroyCmd() *cobra.Command {
	var stateDir string
	cmd := &cobra.Command{
		Use:           "destroy <box-id|token>",
		Short:         "Stop and remove a single Firecracker microVM box by id or token",
		SilenceUsage:  true,
		SilenceErrors: false,
		Args:          cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFirecrackerVMDestroy(cmd.OutOrStdout(), stateDir, args[0])
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "spoke state directory holding per-box state (empty uses the backend default)")
	return cmd
}

// newFirecrackerVMDestroyAllCmd builds `vm destroy-all`: it stops and removes every
// microVM box on the host directly against the state dir, then exits. It requires
// --yes so the destructive sweep is never triggered by a stray invocation.
//
// @return *cobra.Command The configured destroy-all subcommand.
//
// @testcase TestFirecrackerVMCmd exposes --state-dir and --yes on the destroy-all subcommand.
func newFirecrackerVMDestroyAllCmd() *cobra.Command {
	var stateDir string
	var yes bool
	cmd := &cobra.Command{
		Use:           "destroy-all",
		Short:         "Stop and remove every Firecracker microVM box on this host (requires --yes)",
		SilenceUsage:  true,
		SilenceErrors: false,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				return errors.New("refusing to destroy all boxes without --yes")
			}
			return runFirecrackerVMDestroyAll(cmd.OutOrStdout(), stateDir)
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "spoke state directory holding per-box state (empty uses the backend default)")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm destroying every box on this host")
	return cmd
}

// runFirecrackerVMList prints a box-per-line table of the microVM boxes persisted
// under stateDir, or a single "no boxes" line when there are none.
//
// @arg out The writer the table is printed to.
// @arg stateDir The spoke's --state-dir, selecting the state location; empty uses the backend default.
// @error error if the state directory exists but cannot be read.
//
// @testcase TestRunFirecrackerVMList prints a header and a row per persisted box.
func runFirecrackerVMList(out io.Writer, stateDir string) error {
	vms, err := firecracker.ListVMs(stateDir)
	if err != nil {
		return err
	}
	if len(vms) == 0 {
		fmt.Fprintln(out, "no firecracker boxes on this host")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TOKEN\tBOX ID\tNAMESPACE\tPHASE\tSTATE\tSLOT\tCREATED")
	for _, v := range vms {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			v.Token, dashIfEmpty(v.BoxID), dashIfEmpty(v.Namespace), v.Phase,
			vmStateLabel(v), v.NetIndex, time.Unix(v.Created, 0).Format(time.RFC3339))
	}
	return tw.Flush()
}

// runFirecrackerVMDestroy destroys the single box matching idOrToken under stateDir
// and reports what it removed.
//
// @arg out The writer the confirmation is printed to.
// @arg stateDir The spoke's --state-dir, selecting the state location; empty uses the backend default.
// @arg idOrToken The box to destroy: exact token, exact box id, or unique token prefix.
// @error error if no box matches, the match is ambiguous, or the box cannot be removed.
//
// @testcase TestRunFirecrackerVMDestroy removes the matched box and prints its token.
func runFirecrackerVMDestroy(out io.Writer, stateDir, idOrToken string) error {
	v, err := firecracker.DestroyVM(stateDir, idOrToken)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "destroyed firecracker box %s\n", v.Token)
	return nil
}

// runFirecrackerVMDestroyAll destroys every box under stateDir and reports each one
// removed plus a count. It surfaces any per-box teardown failures as its error while
// still having destroyed the boxes it could, so a single stuck VMM never leaves the
// rest of the host un-swept.
//
// @arg out The writer the confirmations are printed to.
// @arg stateDir The spoke's --state-dir, selecting the state location; empty uses the backend default.
// @error error if the state cannot be read, or joining the failures of any boxes that could not be destroyed.
//
// @testcase TestRunFirecrackerVMDestroyAll removes every box and prints a count.
func runFirecrackerVMDestroyAll(out io.Writer, stateDir string) error {
	destroyed, err := firecracker.DestroyAllVMs(stateDir)
	for _, v := range destroyed {
		fmt.Fprintf(out, "destroyed firecracker box %s\n", v.Token)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "destroyed %d firecracker box(es)\n", len(destroyed))
	return nil
}

// vmStateLabel renders a box's runtime state for the list table: paused, running, or
// stopped, from its probed liveness and persisted paused flag.
//
// @arg v The box snapshot.
// @return string "paused", "running", or "stopped".
//
// @testcase TestRunFirecrackerVMList labels a running box.
func vmStateLabel(v firecracker.VMStatus) string {
	switch {
	case v.Paused:
		return "paused"
	case v.Running:
		return "running"
	default:
		return "stopped"
	}
}

// dashIfEmpty renders an empty optional column as "-" so the table stays aligned.
//
// @arg s The value, possibly empty.
// @return string s, or "-" when s is empty.
//
// @testcase TestRunFirecrackerVMList renders a dash for a box with no box id.
func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
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
	f.StringVar(&o.statePath, "state", defaultSpokeStatePath(), "file storing this spoke's issued credential")
	f.StringVar(&o.tlsCAFile, "tls-ca", "", "PEM CA bundle to trust for a wss:// hub with a private-CA or self-signed certificate")
	f.BoolVar(&o.tlsInsecure, "tls-insecure", false, "skip TLS certificate verification when dialing a wss:// hub (testing only; prefer --tls-ca)")
	f.StringVar(&o.box.Namespace, "namespace", "", "scope this spoke's boxes to a namespace so two spokes can share one host without collapsing each other's boxes; empty is unscoped")
	f.StringVar(&o.initScriptPath, "init-script", "", "host path to a script run inside every box on this spoke, once at creation before the box's workload starts, as the box user; empty runs none")
	f.DurationVar(&o.initScriptTimeout, "init-script-timeout", 5*time.Minute, "max time the --init-script may run before box creation fails")
	f.StringArrayVar(&o.copyPaths, "copy", nil, "host file or directory copied into every box on this spoke at creation, as HOST_SRC[:BOX_DEST] (BOX_DEST is an absolute in-box path, defaulting to HOST_SRC's absolute path); like docker -v but a copy, not a mount, and owned by the box user (repeatable)")
	f.StringArrayVar(&o.publishPorts, "publish-port", nil, "in-box TCP port to expose as an HTTP proxy for every box on this spoke, as PORT[:DESCRIPTION] (repeatable); needs proxying enabled on the hub")
	f.IntVar(&o.box.MemoryMB, "box-memory-mb", boxconfig.DefaultBoxMemoryMB, "hard memory limit per box in MiB (0 = unlimited)")
	f.Float64Var(&o.box.CPUs, "box-cpus", boxconfig.DefaultBoxCPUs, "CPU quota per box, fractional allowed (0 = unlimited)")
	f.Int64Var(&o.box.PidsLimit, "box-pids-limit", boxconfig.DefaultBoxPidsLimit, "max processes/threads per box, blunts fork bombs (0 = unlimited)")
	f.IntVar(&o.box.MaxBoxes, "max-boxes", 0, "max concurrent boxes on this spoke (0 = unlimited)")
	f.Float64Var(&o.box.DiskGB, "box-disk-gb", boxconfig.DefaultBoxDiskGB, "default writable-disk size per box in GiB when a create names none; Firecracker only, grown from the small base image (0 = keep base size)")
	f.Float64Var(&o.box.MaxDiskGB, "box-max-disk-gb", boxconfig.DefaultBoxMaxDiskGB, "hard ceiling on a per-create disk request in GiB (0 = no ceiling)")
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
	f.StringVar(&o.fcPayloadImage, "payload", "", "host path to a read-only ext4 carrying the guest binary, attached as a shared second drive so the guest updates without rebuilding the rootfs; empty pulls the published payload (unless --rootfs is a custom all-in-one image)")
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
// @error error if the Docker manager cannot be built, the GPU spec is malformed, a registry password file cannot be read, the init script cannot be read, a --copy source is missing or too large, no credential or token is available, the state path is not writable for a first enrollment, or enrollment is rejected.
//
// @testcase TestRunSpokeRequiresTokenOrCreds errors when neither a token nor saved credentials are available.
// @testcase TestRunSpokeRejectsBadGPUs errors when the GPU spec is malformed.
// @testcase TestRunSpokeRejectsBadInitScript errors when --init-script names an unreadable file.
func runSpoke(parent context.Context, o spokeOptions) error {
	statePath, token, hubURL := o.statePath, o.token, o.hubURL
	// The spoke owns the box image: every box it runs launches o.image (empty uses
	// the backend's built-in default). The hub never names an image, so nothing
	// about the image crosses the hub/spoke boundary.
	regs, err := o.registries()
	if err != nil {
		return err
	}
	// The init script is a spoke-local file read once here (fail fast if it is
	// missing or empty) and run inside every box this spoke spawns; it never crosses
	// the hub/spoke boundary.
	initScript, err := o.initScript()
	if err != nil {
		return err
	}
	// Host files this spoke copies into every box at creation (--copy), resolved
	// once here so a missing source or oversize copy fails the spoke at startup
	// rather than on every box create; they never cross the hub/spoke boundary.
	copyFiles, err := o.copyFiles()
	if err != nil {
		return err
	}
	// Ports this spoke exposes as HTTP proxies for every box it creates; parsed once
	// here so a malformed --publish-port fails the spoke at startup.
	publishPorts, err := o.parsePublishPorts()
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
	mgr := box.NewManager(prov, box.Config{
		MaxBoxes:          o.box.MaxBoxes,
		InitScript:        initScript,
		InitScriptTimeout: o.initScriptTimeout,
		CopyFiles:         copyFiles,
		PublishPorts:      publishPorts,
	})

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
