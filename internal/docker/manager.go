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
	"bufio"
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	// ManagedLabel marks every container created by this server.
	ManagedLabel = "com.llmbox.managed"

	// DefaultImage is launched when the caller does not specify one.
	DefaultImage = "claude-remote"

	// pendingPrefix / readyPrefix encode a box's auth phase in its name, so the
	// phase survives a restart of this server (Docker persists names, but not
	// our in-memory state). Reaping targets pendingPrefix only.
	pendingPrefix = "llmbox-pending-"
	readyPrefix   = "llmbox-"

	// Default remote-control flags; --spawn must be explicit for headless start.
	defaultRemoteArgs = "--spawn same-dir"

	// ttyWidth is wide enough that the authorize URL prints on a single line
	// instead of being wrapped by the TTY (which would break URL extraction).
	ttyWidth  = 1000
	ttyHeight = 50
)

// dockerAPI is the subset of the Docker client used by Manager. It exists so
// the Docker layer can be faked in tests; *client.Client satisfies it.
type dockerAPI interface {
	ContainerList(ctx context.Context, opts container.ListOptions) ([]container.Summary, error)
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, opts container.StartOptions) error
	ContainerRemove(ctx context.Context, containerID string, opts container.RemoveOptions) error
	ContainerRename(ctx context.Context, containerID, newName string) error
	ContainerResize(ctx context.Context, containerID string, opts container.ResizeOptions) error
	ContainerAttach(ctx context.Context, containerID string, opts container.AttachOptions) (types.HijackedResponse, error)
	Close() error
}

// Manager talks to the Docker daemon.
type Manager struct {
	cli          dockerAPI
	defaultImage string
	remoteArgs   string
}

// Box is a view of a managed container returned to callers.
type Box struct {
	ID      string `json:"id" jsonschema:"the short box ID"`
	Name    string `json:"name" jsonschema:"the container name"`
	Image   string `json:"image" jsonschema:"the image the box runs"`
	State   string `json:"state" jsonschema:"the container state, e.g. running or exited"`
	Status  string `json:"status" jsonschema:"a human readable status string"`
	Phase   string `json:"phase" jsonschema:"auth phase: pending (awaiting login) or ready (authenticated)"`
	Created int64  `json:"created" jsonschema:"creation time as a unix timestamp"`
}

// NewManager creates a Manager using Docker configuration from the environment.
// defaultImage and remoteArgs fall back to sensible defaults when empty.
func NewManager(defaultImage, remoteArgs string) (*Manager, error) {
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
	return &Manager{cli: cli, defaultImage: defaultImage, remoteArgs: remoteArgs}, nil
}

// Close releases the underlying Docker client.
func (m *Manager) Close() error { return m.cli.Close() }

func managedFilter() filters.Args {
	return filters.NewArgs(filters.Arg("label", ManagedLabel+"=true"))
}

// phaseOf reports a box's auth phase from its container name.
func phaseOf(name string) string {
	if strings.HasPrefix(name, pendingPrefix) {
		return "pending"
	}
	return "ready"
}

// List returns all boxes created by this server, running or not.
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
			ID:      c.ID[:12],
			Name:    name,
			Image:   c.Image,
			State:   c.State,
			Status:  c.Status,
			Phase:   phaseOf(name),
			Created: c.Created,
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
// running, parked at the "paste code" prompt, ready for SubmitCode.
func (m *Manager) CreateLLMBox(ctx context.Context, image string) (id, authorizeURL string, err error) {
	if image == "" {
		image = m.defaultImage
	}

	// Entrypoint: authenticate, then hand off to remote-control. `script`
	// allocates a fresh PTY for remote-control's UI, which it needs to reach
	// the "Ready" state and register the session.
	entry := fmt.Sprintf(
		`claude auth login --claudeai && exec script -qfc "claude remote-control %s" /dev/null`,
		m.remoteArgs,
	)

	resp, err := m.cli.ContainerCreate(ctx,
		&container.Config{
			Image:      image,
			Entrypoint: []string{"/bin/sh", "-c", entry},
			Tty:        true,
			OpenStdin:  true,
			Labels:     map[string]string{ManagedLabel: "true"},
		},
		&container.HostConfig{
			// Start the PTY wide so the authorize URL prints unwrapped.
			ConsoleSize:   [2]uint{ttyHeight, ttyWidth},
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		},
		nil, nil, "",
	)
	if err != nil {
		return "", "", fmt.Errorf("creating box from image %q: %w", image, err)
	}
	id = resp.ID

	// From here on, clean up the container on any failure.
	cleanup := func() { _ = m.cli.ContainerRemove(context.Background(), id, container.RemoveOptions{Force: true}) }

	if err := m.cli.ContainerRename(ctx, id, pendingPrefix+id[:12]); err != nil {
		cleanup()
		return "", "", fmt.Errorf("naming box: %w", err)
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

// readAuthorizeURL attaches to a box and reads its output until the OAuth
// authorize URL appears (or the timeout elapses).
func (m *Manager) readAuthorizeURL(ctx context.Context, id string) (string, error) {
	hj, err := m.cli.ContainerAttach(ctx, id, container.AttachOptions{
		Stream: true, Stdout: true, Stderr: true,
	})
	if err != nil {
		return "", fmt.Errorf("attaching to box: %w", err)
	}
	defer hj.Close()

	url, err := scanFor(hj.Reader, authorizeURLRe, 30*time.Second, func() { hj.Close() })
	if err != nil {
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

	url, err := scanFor(hj.Reader, sessionURLRe, 60*time.Second, func() { hj.Close() })
	if err != nil {
		return "", fmt.Errorf("login did not complete (the code may be invalid or expired): %w", err)
	}

	// Mark the box authenticated so the reaper leaves it alone.
	if rerr := m.cli.ContainerRename(ctx, id, readyPrefix+id[:12]); rerr != nil {
		// Non-fatal: the box is authenticated; reaping it later is the only risk.
		return url, fmt.Errorf("box authenticated but could not be renamed to ready: %w", rerr)
	}
	return url, nil
}

// Destroy stops and removes a managed box identified by ID or name.
func (m *Manager) Destroy(ctx context.Context, idOrName string) error {
	b, err := m.findManaged(ctx, idOrName)
	if err != nil {
		return err
	}
	if err := m.cli.ContainerRemove(ctx, b.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
		return fmt.Errorf("removing box %s: %w", idOrName, err)
	}
	return nil
}

// ReapOrphans destroys pending (never-authenticated) boxes older than ttl.
// Authenticated ("ready") boxes are never reaped. It returns the IDs reaped.
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
				reaped = append(reaped, b.ID)
			}
		}
	}
	return reaped, nil
}

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
// closing the connection) when the deadline passes.
func scanFor(r *bufio.Reader, re *regexp.Regexp, timeout time.Duration, onTimeout func()) (string, error) {
	type result struct {
		match string
		err   error
	}
	done := make(chan result, 1)

	go func() {
		var acc []byte
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
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
			if err != nil {
				done <- result{err: err}
				return
			}
		}
	}()

	select {
	case res := <-done:
		if res.match != "" {
			return res.match, nil
		}
		return "", fmt.Errorf("stream ended before match: %v", res.err)
	case <-time.After(timeout):
		onTimeout()
		return "", fmt.Errorf("timed out after %s", timeout)
	}
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07|\x1b[()][AB0]|[\r]`)

// stripANSI removes ANSI escape sequences and carriage returns so regexes can
// match text the TUI rendered.
func stripANSI(b []byte) []byte {
	return ansiRe.ReplaceAll(b, nil)
}
