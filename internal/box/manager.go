package box

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/clems4ever/llmbox/internal/agent"
	"github.com/clems4ever/llmbox/internal/sandbox"
)

// Config tunes a Manager. The zero value is valid: no remote-control args and no
// box-count cap.
type Config struct {
	// RemoteArgs is passed through to the box entrypoint's `claude remote-control`
	// invocation (the agent appends a --name for the default session).
	RemoteArgs string
	// MaxBoxes caps how many managed boxes may exist at once; Create rejects a new
	// box once the count is reached (0 = unlimited). It bounds the by-design
	// unauthenticated create path.
	MaxBoxes int
}

// Manager drives box lifecycle over a Provisioner and the in-box agent. It
// implements cluster.BoxManager (and the BoxDialer the proxy layer uses) without
// knowing how the backend runs compute. It is safe for concurrent use.
type Manager struct {
	prov Provisioner
	cfg  Config

	// createMu serialises the uniqueness/cap check and provisioning so two
	// concurrent creates cannot both pass the check with the same box ID.
	createMu sync.Mutex
}

// NewManager returns a Manager backed by prov and configured by cfg.
//
// @arg prov The provisioner that owns box compute.
// @arg cfg The manager configuration (zero value imposes no limits).
// @return *Manager A ready box manager.
//
// @testcase TestBoxManager exercises a Manager built by NewManager.
func NewManager(prov Provisioner, cfg Config) *Manager {
	return &Manager{prov: prov, cfg: cfg}
}

// client returns an agent client that dials inst's control channel.
//
// @arg inst The box instance to talk to.
// @return *agent.Client A client bound to the instance's control channel.
//
// @testcase TestBoxManager exercises client through every verb.
func (m *Manager) client(inst Instance) *agent.Client {
	return &agent.Client{Dial: inst.Control}
}

// Create provisions a new box, injects its files and parameters, launches claude,
// and returns the box ID and the OAuth authorize URL to complete login with. The
// uniqueness and box-count checks run under a lock; the slow login handshake does
// not. On any post-provision failure the box is destroyed so a half-created box
// is never left behind.
//
// @arg ctx Context for the provision and login handshake.
// @arg opts The caller-controlled inputs for the box.
// @return id The new box's container/instance ID.
// @return authorizeURL The OAuth authorize URL to complete login with.
// @error error if the box id is invalid, already in use, the cap is reached, or provisioning/login fails.
//
// @testcase TestBoxManager covers create plus box-id validation, duplicate rejection, and the box cap.
func (m *Manager) Create(ctx context.Context, opts sandbox.CreateOptions) (id, authorizeURL string, err error) {
	if opts.BoxID != "" && !sandbox.ValidBoxID(opts.BoxID) {
		return "", "", fmt.Errorf("invalid box id %q: must be 1-63 chars of lowercase letters, digits, or hyphens (not starting or ending with a hyphen)", opts.BoxID)
	}

	m.createMu.Lock()
	if opts.BoxID != "" || m.cfg.MaxBoxes > 0 {
		insts, lerr := m.prov.List(ctx)
		if lerr != nil {
			m.createMu.Unlock()
			return "", "", fmt.Errorf("checking box uniqueness: %w", lerr)
		}
		if m.cfg.MaxBoxes > 0 && len(insts) >= m.cfg.MaxBoxes {
			m.createMu.Unlock()
			return "", "", fmt.Errorf("box limit reached (%d boxes already running); destroy a box before creating another", m.cfg.MaxBoxes)
		}
		for _, inst := range insts {
			if opts.BoxID != "" && strings.EqualFold(inst.Meta().BoxID, opts.BoxID) {
				m.createMu.Unlock()
				return "", "", fmt.Errorf("box ID %q is already used by %s; choose a different box ID", opts.BoxID, inst.Meta().ContainerID)
			}
		}
	}
	inst, err := m.prov.Provision(ctx, opts)
	m.createMu.Unlock()
	if err != nil {
		return "", "", fmt.Errorf("provisioning box: %w", err)
	}

	// From here on, tear the box down on any failure so a half-created box is
	// never left running.
	c := m.client(inst)
	if err := c.Init(ctx, agent.InitReq{
		Files:      opts.Files,
		RemoteArgs: m.cfg.RemoteArgs,
		BoxID:      opts.BoxID,
	}); err != nil {
		_ = inst.Destroy(context.Background())
		return "", "", fmt.Errorf("initialising box: %w", err)
	}
	start, err := c.Start(ctx)
	if err != nil {
		_ = inst.Destroy(context.Background())
		return "", "", fmt.Errorf("starting box: %w", err)
	}
	return inst.Meta().ContainerID, start.AuthorizeURL, nil
}

// SubmitCode writes the OAuth code to a pending box, waits for the session URL,
// and marks the box ready so the reaper spares it.
//
// @arg ctx Context for the login handshake.
// @arg idOrName The ID or name identifying the pending box.
// @arg code The OAuth code to submit.
// @return sessionURL The remote-control session URL printed once login completes.
// @error error if no managed box matches, login does not complete, or the box cannot be marked ready.
//
// @testcase TestBoxManager submits the code and returns the session URL.
func (m *Manager) SubmitCode(ctx context.Context, idOrName, code string) (sessionURL string, err error) {
	inst, err := m.prov.Find(ctx, idOrName)
	if err != nil {
		return "", err
	}
	url, err := m.client(inst).SubmitCode(ctx, code)
	if err != nil {
		return "", err
	}
	if err := inst.MarkReady(ctx); err != nil {
		// Non-fatal: the box is authenticated; the only risk is the reaper later
		// removing it as if still pending.
		return url, fmt.Errorf("box authenticated but could not be marked ready: %w", err)
	}
	return url, nil
}

// List returns a view of every managed box.
//
// @arg ctx Context for the underlying provisioner list.
// @return []sandbox.Box One view per managed box.
// @error error if the provisioner cannot list boxes.
//
// @testcase TestBoxManager lists created boxes.
func (m *Manager) List(ctx context.Context) ([]sandbox.Box, error) {
	insts, err := m.prov.List(ctx)
	if err != nil {
		return nil, err
	}
	boxes := make([]sandbox.Box, 0, len(insts))
	for _, inst := range insts {
		boxes = append(boxes, inst.Meta())
	}
	return boxes, nil
}

// Destroy removes a managed box. It is idempotent: destroying a box that no
// longer exists returns nil rather than an error.
//
// @arg ctx Context for the resolve and destroy.
// @arg idOrName The ID or name identifying the box to remove.
// @error error if resolving or destroying the box fails for a reason other than it being already gone.
//
// @testcase TestBoxManager destroys a box and is idempotent on a second call.
func (m *Manager) Destroy(ctx context.Context, idOrName string) error {
	inst, err := m.prov.Find(ctx, idOrName)
	if errors.Is(err, sandbox.ErrBoxNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := inst.Destroy(ctx); err != nil && !errors.Is(err, sandbox.ErrBoxNotFound) {
		return err
	}
	return nil
}

// Logs returns the recent console transcript of a managed box.
//
// @arg ctx Context for the resolve and the agent call.
// @arg idOrName The ID or name identifying the box.
// @arg tail The maximum number of trailing lines (non-positive uses the agent default).
// @return string The box's trailing transcript.
// @error error if no managed box matches or the agent call fails.
//
// @testcase TestBoxManager reads back a box transcript.
func (m *Manager) Logs(ctx context.Context, idOrName string, tail int) (string, error) {
	inst, err := m.prov.Find(ctx, idOrName)
	if err != nil {
		return "", err
	}
	return m.client(inst).Logs(ctx, tail)
}

// Exec runs a command inside a managed box and returns its captured result.
//
// @arg ctx Context for the resolve and the agent call.
// @arg idOrName The ID or name identifying the box.
// @arg cmd The command and arguments to run.
// @return sandbox.ExecResult The command's output and exit code.
// @error error if no managed box matches or the agent call fails.
//
// @testcase TestBoxManager runs a command via Exec.
func (m *Manager) Exec(ctx context.Context, idOrName string, cmd []string) (sandbox.ExecResult, error) {
	inst, err := m.prov.Find(ctx, idOrName)
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	return m.client(inst).Exec(ctx, cmd)
}

// DialBox opens a connection to a TCP port inside a managed box, by asking the
// box's agent to splice the control channel to localhost:port. It is the box
// reachability primitive the proxy layer builds on; it resolves through Find
// first so it can only ever reach a box this manager created.
//
// @arg ctx Context for the resolve and the dial.
// @arg idOrName The ID or name identifying the box.
// @arg port The TCP port inside the box to connect to.
// @return net.Conn A connection spliced to the in-box port; the caller must close it.
// @error error if no managed box matches or the port cannot be reached.
//
// @testcase TestBoxManager reaches an in-box listener through DialBox.
func (m *Manager) DialBox(ctx context.Context, idOrName string, port int) (net.Conn, error) {
	inst, err := m.prov.Find(ctx, idOrName)
	if err != nil {
		return nil, err
	}
	return m.client(inst).DialPort(ctx, port)
}

// ReapOrphans destroys pending (never-authenticated) boxes older than ttl,
// sparing ready ones, and returns the IDs reaped. It is generic over the backend:
// it reads each box's phase and creation time from its Meta and destroys the
// stale pending ones.
//
// @arg ctx Context for the list and destroys.
// @arg ttl The maximum age a pending box may reach before it is reaped.
// @return []string The IDs of the boxes that were reaped.
// @error error if listing boxes fails.
//
// @testcase TestBoxManager reaps old pending boxes while sparing ready and fresh ones.
func (m *Manager) ReapOrphans(ctx context.Context, ttl time.Duration) ([]string, error) {
	insts, err := m.prov.List(ctx)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-ttl).Unix()
	var reaped []string
	for _, inst := range insts {
		meta := inst.Meta()
		if meta.Phase == "pending" && meta.Created < cutoff {
			if err := inst.Destroy(ctx); err == nil || errors.Is(err, sandbox.ErrBoxNotFound) {
				reaped = append(reaped, meta.ContainerID)
			}
		}
	}
	return reaped, nil
}
