//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/docker"
)

// fakeBoxManager simulates the Docker box-lifecycle layer. The real
// implementation is *docker.Manager, which launches a container per box and
// drives the Claude CLI inside it; this stand-in keeps boxes in memory and runs
// each box's OAuth handshake against the fake Anthropic platform instead. It
// satisfies the (unexported) boxManager interface hub.New expects, so the
// real server, MCP tools, and auth web UI all run unchanged on top of it — only
// Docker and the real Claude binary are simulated.
type fakeBoxManager struct {
	platform *fakeAnthropic

	miscRand io.Reader

	// dialTarget is the address DialBox connects to, standing in for a server
	// listening inside a box (a real loopback server in the proxy e2e test).
	dialTarget string

	mu    sync.Mutex
	boxes map[string]*fakeBox // keyed by full container ID
}

// fakeBox is the in-memory state of one simulated box.
type fakeBox struct {
	containerID string
	boxID       string
	description string
	image       string
	authState   string // the platform OAuth state this box began login with
	sessionURL  string
	ready       bool
	created     int64
}

// newFakeBoxManager builds a box manager backed by the given platform.
//
// @arg platform The simulated Anthropic platform boxes authenticate against.
// @return *fakeBoxManager A ready, empty simulated box manager.
func newFakeBoxManager(platform *fakeAnthropic) *fakeBoxManager {
	return &fakeBoxManager{
		miscRand: rand.New(rand.NewSource(100)),
		platform: platform,
		boxes:    map[string]*fakeBox{},
	}
}

// Create simulates launching a box: it rejects a duplicate box ID, begins
// an OAuth login on the platform, and returns the new container ID plus the
// authorize URL the user must open — exactly the contract the real manager has.
//
// @arg ctx Context (unused by the simulation).
// @arg opts The caller-controlled box ID and description (the image is the spoke's own).
// @return id The simulated container ID of the new box.
// @return authorizeURL The OAuth authorize URL for the box's login.
// @error error if the requested box ID is already in use.
func (m *fakeBoxManager) Create(_ context.Context, opts sandbox.CreateOptions) (sandbox.CreateResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if opts.BoxID != "" {
		for _, b := range m.boxes {
			if strings.EqualFold(b.boxID, opts.BoxID) {
				return sandbox.CreateResult{}, fmt.Errorf("box ID %q is already used by container %s; choose a different box ID", opts.BoxID, b.containerID[:12])
			}
		}
	}
	// The spoke owns the image: every box launches the spoke's configured default,
	// not one named by the create request.
	image := docker.DefaultImage
	id := randHex(m.miscRand, 20)
	state, authorizeURL := m.platform.beginLogin()
	m.boxes[id] = &fakeBox{
		containerID: id,
		boxID:       opts.BoxID,
		description: opts.Description,
		image:       image,
		authState:   state,
		created:     time.Now().Unix(),
	}
	return sandbox.CreateResult{InstanceID: id, AuthorizeURL: authorizeURL}, nil
}

// SubmitCode simulates feeding the user's code to the box's login process: it
// exchanges the code with the platform and, on success, marks the box ready and
// returns its session URL.
//
// @arg ctx Context (unused by the simulation).
// @arg id The container ID of the pending box.
// @arg code The OAuth code the user pasted.
// @return sessionURL The remote-control session URL once login completes.
// @error error if the box is unknown or the code is rejected by the platform.
func (m *fakeBoxManager) SubmitCode(_ context.Context, id, code string) (sessionURL string, err error) {
	m.mu.Lock()
	b := m.find(id)
	m.mu.Unlock()
	if b == nil {
		return "", fmt.Errorf("no managed box matches %q", id)
	}
	url, err := m.platform.exchange(b.authState, code)
	if err != nil {
		return "", fmt.Errorf("login did not complete; box said: %v", err)
	}
	m.mu.Lock()
	b.ready = true
	b.sessionURL = url
	m.mu.Unlock()
	return url, nil
}

// List returns all simulated boxes as the server expects them, with the auth
// phase and a phase-encoding name derived from each box's readiness.
//
// @arg ctx Context (unused by the simulation).
// @return []sandbox.Box One Box per simulated box.
// @error error never; present to satisfy the interface.
func (m *fakeBoxManager) List(_ context.Context) ([]sandbox.Box, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]sandbox.Box, 0, len(m.boxes))
	for _, b := range m.boxes {
		short := b.containerID[:12]
		name, phase := "llmbox-pending-"+short, "pending"
		if b.ready {
			name, phase = "llmbox-"+short, "ready"
		}
		out = append(out, sandbox.Box{
			InstanceID:  short,
			Name:        name,
			BoxID:       b.boxID,
			Description: b.description,
			Image:       b.image,
			State:       "running",
			Status:      "Up",
			Phase:       phase,
			Created:     b.created,
		})
	}
	return out, nil
}

// Destroy removes the simulated box identified by ID or name.
//
// @arg ctx Context (unused by the simulation).
// @arg idOrName The ID (full or short) or name identifying the box.
// @error error if no simulated box matches.
func (m *fakeBoxManager) Destroy(_ context.Context, idOrName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.find(idOrName)
	if b == nil {
		return fmt.Errorf("no managed box matches %q", idOrName)
	}
	delete(m.boxes, b.containerID)
	return nil
}

// Pause simulates pausing a box: it is a no-op beyond verifying the box exists.
//
// @arg ctx Context (unused by the simulation).
// @arg idOrName The ID or name identifying the box.
// @error error if no simulated box matches.
func (m *fakeBoxManager) Pause(_ context.Context, idOrName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.find(idOrName) == nil {
		return fmt.Errorf("no managed box matches %q", idOrName)
	}
	return nil
}

// Resume simulates resuming a box: it verifies the box exists and returns a
// canned session URL.
//
// @arg ctx Context (unused by the simulation).
// @arg idOrName The ID or name identifying the box.
// @return string A canned session URL.
// @error error if no simulated box matches.
func (m *fakeBoxManager) Resume(_ context.Context, idOrName string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.find(idOrName) == nil {
		return "", fmt.Errorf("no managed box matches %q", idOrName)
	}
	return "https://claude.ai/code/session", nil
}

// Logs returns canned console output for the box, standing in for a real box's
// remote-control logs.
//
// @arg ctx Context (unused by the simulation).
// @arg idOrName The ID or name identifying the box.
// @arg tail The requested tail (ignored; the canned output is short).
// @return string The box's simulated console output.
// @error error if no simulated box matches.
func (m *fakeBoxManager) Logs(_ context.Context, idOrName string, _ int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.find(idOrName) == nil {
		return "", fmt.Errorf("no managed box matches %q", idOrName)
	}
	return "claude remote-control: Ready\nlistening for sessions\n", nil
}

// Exec simulates running a shell command in the box. It understands a single
// `echo <text>` line — enough to prove a command round-trips end to end — and
// returns empty output with a zero exit code for anything else.
//
// @arg ctx Context (unused by the simulation).
// @arg idOrName The ID or name identifying the box.
// @arg cmd The command to run, as the server passes it (/bin/sh -c <line>).
// @return sandbox.ExecResult The simulated stdout, stderr, and exit code.
// @error error if no simulated box matches.
func (m *fakeBoxManager) Exec(_ context.Context, idOrName string, cmd []string) (sandbox.ExecResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.find(idOrName) == nil {
		return sandbox.ExecResult{}, fmt.Errorf("no managed box matches %q", idOrName)
	}
	line := ""
	if len(cmd) > 0 {
		line = cmd[len(cmd)-1]
	}
	if rest, ok := strings.CutPrefix(line, "echo "); ok {
		return sandbox.ExecResult{Stdout: rest + "\n"}, nil
	}
	return sandbox.ExecResult{}, nil
}

// DialBox connects to the fake's dialTarget, standing in for a real box's port.
// It satisfies the server's boxDialer interface so the proxy path can be driven
// end to end against a real loopback server without Docker.
//
// @arg ctx Context for the dial.
// @arg idOrName The box identifier (ignored; the simulation has one target).
// @arg port The port (ignored).
// @return net.Conn A connection to the dial target.
// @error error if no dial target is configured or the dial fails.
func (m *fakeBoxManager) DialBox(ctx context.Context, idOrName string, port int) (net.Conn, error) {
	if m.dialTarget == "" {
		return nil, fmt.Errorf("no dial target configured")
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", m.dialTarget)
}

// ReapOrphans is a no-op in the simulation; the workflow never leaves orphans.
//
// @arg ctx Context (unused by the simulation).
// @arg ttl The orphan TTL (unused).
// @return []string Always nil; nothing is reaped.
// @error error never; present to satisfy the interface.
func (m *fakeBoxManager) ReapOrphans(_ context.Context, _ time.Duration) ([]string, error) {
	return nil, nil
}

// find resolves a box ID, container ID (full or short), or phase-prefixed name
// to a box, matching the real provisioner's Find (which the hub now addresses by
// box ID, e.g. SubmitCode/Destroy/Logs/Exec pass sess.BoxID). The caller must
// hold m.mu.
//
// @arg idOrName The identifier to resolve.
// @return *fakeBox The matching box, or nil.
func (m *fakeBoxManager) find(idOrName string) *fakeBox {
	for _, b := range m.boxes {
		short := b.containerID[:12]
		switch {
		case b.boxID != "" && b.boxID == idOrName,
			b.containerID == idOrName,
			strings.HasPrefix(b.containerID, idOrName),
			strings.HasPrefix(idOrName, short),
			idOrName == "llmbox-pending-"+short,
			idOrName == "llmbox-"+short:
			return b
		}
	}
	return nil
}
