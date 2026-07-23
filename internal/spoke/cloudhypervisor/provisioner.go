package cloudhypervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"sync"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/box"
	"github.com/clems4ever/llmbox/internal/spoke/microvm/mvmnet"
)

const (
	// defaultStateDir is where per-box runtime files and metadata live when the spoke
	// configures none.
	defaultStateDir = "/run/llmbox/cloud-hypervisor"

	// defaultVCPUs and defaultMemSizeMib size a box whose per-box limits leave the
	// value unset.
	defaultVCPUs      = 1
	defaultMemSizeMib = 512
	// minMemSizeMib floors the derived memory so a tiny limit can't produce a VM too
	// small to boot.
	minMemSizeMib = 128

	// defaultPoolSize is the number of egress TAP slots provisioned at startup when
	// the spoke configures none; it caps concurrent networked boxes.
	defaultPoolSize = 16
	// defaultTapGroup owns the pooled TAP devices by default, matching the Firecracker
	// backend's fc-net group so a shared host's operator manages one group.
	defaultTapGroup = 100000
)

// Provisioner runs boxes as Cloud Hypervisor microVMs. It owns box identity,
// persistence, and bookkeeping; the VMM mechanics live behind the launcher seam, so
// the same lifecycle code is exercised in CI against a fake launcher and, on a KVM
// host, against real Cloud Hypervisor. It has no daemon to remember boxes, so it
// persists per-box metadata under stateDir and reloads it on startup.
type Provisioner struct {
	kernelImage   string
	defaultRootfs string
	stateDir      string
	namespace     string
	limits        sandbox.Limits
	// gpus holds the host PCI addresses passed through to every box by VFIO; mdevs
	// holds mediated-device refs (vGPU / MIG-backed vGPU). Both are machine-local, so
	// they are attached to every box this spoke runs.
	gpus     []string
	mdevs    []string
	launcher launcher
	log      *slog.Logger

	// egress provisions and validates the shared TAP/NAT pool that gives boxes
	// outbound connectivity; egressMode selects who owns it (managed/external/disabled)
	// and poolSize/tapGroup size and own it. poolOnce guards one-time provisioning so
	// the host interfaces are readied once at startup, not per box.
	egress     mvmnet.Egress
	egressMode egressMode
	poolSize   int
	tapGroup   int
	poolOnce   sync.Once
	poolErr    error

	mu    sync.Mutex
	boxes map[string]*chInstance
	// used tracks the pooled network slots held by live boxes, so a slot is never
	// assigned to two boxes and is freed on Destroy.
	used map[int]bool
}

// NewProvisioner builds a Cloud Hypervisor provisioner. kernelImage and
// defaultRootfs are host paths to the guest kernel and the default rootfs image; an
// empty stateDir falls back to defaultStateDir. It wires the real Cloud Hypervisor
// launcher and rehydrates any boxes a previous spoke run left under stateDir so
// List/Find/Destroy survive a restart.
//
// @arg kernelImage Host path to the guest kernel every box boots.
// @arg defaultRootfs Host path to the rootfs image booted when a create supplies none.
// @arg stateDir Directory for per-box runtime files and metadata; empty uses defaultStateDir.
// @return *Provisioner A provisioner wired to the real launcher.
// @error error if persisted box metadata exists but cannot be read.
//
// @testcase TestNewBackendConfiguresProvisioner builds a provisioner through the factory.
func NewProvisioner(kernelImage, defaultRootfs, stateDir string) (*Provisioner, error) {
	if stateDir == "" {
		stateDir = defaultStateDir
	}
	p := &Provisioner{
		kernelImage:   kernelImage,
		defaultRootfs: defaultRootfs,
		stateDir:      stateDir,
		launcher:      newCHLauncher(""),
		log:           slog.Default(),
		egress:        mvmnet.NewHostEgress(chNet),
		egressMode:    egressManaged,
		poolSize:      defaultPoolSize,
		tapGroup:      defaultTapGroup,
		boxes:         map[string]*chInstance{},
		used:          map[int]bool{},
	}
	if err := p.rehydrate(); err != nil {
		return nil, err
	}
	return p, nil
}

// SetEgressMode selects who owns the host-side TAP/NAT egress plumbing: managed (the
// spoke provisions it, needs CAP_NET_ADMIN/root), external (attach to a
// pre-provisioned pool, spoke never mutates host networking), or disabled
// (control-only, no egress NIC).
//
// @arg mode The egress mode.
//
// @testcase TestEnsureNetworkModes drives each mode through EnsureNetwork.
func (p *Provisioner) SetEgressMode(mode egressMode) { p.egressMode = mode }

// SetPoolSize sets the number of egress TAP slots provisioned at startup, capping
// concurrent networked boxes; 0 keeps the default.
//
// @arg n The pool size.
//
// @testcase TestNewBackendConfiguresProvisioner applies the pool size through the factory.
func (p *Provisioner) SetPoolSize(n int) {
	if n > 0 {
		p.poolSize = n
	}
}

// SetTapGroup sets the GID that owns the pooled TAP devices; 0 keeps the default.
//
// @arg gid The owning GID.
//
// @testcase TestNewBackendConfiguresProvisioner applies the tap group through the factory.
func (p *Provisioner) SetTapGroup(gid int) {
	if gid > 0 {
		p.tapGroup = gid
	}
}

// SetEgress overrides the egress implementation, for tests that inject a fake instead
// of touching the host network stack.
//
// @arg e The egress implementation.
//
// @testcase TestEnsureNetworkModes injects a fake egress through this setter.
func (p *Provisioner) SetEgress(e mvmnet.Egress) { p.egress = e }

// guestNetEnabled reports whether boxes get an egress NIC (every mode except
// disabled).
//
// @return bool True when boxes get a TAP-backed egress interface.
//
// @testcase TestEnsureNetworkModes checks disabled mode gives no egress.
func (p *Provisioner) guestNetEnabled() bool { return p.egressMode != egressDisabled }

// EnsureNetwork readies the egress TAP pool once, so the host interfaces exist before
// any box is created. It is a no-op when egress is disabled, provisions the pool in
// managed mode (needs CAP_NET_ADMIN), and only validates a pre-provisioned pool in
// external mode (never mutating host networking). Safe to call repeatedly.
//
// @arg ctx Context for the pool provisioning/validation.
// @error error if the pool cannot be provisioned (managed) or is incomplete (external).
//
// @testcase TestEnsureNetworkModes provisions, validates, or skips per mode.
func (p *Provisioner) EnsureNetwork(ctx context.Context) error {
	switch p.egressMode {
	case egressDisabled:
		return nil
	case egressExternal:
		p.poolOnce.Do(func() { p.poolErr = p.egress.ValidatePool(ctx, p.poolSize) })
	default: // managed
		p.poolOnce.Do(func() {
			if he, ok := p.egress.(*mvmnet.HostEgress); ok {
				he.SetTapGroup(p.tapGroup)
			}
			p.poolErr = p.egress.EnsurePool(ctx, p.poolSize)
		})
	}
	return p.poolErr
}

// allocIndexLocked reserves a free pooled network slot, returning false when the pool
// is exhausted. The caller must hold p.mu.
//
// @return int The reserved slot.
// @return bool False when no slot is free.
//
// @testcase TestConformanceCloudHypervisorFake allocates slots for networked boxes.
func (p *Provisioner) allocIndexLocked() (int, bool) {
	for i := 0; i < p.poolSize; i++ {
		if !p.used[i] {
			p.used[i] = true
			return i, true
		}
	}
	return 0, false
}

// SetPerBoxLimits sets the per-box resource caps the provisioner derives vCPU/memory/
// disk sizing from.
//
// @arg l The per-box limits.
//
// @testcase TestNewBackendConfiguresProvisioner applies limits through the factory.
func (p *Provisioner) SetPerBoxLimits(l sandbox.Limits) { p.limits = l }

// SetNamespace scopes the provisioner to the boxes it created, so two spokes sharing
// a host never list, reap, or destroy each other's boxes.
//
// @arg ns The provisioner namespace.
//
// @testcase TestNewBackendConfiguresProvisioner applies the namespace through the factory.
func (p *Provisioner) SetNamespace(ns string) { p.namespace = ns }

// SetGPUs sets the host PCI addresses passed through to every box by VFIO.
//
// @arg addrs The GPU PCI addresses.
//
// @testcase TestNewBackendConfiguresProvisioner applies GPU passthrough through the factory.
func (p *Provisioner) SetGPUs(addrs []string) { p.gpus = addrs }

// SetMDEVs sets the mediated-device refs (vGPU / MIG-backed vGPU) passed through to
// every box.
//
// @arg mdevs The mdev UUIDs or /sys paths.
//
// @testcase TestNewBackendConfiguresProvisioner applies mdev passthrough through the factory.
func (p *Provisioner) SetMDEVs(mdevs []string) { p.mdevs = mdevs }

// SetCHBinary points the provisioner at a specific cloud-hypervisor executable;
// empty resolves it from PATH.
//
// @arg bin The cloud-hypervisor path or PATH name.
//
// @testcase TestNewBackendConfiguresProvisioner applies the binary path through the factory.
func (p *Provisioner) SetCHBinary(bin string) { p.launcher = newCHLauncher(bin) }

// rehydrate reloads boxes persisted by a previous spoke run, probing each one's VMM
// so a box whose orphaned VMM is still running is reported live and reachable, while
// a dead one is kept only for List/Destroy.
//
// @error error if persisted metadata exists but cannot be read.
//
// @testcase TestNewBackendConfiguresProvisioner rehydrates an empty state dir on construction.
func (p *Provisioner) rehydrate() error {
	metas, err := loadMetas(p.stateDir)
	if err != nil {
		return err
	}
	for _, m := range metas {
		if p.namespace != "" && m.Namespace != p.namespace {
			continue
		}
		inst := &chInstance{prov: p, meta: m}
		if !m.Paused {
			inst.alive = p.launcher.Alive(m.apiSockPath(p.stateDir))
		}
		p.mu.Lock()
		p.boxes[m.Token] = inst
		// Re-reserve the box's pooled network slot so a fresh box never reuses a live
		// box's TAP/IP.
		if m.Egress {
			p.used[m.NetIndex] = true
		}
		p.mu.Unlock()
	}
	return nil
}

// Provision creates a new box (in the pending auth phase): it allocates a token,
// persists the box's identity and requested GPUs, and boots its VM through the
// launcher. On any failure it removes the box's state so nothing leaks.
//
// @arg ctx Context bounding the boot.
// @arg opts The caller-controlled create inputs (box id, description, disk size).
// @return box.Instance A handle to the new box.
// @error error if the box id is invalid, the kernel/rootfs are unset, or the VM cannot be booted.
//
// @testcase TestConformanceCloudHypervisor provisions every box through this method.
func (p *Provisioner) Provision(ctx context.Context, opts sandbox.CreateOptions) (box.Instance, error) {
	if opts.BoxID != "" && !sandbox.ValidBoxID(opts.BoxID) {
		return nil, fmt.Errorf("invalid box id %q", opts.BoxID)
	}
	if p.kernelImage == "" || p.defaultRootfs == "" {
		return nil, fmt.Errorf("cloud-hypervisor backend requires a kernel image and a rootfs image")
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	meta := boxMeta{
		Token: token, BoxID: opts.BoxID, Description: opts.Description,
		Image: p.defaultRootfs, Phase: "pending", Created: time.Now().Unix(),
		DiskBytes: p.diskBytesFor(opts.DiskBytes), GPUs: p.gpus, MDEVs: p.mdevs, Namespace: p.namespace,
	}

	// Give the box an egress NIC unless egress is disabled: ready the shared TAP pool
	// (once) and reserve a slot, whose TAP/IP the box keeps for its lifetime. A
	// control-only box (disabled) skips all of this.
	if p.guestNetEnabled() {
		if err := p.EnsureNetwork(ctx); err != nil {
			return nil, fmt.Errorf("provisioning egress network: %w", err)
		}
		p.mu.Lock()
		index, ok := p.allocIndexLocked()
		p.mu.Unlock()
		if !ok {
			return nil, fmt.Errorf("no free box slot (max %d concurrent networked microVM boxes)", p.poolSize)
		}
		meta.Egress = true
		meta.NetIndex = index
	}
	freeSlot := func() {
		if meta.Egress {
			p.mu.Lock()
			delete(p.used, meta.NetIndex)
			p.mu.Unlock()
		}
	}

	handle, err := p.launcher.Launch(ctx, p.specFor(meta))
	if err != nil {
		freeSlot()
		_ = os.RemoveAll(boxDir(p.stateDir, token))
		return nil, err
	}
	if err := meta.save(p.stateDir); err != nil {
		_ = handle.Stop()
		freeSlot()
		_ = os.RemoveAll(boxDir(p.stateDir, token))
		return nil, fmt.Errorf("saving box meta: %w", err)
	}
	inst := &chInstance{prov: p, meta: meta, handle: handle}
	p.mu.Lock()
	p.boxes[token] = inst
	p.mu.Unlock()
	return inst, nil
}

// specFor builds the launcher vmSpec for a box from its metadata and the spoke's
// kernel/rootfs and sizing.
//
// @arg meta The box's persisted identity.
// @return vmSpec The launch spec for the box's VM.
//
// @testcase TestConformanceCloudHypervisor launches boxes from specs built here.
func (p *Provisioner) specFor(meta boxMeta) vmSpec {
	spec := vmSpec{
		Token:       meta.Token,
		BoxDir:      boxDir(p.stateDir, meta.Token),
		Kernel:      p.kernelImage,
		RootfsSrc:   p.defaultRootfs,
		APISock:     meta.apiSockPath(p.stateDir),
		VsockUDS:    meta.vsockUDSPath(p.stateDir),
		DiskBytes:   meta.DiskBytes,
		VCPUs:       p.vcpuCount(),
		MemoryBytes: p.memSizeMib() * (1 << 20),
		GPUs:        meta.GPUs,
		MDEVs:       meta.MDEVs,
	}
	// A networked box gets a virtio-net device on its pooled TAP with a deterministic
	// MAC, and its guest IP set statically via the kernel ip= arg — the same scheme the
	// Firecracker backend uses, so the same guest rootfs configures either.
	if meta.Egress {
		n := chNet.NetFor(meta.NetIndex)
		spec.TapName = n.TapName
		spec.MAC = mvmnet.MACForIndex(meta.NetIndex)
		spec.IPArg = n.KernelIPArg()
	}
	return spec
}

// List returns a handle to every managed box.
//
// @arg ctx Unused; present to satisfy the interface.
// @return []box.Instance One handle per managed box.
// @error error Always nil.
//
// @testcase TestConformanceCloudHypervisor lists boxes through this method.
func (p *Provisioner) List(ctx context.Context) ([]box.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]box.Instance, 0, len(p.boxes))
	for _, inst := range p.boxes {
		out = append(out, inst)
	}
	return out, nil
}

// Find resolves a box handle — its token (InstanceID) or its caller-assigned box id
// — to the single box it identifies, returning a wrapped sandbox.ErrBoxNotFound when
// none matches.
//
// @arg ctx Unused; present to satisfy the interface.
// @arg idOrName The token or box id to resolve.
// @return box.Instance The matched box.
// @error error Wrapped sandbox.ErrBoxNotFound when no box matches.
//
// @testcase TestConformanceCloudHypervisor resolves boxes and rejects unknown ones through Find.
func (p *Provisioner) Find(ctx context.Context, idOrName string) (box.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if inst, ok := p.boxes[idOrName]; ok {
		return inst, nil
	}
	for _, inst := range p.boxes {
		if inst.meta.BoxID != "" && inst.meta.BoxID == idOrName {
			return inst, nil
		}
	}
	return nil, fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, idOrName)
}

// Close releases the provisioner without stopping boxes: like the Firecracker
// backend, box VMMs deliberately outlive the spoke so a respawned spoke rehydrates
// them. It holds no host resources of its own, so there is nothing to release.
//
// @error error Always nil.
//
// @testcase TestNewBackendConfiguresProvisioner closes a freshly built provisioner.
func (p *Provisioner) Close() error { return nil }

// diskBytesFor resolves a box's writable-disk size from the create request and the
// spoke's limits. The launcher floors it to the base rootfs size (it only ever grows
// the copy), so a value below the image size simply keeps the image size.
//
// @arg requested The create request's DiskBytes (0 = use the default).
// @return int64 The resolved disk size in bytes (0 = no explicit size; use the image size).
//
// @testcase TestDiskBytesFor resolves and clamps the disk size.
func (p *Provisioner) diskBytesFor(requested int64) int64 {
	want := requested
	if want <= 0 {
		want = p.limits.DiskBytes
	}
	if p.limits.MaxDiskBytes > 0 && want > p.limits.MaxDiskBytes {
		want = p.limits.MaxDiskBytes
	}
	return want
}

// vcpuCount derives the guest vCPU count from the CPU limit (rounded up, at least 1).
//
// @return int64 The vCPU count to boot the VM with.
//
// @testcase TestMachineSizing rounds CPU limits to a vCPU count.
func (p *Provisioner) vcpuCount() int64 {
	if p.limits.NanoCPUs <= 0 {
		return defaultVCPUs
	}
	n := int64(math.Ceil(float64(p.limits.NanoCPUs) / 1e9))
	if n < 1 {
		n = 1
	}
	return n
}

// memSizeMib derives the guest memory size in MiB from the memory limit, floored so a
// VM always has enough to boot.
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

// newToken returns a random 12-hex-char box token (instance id and state
// subdirectory name).
//
// @return string A 12-char random hex token.
// @error error if the system random source fails.
//
// @testcase TestConformanceCloudHypervisor derives box tokens from this.
func newToken() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating box token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// chInstance is a handle to one Cloud Hypervisor box.
type chInstance struct {
	prov *Provisioner
	meta boxMeta
	// handle is the live VMM handle; nil when the box is paused or was rehydrated
	// from a previous spoke run (its VMM has no in-process handle).
	handle vmHandle
	// alive records that a rehydrated box's orphaned VMM answered the liveness probe.
	alive bool
}

// Meta returns the box's neutral view. A box with a live handle is running, as is a
// rehydrated box whose orphaned VMM answered the probe; a paused box reports paused;
// anything else is stopped.
//
// @return sandbox.Box The box's ID, name, phase, and other fields.
//
// @testcase TestConformanceCloudHypervisor reads box metadata via Meta.
func (i *chInstance) Meta() sandbox.Box {
	state := "stopped"
	switch {
	case i.meta.Paused:
		state = sandbox.StatePaused
	case i.handle != nil || i.alive:
		state = "running"
	}
	return i.meta.toBox(state)
}

// Control opens a control connection to the box's guest over its vsock.
//
// @arg ctx Context for the dial and handshake.
// @return net.Conn A control connection to the box's guest.
// @error error if the vsock cannot be dialled or the handshake fails.
//
// @testcase TestConformanceCloudHypervisor drives the guest through Control.
func (i *chInstance) Control(ctx context.Context) (net.Conn, error) {
	return i.prov.launcher.Dial(ctx, i.meta.vsockUDSPath(i.prov.stateDir))
}

// MarkReady moves the box from the pending phase to ready and persists the change, so
// the orphan reaper spares it and the phase survives a restart.
//
// @arg ctx Unused; present to satisfy the interface.
// @error error if the phase change cannot be persisted.
//
// @testcase TestConformanceCloudHypervisor marks a box ready after login.
func (i *chInstance) MarkReady(ctx context.Context) error {
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

// Pause stops the box's VMM to free compute while keeping its rootfs (auth,
// workspace) and metadata, so Resume can boot it back from the kept disk. Pausing an
// already-gone box returns a wrapped sandbox.ErrBoxNotFound.
//
// @arg ctx Unused; present to satisfy the interface.
// @error error wrapping sandbox.ErrBoxNotFound if the box is already gone, or if the paused state cannot be persisted.
//
// @testcase TestConformanceCloudHypervisor pauses a box and reports it paused.
func (i *chInstance) Pause(ctx context.Context) error {
	p := i.prov
	p.mu.Lock()
	_, present := p.boxes[i.meta.Token]
	p.mu.Unlock()
	if !present {
		return fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, i.meta.Token)
	}
	i.stopCompute()
	p.mu.Lock()
	i.handle = nil
	i.alive = false
	i.meta.Paused = true
	meta := i.meta
	p.mu.Unlock()
	if err := meta.save(p.stateDir); err != nil {
		return fmt.Errorf("persisting paused box %s: %w", i.meta.Token, err)
	}
	return nil
}

// Resume boots a paused box's VM back up from its kept rootfs and re-establishes its
// control channel. The paused state is cleared only after a successful boot, so a
// resume that fails leaves the box paused and retryable. Resuming an already-gone box
// returns a wrapped sandbox.ErrBoxNotFound.
//
// @arg ctx Context bounding the boot.
// @error error wrapping sandbox.ErrBoxNotFound if the box is already gone, or if the VM cannot be booted or its state persisted.
//
// @testcase TestConformanceCloudHypervisor resumes a paused box and reports it running.
func (i *chInstance) Resume(ctx context.Context) error {
	p := i.prov
	p.mu.Lock()
	_, present := p.boxes[i.meta.Token]
	p.mu.Unlock()
	if !present {
		return fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, i.meta.Token)
	}
	handle, err := p.launcher.Launch(ctx, p.specFor(i.meta))
	if err != nil {
		return fmt.Errorf("resuming box %s: %w", i.meta.Token, err)
	}
	p.mu.Lock()
	i.handle = handle
	i.alive = false
	i.meta.Paused = false
	meta := i.meta
	p.mu.Unlock()
	if err := meta.save(p.stateDir); err != nil {
		return fmt.Errorf("persisting resumed box %s: %w", i.meta.Token, err)
	}
	return nil
}

// Destroy stops the box's VM, removes its state directory, and deregisters it. A
// rehydrated box whose orphaned VMM survived is halted best-effort so the rootfs is
// not removed under a running VM. Destroying an already-gone box returns a wrapped
// sandbox.ErrBoxNotFound.
//
// @arg ctx Unused; present to satisfy the interface.
// @error error wrapping sandbox.ErrBoxNotFound if the box is already gone.
//
// @testcase TestConformanceCloudHypervisor destroys boxes and is idempotent through Destroy.
func (i *chInstance) Destroy(ctx context.Context) error {
	p := i.prov
	p.mu.Lock()
	_, present := p.boxes[i.meta.Token]
	delete(p.boxes, i.meta.Token)
	// Free the box's pooled network slot for reuse (the pooled TAP itself stays up).
	if i.meta.Egress {
		delete(p.used, i.meta.NetIndex)
	}
	p.mu.Unlock()

	i.stopCompute()
	_ = os.RemoveAll(boxDir(p.stateDir, i.meta.Token))

	if !present {
		return fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, i.meta.Token)
	}
	return nil
}

// stopCompute stops the box's VM: a live handle is stopped directly; a rehydrated
// orphan (no handle) is halted best-effort over its API socket, so the rootfs is
// never touched under a running VM. Callers must not hold prov.mu.
//
// @testcase TestConformanceCloudHypervisor stops a box's compute via Pause and Destroy.
func (i *chInstance) stopCompute() {
	switch {
	case i.handle != nil:
		_ = i.handle.Stop()
	case i.alive:
		if err := i.prov.launcher.Halt(i.meta.apiSockPath(i.prov.stateDir)); err != nil {
			i.prov.log.Warn("failed to halt orphaned box VMM", "box", i.meta.Token, "err", err)
		}
	}
}
