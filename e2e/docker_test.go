//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/docker"
)

// fakeBoxManager simulates the Docker box-lifecycle layer. The real
// implementation is *docker.Manager, which launches a container per box and
// provisions it with the spoke's init script; this stand-in keeps boxes in
// memory. It satisfies cluster.BoxManager (plus the BoxDialer the proxy layer
// uses) so the real server, box-control API, and admin web UI all run unchanged on top
// of it — only Docker is simulated. A created box is immediately ready.
type fakeBoxManager struct {
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
	created     int64
}

// newFakeBoxManager builds a ready, empty simulated box manager.
//
// @return *fakeBoxManager A ready, empty simulated box manager.
func newFakeBoxManager() *fakeBoxManager {
	return &fakeBoxManager{boxes: map[string]*fakeBox{}}
}

// Create simulates launching a box: it rejects a duplicate box ID and returns
// the new container ID. A box is ready as soon as it is created — there is no
// activation step.
//
// @arg ctx Context (unused by the simulation).
// @arg opts The caller-controlled box ID and description (the image is the spoke's own).
// @return sandbox.CreateResult The simulated container ID of the new box.
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
	id := randContainerID()
	m.boxes[id] = &fakeBox{
		containerID: id,
		boxID:       opts.BoxID,
		description: opts.Description,
		image:       image,
		created:     time.Now().Unix(),
	}
	return sandbox.CreateResult{InstanceID: id}, nil
}

// List returns all simulated boxes as the server expects them. A simulated box
// is always a healthy, running box (its init script never fails), so none carry
// the "broken" phase.
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
		out = append(out, sandbox.Box{
			InstanceID:  short,
			Name:        "llmbox-" + short,
			BoxID:       b.boxID,
			Description: b.description,
			Image:       b.image,
			State:       "running",
			Status:      "Up",
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

// Resume simulates resuming a box: it is a no-op beyond verifying the box exists.
//
// @arg ctx Context (unused by the simulation).
// @arg idOrName The ID or name identifying the box.
// @error error if no simulated box matches.
func (m *fakeBoxManager) Resume(_ context.Context, idOrName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.find(idOrName) == nil {
		return fmt.Errorf("no managed box matches %q", idOrName)
	}
	return nil
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

func (m *fakeBoxManager) SetNetworkPolicy(context.Context, string, sandbox.NetworkPolicy) error {
	return nil
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

// find resolves a box ID, container ID (full or short), or box name to a box,
// matching the real provisioner's Find (which the hub addresses by box ID). The
// caller must hold m.mu.
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
			idOrName == "llmbox-"+short:
			return b
		}
	}
	return nil
}

// randContainerID returns a random 40-character hex string for a fake container ID.
//
// @return string A 40-character hex container ID.
func randContainerID() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
