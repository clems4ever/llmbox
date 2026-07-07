// Package firecracker implements a box.Provisioner backed by Firecracker microVMs.
// Each box is a microVM booting a shared guest kernel and a per-box copy of a
// rootfs whose init is the llmbox guest agent listening on AF_VSOCK. The host
// reaches the agent over the VM's vsock (control and port-proxy), exactly as the
// Docker backend reaches its agent over a bind-mounted Unix socket — so all box
// behaviour (login, exec, logs, dialing) runs through the agent, not through the
// provisioner. Guest outbound traffic (egress) goes through a per-box TAP device
// the host NATs; the agent is never in the egress path.
//
// Firecracker has no daemon that tracks boxes, so the provisioner persists each
// box's metadata under a state directory and holds live machine handles in memory;
// List/Find/Destroy consult that state. Like the Docker backend it can be pinned
// to a namespace so two spokes sharing a host never see each other's boxes.
package firecracker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	fcsdk "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/sirupsen/logrus"

	"github.com/clems4ever/llmbox/internal/box"
	"github.com/clems4ever/llmbox/internal/sandbox"
)

const (
	// defaultFirecrackerBin is the firecracker executable launched per box when no
	// path is configured; it is resolved from PATH.
	defaultFirecrackerBin = "firecracker"
	// defaultStateDir holds per-box runtime files (rootfs copy, vsock socket, api
	// socket, metadata) when the operator configures none.
	defaultStateDir = "/run/llmbox/firecracker"

	// defaultVcpuCount and defaultMemSizeMib are used when the per-box limits leave
	// CPU/memory unset, so a box always boots with a sane machine size.
	defaultVcpuCount  = 1
	defaultMemSizeMib = 512
	// minMemSizeMib floors the derived memory so a tiny limit can't produce a VM too
	// small to boot Linux.
	minMemSizeMib = 128

	// bootWait bounds how long Provision waits for the guest agent to answer on
	// vsock after the VM starts.
	bootWait = 45 * time.Second
	// bootPoll is the interval between agent-reachability probes during bootWait.
	bootPoll = 100 * time.Millisecond

	// defaultPoolSize is the number of egress TAP slots provisioned at startup when
	// the operator configures none; it also caps concurrent networked boxes.
	defaultPoolSize = 16
)

// machine is the subset of *fcsdk.Machine the provisioner drives, extracted so the
// bookkeeping can be unit-tested with a fake that never boots a real VM.
type machine interface {
	// Start creates the VMM process and boots the guest.
	Start(ctx context.Context) error
	// StopVMM terminates the VMM process.
	StopVMM() error
	// Wait blocks until the VMM process exits.
	Wait(ctx context.Context) error
}

// machineFactory builds a machine from a Firecracker config. The real factory
// calls fcsdk.NewMachine; tests substitute a fake.
type machineFactory func(ctx context.Context, cfg fcsdk.Config) (machine, error)

// Provisioner creates and tears down Firecracker microVM boxes and exposes each
// box's agent over the VM's vsock. It implements box.Provisioner (plus Close).
type Provisioner struct {
	kernelImage   string
	defaultRootfs string
	// payloadImage is an optional host path to a small read-only ext4 carrying the
	// guest agent (plus the claude binary and trust seed). When set, every box
	// attaches it as a second, shared, read-only drive (/dev/vdb) that the base
	// rootfs's loader unit mounts and runs — so the agent can be updated by
	// swapping this tiny image without rebuilding the multi-GiB base rootfs. Empty
	// keeps the legacy all-in-one layout where the agent is baked into the rootfs.
	payloadImage   string
	stateDir       string
	firecrackerBin string
	namespace      string
	limits         sandbox.Limits
	egress         egress
	newMachine     machineFactory
	log            *slog.Logger
	// netEnabled controls whether boxes get a TAP + NAT egress interface. When
	// false, a box boots with only loopback and vsock (control-only / air-gapped):
	// the agent is still reachable over vsock, but the guest has no outbound
	// network. Egress setup needs CAP_NET_ADMIN, so this also allows booting boxes
	// as an unprivileged user.
	netEnabled bool
	// poolSize is the number of pre-created egress TAP slots; it caps concurrent
	// networked boxes.
	poolSize int
	// poolOnce guards one-time provisioning of the TAP pool (EnsureNetwork), so the
	// host interfaces are created once at startup rather than per box. poolUp records
	// that the pool was provisioned, so Close tears it down.
	poolOnce sync.Once
	poolErr  error
	poolUp   bool

	// procCtx bounds every box VM process to the provisioner's lifetime rather than
	// to the request that created it. The firecracker binary is launched via
	// exec.CommandContext, which kills the process when its context is done; using
	// the create request's context there would kill the VM as soon as create
	// returns, so a later request (submit code, exec, logs) finds a dead vsock.
	// procCancel fires on Close to stop any still-running VMs.
	procCtx    context.Context
	procCancel context.CancelFunc

	mu    sync.Mutex
	boxes map[string]*fcInstance
	used  map[int]bool
}

// NewProvisioner builds a Firecracker provisioner. kernelImage and defaultRootfs
// are host paths to the guest kernel and the default rootfs image; an empty
// stateDir falls back to defaultStateDir. The returned provisioner uses the real
// host egress and the real Firecracker machine factory.
//
// @arg kernelImage Host path to the guest kernel (vmlinux) every box boots.
// @arg defaultRootfs Host path to the rootfs image booted when a create supplies none.
// @arg stateDir Directory for per-box runtime files and metadata; empty uses defaultStateDir.
// @return *Provisioner A provisioner wired to the real egress and machine factory.
// @error error is always nil today; the signature carries one for symmetry with other backends and future validation.
//
// @testcase TestProvisionerBookkeeping builds a provisioner and exercises its lifecycle with fakes.
func NewProvisioner(kernelImage, defaultRootfs, stateDir string) (*Provisioner, error) {
	if stateDir == "" {
		stateDir = defaultStateDir
	}
	p := &Provisioner{
		kernelImage:    kernelImage,
		defaultRootfs:  defaultRootfs,
		stateDir:       stateDir,
		firecrackerBin: defaultFirecrackerBin,
		egress:         &hostEgress{},
		log:            slog.Default(),
		netEnabled:     true,
		poolSize:       defaultPoolSize,
		boxes:          map[string]*fcInstance{},
		used:           map[int]bool{},
	}
	p.procCtx, p.procCancel = context.WithCancel(context.Background())
	p.newMachine = p.realMachineFactory
	return p, nil
}

// SetNetworking enables or disables per-box egress networking. Disabled boots
// control-only boxes (loopback + vsock, no TAP/NAT), which also removes the
// CAP_NET_ADMIN requirement. It is enabled by default.
//
// @arg enabled Whether boxes get a TAP + NAT egress interface.
//
// @testcase TestConformanceFirecracker boots control-only boxes when networking is disabled.
func (p *Provisioner) SetNetworking(enabled bool) { p.netEnabled = enabled }

// SetPoolSize sets the number of egress TAP slots provisioned at startup, which
// caps concurrent networked boxes. A non-positive value keeps the default.
//
// @arg n The pool size; <= 0 keeps the default.
//
// @testcase TestProvisionerBookkeeping bounds box slots by the pool size.
func (p *Provisioner) SetPoolSize(n int) {
	if n > 0 {
		p.poolSize = n
	}
}

// SetPayloadImage sets an optional host path to a read-only ext4 carrying the
// guest agent (and claude + trust seed). When non-empty, every box boots with it
// attached as a shared read-only second drive (/dev/vdb) that the base rootfs's
// loader unit mounts; this decouples the fast-changing agent from the slow,
// multi-GiB base rootfs. Empty keeps the all-in-one rootfs (agent baked in).
//
// @arg path Host path to the payload ext4; empty disables the second drive.
//
// @testcase TestProvisionAttachesPayloadDrive attaches the payload as a read-only /dev/vdb.
// @testcase TestProvisionWithoutPayloadHasSingleDrive omits the second drive when unset.
func (p *Provisioner) SetPayloadImage(path string) { p.payloadImage = path }

// EnsureNetwork provisions the egress TAP pool once, so the host interfaces exist
// before any box is created (and before a same-host browser connects) rather than
// being churned per box. It is a no-op when networking is disabled, and safe to
// call repeatedly — the pool is created on the first call only. The server calls
// it at startup; Provision also calls it as a fallback.
//
// @arg ctx Context for the pool provisioning.
// @error error if the pool cannot be provisioned (e.g. missing CAP_NET_ADMIN).
//
// @testcase TestProvisionerBookkeeping provisions the pool before the first box.
func (p *Provisioner) EnsureNetwork(ctx context.Context) error {
	if !p.netEnabled {
		return nil
	}
	p.poolOnce.Do(func() {
		if p.poolErr = p.egress.EnsurePool(ctx, p.poolSize); p.poolErr == nil {
			p.poolUp = true
		}
	})
	return p.poolErr
}

// SetPerBoxLimits sets the per-box CPU/memory caps applied at boot. The MaxBoxes
// field is enforced by box.Manager, not here.
//
// @arg l The resource limits; only MemoryBytes/NanoCPUs are used.
//
// @testcase TestProvisionerBookkeeping applies limits to the machine config.
func (p *Provisioner) SetPerBoxLimits(l sandbox.Limits) { p.limits = l }

// SetNamespace pins this provisioner to a namespace so List/Find/Destroy only ever
// see the boxes it created, letting two spokes share a host. Empty is unscoped.
//
// @arg ns The namespace to scope to; empty leaves the provisioner unscoped.
//
// @testcase TestProvisionerNamespaceScoping hides boxes of another namespace.
func (p *Provisioner) SetNamespace(ns string) { p.namespace = ns }

// realMachineFactory boots a real Firecracker VM for cfg via the go SDK, pointing
// it at the configured firecracker binary.
//
// @arg _ The request context, deliberately ignored; the VM uses the provisioner's lifetime context.
// @arg cfg The Firecracker machine configuration.
// @return machine The started-able machine handle.
// @error error if the machine cannot be constructed.
//
// @testcase TestConformanceFirecracker boots real VMs through this factory.
// @testcase TestVMSurvivesRequestContextCancel checks the VM outlives the create request's context.
func (p *Provisioner) realMachineFactory(_ context.Context, cfg fcsdk.Config) (machine, error) {
	// Deliberately use the provisioner's lifetime context, NOT the passed (request)
	// context: exec.CommandContext kills the firecracker process when its context is
	// done, and the box must outlive the create request so later operations (submit
	// code, exec, logs) still reach its agent. StopVMM / Close stop it explicitly.
	logger := logrus.NewEntry(logrus.New())
	cmd := fcsdk.VMCommandBuilder{}.WithSocketPath(cfg.SocketPath).WithBin(p.firecrackerBin).Build(p.procCtx)
	return fcsdk.NewMachine(p.procCtx, cfg, fcsdk.WithProcessRunner(cmd), fcsdk.WithLogger(logger))
}

// Provision boots a new microVM box: it copies the rootfs, attaches the shared
// read-only payload drive when one is configured, assigns a pooled TAP for egress,
// boots the VM with the guest kernel and a vsock device, and waits for the guest
// agent to answer on vsock. The box is created in the pending phase.
//
// @arg ctx Context for the boot and the agent wait.
// @arg opts The caller-controlled image (rootfs), box ID, and description.
// @return box.Instance A handle to the booted box, in the pending phase.
// @error error if the box id is invalid, the kernel/rootfs is missing, the egress pool cannot be provisioned, or the VM cannot be prepared, booted, or its agent does not answer.
//
// @testcase TestProvisionerBookkeeping provisions a box with a fake machine and finds it back.
// @testcase TestProvisionCleansUpOnBootFailure cleans up files and the slot index when boot fails.
func (p *Provisioner) Provision(ctx context.Context, opts sandbox.CreateOptions) (box.Instance, error) {
	if opts.BoxID != "" && !sandbox.ValidBoxID(opts.BoxID) {
		return nil, fmt.Errorf("invalid box id %q", opts.BoxID)
	}
	rootfsSrc := opts.Image
	if rootfsSrc == "" {
		rootfsSrc = p.defaultRootfs
	}
	if p.kernelImage == "" || rootfsSrc == "" {
		return nil, fmt.Errorf("firecracker backend requires a kernel image and a rootfs image")
	}

	// Ensure the egress TAP pool exists (once). Provisioning it here as a fallback
	// is a no-op after the server did it at startup, so a box create never adds a
	// host interface — which is what keeps a same-host browser from aborting
	// requests with ERR_NETWORK_CHANGED.
	if err := p.EnsureNetwork(ctx); err != nil {
		return nil, fmt.Errorf("provisioning egress network: %w", err)
	}

	token, err := newToken()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	index, ok := p.allocIndexLocked()
	if !ok {
		p.mu.Unlock()
		return nil, fmt.Errorf("no free box slot (max %d concurrent microVM boxes)", p.poolSize)
	}
	p.mu.Unlock()

	dir := boxDir(p.stateDir, token)
	n := netFor(index)
	vsockUDS := filepath.Join(dir, "vsock.sock")
	apiSock := filepath.Join(dir, "fc.sock")
	perBoxRootfs := filepath.Join(dir, "rootfs.ext4")

	// cleanup undoes partial provisioning in reverse order. The TAP is pooled (not
	// per box), so it is freed via the slot index, not torn down here.
	cleanup := func(m machine) {
		if m != nil {
			_ = m.StopVMM()
		}
		_ = os.RemoveAll(dir)
		p.mu.Lock()
		delete(p.used, index)
		p.mu.Unlock()
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		cleanup(nil)
		return nil, fmt.Errorf("creating box dir: %w", err)
	}
	if err := copyFile(rootfsSrc, perBoxRootfs); err != nil {
		cleanup(nil)
		return nil, fmt.Errorf("copying rootfs: %w", err)
	}

	// Kernel args: the guest gets a static eth0 (via the ip= arg, on its pooled TAP)
	// only when egress networking is enabled; a control-only box boots with just
	// loopback and vsock. net.ifnames=0 keeps the NIC named eth0 (so the ip= arg
	// and a systemd guest's network config agree); init=/init lets a rootfs point
	// /init at its real init (systemd, or the agent directly).
	kernelArgs := "console=ttyS0 reboot=k panic=1 pci=off net.ifnames=0 init=/init"
	if p.netEnabled {
		kernelArgs += " " + n.kernelIPArg()
	}

	// The root drive is a per-box writable copy of the rootfs. When a payload image
	// is configured, it rides along as a second, read-only drive (/dev/vdb) shared
	// unchanged across every box — never copied — which is what lets the agent be
	// swapped without rebuilding the base rootfs and keeps concurrent boxes sharing
	// one on-disk payload.
	drives := []models.Drive{{
		DriveID:      fcsdk.String("rootfs"),
		PathOnHost:   fcsdk.String(perBoxRootfs),
		IsRootDevice: fcsdk.Bool(true),
		IsReadOnly:   fcsdk.Bool(false),
	}}
	if p.payloadImage != "" {
		drives = append(drives, models.Drive{
			DriveID:      fcsdk.String("payload"),
			PathOnHost:   fcsdk.String(p.payloadImage),
			IsRootDevice: fcsdk.Bool(false),
			IsReadOnly:   fcsdk.Bool(true),
		})
	}

	cfg := fcsdk.Config{
		SocketPath:      apiSock,
		KernelImagePath: p.kernelImage,
		KernelArgs:      kernelArgs,
		Drives:          drives,
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  fcsdk.Int64(p.vcpuCount()),
			MemSizeMib: fcsdk.Int64(p.memSizeMib()),
			Smt:        fcsdk.Bool(false),
		},
		VsockDevices: []fcsdk.VsockDevice{{ID: "vsock0", CID: 3, Path: vsockUDS}},
	}
	if p.netEnabled {
		cfg.NetworkInterfaces = fcsdk.NetworkInterfaces{{
			StaticConfiguration: &fcsdk.StaticNetworkConfiguration{
				MacAddress:  macForIndex(index),
				HostDevName: n.TapName,
			},
		}}
	}

	m, err := p.newMachine(ctx, cfg)
	if err != nil {
		cleanup(nil)
		return nil, fmt.Errorf("creating microVM: %w", err)
	}
	// Start on the provisioner's lifetime context, not the request context: the SDK
	// spawns a goroutine that calls StopVMM when the Start context is done, so
	// starting on the request context would kill the VM the moment create returns.
	// The agent wait below still honours the request context for its own timeout.
	if err := m.Start(p.procCtx); err != nil {
		cleanup(m)
		return nil, fmt.Errorf("starting microVM: %w", err)
	}
	if err := waitForAgent(ctx, vsockUDS, agentVsockPort, bootWait); err != nil {
		cleanup(m)
		return nil, fmt.Errorf("waiting for box agent: %w", err)
	}

	meta := boxMeta{
		Token: token, BoxID: opts.BoxID, Description: opts.Description,
		Image: rootfsSrc, Phase: "pending", Created: time.Now().Unix(),
		NetIndex: index, Namespace: p.namespace,
	}
	if err := meta.save(p.stateDir); err != nil {
		cleanup(m)
		return nil, fmt.Errorf("saving box meta: %w", err)
	}
	inst := &fcInstance{prov: p, meta: meta, vsockUDS: vsockUDS, net: n, machine: m}

	p.mu.Lock()
	p.boxes[token] = inst
	p.mu.Unlock()
	return inst, nil
}

// List returns a handle to every managed box in this provisioner's namespace.
//
// @arg ctx Unused; present to satisfy the interface.
// @return []box.Instance One handle per managed box.
// @error error is always nil.
//
// @testcase TestProvisionerBookkeeping lists the provisioned boxes.
// @testcase TestProvisionerNamespaceScoping lists only the namespace's boxes.
func (p *Provisioner) List(ctx context.Context) ([]box.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]box.Instance, 0, len(p.boxes))
	for _, inst := range p.boxes {
		if p.namespace != "" && inst.meta.Namespace != p.namespace {
			continue
		}
		out = append(out, inst)
	}
	return out, nil
}

// Find resolves an instance ID (token), box ID, or instance name to the single
// managed box it identifies.
//
// @arg ctx Context passed to the underlying List.
// @arg idOrName The token, box ID, or name to resolve.
// @return box.Instance The matched box.
// @error error wrapping sandbox.ErrBoxNotFound if no managed box matches.
//
// @testcase TestProvisionerBookkeeping finds a box by token and by box id.
// @testcase TestFindUnknownBox errors when no managed box matches.
func (p *Provisioner) Find(ctx context.Context, idOrName string) (box.Instance, error) {
	insts, _ := p.List(ctx)
	for _, inst := range insts {
		b := inst.Meta()
		if b.InstanceID == idOrName ||
			(b.BoxID != "" && b.BoxID == idOrName) ||
			b.Name == idOrName ||
			strings.HasPrefix(b.InstanceID, idOrName) ||
			b.Name == namePrefix("pending")+idOrName ||
			b.Name == namePrefix("ready")+idOrName {
			return inst, nil
		}
	}
	return nil, fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, idOrName)
}

// Close stops every running box VM and releases their resources. It is called when
// the provisioner is shut down.
//
// @error error is always nil; teardown is best-effort and logged.
//
// @testcase TestProvisionerBookkeeping closes the provisioner after use.
func (p *Provisioner) Close() error {
	p.mu.Lock()
	insts := make([]*fcInstance, 0, len(p.boxes))
	for _, inst := range p.boxes {
		insts = append(insts, inst)
	}
	p.mu.Unlock()
	for _, inst := range insts {
		if err := inst.Destroy(context.Background()); err != nil {
			p.log.Warn("closing firecracker box", "box", inst.meta.Token, "err", err)
		}
	}
	// Tear down the egress TAP pool if it was provisioned.
	if p.poolUp {
		_ = p.egress.TeardownPool(context.Background(), p.poolSize)
	}
	// Cancel the process context as a backstop, killing any VM whose StopVMM was
	// missed above.
	if p.procCancel != nil {
		p.procCancel()
	}
	return nil
}

// allocIndexLocked reserves the lowest free pool slot (bounded by the pool size).
// The caller must hold p.mu.
//
// @return int The reserved slot index.
// @return bool False when every slot is in use.
//
// @testcase TestProvisionerBookkeeping allocates and frees slots across boxes.
func (p *Provisioner) allocIndexLocked() (int, bool) {
	for i := 0; i < p.poolSize; i++ {
		if !p.used[i] {
			p.used[i] = true
			return i, true
		}
	}
	return 0, false
}

// vcpuCount derives the guest vCPU count from the CPU limit, honouring
// Firecracker's rule that the count is 1 or an even number.
//
// @return int64 The vCPU count to boot the VM with.
//
// @testcase TestMachineSizing rounds CPU limits to a valid vCPU count.
func (p *Provisioner) vcpuCount() int64 {
	if p.limits.NanoCPUs <= 0 {
		return defaultVcpuCount
	}
	n := int64(math.Ceil(float64(p.limits.NanoCPUs) / 1e9))
	if n < 1 {
		n = 1
	}
	if n > 1 && n%2 == 1 {
		n++
	}
	return n
}

// memSizeMib derives the guest memory size in MiB from the memory limit, floored
// so a VM always has enough to boot.
//
// @return int64 The guest memory size in MiB.
//
// @testcase TestMachineSizing floors and converts memory limits.
func (p *Provisioner) memSizeMib() int64 {
	if p.limits.MemoryBytes <= 0 {
		return defaultMemSizeMib
	}
	mib := p.limits.MemoryBytes / (1 << 20)
	if mib < minMemSizeMib {
		mib = minMemSizeMib
	}
	return mib
}

// newToken returns a random 12-hex-char box token (instance ID and state
// subdirectory name).
//
// @return string A 12-char random hex token.
// @error error if the system random source fails.
//
// @testcase TestProvisionerBookkeeping derives box tokens from this.
func newToken() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating box token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// macForIndex derives a deterministic locally-administered MAC for a box slot, so
// each box's guest NIC has a distinct, stable address.
//
// @arg index The per-box slot.
// @return string A MAC address string.
//
// @testcase TestMacForIndex derives distinct MACs per index.
func macForIndex(index int) string {
	return fmt.Sprintf("AA:FC:00:00:%02X:%02X", (index>>8)&0xFF, index&0xFF)
}

// copyFile copies src to dst (creating or truncating dst). It is used to give each
// box its own writable rootfs from the shared image.
//
// @arg src The source file path.
// @arg dst The destination file path.
// @error error if src cannot be read or dst cannot be written.
//
// @testcase TestCopyFile copies bytes from a source to a destination.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// waitForAgent polls the box's vsock until the guest agent accepts a CONNECT (it is
// listening) or the timeout elapses. A successful probe is closed immediately; the
// caller opens its own control connections afterwards.
//
// @arg ctx Context whose cancellation aborts the wait.
// @arg udsPath The box's Firecracker vsock Unix-socket path.
// @arg port The guest agent's AF_VSOCK port.
// @arg timeout How long to wait before giving up.
// @error error if the agent does not answer before the timeout or ctx is cancelled.
//
// @testcase TestWaitForAgent succeeds once a fake vsock accepts the CONNECT.
func waitForAgent(ctx context.Context, udsPath string, port uint32, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		probeCtx, cancel := context.WithTimeout(ctx, bootPoll)
		conn, err := dialVsock(probeCtx, udsPath, port)
		cancel()
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("box agent did not answer on vsock within %s: %w", timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(bootPoll):
		}
	}
}

// fcInstance is a handle to one managed Firecracker box.
type fcInstance struct {
	prov     *Provisioner
	meta     boxMeta
	vsockUDS string
	net      boxNet
	machine  machine
}

// Meta returns the box's neutral view. A box with a live machine handle is
// running; one reloaded from disk without a handle is reported stopped.
//
// @return sandbox.Box The box's ID, name, phase, and other fields.
//
// @testcase TestProvisionerBookkeeping reads box metadata via Meta.
func (i *fcInstance) Meta() sandbox.Box {
	state := "stopped"
	if i.machine != nil {
		state = "running"
	}
	return i.meta.toBox(state)
}

// Control opens a control connection to the box's agent over the VM's vsock,
// performing Firecracker's CONNECT handshake.
//
// @arg ctx Context for the dial and handshake.
// @return net.Conn A control connection to the box's agent.
// @error error if the vsock cannot be dialled or the handshake fails.
//
// @testcase TestConformanceFirecracker drives the agent through Control.
func (i *fcInstance) Control(ctx context.Context) (net.Conn, error) {
	return dialVsock(ctx, i.vsockUDS, agentVsockPort)
}

// MarkReady moves the box from the pending phase to ready and persists the change,
// so the orphan reaper spares it and the phase survives a restart.
//
// @arg ctx Unused; present to satisfy the interface.
// @error error if the phase change cannot be persisted.
//
// @testcase TestProvisionerBookkeeping marks a box ready and sees the phase change.
func (i *fcInstance) MarkReady(ctx context.Context) error {
	i.prov.mu.Lock()
	i.meta.Phase = "ready"
	if live, ok := i.prov.boxes[i.meta.Token]; ok {
		live.meta.Phase = "ready"
	}
	meta := i.meta
	i.prov.mu.Unlock()
	if err := meta.save(i.prov.stateDir); err != nil {
		return fmt.Errorf("marking box %s ready: %w", i.meta.Token, err)
	}
	return nil
}

// Destroy stops the box's VM, removes its state directory, and frees its pool slot
// (the pooled TAP stays up for reuse). Destroying an already-gone box returns a
// wrapped sandbox.ErrBoxNotFound.
//
// @arg ctx Unused; present to satisfy the interface.
// @error error wrapping sandbox.ErrBoxNotFound if the box is already gone.
//
// @testcase TestProvisionerBookkeeping destroys a box and no longer finds it.
// @testcase TestDestroyAlreadyGone reports ErrBoxNotFound for an unknown box.
func (i *fcInstance) Destroy(ctx context.Context) error {
	p := i.prov
	p.mu.Lock()
	_, present := p.boxes[i.meta.Token]
	delete(p.boxes, i.meta.Token)
	delete(p.used, i.meta.NetIndex)
	p.mu.Unlock()

	if i.machine != nil {
		_ = i.machine.StopVMM()
	}
	_ = os.RemoveAll(boxDir(p.stateDir, i.meta.Token))

	if !present {
		return fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, i.meta.Token)
	}
	return nil
}
