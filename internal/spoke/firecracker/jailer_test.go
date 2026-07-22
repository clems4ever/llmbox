package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fcsdk "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// TestDefaultJailerConfig checks the chroot base defaults under the state dir (so it
// shares a filesystem with each box's rootfs, letting the jailer hard-link it) and
// the identity/binary defaults are set.
func TestDefaultJailerConfig(t *testing.T) {
	jc := defaultJailerConfig("/var/lib/fc")
	if jc.chrootBase != "/var/lib/fc/chroot" {
		t.Fatalf("chrootBase = %q, want /var/lib/fc/chroot", jc.chrootBase)
	}
	if jc.jailerBin != defaultJailerBin || jc.execFile != defaultFirecrackerBin {
		t.Fatalf("binaries = %q/%q, want %q/%q", jc.jailerBin, jc.execFile, defaultJailerBin, defaultFirecrackerBin)
	}
	if jc.uidMin != defaultUIDMin || jc.uidMax != defaultUIDMax || jc.gid != defaultFcGID {
		t.Fatalf("identity range = [%d,%d] gid %d, want [%d,%d] gid %d", jc.uidMin, jc.uidMax, jc.gid, defaultUIDMin, defaultUIDMax, defaultFcGID)
	}
	if jc.cgroupVersion != "1" && jc.cgroupVersion != "2" {
		t.Fatalf("cgroupVersion = %q, want 1 or 2", jc.cgroupVersion)
	}
}

// TestDetectCgroupVersion returns a valid version string for whatever host runs the
// test.
func TestDetectCgroupVersion(t *testing.T) {
	if v := detectCgroupVersion(); v != "1" && v != "2" {
		t.Fatalf("detectCgroupVersion() = %q, want 1 or 2", v)
	}
}

// TestChrootPaths checks the exec basename (which the jailer uses as the chroot
// subdirectory) is derived from the resolved firecracker binary path.
func TestChrootPaths(t *testing.T) {
	jc := jailerConfig{execFile: "/usr/local/bin/firecracker"}
	if got := jc.execBase(); got != "firecracker" {
		t.Fatalf("execBase() = %q, want firecracker", got)
	}
	jc.execFile = "/opt/fc/firecracker-v1.7"
	if got := jc.execBase(); got != "firecracker-v1.7" {
		t.Fatalf("execBase() = %q, want firecracker-v1.7", got)
	}
}

// TestJailerCfgFor builds a per-box jailer config and checks the per-VM UID and ID
// are layered onto the shared host settings, with Daemonize (setsid, for
// restart-survival) and a non-nil chroot strategy set.
func TestJailerCfgFor(t *testing.T) {
	jc := jailerConfig{
		jailerBin: "/opt/jailer", execFile: "/opt/firecracker",
		chrootBase: "/srv/jail", gid: 4242, cgroupVersion: "2",
	}
	cfg := jc.jailerCfgFor(200007, "box-token", "/assets/vmlinux")
	if cfg.UID == nil || *cfg.UID != 200007 {
		t.Fatalf("UID = %v, want 200007", cfg.UID)
	}
	if cfg.GID == nil || *cfg.GID != 4242 {
		t.Fatalf("GID = %v, want 4242", cfg.GID)
	}
	if cfg.ID != "box-token" {
		t.Fatalf("ID = %q, want box-token", cfg.ID)
	}
	if cfg.ExecFile != "/opt/firecracker" || cfg.JailerBinary != "/opt/jailer" || cfg.ChrootBaseDir != "/srv/jail" {
		t.Fatalf("binaries/chroot not set: %+v", cfg)
	}
	if cfg.CgroupVersion != "2" || !cfg.Daemonize {
		t.Fatalf("cgroup/daemonize not set: version=%q daemonize=%v", cfg.CgroupVersion, cfg.Daemonize)
	}
	if cfg.ChrootStrategy == nil {
		t.Fatal("ChrootStrategy is nil; the jailer needs one to stage kernel/drives")
	}
}

// TestCheckJailerPrereqsReportsProblems checks the prerequisite validation fails
// closed and names each missing prerequisite in one actionable error. The unit host
// has no firecracker/jailer binary, no /dev/kvm, and is not root, so every check
// fires.
func TestCheckJailerPrereqsReportsProblems(t *testing.T) {
	jc := defaultJailerConfig(t.TempDir())
	err := jc.checkJailerPrereqs(true)
	if err == nil {
		t.Fatal("checkJailerPrereqs succeeded on a host with no jailer/kvm; want failure (fail closed)")
	}
	for _, want := range []string{"firecracker binary", "jailer binary", "/dev/kvm"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing mention of %q", err.Error(), want)
		}
	}
	// An invalid UID range is reported too.
	jc2 := defaultJailerConfig(t.TempDir())
	jc2.uidMin, jc2.uidMax = 500, 100
	if err := jc2.checkJailerPrereqs(true); err == nil || !strings.Contains(err.Error(), "UID range") {
		t.Fatalf("checkJailerPrereqs did not report the inverted UID range: %v", err)
	}
}

// TestRuntimeDirJailedAndLegacy checks a jailed box resolves its sockets into its
// chroot root, a legacy direct box (no chroot base) resolves them flat in its state
// dir, and only a jailed box reports a chroot dir to clean.
func TestRuntimeDirJailedAndLegacy(t *testing.T) {
	jailed := boxMeta{Token: "abc123", ChrootBase: "/srv/jail", ExecBase: "firecracker"}
	wantRoot := "/srv/jail/firecracker/abc123/root"
	if got := jailed.runtimeDir("/state"); got != wantRoot {
		t.Fatalf("jailed runtimeDir = %q, want %q", got, wantRoot)
	}
	if got := jailed.apiSockPath("/state"); got != wantRoot+"/fc.sock" {
		t.Fatalf("jailed apiSockPath = %q", got)
	}
	if got := jailed.vsockUDSPath("/state"); got != wantRoot+"/vsock.sock" {
		t.Fatalf("jailed vsockUDSPath = %q", got)
	}
	if got := jailed.chrootInstanceDir(); got != "/srv/jail/firecracker/abc123" {
		t.Fatalf("jailed chrootInstanceDir = %q", got)
	}

	legacy := boxMeta{Token: "old99"}
	if got := legacy.runtimeDir("/state"); got != "/state/old99" {
		t.Fatalf("legacy runtimeDir = %q, want /state/old99", got)
	}
	if got := legacy.apiSockPath("/state"); got != "/state/old99/fc.sock" {
		t.Fatalf("legacy apiSockPath = %q", got)
	}
	if got := legacy.chrootInstanceDir(); got != "" {
		t.Fatalf("legacy chrootInstanceDir = %q, want empty (no chroot to clean)", got)
	}
}

// TestChownForBoxSkipsUnprivileged checks chownForBox is a no-op (no error) when not
// root, so unprivileged unit tests are unaffected while production (root) chowns.
func TestChownForBoxSkipsUnprivileged(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test asserts the unprivileged no-op path")
	}
	f := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := chownForBox(f, 123456, 4242); err != nil {
		t.Fatalf("chownForBox unprivileged = %v, want nil", err)
	}
}

// TestEnsureAssetsReadableSkipsUnprivileged checks ensureAssetsReadable is a no-op
// (no error) when not root and ignores empty paths.
func TestEnsureAssetsReadableSkipsUnprivileged(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test asserts the unprivileged no-op path")
	}
	if err := ensureAssetsReadable(4242, "", "/nonexistent/either"); err != nil {
		t.Fatalf("ensureAssetsReadable unprivileged = %v, want nil", err)
	}
}

// TestUIDAllocationUniqueAndReused checks each live box gets a distinct per-VM UID,
// exhausting the range fails the create, and destroying a box frees its UID for
// reuse — so a jailed box never shares its chroot/rootfs owner with another.
func TestUIDAllocationUniqueAndReused(t *testing.T) {
	eg := &fakeEgress{}
	p, _ := newFakeProvisioner(t, eg)
	// Shrink the range to exactly two UIDs so exhaustion is easy to trigger.
	p.SetUIDRange(200000, 200001)
	ctx := context.Background()

	a, err := p.Provision(ctx, sandbox.CreateOptions{BoxID: "a"})
	if err != nil {
		t.Fatalf("Provision a: %v", err)
	}
	b, err := p.Provision(ctx, sandbox.CreateOptions{BoxID: "b"})
	if err != nil {
		t.Fatalf("Provision b: %v", err)
	}
	ua, ub := a.(*fcInstance).meta.UID, b.(*fcInstance).meta.UID
	if ua == ub {
		t.Fatalf("boxes a and b share UID %d; want distinct", ua)
	}
	if ua < 200000 || ua > 200001 || ub < 200000 || ub > 200001 {
		t.Fatalf("UIDs %d/%d outside configured range [200000,200001]", ua, ub)
	}

	// The range is now exhausted.
	if _, err := p.Provision(ctx, sandbox.CreateOptions{BoxID: "c"}); err == nil {
		t.Fatal("Provision c succeeded with the UID range exhausted; want failure")
	}

	// Destroying a frees its UID; the next create reuses it.
	if err := a.Destroy(ctx); err != nil {
		t.Fatalf("Destroy a: %v", err)
	}
	d, err := p.Provision(ctx, sandbox.CreateOptions{BoxID: "d"})
	if err != nil {
		t.Fatalf("Provision d after freeing a UID: %v", err)
	}
	if d.(*fcInstance).meta.UID != ua {
		t.Fatalf("reused UID = %d, want freed %d", d.(*fcInstance).meta.UID, ua)
	}
	if d.(*fcInstance).meta.UID == ub {
		t.Fatalf("box d reused live box b's UID %d", ub)
	}
}

// TestBootMachineConfigIsJailed checks bootMachine attaches a per-box jailer config
// (chrooted, per-VM UID) to every machine — there is no unjailed launch — and points
// the SDK at the box's host-visible chroot socket paths.
func TestBootMachineConfigIsJailed(t *testing.T) {
	eg := &fakeEgress{}
	p, _ := newFakeProvisioner(t, eg)
	var captured fcsdk.Config
	p.newMachine = func(ctx context.Context, cfg fcsdk.Config) (machine, error) {
		captured = cfg
		return &fakeMachine{path: cfg.VsockDevices[0].Path}, nil
	}
	inst, err := p.Provision(context.Background(), sandbox.CreateOptions{BoxID: "jailed"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if captured.JailerCfg == nil {
		t.Fatal("bootMachine built a config with no JailerCfg; jailing must be mandatory")
	}
	uid := inst.(*fcInstance).meta.UID
	if captured.JailerCfg.UID == nil || *captured.JailerCfg.UID != uid {
		t.Fatalf("JailerCfg.UID = %v, want the box's UID %d", captured.JailerCfg.UID, uid)
	}
	if captured.JailerCfg.ID != inst.Meta().InstanceID {
		t.Fatalf("JailerCfg.ID = %q, want the box token %q", captured.JailerCfg.ID, inst.Meta().InstanceID)
	}
	// The SDK is pointed at the host-visible chroot paths; realMachineFactory later
	// translates them to chroot-relative basenames.
	wantVsock := inst.(*fcInstance).meta.vsockUDSPath(p.stateDir)
	if captured.VsockDevices[0].Path != wantVsock {
		t.Fatalf("vsock path = %q, want host-visible %q", captured.VsockDevices[0].Path, wantVsock)
	}
}

// TestRealMachineFactoryJailsAndTranslatesPaths checks the real machine factory
// translates the host-visible socket paths bootMachine sets into the chroot-relative
// paths the jailer expects, and that the SDK's jailer then rewrites the API socket
// back to the same host-visible chroot path the control plane dials — the contract
// that keeps the two views in agreement.
func TestRealMachineFactoryJailsAndTranslatesPaths(t *testing.T) {
	p, _ := newFakeProvisioner(t, &fakeEgress{})
	meta := boxMeta{
		Token: "tok", UID: 200000, GID: p.jailer.gid,
		ChrootBase: p.jailer.chrootBase, ExecBase: p.jailer.execBase(),
	}
	apiSock := meta.apiSockPath(p.stateDir)
	vsock := meta.vsockUDSPath(p.stateDir)
	cfg := fcsdk.Config{
		SocketPath:      apiSock,
		KernelImagePath: p.kernelImage,
		Drives: []models.Drive{{
			DriveID: fcsdk.String("rootfs"), PathOnHost: fcsdk.String("/x/rootfs.ext4"),
			IsRootDevice: fcsdk.Bool(true), IsReadOnly: fcsdk.Bool(false),
		}},
		VsockDevices:   []fcsdk.VsockDevice{{ID: "vsock0", CID: 3, Path: vsock}},
		JailerCfg:      p.jailer.jailerCfgFor(meta.UID, meta.Token, p.kernelImage),
		ForwardSignals: []os.Signal{},
	}
	m, err := p.realMachineFactory(context.Background(), cfg)
	if err != nil {
		t.Fatalf("realMachineFactory: %v", err)
	}
	fm, ok := m.(*fcsdk.Machine)
	if !ok {
		t.Fatalf("realMachineFactory returned %T, want *fcsdk.Machine", m)
	}
	// The jailer rewrote the API socket to the host-visible chroot path (what the
	// client dials) — it must match the path bootMachine computed independently.
	if fm.Cfg.SocketPath != apiSock {
		t.Fatalf("jailed SocketPath = %q, want host-visible %q", fm.Cfg.SocketPath, apiSock)
	}
	// The vsock path is now chroot-relative, so the chrooted VMM creates it at the
	// host-visible location.
	if fm.Cfg.VsockDevices[0].Path != "vsock.sock" {
		t.Fatalf("jailed vsock path = %q, want chroot-relative vsock.sock", fm.Cfg.VsockDevices[0].Path)
	}
}
