package firecracker

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fcsdk "github.com/firecracker-microvm/firecracker-go-sdk"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// shortStateDir returns a short-pathed temp dir (cleaned up on test end), so the
// per-box vsock Unix-socket path stays under the AF_UNIX length limit even when
// the default test tempdir is deep.
func shortStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fc-test-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// serveFakeVsock accepts connections on ln and answers Firecracker's CONNECT
// handshake with "OK 1", then writes any preloaded bytes so tests can assert the
// post-handshake byte pipe works. It stops when ln closes.
func serveFakeVsock(ln net.Listener, preload []byte) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			br := bufio.NewReader(c)
			if _, err := br.ReadString('\n'); err != nil {
				_ = c.Close()
				return
			}
			_, _ = c.Write([]byte("OK 1\n"))
			if len(preload) > 0 {
				_, _ = c.Write(preload)
			}
		}(c)
	}
}

// fakeMachine stands in for a booted Firecracker VM: on Start it opens a Unix
// listener at the box's vsock path speaking the CONNECT handshake, so the
// provisioner's boot-wait and Control succeed without a real microVM.
type fakeMachine struct {
	path     string
	startErr error
	ln       net.Listener
	started  bool
	stopped  bool
}

// Start opens the fake vsock listener at the box's path (or returns the injected
// startErr).
func (m *fakeMachine) Start(ctx context.Context) error {
	if m.startErr != nil {
		return m.startErr
	}
	ln, err := net.Listen("unix", m.path)
	if err != nil {
		return err
	}
	m.ln = ln
	m.started = true
	go serveFakeVsock(ln, nil)
	return nil
}

// StopVMM records the stop and closes the fake vsock listener.
func (m *fakeMachine) StopVMM() error {
	m.stopped = true
	if m.ln != nil {
		_ = m.ln.Close()
	}
	return nil
}

// Wait blocks until the context is cancelled, mimicking the VMM lifetime.
func (m *fakeMachine) Wait(ctx context.Context) error { <-ctx.Done(); return nil }

// fakeEgress records pool provisioning/teardown without touching the host.
type fakeEgress struct {
	ensures, teardowns int
	poolSize           int
	ensureErr          error
}

// EnsurePool records a pool provisioning and returns the injected ensureErr.
func (e *fakeEgress) EnsurePool(ctx context.Context, size int) error {
	e.ensures++
	e.poolSize = size
	return e.ensureErr
}

// TeardownPool records a pool teardown.
func (e *fakeEgress) TeardownPool(ctx context.Context, size int) error {
	e.teardowns++
	return nil
}

// newFakeProvisioner builds a provisioner wired to the fakes, with a real rootfs
// file to copy and a non-empty kernel path.
func newFakeProvisioner(t *testing.T, eg *fakeEgress) (*Provisioner, *[]*fakeMachine) {
	t.Helper()
	stateDir := shortStateDir(t)
	rootfs := filepath.Join(stateDir, "base-rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("rootfs-bytes"), 0o600); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}
	p, err := NewProvisioner("/fake/vmlinux", rootfs, stateDir, nil)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.egress = eg
	var machines []*fakeMachine
	p.newMachine = func(ctx context.Context, cfg fcsdk.Config) (machine, error) {
		m := &fakeMachine{path: cfg.VsockDevices[0].Path}
		machines = append(machines, m)
		return m, nil
	}
	return p, &machines
}

// TestProvisionerBookkeeping runs the full create/find/mark-ready/destroy flow with
// fakes, checking List/Find/Meta/Control and slot/egress accounting.
func TestProvisionerBookkeeping(t *testing.T) {
	eg := &fakeEgress{}
	p, machines := newFakeProvisioner(t, eg)
	ctx := context.Background()

	inst, err := p.Provision(ctx, sandbox.CreateOptions{BoxID: "book-box", Description: "d"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if eg.ensures != 1 {
		t.Fatalf("egress pool ensures = %d, want 1 (provisioned once)", eg.ensures)
	}
	b := inst.Meta()
	if b.BoxID != "book-box" || b.Phase != "pending" || b.State != "running" || b.InstanceID == "" {
		t.Fatalf("unexpected meta: %+v", b)
	}

	// Control must complete the CONNECT handshake against the fake vsock.
	conn, err := inst.Control(ctx)
	if err != nil {
		t.Fatalf("Control: %v", err)
	}
	_ = conn.Close()

	// Find by token and by box id.
	if _, err := p.Find(ctx, b.InstanceID); err != nil {
		t.Fatalf("Find by token: %v", err)
	}
	if _, err := p.Find(ctx, "book-box"); err != nil {
		t.Fatalf("Find by box id: %v", err)
	}

	if err := inst.MarkReady(ctx); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	if got := mustFind(t, p, "book-box").Meta().Phase; got != "ready" {
		t.Fatalf("phase after MarkReady = %q, want ready", got)
	}

	list, _ := p.List(ctx)
	if len(list) != 1 {
		t.Fatalf("List len = %d, want 1", len(list))
	}

	if err := inst.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	// Destroy does not tear down egress — the TAP pool is reused across boxes.
	if eg.teardowns != 0 {
		t.Fatalf("Destroy tore down the pool (%d); it should be reused", eg.teardowns)
	}
	if !(*machines)[0].stopped {
		t.Fatal("Destroy did not stop the VM")
	}
	if list, _ := p.List(ctx); len(list) != 0 {
		t.Fatalf("List after destroy = %d, want 0", len(list))
	}
	// The slot was freed, so a new box reuses it without re-provisioning the pool.
	if _, err := p.Provision(ctx, sandbox.CreateOptions{}); err != nil {
		t.Fatalf("Provision after destroy: %v", err)
	}
	if eg.ensures != 1 {
		t.Fatalf("pool re-provisioned (%d ensures); it should be once", eg.ensures)
	}
	// Close tears the pool down.
	_ = p.Close()
	if eg.teardowns != 1 {
		t.Fatalf("Close pool teardowns = %d, want 1", eg.teardowns)
	}
}

// mustFind resolves id through the provisioner or fails the test.
func mustFind(t *testing.T, p *Provisioner, id string) interface{ Meta() sandbox.Box } {
	t.Helper()
	inst, err := p.Find(context.Background(), id)
	if err != nil {
		t.Fatalf("Find %q: %v", id, err)
	}
	return inst
}

// TestProvisionCleansUpOnBootFailure checks a failed VM start rolls back the state
// dir and the slot (the pooled TAP is left intact for reuse).
func TestProvisionCleansUpOnBootFailure(t *testing.T) {
	eg := &fakeEgress{}
	p, _ := newFakeProvisioner(t, eg)
	p.newMachine = func(ctx context.Context, cfg fcsdk.Config) (machine, error) {
		return &fakeMachine{path: cfg.VsockDevices[0].Path, startErr: errors.New("boom")}, nil
	}
	if _, err := p.Provision(context.Background(), sandbox.CreateOptions{}); err == nil {
		t.Fatal("Provision should fail when the VM cannot start")
	}
	if eg.teardowns != 0 {
		t.Fatalf("boot failure tore down the pool (%d); it should persist", eg.teardowns)
	}
	if list, _ := p.List(context.Background()); len(list) != 0 {
		t.Fatalf("failed box left in registry: %d", len(list))
	}
	p.mu.Lock()
	used := len(p.used)
	p.mu.Unlock()
	if used != 0 {
		t.Fatalf("network slot not freed: %d used", used)
	}
}

// provisionCapturingCfg provisions one box through p, capturing the Firecracker
// config the box booted with (its drives, kernel args, etc.) for assertions.
func provisionCapturingCfg(t *testing.T, p *Provisioner) fcsdk.Config {
	t.Helper()
	var got fcsdk.Config
	p.newMachine = func(ctx context.Context, cfg fcsdk.Config) (machine, error) {
		got = cfg
		return &fakeMachine{path: cfg.VsockDevices[0].Path}, nil
	}
	if _, err := p.Provision(context.Background(), sandbox.CreateOptions{BoxID: "drv"}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	return got
}

// TestProvisionAttachesPayloadDrive checks that when a payload image is configured
// the box boots with a second, read-only, non-root drive pointing at the shared
// payload path unchanged — i.e. the payload is referenced, never copied per box.
func TestProvisionAttachesPayloadDrive(t *testing.T) {
	p, _ := newFakeProvisioner(t, &fakeEgress{})
	const payload = "/host/fc-assets/payload.ext4"
	p.SetPayloadImage(payload)

	cfg := provisionCapturingCfg(t, p)
	if len(cfg.Drives) != 2 {
		t.Fatalf("drives = %d, want 2 (rootfs + payload)", len(cfg.Drives))
	}
	root := cfg.Drives[0]
	if !*root.IsRootDevice || *root.IsReadOnly {
		t.Errorf("root drive = %+v, want root & writable", root)
	}
	pd := cfg.Drives[1]
	if *pd.DriveID != "payload" {
		t.Errorf("payload drive id = %q, want %q", *pd.DriveID, "payload")
	}
	if *pd.IsRootDevice {
		t.Error("payload drive marked as root device")
	}
	if !*pd.IsReadOnly {
		t.Error("payload drive not read-only")
	}
	if *pd.PathOnHost != payload {
		t.Errorf("payload PathOnHost = %q, want the shared image %q (must not be copied per box)", *pd.PathOnHost, payload)
	}
}

// TestProvisionWithoutPayloadHasSingleDrive checks the default (all-in-one) layout
// boots with just the writable root rootfs and no second drive.
func TestProvisionWithoutPayloadHasSingleDrive(t *testing.T) {
	p, _ := newFakeProvisioner(t, &fakeEgress{})

	cfg := provisionCapturingCfg(t, p)
	if len(cfg.Drives) != 1 {
		t.Fatalf("drives = %d, want 1 (rootfs only)", len(cfg.Drives))
	}
	if !*cfg.Drives[0].IsRootDevice || *cfg.Drives[0].IsReadOnly {
		t.Errorf("root drive = %+v, want root & writable", cfg.Drives[0])
	}
}

// TestProvisionerNamespaceScoping checks a namespaced provisioner only sees its own
// boxes.
func TestProvisionerNamespaceScoping(t *testing.T) {
	eg := &fakeEgress{}
	p, _ := newFakeProvisioner(t, eg)
	p.SetNamespace("ns-a")
	if _, err := p.Provision(context.Background(), sandbox.CreateOptions{BoxID: "a"}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// Inject a box belonging to another namespace directly.
	p.mu.Lock()
	p.boxes["other"] = &fcInstance{prov: p, meta: boxMeta{Token: "other", Namespace: "ns-b", Phase: "ready"}}
	p.mu.Unlock()

	list, _ := p.List(context.Background())
	if len(list) != 1 || list[0].Meta().BoxID != "a" {
		t.Fatalf("namespaced List = %+v, want only box a", list)
	}
	if _, err := p.Find(context.Background(), "other"); !errors.Is(err, sandbox.ErrBoxNotFound) {
		t.Fatalf("Find across namespace err = %v, want ErrBoxNotFound", err)
	}
}

// TestFindUnknownBox checks Find reports ErrBoxNotFound for an unknown id.
func TestFindUnknownBox(t *testing.T) {
	p, _ := newFakeProvisioner(t, &fakeEgress{})
	if _, err := p.Find(context.Background(), "nope"); !errors.Is(err, sandbox.ErrBoxNotFound) {
		t.Fatalf("Find err = %v, want ErrBoxNotFound", err)
	}
}

// TestDestroyAlreadyGone checks destroying a box not in the registry reports
// ErrBoxNotFound.
func TestDestroyAlreadyGone(t *testing.T) {
	p, _ := newFakeProvisioner(t, &fakeEgress{})
	ghost := &fcInstance{prov: p, meta: boxMeta{Token: "ghost", NetIndex: 7}}
	if err := ghost.Destroy(context.Background()); !errors.Is(err, sandbox.ErrBoxNotFound) {
		t.Fatalf("Destroy of unknown box err = %v, want ErrBoxNotFound", err)
	}
}

// TestMachineSizing checks CPU/memory limits map to a valid vCPU count and floored
// memory.
func TestMachineSizing(t *testing.T) {
	p := &Provisioner{}
	if got := p.vcpuCount(); got != defaultVcpuCount {
		t.Fatalf("default vcpu = %d, want %d", got, defaultVcpuCount)
	}
	if got := p.memSizeMib(); got != defaultMemSizeMib {
		t.Fatalf("default mem = %d, want %d", got, defaultMemSizeMib)
	}
	p.limits = sandbox.Limits{NanoCPUs: 3e9, MemoryBytes: 64 << 20}
	if got := p.vcpuCount(); got != 4 {
		t.Fatalf("vcpu for 3 CPUs = %d, want 4 (even)", got)
	}
	if got := p.memSizeMib(); got != minMemSizeMib {
		t.Fatalf("mem for 64MiB = %d, want floored to %d", got, minMemSizeMib)
	}
	p.limits = sandbox.Limits{NanoCPUs: 1e9, MemoryBytes: 1 << 30}
	if got := p.vcpuCount(); got != 1 {
		t.Fatalf("vcpu for 1 CPU = %d, want 1", got)
	}
	if got := p.memSizeMib(); got != 1024 {
		t.Fatalf("mem for 1GiB = %d, want 1024", got)
	}
}

// TestMacForIndex checks distinct indices produce distinct MACs.
func TestMacForIndex(t *testing.T) {
	if macForIndex(0) == macForIndex(1) {
		t.Fatal("MACs collide across indices")
	}
	if got := macForIndex(0x0102); got != "AA:FC:00:00:01:02" {
		t.Fatalf("mac(0x0102) = %q", got)
	}
}

// TestCopyFile checks copyFile duplicates bytes.
func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "hello" {
		t.Fatalf("copied %q, want hello", got)
	}
}

// TestWaitForGuest checks the boot-wait succeeds once a fake vsock accepts CONNECT.
func TestWaitForGuest(t *testing.T) {
	dir := shortStateDir(t)
	sock := filepath.Join(dir, "v.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go serveFakeVsock(ln, nil)
	if err := waitForGuest(context.Background(), sock, guestVsockPort, 3*time.Second); err != nil {
		t.Fatalf("waitForGuest: %v", err)
	}
}

// TestWaitForGuestTimesOut checks the boot-wait fails when nothing listens.
func TestWaitForGuestTimesOut(t *testing.T) {
	dir := shortStateDir(t)
	sock := filepath.Join(dir, "absent.sock")
	if err := waitForGuest(context.Background(), sock, guestVsockPort, 200*time.Millisecond); err == nil {
		t.Fatal("waitForGuest should fail when no guest listens")
	}
}

// TestDialVsockHandshake checks a successful CONNECT/OK exchange yields a byte pipe
// carrying bytes the peer sent right after the handshake.
func TestDialVsockHandshake(t *testing.T) {
	dir := shortStateDir(t)
	sock := filepath.Join(dir, "v.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go serveFakeVsock(ln, []byte("post-handshake"))

	conn, err := dialVsock(context.Background(), sock, guestVsockPort)
	if err != nil {
		t.Fatalf("dialVsock: %v", err)
	}
	defer conn.Close()
	buf := make([]byte, len("post-handshake"))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := readFull(conn, buf); err != nil {
		t.Fatalf("read post-handshake bytes: %v", err)
	}
	if string(buf) != "post-handshake" {
		t.Fatalf("got %q, want post-handshake", buf)
	}
}

// readFull reads exactly len(buf) bytes from c, or returns the first error.
func readFull(c net.Conn, buf []byte) (int, error) {
	got := 0
	for got < len(buf) {
		n, err := c.Read(buf[got:])
		got += n
		if err != nil {
			return got, err
		}
	}
	return got, nil
}

// TestDialVsockRejected checks a non-OK reply is surfaced as an error.
func TestDialVsockRejected(t *testing.T) {
	dir := shortStateDir(t)
	sock := filepath.Join(dir, "v.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		br := bufio.NewReader(c)
		_, _ = br.ReadString('\n')
		_, _ = c.Write([]byte("ERR no listener\n"))
		_ = c.Close()
	}()
	if _, err := dialVsock(context.Background(), sock, guestVsockPort); err == nil {
		t.Fatal("dialVsock should fail on a non-OK reply")
	}
}

// TestNetFor checks distinct slots yield distinct, well-formed /30 addressing on
// deterministic pool TAP names.
func TestNetFor(t *testing.T) {
	a := netFor(0)
	b := netFor(1)
	if a.HostIP == b.HostIP || a.GuestIP == b.GuestIP || a.TapName == b.TapName {
		t.Fatal("addresses/taps collide across slots")
	}
	if a.HostIP != "172.16.0.1" || a.GuestIP != "172.16.0.2" || a.TapName != "llmboxfc0" {
		t.Fatalf("unexpected netFor(0): %+v", a)
	}
}

// TestKernelIPArg checks the ip= boot argument renders from the addressing.
func TestKernelIPArg(t *testing.T) {
	got := netFor(5).kernelIPArg()
	want := "ip=172.16.5.2::172.16.5.1:255.255.255.252::eth0:off"
	if got != want {
		t.Fatalf("kernelIPArg = %q, want %q", got, want)
	}
}

// TestInsertVerb checks the iptables verb is spliced after an optional table prefix.
func TestInsertVerb(t *testing.T) {
	got := insertVerb([]string{"iptables", "FORWARD", "-j", "ACCEPT"}, 1, "-C")
	if strings.Join(got, " ") != "iptables -C FORWARD -j ACCEPT" {
		t.Fatalf("insertVerb (no table) = %q", got)
	}
	rule := []string{"-t", "nat", "POSTROUTING", "-j", "MASQUERADE"}
	got = insertVerb(append([]string{"iptables"}, rule...), delInsertAt(rule), "-D")
	if strings.Join(got, " ") != "iptables -t nat -D POSTROUTING -j MASQUERADE" {
		t.Fatalf("insertVerb (table) = %q", got)
	}
}

// TestRunReportsFailure checks run surfaces a command's output on failure.
func TestRunReportsFailure(t *testing.T) {
	err := run(context.Background(), "sh", "-c", "echo boom >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("run err = %v, want it to include boom", err)
	}
}

// TestHostEgressPoolSkipsWithoutRoot exercises the real pool provisioning only when
// ip/iptables exist and the test runs as root; otherwise it is skipped. It uses a
// tiny pool and tears it down so it leaves no devices behind.
func TestHostEgressPoolSkipsWithoutRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("real egress pool needs root (CAP_NET_ADMIN)")
	}
	if _, err := exec.LookPath("iptables"); err != nil {
		t.Skip("iptables not available")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("ip not available")
	}
	e := &hostEgress{}
	const size = 2
	if err := e.EnsurePool(context.Background(), size); err != nil {
		t.Fatalf("EnsurePool: %v", err)
	}
	// Idempotent: a second call must not error or duplicate rules.
	if err := e.EnsurePool(context.Background(), size); err != nil {
		t.Fatalf("EnsurePool (second): %v", err)
	}
	if err := e.TeardownPool(context.Background(), size); err != nil {
		t.Fatalf("TeardownPool: %v", err)
	}
}

// TestSaveLoadMeta round-trips box metadata through the state dir and maps it to a
// box view.
func TestSaveLoadMeta(t *testing.T) {
	dir := t.TempDir()
	m := boxMeta{Token: "tok1", BoxID: "b", Phase: "pending", Created: 42, NetIndex: 3}
	if err := m.save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}
	metas, err := loadMetas(dir)
	if err != nil {
		t.Fatalf("loadMetas: %v", err)
	}
	if len(metas) != 1 || metas[0].Token != "tok1" {
		t.Fatalf("loaded %+v, want one meta tok1", metas)
	}
	b := metas[0].toBox("running")
	if b.InstanceID != "tok1" || b.Name != "llmbox-pending-tok1" || b.State != "running" {
		t.Fatalf("toBox = %+v", b)
	}
}

// TestLoadMetasSkipsJunk checks non-box entries and a missing dir are ignored.
func TestLoadMetasSkipsJunk(t *testing.T) {
	if metas, err := loadMetas(filepath.Join(t.TempDir(), "absent")); err != nil || metas != nil {
		t.Fatalf("loadMetas(absent) = %v, %v, want nil,nil", metas, err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "loose-file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "not-a-box"), 0o700); err != nil {
		t.Fatal(err)
	}
	metas, err := loadMetas(dir)
	if err != nil || len(metas) != 0 {
		t.Fatalf("loadMetas = %v, %v, want no metas", metas, err)
	}
}
