// Package docker wraps the Docker Engine API to manage the lifecycle of
// "llmboxes": containers that run Claude Code in remote-control mode, each
// authenticated by an end user via OAuth.
//
// Lifecycle of a box:
//
//  1. Create starts a container whose entrypoint runs `claude auth login`.
//     The container has a TTY; the login process parks at a "paste code" prompt
//     after printing an OAuth authorize URL. Create captures that URL and
//     returns it. The box is named "llmbox-pending-<id>".
//  2. SubmitCode writes the OAuth code (obtained out-of-band by the user) to the
//     login process's stdin. On success the CLI stores credentials inside the
//     container and the entrypoint execs `claude remote-control`, which prints a
//     session URL. The box is renamed "llmbox-<id>" to mark it authenticated.
//  3. ReapOrphans destroys boxes that are still "pending" past a TTL — e.g. a
//     user who never finished authenticating, or boxes orphaned by a restart.
//
// The OAuth code never passes through the MCP layer: it travels from the user's
// browser to this binary's web server to the container's stdin only.
//
// Safety: every container created here carries ManagedLabel; list/destroy/reap
// operations are scoped to that label so unrelated host containers are untouched.
package docker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	// ManagedLabel marks every container created by this server.
	ManagedLabel = "com.llmbox.managed"

	// BoxIDLabel and DescriptionLabel persist the caller-assigned box ID and
	// description so List can report them straight from a container list
	// (ContainerList summaries carry labels but neither the box ID nor the
	// rest of the container config). The box ID is also set as the container
	// hostname, but the label is the authoritative copy List reads.
	BoxIDLabel       = "com.llmbox.box-id"
	DescriptionLabel = "com.llmbox.description"

	// DefaultImage is launched when the caller does not specify one. Claude is
	// always injected at create time, so this is a plain glibc base rather than a
	// Claude-specific image: it only needs /bin/sh, util-linux (for `script`),
	// and the CA-certificate bundle the box's HTTPS calls rely on. Any glibc
	// image with those works as a substitute; this one is built by Dockerfile.box
	// (debian:bookworm-slim + ca-certificates), since plain debian:bookworm-slim
	// omits ca-certificates and breaks TLS from inside the box.
	DefaultImage = "ghcr.io/clems4ever/llmbox-box:latest"

	// DefaultClaudeBin is where the Dockerfile bakes the standalone Claude binary;
	// it is the fallback source the binary is injected from when no path is set.
	DefaultClaudeBin = "/opt/llmbox/claude"

	// claudeBinTarget is where the injected Claude binary lands inside a box; it
	// is on the default PATH so the entrypoint can invoke `claude` directly.
	claudeBinTarget = "/usr/local/bin/claude"

	// boxHome and boxWorkdir are the home and working directory forced on a box
	// in injection mode, so the injected config and the trusted-project key are
	// deterministic regardless of the base image's own user/WORKDIR. The box runs
	// as root (see Create) so both stay writable.
	boxHome    = "/root"
	boxWorkdir = "/workspace"

	// pendingPrefix / readyPrefix encode a box's auth phase in its name, so the
	// phase survives a restart of this server (Docker persists names, but not
	// our in-memory state). Reaping targets pendingPrefix only.
	pendingPrefix = "llmbox-pending-"
	readyPrefix   = "llmbox-"

	// Default remote-control flags; --spawn must be explicit for headless start.
	defaultRemoteArgs = "--spawn same-dir"

	// defaultBridgeNetwork is Docker's default bridge network. A box is created
	// on it, then detached (once on its own per-box network) so boxes don't all
	// share it and become mutually reachable.
	defaultBridgeNetwork = "bridge"

	// ttyWidth is wide enough that the authorize URL prints on a single line
	// instead of being wrapped by the TTY (which would break URL extraction).
	ttyWidth  = 1000
	ttyHeight = 50

	// stopTimeout is how long Docker waits after sending SIGTERM before it
	// escalates to SIGKILL when stopping a box, giving Claude a chance to shut
	// down cleanly (e.g. deregister its remote-control session).
	stopTimeout = 10 * time.Second

	// defaultLogTail is how many trailing log lines Logs returns when the caller
	// does not ask for a specific count, keeping the output bounded.
	defaultLogTail = 200

	// maxExecOutput caps each of an exec's stdout and stderr so a chatty command
	// can't return an unbounded payload; output past it is dropped with a marker.
	maxExecOutput = 64 << 10
)

// dockerAPI is the subset of the Docker client used by Manager. It exists so
// the Docker layer can be faked in tests; *client.Client satisfies it.
type dockerAPI interface {
	ContainerList(ctx context.Context, opts container.ListOptions) ([]container.Summary, error)
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, opts container.StartOptions) error
	ImagePull(ctx context.Context, refStr string, opts image.PullOptions) (io.ReadCloser, error)
	ContainerStop(ctx context.Context, containerID string, opts container.StopOptions) error
	ContainerRemove(ctx context.Context, containerID string, opts container.RemoveOptions) error
	ContainerLogs(ctx context.Context, containerID string, opts container.LogsOptions) (io.ReadCloser, error)
	ContainerExecCreate(ctx context.Context, containerID string, opts container.ExecOptions) (container.ExecCreateResponse, error)
	ContainerExecAttach(ctx context.Context, execID string, opts container.ExecAttachOptions) (types.HijackedResponse, error)
	ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error)
	ContainerRename(ctx context.Context, containerID, newName string) error
	ContainerResize(ctx context.Context, containerID string, opts container.ResizeOptions) error
	CopyToContainer(ctx context.Context, containerID, dstPath string, content io.Reader, opts container.CopyToContainerOptions) error
	ContainerAttach(ctx context.Context, containerID string, opts container.AttachOptions) (types.HijackedResponse, error)
	NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error)
	NetworkConnect(ctx context.Context, networkID, containerID string, config *network.EndpointSettings) error
	NetworkDisconnect(ctx context.Context, networkID, containerID string, force bool) error
	NetworkRemove(ctx context.Context, networkID string) error
	Close() error
}

// Manager talks to the Docker daemon.
type Manager struct {
	cli          dockerAPI
	defaultImage string
	remoteArgs   string
	// peers are container names (the resource servers) connected into every
	// box's own dedicated network so boxes can reach them. Each box gets its
	// own network and is attached to nothing else, so boxes are isolated from
	// one another while still reaching these shared peers.
	peers []string

	// claudeBinSrc is the path on THIS host (the MCP server) to the standalone
	// Claude native binary that is injected into each box at creation. Empty
	// disables injection, in which case the base image must already bundle Claude.
	claudeBinSrc string
	// claudeBinOnce/claudeBin/claudeBinErr lazily read and cache the binary bytes
	// the first time a box is created, so the file is read once per process.
	claudeBinOnce sync.Once
	claudeBin     []byte
	claudeBinErr  error

	// createMu serializes the box-ID-uniqueness check and container creation so
	// two concurrent creates can't both pass the check with the same box ID.
	createMu sync.Mutex

	// limits caps each box's resources and the total number of concurrent boxes,
	// bounding resource-exhaustion by a caller that reaches the (by-design
	// unauthenticated) create path. The zero value imposes no limits.
	limits BoxLimits

	// log records best-effort failures (cleanup, network teardown, etc.) that are
	// not propagated to the caller; nil falls back to slog.Default() via logger().
	log *slog.Logger
}

// logger returns the Manager's logger, or slog.Default() when none was set (e.g.
// a Manager built directly in a test).
//
// @return *slog.Logger The configured logger, or the slog default.
//
// @testcase TestListMapsPhaseFromName exercises a Manager whose logger defaults.
func (m *Manager) logger() *slog.Logger {
	if m.log != nil {
		return m.log
	}
	return slog.Default()
}

// Box is a view of a managed container returned to callers.
type Box struct {
	ContainerID string `json:"container_id" jsonschema:"the short Docker container ID"`
	Name        string `json:"name" jsonschema:"the container name"`
	BoxID       string `json:"box_id,omitempty" jsonschema:"the box ID the caller assigned, if any (also set as the container hostname)"`
	Description string `json:"description,omitempty" jsonschema:"the caller-supplied description label, if any"`
	Spoke       string `json:"spoke,omitempty" jsonschema:"the cluster spoke the box runs on; 'local' for the in-process spoke"`
	Image       string `json:"image" jsonschema:"the image the box runs"`
	State       string `json:"state" jsonschema:"the container state, e.g. running or exited"`
	Status      string `json:"status" jsonschema:"a human readable status string"`
	Phase       string `json:"phase" jsonschema:"auth phase: pending (awaiting login) or ready (authenticated)"`
	Created     int64  `json:"created" jsonschema:"creation time as a unix timestamp"`
}

// ExecResult is the captured outcome of a command run inside a box.
type ExecResult struct {
	Stdout   string `json:"stdout" jsonschema:"the command's standard output"`
	Stderr   string `json:"stderr" jsonschema:"the command's standard error"`
	ExitCode int    `json:"exit_code" jsonschema:"the command's exit code (0 means success)"`
}

// CreateOptions holds the caller-controlled inputs for a new box.
type CreateOptions struct {
	// Image is the container image to launch; empty means the Manager default.
	Image string
	// BoxID is the caller-assigned identifier for the box. When set, it is also
	// applied as the box's container hostname (what `hostname` reports inside it,
	// and the name shown in claude.ai/code), so it must be a valid hostname or
	// Docker rejects creation. It must be unique across managed boxes.
	BoxID string
	// Description is a free-form label shown by list/get to help the caller tell
	// boxes apart. It has no effect on the box itself.
	Description string
	// SpokeName selects which cluster spoke the box is created on (empty or
	// "local" means the in-process spoke). It is routing metadata used by the
	// server's cluster layer; the Docker manager itself ignores it.
	SpokeName string
	// Files are written into the box's filesystem after it is created but before
	// it starts, so they are present when the entrypoint runs. Used to inject
	// per-box secrets (e.g. a granular subject token) without baking them into
	// the image, an env var, or a label where `docker inspect` would expose them.
	Files []InjectFile
}

// InjectFile is one file to write into a new box. Path is absolute inside the
// container; Content is its bytes; Mode/UID/GID set its permissions and owner
// (UID/GID matter so a file landing in a non-root user's home stays readable by
// that user).
type InjectFile struct {
	Path    string
	Content []byte
	Mode    int64
	UID     int
	GID     int
}

// BoxLimits caps the resources a single box container may consume and the total
// number of concurrent boxes a Manager will run. It bounds resource-exhaustion
// (CPU/memory/PID fork-bombs, unbounded box counts) by a caller that can reach
// the by-design-unauthenticated create/exec path. A zero field means "no limit"
// for that dimension, so the zero BoxLimits preserves the original unbounded
// behaviour for a deployment that opts out.
type BoxLimits struct {
	// MemoryBytes is the hard memory limit per box, in bytes (0 = unlimited).
	MemoryBytes int64
	// NanoCPUs is the CPU quota per box in units of 1e-9 CPUs, i.e. 1_000_000_000
	// is one full CPU (0 = unlimited).
	NanoCPUs int64
	// PidsLimit caps the number of processes/threads in a box, blunting fork
	// bombs (0 = unlimited).
	PidsLimit int64
	// MaxBoxes caps how many managed boxes may exist at once; Create rejects a new
	// box once the count is reached (0 = unlimited).
	MaxBoxes int
}

// SetBoxLimits sets the per-box resource caps and the max concurrent-box count
// applied by Create. It is called once at startup after NewManager (kept off the
// constructor so existing callers and tests are unaffected); the zero BoxLimits
// leaves every dimension unlimited.
//
// @arg l The resource and count limits to enforce on subsequently created boxes.
//
// @testcase TestCreateAppliesBoxLimits sets limits and checks they reach the host config.
// @testcase TestCreateRejectsOverMaxBoxes rejects a create once MaxBoxes is reached.
func (m *Manager) SetBoxLimits(l BoxLimits) { m.limits = l }

// boxIDRe is the canonical box-id format: a single DNS hostname label (1-63
// chars of lowercase letters, digits, or hyphens, not starting or ending with a
// hyphen). The box ID is interpolated into the container entrypoint and applied
// as the container hostname, so it must carry no shell metacharacters; this is
// the authoritative definition the cluster admission policy also enforces.
var boxIDRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidBoxID reports whether id is a well-formed box ID (a single DNS hostname
// label). It is the single source of truth for box-id validation: Create calls
// it so the local box-creation path validates inputs exactly as the cluster
// admission policy does on the remote path, rather than relying on the Docker
// daemon's implicit hostname check to reject a malformed (and potentially
// shell-injecting) box ID.
//
// @arg id The candidate box ID.
// @return bool True when id is a valid 1-63 char lowercase hostname label.
//
// @testcase TestValidBoxID accepts well-formed ids and rejects malformed ones.
func ValidBoxID(id string) bool {
	return boxIDRe.MatchString(id)
}

// NewManager creates a Manager using Docker configuration from the environment.
// defaultImage, remoteArgs, and claudeBin fall back to sensible defaults when
// empty. claudeBin is the path to the standalone Claude binary that is always
// injected into each box at creation, which is what lets boxes run on any plain
// glibc image rather than a Claude-specific one.
//
// @arg defaultImage The image launched when a caller does not specify one; empty falls back to DefaultImage.
// @arg remoteArgs The remote-control flags; empty falls back to the default flags.
// @arg claudeBin Path on this host to the standalone Claude binary injected into each box; empty falls back to DefaultClaudeBin.
// @arg peers Container names (resource servers) connected into every box's own network; empty isolates boxes with no shared peers.
// @return *Manager A Manager wired to a Docker client built from the environment.
// @error error if the Docker client cannot be created.
//
// @testcase TestListMapsPhaseFromName covers Manager behaviour via a constructed Manager.
func NewManager(defaultImage, remoteArgs, claudeBin string, peers []string) (*Manager, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("creating docker client: %w", err)
	}
	if defaultImage == "" {
		defaultImage = DefaultImage
	}
	if remoteArgs == "" {
		remoteArgs = defaultRemoteArgs
	}
	if claudeBin == "" {
		claudeBin = DefaultClaudeBin
	}
	return &Manager{cli: cli, defaultImage: defaultImage, remoteArgs: remoteArgs, claudeBinSrc: claudeBin, peers: peers, log: slog.Default()}, nil
}

// loadClaudeBinary reads and caches the standalone Claude binary from
// claudeBinSrc, reading the file once per process. It is the bytes injected into
// each box at claudeBinTarget.
//
// @return []byte The Claude binary contents to inject.
// @error error if the binary cannot be read.
//
// @testcase TestCreateInjectsClaude injects the loaded binary into the box.
// @testcase TestCreateMissingClaudeBinary fails create when the binary is unreadable.
func (m *Manager) loadClaudeBinary() ([]byte, error) {
	m.claudeBinOnce.Do(func() {
		b, err := os.ReadFile(m.claudeBinSrc)
		if err != nil {
			m.claudeBinErr = fmt.Errorf("reading claude binary %q for injection: %w", m.claudeBinSrc, err)
			return
		}
		m.claudeBin = b
	})
	return m.claudeBin, m.claudeBinErr
}

// claudeConfigSeed returns the bytes of the ~/.claude.json seed injected into
// every box. It pre-answers the two interactive gates a fresh box hits —
// projects[boxWorkdir].hasTrustDialogAccepted (else remote-control aborts
// "Workspace not trusted") and remoteDialogSeen (else it blocks on "Enable
// Remote Control?"). Injecting it as a file means boxes need no Node runtime to
// set these; `claude auth login` merges its account fields into the file at
// start without clobbering these keys.
//
// @return []byte The JSON contents of the ~/.claude.json seed.
//
// @testcase TestCreateInjectsClaude checks the injected seed enables trust and remote control.
func claudeConfigSeed() []byte {
	cfg := map[string]any{
		"projects": map[string]any{
			boxWorkdir: map[string]any{"hasTrustDialogAccepted": true},
		},
		"remoteDialogSeen": true,
	}
	b, _ := json.Marshal(cfg)
	return b
}

// Close releases the underlying Docker client.
//
// @error error if the underlying Docker client fails to close.
//
// @testcase TestListMapsPhaseFromName uses a Manager whose lifecycle includes Close.
func (m *Manager) Close() error { return m.cli.Close() }

// managedFilter builds the Docker list filter that scopes operations to
// containers created by this server.
//
// @return filters.Args A filter matching only containers carrying ManagedLabel.
//
// @testcase TestListMapsPhaseFromName exercises managedFilter via List.
func managedFilter() filters.Args {
	return filters.NewArgs(filters.Arg("label", ManagedLabel+"=true"))
}

// phaseOf reports a box's auth phase from its container name.
//
// @arg name The container name to inspect.
// @return string "pending" if the name has the pending prefix, else "ready".
//
// @testcase TestListMapsPhaseFromName checks the phase derived from names.
func phaseOf(name string) string {
	if strings.HasPrefix(name, pendingPrefix) {
		return "pending"
	}
	return "ready"
}

// List returns all boxes created by this server, running or not.
//
// @arg ctx Context for the Docker list request.
// @return []Box One Box per managed container, with phase, box ID, and description filled in.
// @error error if listing containers from Docker fails.
//
// @testcase TestListMapsPhaseFromName checks phase, container ID, box ID, and description mapping.
func (m *Manager) List(ctx context.Context) ([]Box, error) {
	cs, err := m.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: managedFilter()})
	if err != nil {
		return nil, fmt.Errorf("listing boxes: %w", err)
	}
	out := make([]Box, 0, len(cs))
	for _, c := range cs {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, Box{
			ContainerID: c.ID[:12],
			Name:        name,
			BoxID:       c.Labels[BoxIDLabel],
			Description: c.Labels[DescriptionLabel],
			Image:       c.Image,
			State:       c.State,
			Status:      c.Status,
			Phase:       phaseOf(name),
			Created:     c.Created,
		})
	}
	return out, nil
}

// authorizeURLRe matches the OAuth authorize URL the login TUI prints. It
// requires the trailing PKCE/state params so a partially-rendered (wrapped) URL
// is not accepted.
var authorizeURLRe = regexp.MustCompile(`https://claude\.com/cai/oauth/authorize\?\S*code_challenge=\S*state=[A-Za-z0-9_\-]+`)

// Create creates and starts a box, captures the OAuth authorize URL its
// login process prints, and returns the container ID plus that URL. The box is
// left running, parked at the "paste code" prompt, ready for SubmitCode.
// opts.BoxID is applied as the container hostname, and opts.BoxID/opts.Description
// are persisted as labels so List can report them. A non-empty opts.BoxID must be
// a valid hostname label (see ValidBoxID) and unique across managed boxes; the
// create is rejected otherwise. If the image is not present locally, it is pulled
// and the create is retried once. Any opts.Files are written into the box after
// creation but before it starts.
//
// The standalone Claude binary and a ~/.claude.json seed are always injected,
// the box is forced to run as root with HOME=boxHome and WorkingDir=boxWorkdir,
// and a node-free entrypoint is used — so the box runs on any plain glibc image
// without Claude (or Node) baked in. The configured BoxLimits cap the box's
// memory/CPU/PIDs and the total box count, and the box runs with
// no-new-privileges. When opts.BoxID is set (and the remote args don't already
// specify --name), the pre-created first session is named "<box-id>-default" so
// it is identifiable in claude.ai/code.
//
// @arg ctx Context for the Docker create/start/attach calls.
// @arg opts The caller-controlled image, box ID, description, and files for the box.
// @return id The full container ID of the created box.
// @return authorizeURL The OAuth authorize URL captured from the box's login output.
// @error error if opts.BoxID is malformed or already in use, the max-box ceiling is reached, the claude binary cannot be read, the image cannot be pulled, or the box cannot be created, files injected, started, or its authorize URL captured.
//
// @testcase TestCreateCapturesURL captures the authorize URL and sets box-id/description labels.
// @testcase TestCreateCleansUpOnStartFailure removes the container when start fails.
// @testcase TestCreateRejectsDuplicateBoxID rejects a box ID already in use.
// @testcase TestCreateRejectsBadBoxID rejects a malformed box ID before creating a container.
// @testcase TestCreateAppliesBoxLimits applies the configured resource caps and no-new-privileges.
// @testcase TestCreateRejectsOverMaxBoxes rejects a create once the box ceiling is reached.
// @testcase TestCreatePullsMissingImage pulls the image then retries when it is absent.
// @testcase TestCreateInjectsFiles copies injected files into the box before start.
// @testcase TestCreateInjectsClaude injects the binary and seed and forces root/HOME/WorkingDir.
// @testcase TestCreateMissingClaudeBinary fails when the claude binary is unreadable.
func (m *Manager) Create(ctx context.Context, opts CreateOptions) (id, authorizeURL string, err error) {
	// Validate the box ID at the boundary, on EVERY path (local and remote-spoke):
	// it is interpolated into the /bin/sh -c entrypoint below and used as the
	// container hostname, so a malformed value must be rejected here rather than
	// left for the Docker daemon's implicit hostname check to (maybe) catch. An
	// empty box ID is allowed (Docker auto-names the host and no --name is added).
	if opts.BoxID != "" && !ValidBoxID(opts.BoxID) {
		return "", "", fmt.Errorf("invalid box id %q: must be 1-63 chars of lowercase letters, digits, or hyphens (not starting or ending with a hyphen)", opts.BoxID)
	}

	image := opts.Image
	if image == "" {
		image = m.defaultImage
	}

	labels := map[string]string{ManagedLabel: "true"}
	if opts.BoxID != "" {
		labels[BoxIDLabel] = opts.BoxID
	}
	if opts.Description != "" {
		labels[DescriptionLabel] = opts.Description
	}

	// Entrypoint: (1) authenticate only if needed, then (2) hand off to
	// remote-control. The workspace-trust and "Enable Remote Control?" prompts a
	// fresh box would otherwise block on are pre-answered by the ~/.claude.json
	// seed injected below, so no Node runtime is required. `script` allocates a
	// fresh PTY for remote-control's UI, which it needs to reach "Ready";
	// util-linux (and so `script`) is present in the glibc base images this
	// targets.
	//
	// This entrypoint re-runs on every container start, including `docker restart`.
	// `claude auth login` is therefore guarded: the OAuth flow only runs when the
	// box has no credentials yet. A restart finds the token already on disk at
	// ~/.claude/.credentials.json (preserved in the container's writable layer)
	// and skips straight to remote-control, so the user is not asked to
	// authenticate again. The guard also honours CLAUDE_CODE_OAUTH_TOKEN, the
	// token-via-env alternative.
	// Name the pre-created first session "<box-id>-default" so it is
	// identifiable in claude.ai/code (remote-control's --name sets the session
	// name; without it the session gets an auto-generated, random-looking name).
	// Skip when the caller already set --name via the configured remote args. The
	// box ID is Docker-validated (it doubles as the hostname), so it carries no
	// shell metacharacters to worry about inside the quoted command.
	remoteArgs := m.remoteArgs
	if opts.BoxID != "" && !strings.Contains(remoteArgs, "--name") {
		remoteArgs = strings.TrimSpace(remoteArgs + " --name " + opts.BoxID + "-default")
	}
	entry := fmt.Sprintf(
		`{ [ -n "$CLAUDE_CODE_OAUTH_TOKEN" ] || [ -s "$HOME/.claude/.credentials.json" ] || claude auth login --claudeai; } && exec script -qfc "claude remote-control %s" /dev/null`,
		remoteArgs,
	)

	// Always inject the Claude binary and the config seed, so an arbitrary base
	// image needs neither baked in. Read the binary up front so a bad path fails
	// the create cleanly before any container is made.
	bin, berr := m.loadClaudeBinary()
	if berr != nil {
		return "", "", berr
	}
	opts.Files = append(opts.Files,
		InjectFile{Path: claudeBinTarget, Content: bin, Mode: 0o755, UID: 0, GID: 0},
		InjectFile{Path: path.Join(boxHome, ".claude.json"), Content: claudeConfigSeed(), Mode: 0o644, UID: 0, GID: 0},
	)

	// Reserve the box ID atomically: under one lock, reject the create if an
	// existing box already uses it (or the max-box ceiling is reached), then
	// create the container (which carries the box-id label, so a concurrent create
	// will see it). The slow login / URL capture below runs unlocked.
	m.createMu.Lock()
	if opts.BoxID != "" || m.limits.MaxBoxes > 0 {
		boxes, lerr := m.List(ctx)
		if lerr != nil {
			m.createMu.Unlock()
			return "", "", fmt.Errorf("checking box ID uniqueness: %w", lerr)
		}
		// Cap the number of concurrent boxes so the unauthenticated create path
		// cannot be used to spawn containers without bound.
		if m.limits.MaxBoxes > 0 && len(boxes) >= m.limits.MaxBoxes {
			m.createMu.Unlock()
			return "", "", fmt.Errorf("box limit reached (%d boxes already running); destroy a box before creating another", m.limits.MaxBoxes)
		}
		for _, b := range boxes {
			if opts.BoxID != "" && strings.EqualFold(b.BoxID, opts.BoxID) {
				m.createMu.Unlock()
				return "", "", fmt.Errorf("box ID %q is already used by container %s; choose a different box ID", opts.BoxID, b.ContainerID)
			}
		}
	}

	cfg := &container.Config{
		Image:      image,
		Hostname:   opts.BoxID,
		Entrypoint: []string{"/bin/sh", "-c", entry},
		Tty:        true,
		OpenStdin:  true,
		Labels:     labels,
	}
	// Run as root with a fixed HOME/WorkingDir so the injected binary, the
	// ~/.claude.json seed, and the credentials Claude writes all land in known,
	// writable paths regardless of the base image's own user/WORKDIR.
	cfg.User = "0:0"
	cfg.WorkingDir = boxWorkdir
	cfg.Env = append(cfg.Env, "HOME="+boxHome)
	hostCfg := &container.HostConfig{
		// Start the PTY wide so the authorize URL prints unwrapped.
		ConsoleSize:   [2]uint{ttyHeight, ttyWidth},
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		// The box runs as root (see cfg.User above) so injected files land in
		// known writable paths; no-new-privileges keeps that root from escalating
		// further via setuid binaries inside the image, shrinking the blast radius
		// of a compromised box.
		SecurityOpt: []string{"no-new-privileges"},
	}
	// Apply the configured resource caps (each defaults to 0 = unlimited). These
	// bound a single box's CPU, memory, and PID usage so a fork/memory bomb in one
	// box (reachable via the unauthenticated exec path) cannot exhaust the host.
	if m.limits.MemoryBytes > 0 {
		hostCfg.Memory = m.limits.MemoryBytes
	}
	if m.limits.NanoCPUs > 0 {
		hostCfg.NanoCPUs = m.limits.NanoCPUs
	}
	if m.limits.PidsLimit > 0 {
		hostCfg.PidsLimit = &m.limits.PidsLimit
	}

	resp, err := m.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
	if err != nil && client.IsErrNotFound(err) {
		// The image isn't present locally; pull it and try once more.
		if perr := m.pullImage(ctx, image); perr != nil {
			m.createMu.Unlock()
			return "", "", fmt.Errorf("pulling image %q: %w", image, perr)
		}
		resp, err = m.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
	}
	if err != nil {
		m.createMu.Unlock()
		return "", "", fmt.Errorf("creating box from image %q: %w", image, err)
	}
	id = resp.ID
	m.createMu.Unlock()

	// From here on, clean up the container (and its network) on any failure.
	cleanup := func() {
		if err := m.cli.ContainerRemove(context.Background(), id, container.RemoveOptions{Force: true}); err != nil {
			m.logger().Warn("failed to remove box during cleanup", "container", id, "err", err)
		}
		m.removeBoxNetwork(context.Background(), id)
	}

	// Give the box its own network and wire the resource-server peers into it,
	// so it can reach them by name while staying isolated from other boxes.
	if err := m.setupBoxNetwork(ctx, id); err != nil {
		cleanup()
		return "", "", err
	}

	if err := m.cli.ContainerRename(ctx, id, pendingPrefix+id[:12]); err != nil {
		cleanup()
		return "", "", fmt.Errorf("naming box: %w", err)
	}
	// Inject per-box files before start so they exist when the entrypoint runs.
	if len(opts.Files) > 0 {
		if err := m.injectFiles(ctx, id, opts.Files); err != nil {
			cleanup()
			return "", "", err
		}
	}
	if err := m.cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		cleanup()
		return "", "", fmt.Errorf("starting box: %w", err)
	}
	// Belt-and-suspenders: ensure a wide TTY even if ConsoleSize was ignored.
	// Cosmetic only (keeps the authorize URL unwrapped), so a failure is logged
	// at debug level rather than failing the create.
	if err := m.cli.ContainerResize(ctx, id, container.ResizeOptions{Height: ttyHeight, Width: ttyWidth}); err != nil {
		m.logger().Debug("failed to resize box TTY", "container", id, "err", err)
	}

	url, err := m.readAuthorizeURL(ctx, id)
	if err != nil {
		cleanup()
		return "", "", err
	}
	return id, url, nil
}

// injectFiles writes files into a created (not yet started) container by
// streaming a tar archive to the Docker copy API. Each file's parent directories
// are created in the archive with the file's UID/GID, so a secret landing in a
// non-root user's home is owned by that user.
//
// @arg ctx Context for the copy request.
// @arg id The target container ID.
// @arg files The files to write into the container.
// @error error if the archive cannot be built or the copy fails.
//
// @testcase TestCreateInjectsFiles copies injected files into the box before start.
func (m *Manager) injectFiles(ctx context.Context, id string, files []InjectFile) error {
	archive, err := tarFiles(files)
	if err != nil {
		return fmt.Errorf("building file archive for box: %w", err)
	}
	// Paths in the archive are absolute (leading "/" stripped by tarFiles), so
	// the copy destination is the container root.
	if err := m.cli.CopyToContainer(ctx, id, "/", archive, container.CopyToContainerOptions{}); err != nil {
		return fmt.Errorf("injecting files into box: %w", err)
	}
	return nil
}

// tarFiles builds an in-memory tar archive containing files plus a directory
// entry for each file's parent, all owned by the file's UID/GID. Absolute paths
// are made archive-relative (a tar stream extracted at "/" must hold relative
// names) and a default mode of 0600 is used when Mode is zero.
//
// @arg files The files to pack.
// @return io.Reader A reader over the built tar archive.
// @error error if writing an entry to the archive fails.
//
// @testcase TestTarFilesCreatesParentDirs packs files with owned parent directories.
func tarFiles(files []InjectFile) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	seenDirs := map[string]bool{}
	for _, f := range files {
		clean := strings.TrimPrefix(path.Clean(f.Path), "/")
		// Emit a directory entry for each ancestor, owned by the file's UID/GID
		// so the secret stays readable by the box's user.
		dir := path.Dir(clean)
		if dir != "." && dir != "/" && !seenDirs[dir] {
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir,
				Name:     dir + "/",
				Mode:     0o700,
				Uid:      f.UID,
				Gid:      f.GID,
			}); err != nil {
				return nil, err
			}
			seenDirs[dir] = true
		}
		mode := f.Mode
		if mode == 0 {
			mode = 0o600
		}
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     clean,
			Mode:     mode,
			Uid:      f.UID,
			Gid:      f.GID,
			Size:     int64(len(f.Content)),
		}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.Content); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return &buf, nil
}

// pullImage pulls ref from its registry. It drains the progress stream, since
// the pull is only complete once the response body has been fully read.
//
// @arg ctx Context for the pull request.
// @arg ref The image reference to pull.
// @error error if the pull cannot start or its progress stream fails.
//
// @testcase TestCreatePullsMissingImage pulls then retries when the image is absent.
// @testcase TestCreatePullFailure surfaces an error when the pull fails.
func (m *Manager) pullImage(ctx context.Context, ref string) error {
	rc, err := m.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return fmt.Errorf("reading pull progress: %w", err)
	}
	return nil
}

// readAuthorizeURL attaches to a box and reads its output until the OAuth
// authorize URL appears (or the timeout elapses).
//
// @arg ctx Context for the Docker attach call.
// @arg id The container ID to attach to.
// @return string The OAuth authorize URL read from the box's output.
// @error error if attaching fails or the URL does not appear before the timeout.
//
// @testcase TestCreateCapturesURL drives readAuthorizeURL via Create.
func (m *Manager) readAuthorizeURL(ctx context.Context, id string) (string, error) {
	hj, err := m.cli.ContainerAttach(ctx, id, container.AttachOptions{
		Stream: true, Stdout: true, Stderr: true,
	})
	if err != nil {
		return "", fmt.Errorf("attaching to box: %w", err)
	}
	defer hj.Close()

	url, tail, err := scanFor(hj.Reader, authorizeURLRe, 30*time.Second, func() { hj.Close() })
	if err != nil {
		if tail != "" {
			return "", fmt.Errorf("waiting for authorize URL; box said: %s", tail)
		}
		return "", fmt.Errorf("waiting for authorize URL: %w", err)
	}
	return url, nil
}

// sessionURLRe matches the remote-control session URL printed after login.
var sessionURLRe = regexp.MustCompile(`https://claude\.(?:ai|com)/[A-Za-z0-9/_?=&.\-]+`)

// SubmitCode writes the OAuth code to a pending box's login prompt, waits for
// the login to complete and remote-control to print a session URL, then renames
// the box to mark it authenticated. It returns the session URL exactly as the
// box printed it (and any tail of output captured, for diagnostics).
//
// @arg ctx Context for the Docker attach call.
// @arg idOrName The ID or name identifying the pending box.
// @arg code The OAuth code to write to the box's login prompt.
// @return sessionURL The remote-control session URL printed once login completes.
// @error error if no managed box matches, attaching fails, the login does not complete, or the box cannot be renamed to ready.
//
// @testcase TestSubmitCodeReturnsSessionURL writes the code and returns the session URL.
// @testcase TestSubmitCodeAttachError fails when attaching to the box fails.
// @testcase TestSubmitCodeUnmanagedBox refuses a container that is not a managed box.
func (m *Manager) SubmitCode(ctx context.Context, idOrName, code string) (sessionURL string, err error) {
	// Resolve to a managed box first: like destroy/logs/exec, this must never act
	// on a container that is not one of ours, no matter what ID/name is passed in
	// (a spoke must not be coercible into attaching to an arbitrary host container).
	b, err := m.findManaged(ctx, idOrName)
	if err != nil {
		return "", err
	}
	id := b.ContainerID

	hj, err := m.cli.ContainerAttach(ctx, id, container.AttachOptions{
		Stream: true, Stdin: true, Stdout: true, Stderr: true,
	})
	if err != nil {
		return "", fmt.Errorf("attaching to box: %w", err)
	}
	defer hj.Close()

	if _, err := hj.Conn.Write([]byte(strings.TrimSpace(code) + "\r")); err != nil {
		return "", fmt.Errorf("submitting code: %w", err)
	}

	url, tail, err := scanFor(hj.Reader, sessionURLRe, 60*time.Second, func() { hj.Close() })
	if err != nil {
		if tail != "" {
			// Surface the box's real message (e.g. an invalid-code or trust error).
			return "", fmt.Errorf("login did not complete; box said: %s", tail)
		}
		return "", fmt.Errorf("login did not complete (the code may be invalid or expired): %w", err)
	}

	// Mark the box authenticated so the reaper leaves it alone.
	if rerr := m.cli.ContainerRename(ctx, id, readyPrefix+id[:12]); rerr != nil {
		// Non-fatal: the box is authenticated; reaping it later is the only risk.
		return url, fmt.Errorf("box authenticated but could not be renamed to ready: %w", rerr)
	}
	return url, nil
}

// boxNetworkName is the deterministic name of a box's dedicated network, derived
// from its container ID so it can be found again at destroy time.
//
// @arg id The box's container ID.
// @return string The per-box network name.
//
// @testcase TestSetupBoxNetworkConnectsPeers checks the box network is named after the box.
func boxNetworkName(id string) string { return "llmboxnet-" + id[:12] }

// setupBoxNetwork creates the box's own bridge network and connects the box and
// every configured resource-server peer to it, so the box reaches the peers by
// name while remaining isolated from other boxes (which live on other networks).
// The box is then detached from the default bridge it was created on, otherwise
// every box would share that bridge and could reach one another. (The box can't
// be created on no network and connected afterwards: Docker rejects mixing the
// "none" mode with any other network.)
//
// @arg ctx Context for the network create/connect calls.
// @arg id The box's container ID.
// @error error if the network cannot be created or the box cannot be connected to it.
//
// @testcase TestSetupBoxNetworkConnectsPeers creates the network, connects box and peers, and detaches the bridge.
func (m *Manager) setupBoxNetwork(ctx context.Context, id string) error {
	name := boxNetworkName(id)
	if _, err := m.cli.NetworkCreate(ctx, name, network.CreateOptions{
		Driver: "bridge",
		Labels: map[string]string{ManagedLabel: "true"},
	}); err != nil {
		return fmt.Errorf("creating box network: %w", err)
	}
	if err := m.cli.NetworkConnect(ctx, name, id, nil); err != nil {
		return fmt.Errorf("connecting box to its network: %w", err)
	}
	// Connect each resource-server peer. A peer that is missing or already
	// connected is non-fatal — the box still works for the others — but log it so
	// an unreachable peer is diagnosable.
	for _, peer := range m.peers {
		if err := m.cli.NetworkConnect(ctx, name, peer, nil); err != nil {
			m.logger().Warn("failed to connect peer to box network", "network", name, "peer", peer, "err", err)
		}
	}
	// Detach from the default bridge so the box lives only on its own network.
	if err := m.cli.NetworkDisconnect(ctx, defaultBridgeNetwork, id, true); err != nil {
		return fmt.Errorf("detaching box from the default bridge: %w", err)
	}
	return nil
}

// removeBoxNetwork tears down a box's dedicated network, first disconnecting the
// resource-server peers (whose live endpoints would otherwise block removal). It
// is best-effort: failures are logged but not returned, so destroy/reap always
// proceeds.
//
// @arg ctx Context for the disconnect/remove calls.
// @arg id The box's container ID.
//
// @testcase TestDestroyRemovesBoxNetwork checks the box network is removed on destroy.
func (m *Manager) removeBoxNetwork(ctx context.Context, id string) {
	name := boxNetworkName(id)
	for _, peer := range m.peers {
		if err := m.cli.NetworkDisconnect(ctx, name, peer, true); err != nil {
			m.logger().Warn("failed to disconnect peer from box network", "network", name, "peer", peer, "err", err)
		}
	}
	if err := m.cli.NetworkRemove(ctx, name); err != nil {
		m.logger().Warn("failed to remove box network", "network", name, "err", err)
	}
}

// Destroy gracefully stops and removes a managed box identified by ID or name.
// It asks the box to stop (SIGTERM to its main process, so Claude can shut down
// cleanly), waiting up to stopTimeout before Docker escalates to SIGKILL; the
// stop blocks until the box has terminated. Only then is the container removed.
//
// @arg ctx Context for the Docker stop and remove calls.
// @arg idOrName The ID or name identifying the box to remove.
// @error error if no managed box matches, or the container cannot be stopped or removed.
//
// @testcase TestDestroyStopsThenRemoves stops the box before removing it.
func (m *Manager) Destroy(ctx context.Context, idOrName string) error {
	b, err := m.findManaged(ctx, idOrName)
	if err != nil {
		return err
	}
	// Graceful stop: SIGTERM, then SIGKILL after the timeout. Returns once the
	// box has actually terminated.
	timeout := int(stopTimeout.Seconds())
	if err := m.cli.ContainerStop(ctx, b.ContainerID, container.StopOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("stopping box %s: %w", idOrName, err)
	}
	if err := m.cli.ContainerRemove(ctx, b.ContainerID, container.RemoveOptions{RemoveVolumes: true}); err != nil {
		return fmt.Errorf("removing box %s: %w", idOrName, err)
	}
	m.removeBoxNetwork(ctx, b.ContainerID)
	return nil
}

// Logs returns the recent console output of a managed box identified by ID or
// name. tail bounds how many trailing lines are returned; a non-positive tail
// falls back to defaultLogTail. Boxes run with a TTY, so the log stream is raw
// (not stdout/stderr multiplexed); the output is ANSI-stripped so the caller
// gets readable text rather than the TUI's escape sequences.
//
// @arg ctx Context for the Docker logs request.
// @arg idOrName The ID or name identifying the box to read logs from.
// @arg tail The maximum number of trailing log lines to return; non-positive uses defaultLogTail.
// @return string The box's recent console output, ANSI-stripped.
// @error error if no managed box matches, or the logs cannot be read.
//
// @testcase TestLogsReturnsTail reads a box's logs and strips ANSI from the output.
// @testcase TestLogsUnknownBox errors when no managed box matches.
func (m *Manager) Logs(ctx context.Context, idOrName string, tail int) (string, error) {
	b, err := m.findManaged(ctx, idOrName)
	if err != nil {
		return "", err
	}
	if tail <= 0 {
		tail = defaultLogTail
	}
	rc, err := m.cli.ContainerLogs(ctx, b.ContainerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       strconv.Itoa(tail),
	})
	if err != nil {
		return "", fmt.Errorf("reading logs for box %s: %w", idOrName, err)
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return "", fmt.Errorf("reading log stream for box %s: %w", idOrName, err)
	}
	return string(stripANSI(raw)), nil
}

// Exec runs cmd inside a managed box identified by ID or name and returns its
// captured stdout, stderr, and exit code. The exec runs without a TTY so the
// two streams stay separable (demultiplexed with stdcopy); each is capped at
// maxExecOutput. A non-zero exit code is reported in the result, not as an error
// — only a failure to run the command at all returns an error.
//
// @arg ctx Context for the Docker exec create/attach/inspect calls.
// @arg idOrName The ID or name identifying the box to run the command in.
// @arg cmd The command and its arguments to run inside the box.
// @return ExecResult The command's stdout, stderr, and exit code.
// @error error if no managed box matches, or the command cannot be created, started, or read.
//
// @testcase TestExecCapturesOutput runs a command and returns its stdout, stderr, and exit code.
// @testcase TestExecUnknownBox errors when no managed box matches.
func (m *Manager) Exec(ctx context.Context, idOrName string, cmd []string) (ExecResult, error) {
	b, err := m.findManaged(ctx, idOrName)
	if err != nil {
		return ExecResult{}, err
	}
	created, err := m.cli.ContainerExecCreate(ctx, b.ContainerID, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return ExecResult{}, fmt.Errorf("creating exec in box %s: %w", idOrName, err)
	}
	// ContainerExecAttach both starts the exec and streams its output.
	hj, err := m.cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return ExecResult{}, fmt.Errorf("starting exec in box %s: %w", idOrName, err)
	}
	defer hj.Close()

	var stdout, stderr bytes.Buffer
	// The exec has no TTY, so its stream is stdout/stderr multiplexed; demux it.
	if _, err := stdcopy.StdCopy(&stdout, &stderr, hj.Reader); err != nil {
		return ExecResult{}, fmt.Errorf("reading exec output from box %s: %w", idOrName, err)
	}

	// The exit code is only known once the command has finished (the stream above
	// has drained), so inspect after reading.
	insp, err := m.cli.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return ExecResult{}, fmt.Errorf("inspecting exec in box %s: %w", idOrName, err)
	}
	return ExecResult{
		Stdout:   capOutput(stdout.Bytes()),
		Stderr:   capOutput(stderr.Bytes()),
		ExitCode: insp.ExitCode,
	}, nil
}

// capOutput truncates b to maxExecOutput, appending a marker when it overflows,
// so a single exec can't return an unbounded payload.
//
// @arg b The captured output bytes.
// @return string The output, truncated with a marker when it exceeds maxExecOutput.
//
// @testcase TestExecCapsOutput truncates output past the cap and marks it.
func capOutput(b []byte) string {
	if len(b) <= maxExecOutput {
		return string(b)
	}
	return string(b[:maxExecOutput]) + "\n... [output truncated]"
}

// ReapOrphans destroys pending (never-authenticated) boxes older than ttl.
// Authenticated ("ready") boxes are never reaped. It returns the IDs reaped.
//
// @arg ctx Context for the underlying list and remove calls.
// @arg ttl The maximum age a pending box may reach before it is reaped.
// @return []string The short IDs of the boxes that were reaped.
// @error error if listing boxes fails.
//
// @testcase TestReapOrphans reaps only old pending boxes, sparing new and ready ones.
func (m *Manager) ReapOrphans(ctx context.Context, ttl time.Duration) ([]string, error) {
	boxes, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-ttl).Unix()
	var reaped []string
	for _, b := range boxes {
		if b.Phase == "pending" && b.Created < cutoff {
			if err := m.cli.ContainerRemove(ctx, b.ContainerID, container.RemoveOptions{Force: true, RemoveVolumes: true}); err == nil {
				m.removeBoxNetwork(ctx, b.ContainerID)
				reaped = append(reaped, b.ContainerID)
			}
		}
	}
	return reaped, nil
}

// findManaged resolves an ID or name (with or without the phase prefix) to the
// single managed box it matches.
//
// @arg ctx Context for the underlying list call.
// @arg idOrName The ID or name to resolve, with or without a phase prefix.
// @return *Box The matched box.
// @error error if listing fails or no managed box matches.
//
// @testcase TestDestroyStopsThenRemoves resolves a box by short ID via findManaged.
func (m *Manager) findManaged(ctx context.Context, idOrName string) (*Box, error) {
	bs, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range bs {
		b := bs[i]
		if b.Name == idOrName ||
			strings.HasPrefix(b.ContainerID, idOrName) ||
			strings.HasPrefix(idOrName, b.ContainerID) ||
			b.Name == pendingPrefix+idOrName ||
			b.Name == readyPrefix+idOrName {
			return &b, nil
		}
	}
	return nil, fmt.Errorf("no managed box matches %q", idOrName)
}

// scanFor reads from r until re matches the accumulated (ANSI-stripped) output
// or timeout elapses. onTimeout is called to unblock a pending Read (e.g. by
// closing the connection) when the deadline passes. On failure it returns the
// trailing output captured so callers can surface the box's actual message.
//
// @arg r The reader to consume the box's output from.
// @arg re The regexp whose first match terminates the scan.
// @arg timeout How long to wait for a match before giving up.
// @arg onTimeout Called when the deadline passes to unblock a pending Read.
// @return match The matched text, or empty if none was found.
// @return tail The trailing output captured when no match was found, for diagnostics.
// @error error if the stream ends or the timeout elapses before a match.
//
// @testcase TestCreateCapturesURL relies on scanFor to find the authorize URL.
func scanFor(r *bufio.Reader, re *regexp.Regexp, timeout time.Duration, onTimeout func()) (match, tail string, err error) {
	type result struct {
		match string
		tail  string
		err   error
	}
	done := make(chan result, 1)

	go func() {
		var acc []byte
		buf := make([]byte, 4096)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				acc = append(acc, buf[:n]...)
				clean := stripANSI(acc)
				if loc := re.Find(clean); loc != nil {
					done <- result{match: string(loc)}
					return
				}
				// Bound memory for long-lived streams.
				if len(acc) > 1<<20 {
					acc = acc[len(acc)-(1<<19):]
				}
			}
			if rerr != nil {
				done <- result{tail: lastLines(stripANSI(acc), 600), err: rerr}
				return
			}
		}
	}()

	select {
	case res := <-done:
		if res.match != "" {
			return res.match, "", nil
		}
		return "", res.tail, fmt.Errorf("stream ended before match: %v", res.err)
	case <-time.After(timeout):
		onTimeout()
		res := <-done // closing the conn unblocks the reader, yielding its tail
		return "", res.tail, fmt.Errorf("timed out after %s", timeout)
	}
}

// lastLines returns up to the last n bytes of b, trimmed, as a single-spaced
// string (newlines collapsed) suitable for an error message.
//
// @arg b The bytes to take the tail of.
// @arg n The maximum number of trailing bytes to keep.
// @return string The trimmed, single-spaced tail of b.
//
// @testcase TestSubmitCodeReturnsSessionURL exercises lastLines via scanFor diagnostics.
func lastLines(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return strings.TrimSpace(strings.Join(strings.Fields(string(b)), " "))
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07|\x1b[()][AB0]|[\r]`)

// stripANSI removes ANSI escape sequences and carriage returns so regexes can
// match text the TUI rendered.
//
// @arg b The raw TUI output bytes.
// @return []byte The input with ANSI escape sequences and carriage returns removed.
//
// @testcase TestStripANSI checks ANSI and carriage-return removal.
func stripANSI(b []byte) []byte {
	return ansiRe.ReplaceAll(b, nil)
}
