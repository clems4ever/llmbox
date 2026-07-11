// Package docker implements a box.Provisioner backed by the Docker Engine API.
// Each box is a container whose entrypoint is the llmbox guest (run under
// tini); the host reaches the guest over a per-box Unix socket bind-mounted from
// the host into the container, so all box behaviour (login, exec, logs, port
// dialing) runs through the guest rather than through Docker exec/attach. The
// provisioner only owns compute: it creates, lists, resolves, and destroys
// containers, and hands back a control channel to the guest.
//
// Safety: every container carries ManagedLabel; list/find/destroy are scoped to
// that label so unrelated host containers are never touched. A provisioner may
// additionally be pinned to a namespace (SetNamespace): its containers then also
// carry NamespaceLabel and list/find/destroy are scoped to it, so two spokes
// sharing one Docker daemon never see, reap, or destroy each other's boxes.
package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/distribution/reference"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/box"
	"github.com/clems4ever/llmbox/internal/spoke/boxapi"
)

const (
	// ManagedLabel marks every container created by this provisioner.
	ManagedLabel = "com.llmbox.managed"

	// BoxIDLabel and DescriptionLabel persist the caller-assigned box ID and
	// description so List can report them straight from a container summary.
	BoxIDLabel       = "com.llmbox.box-id"
	DescriptionLabel = "com.llmbox.description"

	// GenerationLabel persists the box's opaque generation token: the per-box
	// incarnation identity exposed to the hub as the box's InstanceID. It is a
	// spoke-minted random token (never the Docker container id), stored on the
	// container so List can recover it after a spoke restart. The hub uses it only
	// for staleness/reap equality — it is never parsed, prefix-matched, or used to
	// address the box (addressing is by box ID). Keeping it distinct from the real
	// container id is what prevents any Docker-native handle from reaching the hub.
	GenerationLabel = "com.llmbox.generation"

	// socketLabel persists the per-box socket token (the subdirectory under the
	// provisioner's socket dir holding the box's control socket), so List/Find can
	// reconstruct the socket path from a container summary alone.
	socketLabel = "com.llmbox.socket"

	// NamespaceLabel scopes a container to one provisioner's namespace (see
	// SetNamespace). It is only set when a namespace is configured; boxes created
	// without one carry no NamespaceLabel and are visible to any unscoped
	// provisioner, preserving the pre-namespace behaviour.
	NamespaceLabel = "com.llmbox.namespace"

	// DefaultImage is launched when the caller does not specify one. It bakes in
	// the standalone Claude binary, the llmbox-guest binary (its entrypoint),
	// tini, and a CA bundle (see Dockerfile.box).
	DefaultImage = "ghcr.io/clems4ever/llmbox-box:latest"

	// DefaultSocketDir is the host directory holding per-box control sockets when
	// the operator does not configure one. Each box gets a 0700 subdirectory.
	DefaultSocketDir = "/run/llmbox/boxsockets"

	// socketMountTarget is where each box's socket directory is bind-mounted
	// inside the container; the guest's default --socket lives directly under it.
	socketMountTarget = "/run/llmbox"
	socketFileName    = "control.sock"
	// pausedMarkerFile is written into a box's host socket dir when the box is
	// paused, so List can report it as sandbox.StatePaused rather than the "exited"
	// a stopped container would otherwise show — distinguishing a deliberate pause
	// from a crash. Resume removes it; Destroy removes the whole dir.
	pausedMarkerFile = "paused"

	// boxHome and boxWorkdir are the home and working directory forced on a box,
	// so the baked ~/.claude.json trust seed and the credentials Claude writes land
	// in known, writable paths regardless of the base image's own user/WORKDIR.
	boxHome    = "/root"
	boxWorkdir = "/workspace"

	// pendingPrefix / readyPrefix encode a box's auth phase in its name so the
	// phase survives a restart of this server. Reaping targets pendingPrefix only.
	pendingPrefix = "llmbox-pending-"
	readyPrefix   = "llmbox-"

	// defaultBridgeNetwork is Docker's default bridge. A box is created on it then
	// detached (once on its own per-box network) so boxes can't reach each other.
	defaultBridgeNetwork = "bridge"

	// stopTimeout is how long Docker waits after SIGTERM before SIGKILL when
	// stopping a box, giving Claude a chance to deregister its session.
	stopTimeout = 10 * time.Second

	// socketWait bounds how long Provision waits for the guest to create its
	// control socket after the container starts.
	socketWait = 30 * time.Second
)

// dockerAPI is the subset of the Docker client the provisioner uses. It exists so
// the Docker layer can be faked in tests; *client.Client satisfies it.
type dockerAPI interface {
	ContainerList(ctx context.Context, opts container.ListOptions) ([]container.Summary, error)
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, opts container.StartOptions) error
	ContainerStop(ctx context.Context, containerID string, opts container.StopOptions) error
	ContainerRemove(ctx context.Context, containerID string, opts container.RemoveOptions) error
	ContainerRename(ctx context.Context, containerID, newName string) error
	ImagePull(ctx context.Context, refStr string, opts image.PullOptions) (io.ReadCloser, error)
	NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error)
	NetworkConnect(ctx context.Context, networkID, containerID string, config *network.EndpointSettings) error
	NetworkDisconnect(ctx context.Context, networkID, containerID string, force bool) error
	NetworkRemove(ctx context.Context, networkID string) error
	Close() error
}

// Provisioner creates and tears down boxes on a Docker daemon and exposes each
// box's guest over a bind-mounted Unix socket. It implements box.Provisioner.
type Provisioner struct {
	cli          dockerAPI
	defaultImage string
	// socketDir is the host directory under which each box gets a 0700
	// subdirectory holding its control socket; it must be readable/writable by
	// this process and bind-mountable into containers.
	socketDir string
	// peers are resource-server container names connected into every box's own
	// network so boxes can reach them while staying isolated from one another.
	peers []string
	// namespace scopes this provisioner to a subset of the daemon's managed
	// containers: when non-empty, created boxes carry NamespaceLabel and
	// list/find/destroy only ever see boxes with a matching label. Empty means
	// unscoped (every managed box on the daemon is in view).
	namespace string
	// limits caps each box's memory/CPU/PIDs (the MaxBoxes field is enforced by
	// box.Manager, not here). The zero value imposes no limits.
	limits sandbox.Limits
	// boxGPUs are the GPU device requests attached to every box (the `docker run
	// --gpus` equivalent); machine-local, set per spoke.
	boxGPUs []container.DeviceRequest
	// registryAuths holds pull credentials keyed by registry host; nil disables
	// authenticated pulls.
	registryAuths map[string]registry.AuthConfig
	log           *slog.Logger

	// ports serves box-originated port-publishing requests toward the hub; nil
	// disables the per-box box-port API socket entirely.
	ports boxapi.PortService
	// apiMu guards apiSrvs.
	apiMu sync.Mutex
	// apiSrvs are the live per-box box-port API servers, keyed by the box's
	// generation token. Each serves boxapi.SocketName inside that box's private
	// host socket dir, so the listener — not anything the box sends — decides which
	// box a request acts on.
	apiSrvs map[string]*boxapi.Server
}

// NewProvisioner builds a Provisioner using Docker configuration from the
// environment. An empty defaultImage falls back to DefaultImage; an empty
// socketDir falls back to DefaultSocketDir.
//
// @arg defaultImage The image launched when a caller does not specify one; empty uses DefaultImage.
// @arg socketDir The host directory holding per-box control sockets; empty uses DefaultSocketDir.
// @arg peers Resource-server container names connected into every box's network.
// @arg ports The service serving box-originated port requests; nil disables the per-box box-port API.
// @return *Provisioner A provisioner wired to a Docker client built from the environment.
// @error error if the Docker client cannot be created.
//
// @testcase TestProvisionCreatesGuestBox covers a provisioner built by NewProvisioner.
func NewProvisioner(defaultImage, socketDir string, peers []string, ports boxapi.PortService) (*Provisioner, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("creating docker client: %w", err)
	}
	if defaultImage == "" {
		defaultImage = DefaultImage
	}
	if socketDir == "" {
		socketDir = DefaultSocketDir
	}
	return &Provisioner{
		cli:          cli,
		defaultImage: defaultImage,
		socketDir:    socketDir,
		peers:        peers,
		ports:        ports,
		apiSrvs:      map[string]*boxapi.Server{},
		log:          slog.Default(),
	}, nil
}

// logger returns the provisioner's logger, or slog.Default() when none was set.
//
// @return *slog.Logger The configured logger, or the slog default.
//
// @testcase TestProvisionCreatesGuestBox exercises a provisioner whose logger defaults.
func (p *Provisioner) logger() *slog.Logger {
	if p.log != nil {
		return p.log
	}
	return slog.Default()
}

// SetPerBoxLimits sets the per-box memory/CPU/PID caps applied at create. The
// MaxBoxes field is ignored here (box.Manager enforces the box count).
//
// @arg l The resource limits; only MemoryBytes/NanoCPUs/PidsLimit are used.
//
// @testcase TestProvisionAppliesLimits applies the configured caps to the host config.
func (p *Provisioner) SetPerBoxLimits(l sandbox.Limits) { p.limits = l }

// SetRegistryAuths supplies pull credentials keyed by registry host; nil/empty
// leaves every pull anonymous.
//
// @arg auths Pull credentials keyed by registry host.
//
// @testcase TestProvisionPullsWithRegistryAuth pulls a private image using the configured credentials.
func (p *Provisioner) SetRegistryAuths(auths map[string]registry.AuthConfig) { p.registryAuths = auths }

// SetBoxGPUs configures the GPUs attached to every box, from a `docker run
// --gpus`-style spec: "" attaches none, "all" attaches every GPU, a positive
// integer attaches that many, and a comma-separated (optionally "device="-
// prefixed) list selects GPUs by id/index.
//
// @arg spec The GPU spec: "", "all", a positive count, or a device list.
// @error error if the spec is malformed.
//
// @testcase TestSetBoxGPUsParsesSpec accepts all/count/device-list specs and rejects bad ones.
// @testcase TestProvisionAppliesGPUs attaches the configured device requests to the host config.
func (p *Provisioner) SetBoxGPUs(spec string) error {
	reqs, err := parseGPUs(spec)
	if err != nil {
		return err
	}
	p.boxGPUs = reqs
	return nil
}

// SetNamespace pins this provisioner to a namespace so it only ever sees the
// boxes it created: created boxes carry NamespaceLabel with this value and
// List/Find/Destroy are scoped to it. This lets two spokes share one Docker
// daemon without collapsing each other's containers. An empty namespace leaves
// the provisioner unscoped (the pre-namespace behaviour).
//
// @arg ns The namespace to scope this provisioner to; empty leaves it unscoped.
//
// @testcase TestProvisionSetsNamespaceLabel labels a box with the configured namespace.
// @testcase TestManagedFilterScopesByNamespace scopes list/find to the namespace.
func (p *Provisioner) SetNamespace(ns string) { p.namespace = ns }

// Close stops every live box-port API listener and releases the Docker client.
// The boxes themselves keep running (they are recovered on the next start via
// RecoverBoxAPIs).
//
// @error error if the Docker client cannot be closed.
//
// @testcase TestProvisionCreatesGuestBox builds a provisioner whose client is closed in cleanup.
// @testcase TestCloseStopsBoxAPIListeners stops the box-port listeners on Close.
func (p *Provisioner) Close() error {
	p.stopAllBoxAPIs()
	return p.cli.Close()
}

// managedFilter restricts Docker listings to containers this provisioner created.
// When the provisioner is namespaced, it further restricts them to that
// namespace, so a provisioner never lists boxes belonging to another namespace
// on the same daemon.
//
// @return filters.Args A filter matching containers carrying ManagedLabel (and NamespaceLabel when namespaced).
//
// @testcase TestListMapsManagedContainers exercises managedFilter via List.
// @testcase TestManagedFilterScopesByNamespace adds the namespace label when namespaced.
func (p *Provisioner) managedFilter() filters.Args {
	args := filters.NewArgs(filters.Arg("label", ManagedLabel+"=true"))
	if p.namespace != "" {
		args.Add("label", NamespaceLabel+"="+p.namespace)
	}
	return args
}

// phaseOf reports a box's auth phase from its container name.
//
// @arg name The container name to inspect.
// @return string "pending" if the name has the pending prefix, else "ready".
//
// @testcase TestListMapsManagedContainers checks the phase derived from names.
func phaseOf(name string) string {
	if strings.HasPrefix(name, pendingPrefix) {
		return "pending"
	}
	return "ready"
}

// boxFromSummary maps a Docker container summary to a sandbox.Box view.
//
// @arg c The container summary from a managed list.
// @return sandbox.Box The box view with phase, box ID, and description filled in.
//
// @testcase TestListMapsManagedContainers checks the mapping from a container summary.
func boxFromSummary(c container.Summary) sandbox.Box {
	name := ""
	if len(c.Names) > 0 {
		name = strings.TrimPrefix(c.Names[0], "/")
	}
	return sandbox.Box{
		InstanceID:  c.Labels[GenerationLabel],
		Name:        name,
		BoxID:       c.Labels[BoxIDLabel],
		Description: c.Labels[DescriptionLabel],
		Image:       c.Image,
		State:       c.State,
		Status:      c.Status,
		Phase:       phaseOf(name),
		Created:     c.Created,
	}
}

// List returns a handle to every managed box, running or not.
//
// @arg ctx Context for the Docker list request.
// @return []box.Instance One handle per managed container.
// @error error if listing containers fails.
//
// @testcase TestListMapsManagedContainers checks the mapping and the managed filter.
func (p *Provisioner) List(ctx context.Context) ([]box.Instance, error) {
	cs, err := p.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: p.managedFilter()})
	if err != nil {
		return nil, fmt.Errorf("listing boxes: %w", err)
	}
	out := make([]box.Instance, 0, len(cs))
	for _, c := range cs {
		token := c.Labels[socketLabel]
		b := boxFromSummary(c)
		// A paused box's container is stopped (Docker reports "exited"); the marker
		// distinguishes that deliberate pause from a crash so callers see it as
		// paused, not dead.
		if token != "" && pausedMarkerExists(p.socketDir, token) {
			b.State = sandbox.StatePaused
		}
		out = append(out, &dockerInstance{prov: p, box: b, socketToken: token, containerID: c.ID})
	}
	return out, nil
}

// pausedMarkerExists reports whether the paused marker is present in a box's host
// socket dir, i.e. the box was paused (and not yet resumed or destroyed).
//
// @arg socketDir The provisioner's socket root.
// @arg socketToken The box's socket subdirectory name.
// @return bool True when the box's paused marker file exists.
//
// @testcase TestPauseResumeReportsPausedState reports a paused box after Pause and running after Resume.
func pausedMarkerExists(socketDir, socketToken string) bool {
	_, err := os.Stat(filepath.Join(socketDir, socketToken, pausedMarkerFile))
	return err == nil
}

// Find resolves an ID or name to the single managed box it identifies.
//
// @arg ctx Context for the underlying list.
// @arg idOrName The caller-assigned box ID (the usual handle), the box's generation token, or its container name.
// @return box.Instance The matched box.
// @error error wrapping sandbox.ErrBoxNotFound if no managed box matches.
//
// @testcase TestFindResolvesByIDAndBoxID resolves a box by its generation token and by its box id.
// @testcase TestFindUnknownBox errors when no managed box matches.
func (p *Provisioner) Find(ctx context.Context, idOrName string) (box.Instance, error) {
	insts, err := p.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, inst := range insts {
		b := inst.Meta()
		// Resolve by exact box ID (the hub's usual handle), exact generation token,
		// or exact container name. No prefix matching: the generation token is an
		// opaque incarnation identity, not a Docker short/full container id.
		if (b.BoxID != "" && b.BoxID == idOrName) ||
			(b.InstanceID != "" && b.InstanceID == idOrName) ||
			b.Name == idOrName ||
			b.Name == pendingPrefix+idOrName ||
			b.Name == readyPrefix+idOrName {
			return inst, nil
		}
	}
	return nil, fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, idOrName)
}

// Provision creates and starts a box: a container whose entrypoint is the guest,
// with the box's host socket directory bind-mounted in so the host can
// reach the guest. The box is created on its own network (peers wired in,
// isolated from other boxes), named with the pending prefix, and run as root with
// no-new-privileges, the configured resource caps and GPUs, and a restart policy
// of "unless-stopped" so a crashed box (and its always-on guest) comes back. When
// the provisioner is namespaced, the box (and its network) carry NamespaceLabel.
//
// @arg ctx Context for the Docker calls and the socket wait.
// @arg opts The caller-controlled box ID, description, and files (the image is the spoke's configured default).
// @return box.Instance A handle to the started box, in the pending phase.
// @error error if the box id is invalid, the image cannot be pulled, or the box cannot be created, networked, started, or its guest socket does not appear.
//
// @testcase TestProvisionCreatesGuestBox creates a guest-entrypoint box with the socket mount and restart policy.
// @testcase TestProvisionCleansUpOnStartFailure removes the container, network, and socket dir when start fails.
// @testcase TestProvisionAppliesLimits applies the configured resource caps and no-new-privileges.
// @testcase TestProvisionAppliesGPUs attaches the configured GPU device requests.
// @testcase TestProvisionPullsMissingImage pulls the image then retries when it is absent.
// @testcase TestProvisionSetsNamespaceLabel stamps the configured namespace on the box.
func (p *Provisioner) Provision(ctx context.Context, opts sandbox.CreateOptions) (box.Instance, error) {
	if opts.BoxID != "" && !sandbox.ValidBoxID(opts.BoxID) {
		return nil, fmt.Errorf("invalid box id %q", opts.BoxID)
	}
	// Every box on this spoke launches the spoke's configured image; the request
	// carries none (the image is a property of the spoke, not the create).
	image := p.defaultImage

	// Create the per-box socket directory (0700, owned by this process) before the
	// container so it can be bind-mounted in. The directory is the access gate:
	// only this process (and root) can traverse into it, so no other local user
	// can reach the guest socket the box creates inside it.
	token, err := newSocketToken()
	if err != nil {
		return nil, err
	}
	// The generation token is the box's opaque incarnation identity exposed to the
	// hub as its InstanceID; it is distinct from the socket token and never the
	// Docker container id.
	generation, err := newGenerationToken()
	if err != nil {
		return nil, err
	}
	hostBoxDir := filepath.Join(p.socketDir, token)
	if err := os.MkdirAll(hostBoxDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating box socket dir: %w", err)
	}

	labels := map[string]string{ManagedLabel: "true", socketLabel: token, GenerationLabel: generation}
	if p.namespace != "" {
		labels[NamespaceLabel] = p.namespace
	}
	if opts.BoxID != "" {
		labels[BoxIDLabel] = opts.BoxID
	}
	if opts.Description != "" {
		labels[DescriptionLabel] = opts.Description
	}

	cfg := &container.Config{
		Image:      image,
		Hostname:   opts.BoxID,
		Entrypoint: []string{"tini", "-g", "--", "llmbox-guest", "--socket", filepath.Join(socketMountTarget, socketFileName)},
		Labels:     labels,
		User:       "0:0",
		WorkingDir: boxWorkdir,
		Env:        []string{"HOME=" + boxHome},
	}
	hostCfg := &container.HostConfig{
		// Keep the box (and its always-on guest) alive across crashes; the spoke
		// removes it explicitly on Destroy/reap, which "unless-stopped" honours.
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		SecurityOpt:   []string{"no-new-privileges"},
		Mounts: []mount.Mount{{
			Type:   mount.TypeBind,
			Source: hostBoxDir,
			Target: socketMountTarget,
		}},
	}
	if p.limits.MemoryBytes > 0 {
		hostCfg.Memory = p.limits.MemoryBytes
	}
	if p.limits.NanoCPUs > 0 {
		hostCfg.NanoCPUs = p.limits.NanoCPUs
	}
	if p.limits.PidsLimit > 0 {
		hostCfg.PidsLimit = &p.limits.PidsLimit
	}
	if len(p.boxGPUs) > 0 {
		hostCfg.DeviceRequests = p.boxGPUs
	}

	resp, err := p.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
	if err != nil && errdefs.IsNotFound(err) {
		if perr := p.pullImage(ctx, image); perr != nil {
			_ = os.RemoveAll(hostBoxDir)
			return nil, fmt.Errorf("pulling image %q: %w", image, perr)
		}
		resp, err = p.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
	}
	if err != nil {
		_ = os.RemoveAll(hostBoxDir)
		return nil, fmt.Errorf("creating box from image %q: %w", image, err)
	}
	id := resp.ID

	cleanup := func() {
		p.stopBoxAPI(generation)
		if rerr := p.cli.ContainerRemove(context.Background(), id, container.RemoveOptions{Force: true}); rerr != nil {
			p.logger().Warn("failed to remove box during cleanup", "container", id, "err", rerr)
		}
		p.removeBoxNetwork(context.Background(), id)
		_ = os.RemoveAll(hostBoxDir)
	}

	// Serve the box-port API in the box's socket dir before the container
	// starts, so /run/llmbox/boxapi.sock exists from the box's first instant.
	if err := p.startBoxAPI(generation, opts.BoxID, hostBoxDir); err != nil {
		cleanup()
		return nil, err
	}

	if err := p.setupBoxNetwork(ctx, id); err != nil {
		cleanup()
		return nil, err
	}
	// The container name encodes the auth phase (for reaping) and the generation
	// token (for uniqueness) — never the container id, so nothing about the Docker
	// handle leaks through the box's name.
	if err := p.cli.ContainerRename(ctx, id, pendingPrefix+generation); err != nil {
		cleanup()
		return nil, fmt.Errorf("naming box: %w", err)
	}
	if err := p.cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		cleanup()
		return nil, fmt.Errorf("starting box: %w", err)
	}

	// Wait for the guest to create its control socket so the caller's first
	// guest call connects without racing the container's startup.
	sockPath := filepath.Join(hostBoxDir, socketFileName)
	if err := waitForSocket(ctx, sockPath, socketWait); err != nil {
		cleanup()
		return nil, fmt.Errorf("waiting for box guest: %w", err)
	}

	return &dockerInstance{
		prov: p,
		box: sandbox.Box{
			InstanceID:  generation,
			Name:        pendingPrefix + generation,
			BoxID:       opts.BoxID,
			Description: opts.Description,
			Image:       image,
			State:       "running",
			Phase:       "pending",
			Created:     time.Now().Unix(),
		},
		containerID: id,
		socketToken: token,
	}, nil
}

// waitForSocket polls until path exists or the timeout elapses; the guest creates
// the control socket once it is listening, so its appearance means the box is
// reachable.
//
// @arg ctx Context whose cancellation aborts the wait.
// @arg path The control socket path to wait for.
// @arg timeout How long to wait before giving up.
// @error error if the socket does not appear before the timeout or ctx is cancelled.
//
// @testcase TestProvisionCreatesGuestBox waits for a socket the fake create makes appear.
func waitForSocket(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("control socket %s did not appear within %s", path, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// newSocketToken returns a random hex token used as a box's socket subdirectory
// name (and socketLabel value).
//
// @return string A 16-char random hex token.
// @error error if the system random source fails.
//
// @testcase TestProvisionCreatesGuestBox derives a box's socket dir from this token.
func newSocketToken() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating socket token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// newGenerationToken returns a random hex token used as a box's opaque generation
// token (the GenerationLabel value and the box's hub-facing InstanceID). It is
// independent of the socket token and never derived from the Docker container id.
//
// @return string A 16-char random hex token.
// @error error if the system random source fails.
//
// @testcase TestProvisionCreatesGuestBox stamps a box's generation token from this.
func newGenerationToken() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating generation token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// startBoxAPI serves the box-port API socket inside a box's private host
// socket dir, bound to that box's identity. Through the existing bind mount the
// socket appears in-box at /run/llmbox/boxapi.sock. The per-box listener is the
// spoke-side enforcement: whichever socket a request arrives on decides which
// box it acts on, so nothing inside a box can address another box. A no-op
// when the provisioner has no port service.
//
// @arg generation The box's generation token (the apiSrvs key).
// @arg boxID The box's caller-assigned box ID stamped onto every request ("" serves only an explanatory error).
// @arg hostBoxDir The box's private host socket directory.
// @error error if the socket cannot be created.
//
// @testcase TestProvisionStartsBoxAPIListener serves the box API for a provisioned box.
// @testcase TestRecoverBoxAPIsRestartsListeners restarts listeners for recovered boxes.
func (p *Provisioner) startBoxAPI(generation, boxID, hostBoxDir string) error {
	if p.ports == nil {
		return nil
	}
	srv, err := boxapi.ServeUnix(filepath.Join(hostBoxDir, boxapi.SocketName), boxID, p.ports, p.logger())
	if err != nil {
		return fmt.Errorf("serving box-port API: %w", err)
	}
	p.apiMu.Lock()
	if old := p.apiSrvs[generation]; old != nil {
		_ = old.Close()
	}
	p.apiSrvs[generation] = srv
	p.apiMu.Unlock()
	return nil
}

// stopBoxAPI closes and forgets a box's box-port API server; unknown generation
// tokens are a no-op.
//
// @arg generation The box's generation token.
//
// @testcase TestDestroyStopsBoxAPIListener stops the listener when a box is destroyed.
func (p *Provisioner) stopBoxAPI(generation string) {
	p.apiMu.Lock()
	srv := p.apiSrvs[generation]
	delete(p.apiSrvs, generation)
	p.apiMu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

// stopAllBoxAPIs closes every live box-port API server (used on provisioner
// Close).
//
// @testcase TestCloseStopsBoxAPIListeners stops all listeners on Close.
func (p *Provisioner) stopAllBoxAPIs() {
	p.apiMu.Lock()
	srvs := p.apiSrvs
	p.apiSrvs = map[string]*boxapi.Server{}
	p.apiMu.Unlock()
	for _, srv := range srvs {
		_ = srv.Close()
	}
}

// RecoverBoxAPIs restarts the box-port API listener of every managed box after
// a spoke restart: the containers persist (restart policy unless-stopped) but
// the host-side listeners die with the spoke process. Each box's identity and
// socket dir are recovered from its container labels. Best-effort per box —
// one failure is logged and does not block the others.
//
// @arg ctx Context for the container listing.
// @error error if the managed containers cannot be listed.
//
// @testcase TestRecoverBoxAPIsRestartsListeners restarts listeners from container labels.
func (p *Provisioner) RecoverBoxAPIs(ctx context.Context) error {
	if p.ports == nil {
		return nil
	}
	insts, err := p.List(ctx)
	if err != nil {
		return err
	}
	for _, inst := range insts {
		di, ok := inst.(*dockerInstance)
		if !ok || di.socketToken == "" {
			continue
		}
		b := di.Meta()
		if err := p.startBoxAPI(b.InstanceID, b.BoxID, filepath.Join(p.socketDir, di.socketToken)); err != nil {
			p.logger().Warn("failed to recover box-port API", "box", b.InstanceID, "err", err)
		}
	}
	return nil
}

// boxNetworkName is the deterministic name of a box's dedicated network.
//
// @arg id The box's container ID.
// @return string The per-box network name.
//
// @testcase TestProvisionCreatesGuestBox names the box network after the box.
func boxNetworkName(id string) string { return "llmboxnet-" + id[:12] }

// setupBoxNetwork creates the box's own bridge network, connects the box and the
// resource-server peers to it, and detaches the box from the default bridge so
// boxes stay isolated from one another while reaching the shared peers.
//
// @arg ctx Context for the network calls.
// @arg id The box's container ID.
// @error error if the network cannot be created or the box cannot be connected/detached.
//
// @testcase TestProvisionConnectsPeers creates the network, connects box and peers, and detaches the bridge.
func (p *Provisioner) setupBoxNetwork(ctx context.Context, id string) error {
	name := boxNetworkName(id)
	netLabels := map[string]string{ManagedLabel: "true"}
	if p.namespace != "" {
		netLabels[NamespaceLabel] = p.namespace
	}
	if _, err := p.cli.NetworkCreate(ctx, name, network.CreateOptions{
		Driver: "bridge",
		Labels: netLabels,
	}); err != nil {
		return fmt.Errorf("creating box network: %w", err)
	}
	if err := p.cli.NetworkConnect(ctx, name, id, nil); err != nil {
		return fmt.Errorf("connecting box to its network: %w", err)
	}
	for _, peer := range p.peers {
		if err := p.cli.NetworkConnect(ctx, name, peer, nil); err != nil {
			p.logger().Warn("failed to connect peer to box network", "network", name, "peer", peer, "err", err)
		}
	}
	if err := p.cli.NetworkDisconnect(ctx, defaultBridgeNetwork, id, true); err != nil {
		return fmt.Errorf("detaching box from the default bridge: %w", err)
	}
	return nil
}

// removeBoxNetwork tears down a box's dedicated network, disconnecting the peers
// first. It is best-effort: failures are logged, not returned.
//
// @arg ctx Context for the disconnect/remove calls.
// @arg id The box's container ID.
//
// @testcase TestDestroyRemovesNetworkAndSocket checks the box network is removed on destroy.
func (p *Provisioner) removeBoxNetwork(ctx context.Context, id string) {
	name := boxNetworkName(id)
	for _, peer := range p.peers {
		if err := p.cli.NetworkDisconnect(ctx, name, peer, true); err != nil {
			p.logger().Warn("failed to disconnect peer from box network", "network", name, "peer", peer, "err", err)
		}
	}
	if err := p.cli.NetworkRemove(ctx, name); err != nil {
		p.logger().Warn("failed to remove box network", "network", name, "err", err)
	}
}

// pullImage pulls ref, using the configured registry credentials when one matches
// ref's registry host.
//
// @arg ctx Context for the pull.
// @arg ref The image reference to pull.
// @error error if the pull cannot be started or its progress cannot be read.
//
// @testcase TestProvisionPullsMissingImage pulls the image when the create reports it missing.
func (p *Provisioner) pullImage(ctx context.Context, ref string) error {
	opts := image.PullOptions{}
	if auth, ok := p.registryAuthFor(ref); ok {
		encoded, err := registry.EncodeAuthConfig(auth)
		if err != nil {
			return fmt.Errorf("encoding registry auth for %q: %w", ref, err)
		}
		opts.RegistryAuth = encoded
	}
	rc, err := p.cli.ImagePull(ctx, ref, opts)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return fmt.Errorf("reading pull progress: %w", err)
	}
	return nil
}

// registryAuthFor returns the configured pull credentials for ref's registry
// host, if any (resolved with Docker's normalization rules).
//
// @arg ref The image reference whose registry host is matched against the configured credentials.
// @return registry.AuthConfig The matching credentials (zero value when none match).
// @return bool Whether a matching entry was found.
//
// @testcase TestProvisionPullsWithRegistryAuth matches an image to its registry credentials.
func (p *Provisioner) registryAuthFor(ref string) (registry.AuthConfig, bool) {
	if len(p.registryAuths) == 0 {
		return registry.AuthConfig{}, false
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return registry.AuthConfig{}, false
	}
	auth, ok := p.registryAuths[reference.Domain(named)]
	return auth, ok
}

// parseGPUs turns a `docker run --gpus`-style spec into Docker device requests.
//
// @arg spec The GPU spec: "", "all", a positive count, or a device list like "device=0,1".
// @return []container.DeviceRequest The device requests (nil when spec is empty).
// @error error if the spec is malformed.
//
// @testcase TestSetBoxGPUsParsesSpec drives this through SetBoxGPUs.
func parseGPUs(spec string) ([]container.DeviceRequest, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	req := container.DeviceRequest{Capabilities: [][]string{{"gpu"}}}
	switch {
	case spec == "all":
		req.Count = -1
	case strings.HasPrefix(spec, "device="):
		req.DeviceIDs = splitGPUList(strings.TrimPrefix(spec, "device="))
	case !strings.Contains(spec, ","):
		if n, err := strconv.Atoi(spec); err == nil {
			if n <= 0 {
				return nil, fmt.Errorf("invalid --box-gpus count %q: must be a positive integer or \"all\"", spec)
			}
			req.Count = n
		} else {
			req.DeviceIDs = []string{spec}
		}
	default:
		req.DeviceIDs = splitGPUList(spec)
	}
	if req.Count == 0 && len(req.DeviceIDs) == 0 {
		return nil, fmt.Errorf("invalid --box-gpus %q: no GPUs selected", spec)
	}
	return []container.DeviceRequest{req}, nil
}

// splitGPUList splits a comma-separated GPU id list, trimming spaces and dropping
// empty entries.
//
// @arg list A comma-separated list of GPU ids/indices.
// @return []string The non-empty, space-trimmed entries.
//
// @testcase TestSetBoxGPUsParsesSpec exercises device-list parsing through SetBoxGPUs.
func splitGPUList(list string) []string {
	var ids []string
	for _, part := range strings.Split(list, ",") {
		if part = strings.TrimSpace(part); part != "" {
			ids = append(ids, part)
		}
	}
	return ids
}

// IsNotFound reports whether err indicates that no managed box matched the
// identifier. It recognizes both the typed ErrBoxNotFound (local calls) and an
// error that round-tripped over the cluster transport as a bare string.
//
// @arg err The error to classify; nil is not a not-found error.
// @return bool Whether err means the box does not exist.
//
// @testcase TestIsNotFound recognizes the sentinel, a wrapped error, a wire string, and rejects others.
func IsNotFound(err error) bool {
	return err != nil && (errors.Is(err, sandbox.ErrBoxNotFound) || strings.Contains(err.Error(), sandbox.ErrBoxNotFound.Error()))
}

// dockerInstance is a handle to one managed Docker box.
type dockerInstance struct {
	prov *Provisioner
	box  sandbox.Box
	// containerID is the real Docker container id, kept private to the spoke and
	// used for every Docker API call (stop/remove/rename/network). It never
	// crosses to the hub; the hub-facing handle is box.InstanceID (the generation
	// token).
	containerID string
	socketToken string
}

// socketPath returns the host path of the box's control socket.
//
// @return string The host filesystem path of the box's control socket.
//
// @testcase TestProvisionCreatesGuestBox dials the path socketPath returns.
func (i *dockerInstance) socketPath() string {
	return filepath.Join(i.prov.socketDir, i.socketToken, socketFileName)
}

// Meta returns the box's view as captured when the instance was resolved.
//
// @return sandbox.Box The box's ID, name, phase, and other fields.
//
// @testcase TestListMapsManagedContainers reads box metadata via Meta.
func (i *dockerInstance) Meta() sandbox.Box { return i.box }

// Control opens a new connection to the box's guest over its bind-mounted Unix
// socket.
//
// @arg ctx Context for the dial.
// @return net.Conn A control connection to the box's guest.
// @error error if the socket cannot be dialled.
//
// @testcase TestProvisionCreatesGuestBox connects to a box's guest via Control.
func (i *dockerInstance) Control(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", i.socketPath())
}

// MarkReady renames the container from the pending prefix to the ready prefix so
// the orphan reaper spares it once it has authenticated.
//
// @arg ctx Context for the rename.
// @error error if the container cannot be renamed.
//
// @testcase TestMarkReadyRenamesContainer renames the box to the ready prefix.
func (i *dockerInstance) MarkReady(ctx context.Context) error {
	if err := i.prov.cli.ContainerRename(ctx, i.containerID, readyPrefix+i.box.InstanceID); err != nil {
		return fmt.Errorf("marking box %s ready: %w", i.box.BoxID, err)
	}
	return nil
}

// pausedMarkerPath returns the host path of the box's paused marker file.
//
// @return string The host filesystem path of the box's paused marker.
//
// @testcase TestPauseResumeReportsPausedState pauses a box and finds the marker path present.
func (i *dockerInstance) pausedMarkerPath() string {
	return filepath.Join(i.prov.socketDir, i.socketToken, pausedMarkerFile)
}

// Pause stops the box's container to free CPU/RAM while keeping its writable layer
// (auth, workspace) and identity, so Resume can bring it back. It writes the paused
// marker first (so a stopped container always reports as paused, never dead) and
// removes it again if the stop fails, leaving a clean running box.
//
// @arg ctx Context for the stop.
// @error error wrapping sandbox.ErrBoxNotFound if the box is already gone, or if it cannot be marked or stopped.
//
// @testcase TestPauseStopsAndMarksBox stops the container and writes the paused marker.
// @testcase TestPauseAlreadyGone reports ErrBoxNotFound when the container is missing.
func (i *dockerInstance) Pause(ctx context.Context) error {
	if err := os.WriteFile(i.pausedMarkerPath(), nil, 0o600); err != nil {
		return fmt.Errorf("marking box %s paused: %w", i.box.BoxID, err)
	}
	timeout := int(stopTimeout.Seconds())
	if err := i.prov.cli.ContainerStop(ctx, i.containerID, container.StopOptions{Timeout: &timeout}); err != nil {
		_ = os.Remove(i.pausedMarkerPath())
		if errdefs.IsNotFound(err) {
			return fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, i.box.BoxID)
		}
		return fmt.Errorf("pausing box %s: %w", i.box.BoxID, err)
	}
	return nil
}

// Resume restarts a paused box's container and waits for the guest to recreate its
// control socket, then clears the paused marker. It restores only the compute; the
// Manager re-drives the guest handshake to relaunch claude. The marker is cleared
// last so a resume that fails before the guest is back stays reported as paused.
//
// @arg ctx Context for the start and the socket wait.
// @error error wrapping sandbox.ErrBoxNotFound if the box is already gone, or if it cannot be started or its guest socket does not reappear.
//
// @testcase TestResumeStartsAndUnmarksBox starts the container, waits for the socket, and clears the marker.
// @testcase TestResumeAlreadyGone reports ErrBoxNotFound when the container is missing.
func (i *dockerInstance) Resume(ctx context.Context) error {
	if err := i.prov.cli.ContainerStart(ctx, i.containerID, container.StartOptions{}); err != nil {
		if errdefs.IsNotFound(err) {
			return fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, i.box.BoxID)
		}
		return fmt.Errorf("resuming box %s: %w", i.box.BoxID, err)
	}
	if err := waitForSocket(ctx, i.socketPath(), socketWait); err != nil {
		return fmt.Errorf("waiting for box %s guest after resume: %w", i.box.BoxID, err)
	}
	if err := os.Remove(i.pausedMarkerPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clearing paused marker for box %s: %w", i.box.BoxID, err)
	}
	return nil
}

// Destroy gracefully stops and removes the box, tears down its network, and
// removes its host socket directory. Destroying an already-gone box returns a
// wrapped sandbox.ErrBoxNotFound.
//
// @arg ctx Context for the stop and remove.
// @error error wrapping sandbox.ErrBoxNotFound if the box is already gone, or if it cannot be stopped/removed.
//
// @testcase TestDestroyRemovesNetworkAndSocket stops the box, removes its network, and deletes its socket dir.
// @testcase TestDestroyAlreadyGone reports ErrBoxNotFound when the container is missing.
func (i *dockerInstance) Destroy(ctx context.Context) error {
	id := i.containerID
	timeout := int(stopTimeout.Seconds())
	if err := i.prov.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout}); err != nil {
		if errdefs.IsNotFound(err) {
			return fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, i.box.BoxID)
		}
		return fmt.Errorf("stopping box %s: %w", i.box.BoxID, err)
	}
	if err := i.prov.cli.ContainerRemove(ctx, id, container.RemoveOptions{RemoveVolumes: true}); err != nil {
		if errdefs.IsNotFound(err) {
			return fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, i.box.BoxID)
		}
		return fmt.Errorf("removing box %s: %w", i.box.BoxID, err)
	}
	i.prov.removeBoxNetwork(ctx, id)
	i.prov.stopBoxAPI(i.box.InstanceID)
	if i.socketToken != "" {
		_ = os.RemoveAll(filepath.Join(i.prov.socketDir, i.socketToken))
	}
	return nil
}
