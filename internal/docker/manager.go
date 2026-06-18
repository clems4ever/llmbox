// Package docker wraps the Docker Engine API to manage the lifecycle of
// "llmboxes": containers that run Claude Code in remote-control mode, each
// authenticated by an end user via OAuth.
//
// Lifecycle of a box:
//
//  1. CreateLLMBox starts a container whose entrypoint runs `claude auth login`.
//     The container has a TTY; the login process parks at a "paste code" prompt
//     after printing an OAuth authorize URL. CreateLLMBox captures that URL and
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
	"fmt"
	"io"
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
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	// ManagedLabel marks every container created by this server.
	ManagedLabel = "com.llmbox.managed"

	// HostnameLabel and DescriptionLabel persist the caller-supplied hostname
	// and description so List can report them straight from a container list
	// (ContainerList summaries carry labels but neither the hostname nor the
	// rest of the container config).
	HostnameLabel    = "com.llmbox.hostname"
	DescriptionLabel = "com.llmbox.description"

	// DefaultImage is launched when the caller does not specify one.
	DefaultImage = "claude-remote"

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
)

// prepConfigCmd pre-answers the two interactive gates a fresh box hits after
// login, by writing ~/.claude.json before `claude remote-control` runs:
//
//   - projects[cwd].hasTrustDialogAccepted=true — else remote-control aborts
//     with "Workspace not trusted".
//   - remoteDialogSeen=true (top-level) — else remote-control blocks on an
//     "Enable Remote Control? (y/n)" prompt; setting it takes the enabled path
//     without asking, exactly as answering "y" would.
//
// Run with the Claude image's bundled node; uses only double quotes inside so it
// stays safe within the single-quoted `sh -c` entrypoint.
const prepConfigCmd = `node -e ` +
	`'const fs=require("fs"),os=require("os"),p=os.homedir()+"/.claude.json";` +
	`let c={};try{c=JSON.parse(fs.readFileSync(p,"utf8"))}catch(e){}` +
	`c.projects=c.projects||{};const d=process.cwd();` +
	`c.projects[d]=Object.assign({},c.projects[d],{hasTrustDialogAccepted:true});` +
	`c.remoteDialogSeen=true;` +
	`fs.writeFileSync(p,JSON.stringify(c))'`

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

	// createMu serializes the hostname-uniqueness check and container creation
	// so two concurrent creates can't both pass the check with the same hostname.
	createMu sync.Mutex
}

// Box is a view of a managed container returned to callers.
type Box struct {
	ID          string `json:"id" jsonschema:"the short box ID"`
	Name        string `json:"name" jsonschema:"the container name"`
	Hostname    string `json:"hostname,omitempty" jsonschema:"the hostname set on the box, if the caller supplied one"`
	Description string `json:"description,omitempty" jsonschema:"the caller-supplied description label, if any"`
	Image       string `json:"image" jsonschema:"the image the box runs"`
	State       string `json:"state" jsonschema:"the container state, e.g. running or exited"`
	Status      string `json:"status" jsonschema:"a human readable status string"`
	Phase       string `json:"phase" jsonschema:"auth phase: pending (awaiting login) or ready (authenticated)"`
	Created     int64  `json:"created" jsonschema:"creation time as a unix timestamp"`
}

// CreateOptions holds the caller-controlled inputs for a new box.
type CreateOptions struct {
	// Image is the container image to launch; empty means the Manager default.
	Image string
	// Hostname, when set, becomes the box's container hostname (what `hostname`
	// reports inside it). Must be a valid hostname or Docker rejects creation.
	Hostname string
	// Description is a free-form label shown by list/get to help the caller tell
	// boxes apart. It has no effect on the box itself.
	Description string
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

// NewManager creates a Manager using Docker configuration from the environment.
// defaultImage and remoteArgs fall back to sensible defaults when empty.
//
// @arg defaultImage The image launched when a caller does not specify one; empty falls back to DefaultImage.
// @arg remoteArgs The remote-control flags; empty falls back to the default flags.
// @arg peers Container names (resource servers) connected into every box's own network; empty isolates boxes with no shared peers.
// @return *Manager A Manager wired to a Docker client built from the environment.
// @error error if the Docker client cannot be created.
//
// @testcase TestListMapsPhaseFromName covers Manager behaviour via a constructed Manager.
func NewManager(defaultImage, remoteArgs string, peers []string) (*Manager, error) {
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
	return &Manager{cli: cli, defaultImage: defaultImage, remoteArgs: remoteArgs, peers: peers}, nil
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
// @return []Box One Box per managed container, with phase, hostname, and description filled in.
// @error error if listing containers from Docker fails.
//
// @testcase TestListMapsPhaseFromName checks phase, ID, hostname, and description mapping.
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
			ID:          c.ID[:12],
			Name:        name,
			Hostname:    c.Labels[HostnameLabel],
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

// CreateLLMBox creates and starts a box, captures the OAuth authorize URL its
// login process prints, and returns the box ID plus that URL. The box is left
// running, parked at the "paste code" prompt, ready for SubmitCode. opts.Hostname
// sets the container hostname, and opts.Hostname/opts.Description are persisted
// as labels so List can report them. A non-empty opts.Hostname must be unique
// across managed boxes; the create is rejected otherwise. If the image is not
// present locally, it is pulled and the create is retried once. Any opts.Files
// are written into the box after creation but before it starts.
//
// @arg ctx Context for the Docker create/start/attach calls.
// @arg opts The caller-controlled image, hostname, description, and files for the box.
// @return id The full container ID of the created box.
// @return authorizeURL The OAuth authorize URL captured from the box's login output.
// @error error if opts.Hostname is already in use, the image cannot be pulled, or the box cannot be created, files injected, started, or its authorize URL captured.
//
// @testcase TestCreateLLMBoxCapturesURL captures the authorize URL and sets hostname/description labels.
// @testcase TestCreateLLMBoxCleansUpOnStartFailure removes the container when start fails.
// @testcase TestCreateLLMBoxRejectsDuplicateHostname rejects a hostname already in use.
// @testcase TestCreateLLMBoxPullsMissingImage pulls the image then retries when it is absent.
// @testcase TestCreateLLMBoxInjectsFiles copies injected files into the box before start.
func (m *Manager) CreateLLMBox(ctx context.Context, opts CreateOptions) (id, authorizeURL string, err error) {
	image := opts.Image
	if image == "" {
		image = m.defaultImage
	}

	labels := map[string]string{ManagedLabel: "true"}
	if opts.Hostname != "" {
		labels[HostnameLabel] = opts.Hostname
	}
	if opts.Description != "" {
		labels[DescriptionLabel] = opts.Description
	}

	// Entrypoint: (1) authenticate only if needed, (2) pre-answer the
	// workspace-trust and "Enable Remote Control?" prompts a fresh box would
	// otherwise block on, then (3) hand off to remote-control. `script` allocates
	// a fresh PTY for remote-control's UI, which it needs to reach "Ready". The
	// node step merges into ~/.claude.json so it doesn't clobber what
	// `claude auth login` wrote.
	//
	// This entrypoint re-runs on every container start, including `docker restart`.
	// `claude auth login` is therefore guarded: the OAuth flow only runs when the
	// box has no credentials yet. A restart finds the token already on disk at
	// ~/.claude/.credentials.json (preserved in the container's writable layer)
	// and skips straight to remote-control, so the user is not asked to
	// authenticate again. The guard also honours CLAUDE_CODE_OAUTH_TOKEN, the
	// token-via-env alternative.
	entry := fmt.Sprintf(
		`{ [ -n "$CLAUDE_CODE_OAUTH_TOKEN" ] || [ -s "$HOME/.claude/.credentials.json" ] || claude auth login --claudeai; } && %s && exec script -qfc "claude remote-control %s" /dev/null`,
		prepConfigCmd, m.remoteArgs,
	)

	// Reserve the hostname atomically: under one lock, reject the create if an
	// existing box already uses it, then create the container (which carries the
	// hostname label, so a concurrent create will see it). The slow login / URL
	// capture below runs unlocked.
	m.createMu.Lock()
	if opts.Hostname != "" {
		boxes, lerr := m.List(ctx)
		if lerr != nil {
			m.createMu.Unlock()
			return "", "", fmt.Errorf("checking hostname uniqueness: %w", lerr)
		}
		for _, b := range boxes {
			if strings.EqualFold(b.Hostname, opts.Hostname) {
				m.createMu.Unlock()
				return "", "", fmt.Errorf("hostname %q is already used by box %s; choose a different hostname", opts.Hostname, b.ID)
			}
		}
	}

	cfg := &container.Config{
		Image:      image,
		Hostname:   opts.Hostname,
		Entrypoint: []string{"/bin/sh", "-c", entry},
		Tty:        true,
		OpenStdin:  true,
		Labels:     labels,
	}
	hostCfg := &container.HostConfig{
		// Start the PTY wide so the authorize URL prints unwrapped.
		ConsoleSize:   [2]uint{ttyHeight, ttyWidth},
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
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
		_ = m.cli.ContainerRemove(context.Background(), id, container.RemoveOptions{Force: true})
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
	_ = m.cli.ContainerResize(ctx, id, container.ResizeOptions{Height: ttyHeight, Width: ttyWidth})

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
// @testcase TestCreateLLMBoxInjectsFiles copies injected files into the box before start.
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
// @testcase TestCreateLLMBoxPullsMissingImage pulls then retries when the image is absent.
// @testcase TestCreateLLMBoxPullFailure surfaces an error when the pull fails.
func (m *Manager) pullImage(ctx context.Context, ref string) error {
	rc, err := m.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return err
	}
	defer rc.Close()
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
// @testcase TestCreateLLMBoxCapturesURL drives readAuthorizeURL via CreateLLMBox.
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
// the box to mark it authenticated. It returns the session URL (and any tail of
// output captured, for diagnostics).
//
// @arg ctx Context for the Docker attach call.
// @arg id The container ID of the pending box.
// @arg code The OAuth code to write to the box's login prompt.
// @return sessionURL The remote-control session URL printed once login completes.
// @error error if attaching fails, the login does not complete, or the box cannot be renamed to ready.
//
// @testcase TestSubmitCodeReturnsSessionURL writes the code and returns the session URL.
// @testcase TestSubmitCodeAttachError fails when attaching to the box fails.
func (m *Manager) SubmitCode(ctx context.Context, id, code string) (sessionURL string, err error) {
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
	// connected is non-fatal — the box still works for the others.
	for _, peer := range m.peers {
		_ = m.cli.NetworkConnect(ctx, name, peer, nil)
	}
	// Detach from the default bridge so the box lives only on its own network.
	if err := m.cli.NetworkDisconnect(ctx, defaultBridgeNetwork, id, true); err != nil {
		return fmt.Errorf("detaching box from the default bridge: %w", err)
	}
	return nil
}

// removeBoxNetwork tears down a box's dedicated network, first disconnecting the
// resource-server peers (whose live endpoints would otherwise block removal). It
// is best-effort: errors are ignored so destroy/reap always proceeds.
//
// @arg ctx Context for the disconnect/remove calls.
// @arg id The box's container ID.
//
// @testcase TestDestroyRemovesBoxNetwork checks the box network is removed on destroy.
func (m *Manager) removeBoxNetwork(ctx context.Context, id string) {
	name := boxNetworkName(id)
	for _, peer := range m.peers {
		_ = m.cli.NetworkDisconnect(ctx, name, peer, true)
	}
	_ = m.cli.NetworkRemove(ctx, name)
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
	if err := m.cli.ContainerStop(ctx, b.ID, container.StopOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("stopping box %s: %w", idOrName, err)
	}
	if err := m.cli.ContainerRemove(ctx, b.ID, container.RemoveOptions{RemoveVolumes: true}); err != nil {
		return fmt.Errorf("removing box %s: %w", idOrName, err)
	}
	m.removeBoxNetwork(ctx, b.ID)
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
	rc, err := m.cli.ContainerLogs(ctx, b.ID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       strconv.Itoa(tail),
	})
	if err != nil {
		return "", fmt.Errorf("reading logs for box %s: %w", idOrName, err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return "", fmt.Errorf("reading log stream for box %s: %w", idOrName, err)
	}
	return string(stripANSI(raw)), nil
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
			if err := m.cli.ContainerRemove(ctx, b.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}); err == nil {
				m.removeBoxNetwork(ctx, b.ID)
				reaped = append(reaped, b.ID)
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
			strings.HasPrefix(b.ID, idOrName) ||
			strings.HasPrefix(idOrName, b.ID) ||
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
// @testcase TestCreateLLMBoxCapturesURL relies on scanFor to find the authorize URL.
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
