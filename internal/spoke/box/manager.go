package box

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/clems4ever/llmbox/internal/guest"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// Config tunes a Manager. The zero value is valid: no box-count cap and no init
// script.
type Config struct {
	// MaxBoxes caps how many managed boxes may exist at once; Create rejects a new
	// box once the count is reached (0 = unlimited). It bounds the by-design
	// unauthenticated create path.
	MaxBoxes int
	// InitScript is an optional host-provided provisioning script the guest runs
	// inside every box during Init, as the box user. Nil runs nothing. It lets a
	// spoke customise its boxes (install and start a workload) without rebuilding
	// the image.
	InitScript []byte
	// InitScriptTimeout bounds how long the init script may run before Create fails
	// the box. Non-positive uses the guest default (five minutes).
	InitScriptTimeout time.Duration
	// CopyFiles are host files the spoke copies into every box (its --copy flag),
	// enumerated once at spoke startup. Each is streamed from its host path into the
	// box at creation, before the init script runs, owned by the box user so the
	// workload can use it. Streaming (rather than embedding the bytes in the Init
	// frame) keeps the copy off the bounded control frame and out of memory, so a
	// box can be seeded with content far larger than a single frame. Nil copies
	// nothing.
	CopyFiles []CopyFile
	// PublishPorts are the in-box TCP ports this spoke exposes as HTTP proxies for
	// every box it creates (the spoke's --publish-port). They are echoed back on a
	// successful Create so the hub can publish them once it has registered the box.
	// Nil publishes nothing.
	PublishPorts []sandbox.PublishPort
}

// CopyFile is one host file a spoke stages into every box (its --copy flag),
// resolved to metadata only at startup: the manager opens HostPath and streams it
// to BoxPath at box-create time, so the file's bytes are never held in memory.
// A directory --copy is expanded to one CopyFile per regular file it contains.
type CopyFile struct {
	// HostPath is the file on the spoke host to read.
	HostPath string
	// BoxPath is the absolute in-box path it is written to.
	BoxPath string
	// Mode is the permission bits to set on the written file (0 means 0644).
	Mode int64
}

const (
	// initScriptClientMargin is the extra slack added to InitScriptTimeout for the
	// Init call's own deadline, so the guest's own timeout fires first and returns a
	// descriptive error rather than the host giving up on the connection mid-script.
	initScriptClientMargin = 30 * time.Second
	// defaultInitScriptTimeout mirrors the guest's own default so the bounded Init
	// deadline matches the script timeout the guest applies when Config leaves it
	// unset. Keep it in sync with guest.defaultInitScriptTimeout.
	defaultInitScriptTimeout = 5 * time.Minute
)

// Manager drives box lifecycle over a Provisioner and the in-box guest. It
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

// client returns a guest client that dials inst's control channel.
//
// @arg inst The box instance to talk to.
// @return *guest.Client A client bound to the instance's control channel.
//
// @testcase TestBoxManager exercises client through every verb.
func (m *Manager) client(inst Instance) *guest.Client {
	return &guest.Client{Dial: inst.Control}
}

// Create provisions a new box and runs its init script, returning the box's
// generation token (and the ports the spoke publishes for it). The uniqueness and
// box-count checks run under a lock. A transport-level failure (provisioning or
// file injection) tears the box down so a half-created box is never left behind. A
// failing init script is the one exception: the box is kept and returned with
// InitScriptFailed set (and the script's output) so an operator can inspect the
// broken box rather than have it vanish.
//
// @arg ctx Context for the provision and init.
// @arg opts The caller-controlled inputs for the box.
// @return sandbox.CreateResult The box's generation token and published ports, or the init-script failure and its output.
// @error error if the box id is invalid, already in use, the cap is reached, or provisioning/injection fails.
//
// @testcase TestBoxManager covers create plus box-id validation, duplicate rejection, the box cap, and (via the conformance init-script-failure case) a kept broken box.
func (m *Manager) Create(ctx context.Context, opts sandbox.CreateOptions) (sandbox.CreateResult, error) {
	if opts.BoxID != "" && !sandbox.ValidBoxID(opts.BoxID) {
		return sandbox.CreateResult{}, fmt.Errorf("invalid box id %q: must be 1-63 chars of lowercase letters, digits, or hyphens (not starting or ending with a hyphen)", opts.BoxID)
	}

	m.createMu.Lock()
	if opts.BoxID != "" || m.cfg.MaxBoxes > 0 {
		insts, lerr := m.prov.List(ctx)
		if lerr != nil {
			m.createMu.Unlock()
			return sandbox.CreateResult{}, fmt.Errorf("checking box uniqueness: %w", lerr)
		}
		if m.cfg.MaxBoxes > 0 && len(insts) >= m.cfg.MaxBoxes {
			m.createMu.Unlock()
			return sandbox.CreateResult{}, fmt.Errorf("box limit reached (%d boxes already running); destroy a box before creating another", m.cfg.MaxBoxes)
		}
		for _, inst := range insts {
			if opts.BoxID != "" && strings.EqualFold(inst.Meta().BoxID, opts.BoxID) {
				m.createMu.Unlock()
				return sandbox.CreateResult{}, fmt.Errorf("box ID %q is already used by %s; choose a different box ID", opts.BoxID, inst.Meta().InstanceID)
			}
		}
	}
	inst, err := m.prov.Provision(ctx, opts)
	m.createMu.Unlock()
	if err != nil {
		return sandbox.CreateResult{}, fmt.Errorf("provisioning box: %w", err)
	}

	// Tear the box down on any transport-level failure so a half-created box is
	// never left running.
	c := m.client(inst)
	// Stream the spoke's --copy files into the box before Init, so the init script
	// can rely on them. They are streamed from disk (not embedded in the Init
	// frame), so a copy larger than a control frame is fine.
	if err := m.copyFiles(ctx, c); err != nil {
		_ = inst.Destroy(context.Background())
		return sandbox.CreateResult{}, fmt.Errorf("copying files into box: %w", err)
	}
	initCtx, cancel := m.initContext(ctx)
	initResp, err := c.Init(initCtx, guest.InitReq{
		Files:             opts.Files,
		InitScript:        m.cfg.InitScript,
		InitScriptTimeout: m.cfg.InitScriptTimeout,
	})
	cancel()
	if err != nil {
		_ = inst.Destroy(context.Background())
		return sandbox.CreateResult{}, fmt.Errorf("initialising box: %w", err)
	}
	if initResp.ScriptFailed {
		// Keep the broken box for inspection instead of destroying it, carrying the
		// init script's output so the operator can see why provisioning failed.
		return sandbox.CreateResult{
			InstanceID:       inst.Meta().InstanceID,
			InitScriptFailed: true,
			InitScriptOutput: initScriptFailureDetail(initResp),
		}, nil
	}
	// The box came up, so hand the hub the ports this spoke publishes for every box
	// (empty when none are configured). The hub creates the proxies once it has
	// registered the box; a broken box (handled above) returns no ports.
	return sandbox.CreateResult{
		InstanceID:   inst.Meta().InstanceID,
		PublishPorts: m.cfg.PublishPorts,
	}, nil
}

// copyFiles streams every configured --copy file from its host path into the box
// through the guest's PutFile verb, one connection per file. Each file is opened
// and streamed straight from disk, so an arbitrarily large copy never sits in
// memory and never rides the bounded Init frame. It stops at the first failure so
// the caller can tear the half-provisioned box down.
//
// @arg ctx Context for the transfers.
// @arg c The guest client for the box being created.
// @error error if a host file cannot be opened or statted, or a transfer fails.
//
// @testcase TestConformanceFake covers create with configured --copy files (the CopyFiles case).
func (m *Manager) copyFiles(ctx context.Context, c *guest.Client) error {
	for _, cf := range m.cfg.CopyFiles {
		f, err := os.Open(cf.HostPath)
		if err != nil {
			return fmt.Errorf("opening --copy source %s: %w", cf.HostPath, err)
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return fmt.Errorf("stat --copy source %s: %w", cf.HostPath, err)
		}
		err = c.PutFile(ctx, cf.BoxPath, cf.Mode, info.Size(), f)
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("copying %s to %s: %w", cf.HostPath, cf.BoxPath, err)
		}
	}
	return nil
}

// initScriptFailureDetail composes the human-readable detail stored for a broken
// box: the failure reason followed by the script's captured output when there is
// any, so the operator sees both why it failed and what it printed.
//
// @arg resp The init response reporting the script failure.
// @return string The reason, with the captured output appended when non-empty.
//
// @testcase TestBoxManager reads back the composed detail via the conformance init-script-failure case.
func initScriptFailureDetail(resp guest.InitResp) string {
	if resp.ScriptOutput == "" {
		return resp.ScriptError
	}
	return resp.ScriptError + "\n\n" + resp.ScriptOutput
}

// initContext derives the context for the guest Init call. When an init script is
// configured it bounds the call to the script timeout plus a margin, so the host
// waits long enough for a slow provisioning script yet still gives up if the guest
// hangs entirely (the guest's own, shorter timeout fires first with a descriptive
// error). With no init script it returns the parent context and a no-op cancel, so
// the common path is unchanged.
//
// @arg ctx The parent context for the create.
// @return context.Context The context to pass to the Init call.
// @return context.CancelFunc The cancel to call once Init returns (never nil).
//
// @testcase TestBoxManager covers create with no init script (the unbounded path).
func (m *Manager) initContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if len(m.cfg.InitScript) == 0 {
		return ctx, func() {}
	}
	timeout := m.cfg.InitScriptTimeout
	if timeout <= 0 {
		timeout = defaultInitScriptTimeout
	}
	return context.WithTimeout(ctx, timeout+initScriptClientMargin)
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

// Pause stops a managed box's compute to save CPU/RAM while keeping its disk, so it
// can be resumed later. The box keeps existing (it still appears in List, reported
// as paused); its running workload ends.
//
// @arg ctx Context for the resolve and pause.
// @arg idOrName The ID or name identifying the box to pause.
// @error error if no managed box matches or the box cannot be paused.
//
// @testcase TestBoxManager pauses a box and sees it reported paused.
func (m *Manager) Pause(ctx context.Context, idOrName string) error {
	inst, err := m.prov.Find(ctx, idOrName)
	if err != nil {
		return err
	}
	return inst.Pause(ctx)
}

// Resume restarts a paused box's compute from its kept disk. The box's disk (and
// any files/credentials its init script wrote) survives, but the init script is a
// create-time step and is not re-run, so a workload that does not restart itself
// on boot must be restarted by the operator.
//
// @arg ctx Context for the resolve and resume.
// @arg idOrName The ID or name identifying the box to resume.
// @error error if no managed box matches or the box cannot be resumed.
//
// @testcase TestBoxManager resumes a paused box.
func (m *Manager) Resume(ctx context.Context, idOrName string) error {
	inst, err := m.prov.Find(ctx, idOrName)
	if err != nil {
		return err
	}
	return inst.Resume(ctx)
}

// Exec runs a command inside a managed box and returns its captured result.
//
// @arg ctx Context for the resolve and the guest call.
// @arg idOrName The ID or name identifying the box.
// @arg cmd The command and arguments to run.
// @return sandbox.ExecResult The command's output and exit code.
// @error error if no managed box matches or the guest call fails.
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
// box's guest to splice the control channel to localhost:port. It is the box
// reachability primitive the proxy layer builds on; it resolves through Find
// first so it can only ever reach a box this manager created.
//
// @arg ctx Context for the resolve and the dial.
// @arg idOrName The ID or name identifying the box.
// @arg port The TCP port inside the box to connect to.
// @return net.Conn A connection spliced to the in-box port; the caller must close it.
// @error error if no managed box matches or the port cannot be reached.
//
// @testcase TestBoxManagerDialBox reaches a box-localhost listener through DialBox.
func (m *Manager) DialBox(ctx context.Context, idOrName string, port int) (net.Conn, error) {
	inst, err := m.prov.Find(ctx, idOrName)
	if err != nil {
		return nil, err
	}
	return m.client(inst).DialPort(ctx, port)
}
