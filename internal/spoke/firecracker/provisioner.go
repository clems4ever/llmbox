// Package firecracker implements a box.Provisioner backed by Firecracker microVMs.
// Each box is a microVM booting a shared guest kernel and a per-box copy of a
// rootfs whose init is the llmbox guest listening on AF_VSOCK. The host
// reaches the guest over the VM's vsock (control and port-proxy), exactly as the
// Docker backend reaches its guest over a bind-mounted Unix socket — so all box
// behaviour (init, exec, dialing) runs through the guest, not through the
// provisioner. Guest outbound traffic (egress) goes through a per-box TAP device
// the host NATs; the guest is never in the egress path.
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

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/box"
	"github.com/clems4ever/llmbox/internal/spoke/boxapi"
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

	// bootWait bounds how long Provision waits for the guest to answer on
	// vsock after the VM starts.
	bootWait = 45 * time.Second
	// bootPoll is the interval between guest-reachability probes during bootWait.
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
// box's guest over the VM's vsock. It implements box.Provisioner (plus Close).
type Provisioner struct {
	kernelImage   string
	defaultRootfs string
	// payloadImage is an optional host path to a small read-only ext4 carrying the
	// guest binary and entrypoint. When set, every box
	// attaches it as a second, shared, read-only drive (/dev/vdb) that the base
	// rootfs's loader unit mounts and runs — so the guest can be updated by
	// swapping this tiny image without rebuilding the multi-GiB base rootfs. Empty
	// keeps the legacy all-in-one layout where the guest is baked into the rootfs.
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
	// the guest is still reachable over vsock, but the guest has no outbound
	// network. Egress setup needs CAP_NET_ADMIN, so this also allows booting boxes
	// as an unprivileged user.
	netEnabled bool
	// poolSize is the number of pre-created egress TAP slots; it caps concurrent
	// networked boxes.
	poolSize int
	// poolOnce guards one-time provisioning of the TAP pool (EnsureNetwork), so the
	// host interfaces are created once at startup rather than per box. The pool is
	// deliberately never torn down on Close: boxes outlive the spoke, so their egress
	// interfaces must survive a restart (EnsureNetwork re-adopts them idempotently).
	poolOnce sync.Once
	poolErr  error

	// ports serves box-originated port-publishing requests toward the hub; nil
	// disables the per-box box-port API listener entirely.
	ports boxapi.PortService

	// jailer holds the host-wide jailer launch settings. Every box is started
	// through the jailer (chrooted, unprivileged per-VM UID); there is no direct
	// launch path. checkJailerPrereqs validates the settings at startup.
	jailer jailerConfig

	mu    sync.Mutex
	boxes map[string]*fcInstance
	used  map[int]bool
	// usedUIDs tracks the per-VM UIDs currently held by live boxes, so restart
	// recovery never reuses a UID while its box still exists.
	usedUIDs map[int]bool
}

// NewProvisioner builds a Firecracker provisioner. kernelImage and defaultRootfs
// are host paths to the guest kernel and the default rootfs image; an empty
// stateDir falls back to defaultStateDir. The returned provisioner uses the real
// host egress and the real Firecracker machine factory. Boxes persisted under
// stateDir by a previous run are rehydrated so List/Find/Destroy (and their
// box-port API listeners) survive a spoke restart.
//
// @arg kernelImage Host path to the guest kernel (vmlinux) every box boots.
// @arg defaultRootfs Host path to the rootfs image booted when a create supplies none.
// @arg stateDir Directory for per-box runtime files and metadata; empty uses defaultStateDir.
// @arg ports The service serving box-originated port requests; nil disables the per-box box-port API.
// @return *Provisioner A provisioner wired to the real egress and machine factory.
// @error error if persisted box metadata exists but cannot be read.
//
// @testcase TestProvisionerBookkeeping builds a provisioner and exercises its lifecycle with fakes.
// @testcase TestRehydrateListsPriorBoxes rehydrates persisted boxes on construction.
func NewProvisioner(kernelImage, defaultRootfs, stateDir string, ports boxapi.PortService) (*Provisioner, error) {
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
		ports:          ports,
		jailer:         defaultJailerConfig(stateDir),
		boxes:          map[string]*fcInstance{},
		used:           map[int]bool{},
		usedUIDs:       map[int]bool{},
	}
	p.newMachine = p.realMachineFactory
	if err := p.rehydrate(); err != nil {
		return nil, err
	}
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

// SetJailerBinary overrides the jailer executable (path or PATH-resolved name). An
// empty value keeps the default ("jailer" on PATH).
//
// @arg bin The jailer binary path or name; empty keeps the default.
//
// @testcase TestNewBackendConfiguresProvisioner applies the jailer binary override.
func (p *Provisioner) SetJailerBinary(bin string) {
	if bin != "" {
		p.jailer.jailerBin = bin
	}
}

// SetFirecrackerBinary overrides the firecracker executable the jailer exec-s. An
// empty value keeps the default ("firecracker" on PATH). checkJailerPrereqs resolves
// it to an absolute path (the jailer needs one).
//
// @arg bin The firecracker binary path or name; empty keeps the default.
//
// @testcase TestNewBackendConfiguresProvisioner applies the firecracker binary override.
func (p *Provisioner) SetFirecrackerBinary(bin string) {
	if bin != "" {
		p.firecrackerBin = bin
		p.jailer.execFile = bin
	}
}

// SetChrootBase overrides the jailer chroot base directory. An empty value keeps the
// default (<state-dir>/chroot), which sits on the same filesystem as each box's
// rootfs copy so the jailer can hard-link it into the chroot.
//
// @arg dir The chroot base directory; empty keeps the default.
//
// @testcase TestNewBackendConfiguresProvisioner applies the chroot base override.
func (p *Provisioner) SetChrootBase(dir string) {
	if dir != "" {
		p.jailer.chrootBase = dir
	}
}

// SetUIDRange overrides the inclusive range unique per-VM UIDs are drawn from. A
// non-positive or inverted range keeps the default.
//
// @arg min The lowest per-VM UID.
// @arg max The highest per-VM UID.
//
// @testcase TestNewBackendConfiguresProvisioner applies the UID range override.
func (p *Provisioner) SetUIDRange(min, max int) {
	if min > 0 && max >= min {
		p.jailer.uidMin = min
		p.jailer.uidMax = max
	}
}

// SetTapGroup overrides the shared GID every jailed VMM runs under and that owns the
// pooled TAP devices, so a jailed Firecracker can open its TAP without
// CAP_NET_ADMIN. A non-positive value keeps the default.
//
// @arg gid The shared fc-net GID; <= 0 keeps the default.
//
// @testcase TestNewBackendConfiguresProvisioner applies the tap-group override.
func (p *Provisioner) SetTapGroup(gid int) {
	if gid > 0 {
		p.jailer.gid = gid
	}
}

// SetCgroupVersion overrides the cgroup filesystem version ("1" or "2") the jailer
// places VMMs in. An empty value keeps the auto-detected default.
//
// @arg v The cgroup version ("1" or "2"); empty keeps the detected default.
//
// @testcase TestNewBackendConfiguresProvisioner applies the cgroup version override.
func (p *Provisioner) SetCgroupVersion(v string) {
	if v != "" {
		p.jailer.cgroupVersion = v
	}
}

// SetPayloadImage sets an optional host path to a read-only ext4 carrying the
// guest binary and entrypoint. When non-empty, every box boots with it
// attached as a shared read-only second drive (/dev/vdb) that the base rootfs's
// loader unit mounts; this decouples the fast-changing guest from the slow,
// multi-GiB base rootfs. Empty keeps the all-in-one rootfs (guest baked in).
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
		// Own the pooled TAPs by the shared fc-net group so each jailed, unprivileged
		// VMM can attach to its assigned TAP without CAP_NET_ADMIN.
		if he, ok := p.egress.(*hostEgress); ok {
			he.tapGroup = p.jailer.gid
		}
		p.poolErr = p.egress.EnsurePool(ctx, p.poolSize)
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

// realMachineFactory boots a real Firecracker VM for cfg via the go SDK, launched
// through the jailer (cfg.JailerCfg, set by bootMachine). The jailer chroots the
// VMM, drops it to its per-VM UID, and — because JailerCfg.Daemonize is set —
// setsid()s it into its own session, detaching it from the spoke so it outlives a
// spoke restart (a restart then rehydrates the still-running VMM). There is no
// direct-launch path.
//
// @arg _ The request context, deliberately ignored; the VM must outlive both the request and the provisioner.
// @arg cfg The Firecracker machine configuration, including its JailerCfg.
// @return machine The started-able machine handle.
// @error error if the machine cannot be constructed.
//
// @testcase TestConformanceFirecracker boots real VMs through this factory.
// @testcase TestVMSurvivesRequestContextCancel checks the VM outlives the create request's context.
func (p *Provisioner) realMachineFactory(_ context.Context, cfg fcsdk.Config) (machine, error) {
	// The jailer chroots Firecracker, so its API and vsock socket paths must be
	// relative to the chroot root. bootMachine set them to the host-visible absolute
	// paths (which resolve to <chroot>/root/<name>, the same location) so a test's
	// fake machine and the host control plane share one path; translate them back to
	// basenames here — the one place that actually drives the SDK's jailer. The SDK's
	// jail() then rewrites cfg.SocketPath to the host-visible chroot path the client
	// dials, keeping both views in agreement.
	cfg.SocketPath = filepath.Base(cfg.SocketPath)
	for i := range cfg.VsockDevices {
		cfg.VsockDevices[i].Path = filepath.Base(cfg.VsockDevices[i].Path)
	}
	// Construct on context.Background(), NOT the request or a provisioner-lifetime
	// context: the microVM must outlive both the create request AND the provisioner
	// itself, like a Docker container outlives the spoke. A cancellable context would
	// reap the VM on spoke shutdown; a restart rehydrates the still-running VMM
	// instead. Daemonize (setsid) plus the empty ForwardSignals slice keep the
	// spoke's own shutdown signals from reaching the VMM.
	logger := logrus.NewEntry(logrus.New())
	return fcsdk.NewMachine(context.Background(), cfg, fcsdk.WithLogger(logger))
}

// Provision boots a new microVM box: it copies the rootfs, grows the copy to the
// requested disk size, attaches the shared read-only payload drive when one is
// configured, assigns a pooled TAP for egress, boots the VM with the guest kernel
// and a vsock device, and waits for the guest to answer on vsock. The box is
// created in the pending phase.
//
// @arg ctx Context for the boot and the guest wait.
// @arg opts The caller-controlled box ID, description, files, and requested disk size (the rootfs is the spoke's configured default).
// @return box.Instance A handle to the booted box, in the pending phase.
// @error error if the box id is invalid, the kernel/rootfs is missing, the egress pool cannot be provisioned, the rootfs cannot be resized, or the VM cannot be prepared, booted, or its guest does not answer.
//
// @testcase TestProvisionerBookkeeping provisions a box with a fake machine and finds it back.
// @testcase TestProvisionCleansUpOnBootFailure cleans up files and the slot index when boot fails.
// @testcase TestProvisionGrowsRootfs grows the per-box rootfs to the requested disk size.
func (p *Provisioner) Provision(ctx context.Context, opts sandbox.CreateOptions) (box.Instance, error) {
	if opts.BoxID != "" && !sandbox.ValidBoxID(opts.BoxID) {
		return nil, fmt.Errorf("invalid box id %q", opts.BoxID)
	}
	// Every box boots the spoke's configured rootfs; the request carries no image
	// (the rootfs is a property of the spoke, not the create).
	rootfsSrc := p.defaultRootfs
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
	uid, uidOK := p.allocUIDLocked()
	if !uidOK {
		delete(p.used, index)
		p.mu.Unlock()
		return nil, fmt.Errorf("no free per-VM UID in range [%d,%d]", p.jailer.uidMin, p.jailer.uidMax)
	}
	p.mu.Unlock()

	dir := boxDir(p.stateDir, token)
	n := netFor(index)
	perBoxRootfs := filepath.Join(dir, "rootfs.ext4")

	// The box's persisted identity, built before boot so bootMachine can derive the
	// jailer config and the host-visible (chrooted) socket paths from it, and so a
	// crash mid-boot still leaves a UID/slot that recovery can reclaim.
	meta := boxMeta{
		Token: token, BoxID: opts.BoxID, Description: opts.Description,
		Image: rootfsSrc, Phase: "pending", Created: time.Now().Unix(),
		NetIndex: index, Namespace: p.namespace,
		UID: uid, GID: p.jailer.gid,
		ChrootBase: p.jailer.chrootBase, ExecBase: p.jailer.execBase(),
	}

	// freeSlot returns the box's pooled network slot and its per-VM UID; the pooled
	// TAP itself stays up for reuse and is never torn down per box.
	freeSlot := func() {
		p.mu.Lock()
		delete(p.used, index)
		delete(p.usedUIDs, uid)
		p.mu.Unlock()
	}

	// The base rootfs is a bare ext4 whose file size is the filesystem size, so its
	// stat size is the floor the per-box disk can never go below (the copy is grown,
	// never shrunk).
	baseInfo, err := os.Stat(rootfsSrc)
	if err != nil {
		freeSlot()
		return nil, fmt.Errorf("stat rootfs: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		freeSlot()
		return nil, fmt.Errorf("creating box dir: %w", err)
	}
	if err := copyFile(rootfsSrc, perBoxRootfs); err != nil {
		_ = os.RemoveAll(dir)
		freeSlot()
		return nil, fmt.Errorf("copying rootfs: %w", err)
	}
	// Grow the per-box rootfs file to the requested size (a sparse ftruncate, so the
	// added space costs no host blocks until written). Firecracker exposes the larger
	// file as a bigger virtio-block device; the guest's boot-time resize2fs unit then
	// grows the ext4 to fill it. Shipping a small base image and growing here keeps
	// release artifacts and the per-box copy small instead of carrying empty space.
	diskBytes := p.diskBytesFor(opts.DiskBytes, baseInfo.Size())
	meta.DiskBytes = diskBytes
	if diskBytes > baseInfo.Size() {
		if err := os.Truncate(perBoxRootfs, diskBytes); err != nil {
			_ = os.RemoveAll(dir)
			freeSlot()
			return nil, fmt.Errorf("resizing rootfs to %d bytes: %w", diskBytes, err)
		}
	}
	// Hand the writable rootfs to the box's unprivileged UID (the jailer hard-links it
	// into the chroot, sharing this inode, so the jailed VMM can open it read-write).
	// Mode stays 0600 so no other box's UID can read it. A no-op when unprivileged
	// (jailing needs root, validated at startup); best-effort otherwise.
	if err := chownForBox(perBoxRootfs, uid, p.jailer.gid); err != nil {
		_ = os.RemoveAll(dir)
		freeSlot()
		return nil, fmt.Errorf("assigning rootfs ownership: %w", err)
	}

	m, api, err := p.bootMachine(ctx, meta)
	if err != nil {
		_ = os.RemoveAll(dir)
		_ = os.RemoveAll(meta.chrootInstanceDir())
		freeSlot()
		return nil, err
	}

	if err := meta.save(p.stateDir); err != nil {
		_ = m.StopVMM()
		if api != nil {
			_ = api.Close()
		}
		_ = os.RemoveAll(dir)
		_ = os.RemoveAll(meta.chrootInstanceDir())
		freeSlot()
		return nil, fmt.Errorf("saving box meta: %w", err)
	}
	inst := &fcInstance{prov: p, meta: meta, vsockUDS: meta.vsockUDSPath(p.stateDir), net: n, machine: m, api: api}

	p.mu.Lock()
	p.boxes[token] = inst
	p.mu.Unlock()
	return inst, nil
}

// bootMachine boots the jailed microVM for a box from its on-disk rootfs and waits
// for the guest to answer on vsock. Provision calls it for a fresh box and Resume
// calls it to bring a paused box's compute back up; both reuse the box's existing
// state dir, per-box rootfs, pooled network slot, and per-VM UID, so a resumed box
// keeps the same identity and never churns a host interface. The VMM is launched
// through the jailer (see realMachineFactory), so its API and vsock sockets live in
// its chroot; the box-port listener is created there AFTER the jailer builds the
// chroot (the guest only dials it well after boot). On any failure it stops the VM
// and closes that listener so it never leaks a half-booted machine. A resumed box's
// stale chroot is removed first (the jailer refuses to reuse an existing chroot).
//
// @arg ctx Context bounding the guest wait (the VM itself runs on the provisioner lifetime context).
// @arg meta The box's persisted identity: token, box id, network slot, UID/GID, and chroot location.
// @return machine The started microVM handle.
// @return *boxapi.Server The box-port API listener, or nil when the provisioner has no port service.
// @error error if the chroot cannot be cleared, the box-port API cannot be served, or the VM cannot be created, started, or reached over vsock.
//
// @testcase TestConformanceFirecracker boots every box through bootMachine.
func (p *Provisioner) bootMachine(ctx context.Context, meta boxMeta) (machine, *boxapi.Server, error) {
	dir := boxDir(p.stateDir, meta.Token)
	n := netFor(meta.NetIndex)
	vsockUDS := meta.vsockUDSPath(p.stateDir)
	apiSock := meta.apiSockPath(p.stateDir)
	perBoxRootfs := filepath.Join(dir, "rootfs.ext4")

	// The jailer creates the chroot fresh and refuses to launch into one that already
	// exists, so a resumed box's previous chroot (holding the old sockets) must go
	// first. The per-box rootfs lives in the state dir, not the chroot, so this never
	// touches the box's disk. A first Provision has no chroot yet — a harmless no-op.
	if chroot := meta.chrootInstanceDir(); chroot != "" {
		if err := os.RemoveAll(chroot); err != nil {
			return nil, nil, fmt.Errorf("clearing stale chroot %s: %w", chroot, err)
		}
	}

	bootCleanup := func(m machine, api *boxapi.Server) {
		if m != nil {
			_ = m.StopVMM()
		}
		if api != nil {
			_ = api.Close()
		}
		// Drop the chroot the jailer built, so a failed boot leaks no jail state.
		if chroot := meta.chrootInstanceDir(); chroot != "" {
			_ = os.RemoveAll(chroot)
		}
	}

	// Kernel args: the guest gets a static eth0 (via the ip= arg, on its pooled TAP)
	// only when egress networking is enabled; a control-only box boots with just
	// loopback and vsock. net.ifnames=0 keeps the NIC named eth0 (so the ip= arg
	// and a systemd guest's network config agree); init=/init lets a rootfs point
	// /init at its real init (systemd, or the guest directly).
	kernelArgs := "console=ttyS0 reboot=k panic=1 pci=off net.ifnames=0 init=/init"
	if p.netEnabled {
		kernelArgs += " " + n.kernelIPArg()
	}

	// The root drive is a per-box writable copy of the rootfs. When a payload image
	// is configured, it rides along as a second, read-only drive (/dev/vdb) shared
	// unchanged across every box — never copied — which is what lets the guest be
	// swapped without rebuilding the base rootfs and keeps concurrent boxes sharing
	// one on-disk payload. The jailer hard-links both into the box's chroot.
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
		// Host-visible socket paths: bootMachine sets them to where the sockets end up
		// on the host (inside the chroot); realMachineFactory translates them to the
		// chroot-relative paths the jailer expects before invoking the SDK.
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
		// Every VMM is launched jailed: chrooted, dropped to the box's unprivileged
		// UID, and — via Daemonize — setsid()'d into its own session so it survives a
		// spoke restart. The naive chroot strategy hard-links the kernel and drives in.
		JailerCfg: p.jailer.jailerCfgFor(meta.UID, meta.Token, p.kernelImage),
		// Clear the SDK's default signal forwarding (SIGINT/SIGTERM/SIGQUIT/…): the
		// spoke's own shutdown signal must NOT be relayed to the VMM, so the box
		// survives a spoke restart and is rehydrated. An empty non-nil slice disables
		// forwarding; leaving it nil would make the SDK install the default handler.
		ForwardSignals: []os.Signal{},
	}
	if p.netEnabled {
		cfg.NetworkInterfaces = fcsdk.NetworkInterfaces{{
			StaticConfiguration: &fcsdk.StaticNetworkConfiguration{
				MacAddress:  macForIndex(meta.NetIndex),
				HostDevName: n.TapName,
			},
		}}
	}

	m, err := p.newMachine(ctx, cfg)
	if err != nil {
		bootCleanup(nil, nil)
		return nil, nil, fmt.Errorf("creating microVM: %w", err)
	}
	// Start on context.Background(), not the request context and not a
	// provisioner-lifetime context: the SDK spawns a goroutine that calls StopVMM when
	// the Start context is done, so any cancellable context would kill the VM (on
	// create return, or on spoke shutdown). The box must outlive the spoke and be
	// rehydrated on restart. The guest wait below still honours the request context.
	if err := m.Start(context.Background()); err != nil {
		bootCleanup(m, nil)
		return nil, nil, fmt.Errorf("starting microVM: %w", err)
	}

	// Serve the guest's box-port API AFTER the jailer built the chroot (its parent
	// directory did not exist until now). Firecracker dials this host socket, at
	// "<vsock_uds>_<port>" inside the chroot, when the guest connects to CID 2 on
	// boxAPIVsockPort — which only happens well after boot — so serving it here is not
	// racy. The listener is bound to this one VM's identity (the spoke-side
	// enforcement that a box can only publish its own ports) and the socket is handed
	// to the box's UID/GID so the jailed VMM can connect to it.
	var api *boxapi.Server
	if p.ports != nil {
		boxAPISock := boxAPISocketPath(vsockUDS)
		if api, err = boxapi.ServeUnix(boxAPISock, meta.BoxID, p.ports, p.log); err != nil {
			bootCleanup(m, nil)
			return nil, nil, fmt.Errorf("serving box-port API: %w", err)
		}
		if err := chownForBox(boxAPISock, meta.UID, meta.GID); err != nil {
			bootCleanup(m, api)
			return nil, nil, fmt.Errorf("assigning box-port API socket ownership: %w", err)
		}
	}

	if err := waitForGuest(ctx, vsockUDS, guestVsockPort, bootWait); err != nil {
		bootCleanup(m, api)
		return nil, nil, fmt.Errorf("waiting for box guest: %w", err)
	}
	return m, api, nil
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

// Close releases the provisioner's in-memory resources WITHOUT touching the box
// VMs, mirroring the Docker backend (whose Close never stops containers): shutting
// the spoke down must leave every microVM running so a later spoke run rehydrates
// it. Each VMM was launched detached (its own process group, no SDK signal
// forwarding) so it keeps running as an orphan; only an explicit Destroy (or the
// operator `vm destroy`) stops a VM. Close just closes the box-port API listeners —
// their sockets are re-bound on rehydrate — and leaves the egress TAP pool up so
// surviving VMs keep their outbound networking.
//
// @error error is always nil; closing listeners is best-effort.
//
// @testcase TestProvisionerBookkeeping closes the provisioner without stopping its boxes.
func (p *Provisioner) Close() error {
	p.mu.Lock()
	insts := make([]*fcInstance, 0, len(p.boxes))
	for _, inst := range p.boxes {
		insts = append(insts, inst)
	}
	p.mu.Unlock()
	for _, inst := range insts {
		if inst.api != nil {
			_ = inst.api.Close()
		}
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

// allocUIDLocked reserves the lowest free per-VM UID in the configured range, so
// each live box's jailed VMM runs under a distinct unprivileged identity and no two
// boxes share a chroot/rootfs owner. The caller must hold p.mu.
//
// @return int The reserved UID.
// @return bool False when every UID in the range is in use.
//
// @testcase TestProvisionerBookkeeping allocates and frees per-VM UIDs across boxes.
func (p *Provisioner) allocUIDLocked() (int, bool) {
	for uid := p.jailer.uidMin; uid <= p.jailer.uidMax; uid++ {
		if !p.usedUIDs[uid] {
			p.usedUIDs[uid] = true
			return uid, true
		}
	}
	return 0, false
}

// chownForBox gives a staged file or socket to a box's unprivileged jailer identity
// so the jailed VMM (running as uid:gid) can use it, while another box's UID cannot.
// It is a no-op when the spoke is not root — jailing requires root (validated at
// startup by checkJailerPrereqs), so this only skips in unprivileged unit tests,
// never in production.
//
// @arg path The staged file or socket to hand to the box.
// @arg uid The box's unprivileged UID.
// @arg gid The shared fc-net GID.
// @error error if the chown fails while running as root.
//
// @testcase TestChownForBoxSkipsUnprivileged is a no-op when not root.
func chownForBox(path string, uid, gid int) error {
	if os.Geteuid() != 0 {
		return nil
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("chown %s to %d:%d: %w", path, uid, gid, err)
	}
	return nil
}

// diskBytesFor resolves the size a box's writable rootfs is grown to, from the
// caller's requested size and the base image size. A request of 0 falls back to the
// spoke's configured default (Limits.DiskBytes); the result is capped at the
// spoke's Limits.MaxDiskBytes ceiling and floored at baseBytes — so a caller on the
// unauthenticated create path can neither exceed the operator's cap nor corrupt the
// rootfs by asking for less than the base image it is copied from.
//
// @arg requested The caller-requested disk size in bytes (0 = use the spoke default).
// @arg baseBytes The base rootfs image size in bytes; the returned size is never below it.
// @return int64 The size the per-box rootfs file is truncated to before boot.
//
// @testcase TestDiskBytesFor clamps to the cap, falls back to the default, and floors at the base size.
func (p *Provisioner) diskBytesFor(requested, baseBytes int64) int64 {
	want := requested
	if want <= 0 {
		want = p.limits.DiskBytes
	}
	if p.limits.MaxDiskBytes > 0 && want > p.limits.MaxDiskBytes {
		want = p.limits.MaxDiskBytes
	}
	if want < baseBytes {
		want = baseBytes
	}
	return want
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

// waitForGuest polls the box's vsock until the guest accepts a CONNECT (it is
// listening) or the timeout elapses. A successful probe is closed immediately; the
// caller opens its own control connections afterwards.
//
// @arg ctx Context whose cancellation aborts the wait.
// @arg udsPath The box's Firecracker vsock Unix-socket path.
// @arg port The guest's AF_VSOCK port.
// @arg timeout How long to wait before giving up.
// @error error if the guest does not answer before the timeout or ctx is cancelled.
//
// @testcase TestWaitForGuest succeeds once a fake vsock accepts the CONNECT.
func waitForGuest(ctx context.Context, udsPath string, port uint32, timeout time.Duration) error {
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
			return fmt.Errorf("box guest did not answer on vsock within %s: %w", timeout, err)
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
	// api is the box's box-port API listener; nil when the provisioner has no
	// port service (or its recovery failed).
	api *boxapi.Server
	// alive records that a rehydrated box's VMM (whose machine handle is gone)
	// still answered on its API socket when the box was reloaded.
	alive bool
}

// Meta returns the box's neutral view. A box with a live machine handle is
// running, as is a rehydrated box whose orphaned VMM answered the aliveness
// probe; anything else is reported stopped.
//
// @return sandbox.Box The box's ID, name, phase, and other fields.
//
// @testcase TestProvisionerBookkeeping reads box metadata via Meta.
// @testcase TestRehydrateListsPriorBoxes reports a rehydrated dead box as stopped.
func (i *fcInstance) Meta() sandbox.Box {
	state := "stopped"
	switch {
	case i.meta.Paused:
		// A deliberately paused box: its VM is stopped but its rootfs is kept, so it
		// is reported paused (not merely stopped) and callers can resume it.
		state = sandbox.StatePaused
	case i.machine != nil || i.alive:
		state = "running"
	}
	return i.meta.toBox(state)
}

// Control opens a control connection to the box's guest over the VM's vsock,
// performing Firecracker's CONNECT handshake.
//
// @arg ctx Context for the dial and handshake.
// @return net.Conn A control connection to the box's guest.
// @error error if the vsock cannot be dialled or the handshake fails.
//
// @testcase TestConformanceFirecracker drives the guest through Control.
func (i *fcInstance) Control(ctx context.Context) (net.Conn, error) {
	return dialVsock(ctx, i.vsockUDS, guestVsockPort)
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

// Pause stops the box's VM to free CPU/RAM while keeping its rootfs (auth,
// workspace), pooled network slot, and metadata, so Resume can boot it back. It
// records the paused state (persisted, so it survives a spoke restart), drops the
// live machine handle and the box-port API listener, but keeps the slot reserved so
// a resume reuses the same TAP/IP. Pausing an already-gone box returns a wrapped
// sandbox.ErrBoxNotFound.
//
// @arg ctx Unused; present to satisfy the interface.
// @error error wrapping sandbox.ErrBoxNotFound if the box is already gone, or if the paused state cannot be persisted.
//
// @testcase TestPauseResumeFirecrackerBox pauses a box, sees it reported paused, then resumes it.
// @testcase TestPauseAlreadyGoneFirecracker reports ErrBoxNotFound for an unknown box.
func (i *fcInstance) Pause(ctx context.Context) error {
	p := i.prov
	p.mu.Lock()
	_, present := p.boxes[i.meta.Token]
	p.mu.Unlock()
	if !present {
		return fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, i.meta.Token)
	}
	// Stop the compute. A live handle stops directly; a rehydrated orphan (no
	// handle) is halted over its API socket.
	switch {
	case i.machine != nil:
		_ = i.machine.StopVMM()
	case i.alive:
		if err := haltVMM(i.meta.apiSockPath(p.stateDir)); err != nil {
			p.log.Warn("failed to halt box VMM on pause", "box", i.meta.Token, "err", err)
		}
	}
	if i.api != nil {
		_ = i.api.Close()
	}
	p.mu.Lock()
	i.machine = nil
	i.alive = false
	i.api = nil
	i.meta.Paused = true
	meta := i.meta
	p.mu.Unlock()
	if err := meta.save(p.stateDir); err != nil {
		return fmt.Errorf("persisting paused box %s: %w", i.meta.Token, err)
	}
	return nil
}

// Resume boots a paused box's VM back up from its kept rootfs, reusing its pooled
// network slot so it keeps the same IP/MAC, and re-establishes its box-port API
// listener. It restores only the compute; the box's workload comes back up via its
// own boot. The paused state is cleared only after a successful boot, so a
// resume that fails leaves the box paused and retryable. Resuming an already-gone
// box returns a wrapped sandbox.ErrBoxNotFound.
//
// @arg ctx Context bounding the boot's guest wait.
// @error error wrapping sandbox.ErrBoxNotFound if the box is already gone, or if the VM cannot be booted or its state persisted.
//
// @testcase TestPauseResumeFirecrackerBox resumes a paused box and sees it reported running again.
func (i *fcInstance) Resume(ctx context.Context) error {
	p := i.prov
	p.mu.Lock()
	_, present := p.boxes[i.meta.Token]
	p.mu.Unlock()
	if !present {
		return fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, i.meta.Token)
	}
	m, api, err := p.bootMachine(ctx, i.meta)
	if err != nil {
		return fmt.Errorf("resuming box %s: %w", i.meta.Token, err)
	}
	p.mu.Lock()
	i.machine = m
	i.api = api
	i.alive = false
	i.meta.Paused = false
	meta := i.meta
	p.mu.Unlock()
	if err := meta.save(p.stateDir); err != nil {
		return fmt.Errorf("persisting resumed box %s: %w", i.meta.Token, err)
	}
	return nil
}

// Destroy stops the box's VM, stops its box-port API listener, removes its state
// directory and jailer chroot, and frees its pool slot and per-VM UID (the pooled
// TAP stays up for reuse). A rehydrated box whose VMM survived as an orphan (no
// machine handle) is halted best-effort over its host-visible API socket, so the
// rootfs is not deleted under a running VM. Destroying an already-gone box returns a
// wrapped sandbox.ErrBoxNotFound.
//
// @arg ctx Unused; present to satisfy the interface.
// @error error wrapping sandbox.ErrBoxNotFound if the box is already gone.
//
// @testcase TestProvisionerBookkeeping destroys a box and no longer finds it.
// @testcase TestDestroyAlreadyGone reports ErrBoxNotFound for an unknown box.
// @testcase TestRehydrateDestroysDeadBox destroys a rehydrated box, cleaning its dir and slot.
func (i *fcInstance) Destroy(ctx context.Context) error {
	p := i.prov
	p.mu.Lock()
	_, present := p.boxes[i.meta.Token]
	delete(p.boxes, i.meta.Token)
	delete(p.used, i.meta.NetIndex)
	delete(p.usedUIDs, i.meta.UID)
	p.mu.Unlock()

	if i.api != nil {
		_ = i.api.Close()
	}
	switch {
	case i.machine != nil:
		_ = i.machine.StopVMM()
	case i.alive:
		// A rehydrated orphan VMM: no SDK handle to stop it with, so ask it to
		// shut down over its (chrooted) API socket before its rootfs is removed.
		if err := haltVMM(i.meta.apiSockPath(p.stateDir)); err != nil {
			p.log.Warn("failed to halt orphaned box VMM", "box", i.meta.Token, "err", err)
		}
	}
	_ = os.RemoveAll(boxDir(p.stateDir, i.meta.Token))
	if chroot := i.meta.chrootInstanceDir(); chroot != "" {
		_ = os.RemoveAll(chroot)
	}

	if !present {
		return fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, i.meta.Token)
	}
	return nil
}
