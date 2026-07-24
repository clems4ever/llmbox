package cloudhypervisor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/box/backend"
)

// fakeEgress records pool operations instead of touching the host network stack, so
// the provisioner's egress wiring is testable in CI.
type fakeEgress struct {
	ensures, validates, teardowns int
	poolSize                      int
	ensureErr, validateErr        error
}

func (e *fakeEgress) EnsurePool(ctx context.Context, size int) error {
	e.ensures++
	e.poolSize = size
	return e.ensureErr
}

func (e *fakeEgress) ValidatePool(ctx context.Context, size int) error {
	e.validates++
	e.poolSize = size
	return e.validateErr
}

func (e *fakeEgress) TeardownPool(ctx context.Context, size int) error {
	e.teardowns++
	return nil
}

// TestEnsureNetworkModes checks each egress mode drives the right pool operation:
// disabled does nothing, external only validates (never mutates), managed provisions.
func TestEnsureNetworkModes(t *testing.T) {
	newProv := func(t *testing.T, mode egressMode, eg *fakeEgress) *Provisioner {
		p, err := NewProvisioner("k", "r", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		p.SetEgress(eg)
		p.SetEgressMode(mode)
		return p
	}

	dis := &fakeEgress{}
	if err := newProv(t, egressDisabled, dis).EnsureNetwork(context.Background()); err != nil {
		t.Fatalf("disabled EnsureNetwork: %v", err)
	}
	if dis.ensures != 0 || dis.validates != 0 {
		t.Fatalf("disabled mode touched the pool: %+v", dis)
	}

	ext := &fakeEgress{}
	if err := newProv(t, egressExternal, ext).EnsureNetwork(context.Background()); err != nil {
		t.Fatalf("external EnsureNetwork: %v", err)
	}
	if ext.validates != 1 || ext.ensures != 0 {
		t.Fatalf("external mode should validate only: %+v", ext)
	}

	man := &fakeEgress{}
	if err := newProv(t, egressManaged, man).EnsureNetwork(context.Background()); err != nil {
		t.Fatalf("managed EnsureNetwork: %v", err)
	}
	if man.ensures != 1 || man.validates != 0 {
		t.Fatalf("managed mode should provision: %+v", man)
	}

	// A validation failure in external mode must surface (fail closed on a missing TAP).
	bad := &fakeEgress{validateErr: errors.New("tap missing")}
	if err := newProv(t, egressExternal, bad).EnsureNetwork(context.Background()); err == nil || !strings.Contains(err.Error(), "tap missing") {
		t.Fatalf("external validation failure should surface: %v", err)
	}
}

// TestProvisionNetworkedBox checks a networked box reserves a pooled slot, carries the
// egress addressing into its VM spec (TAP name, deterministic MAC, guest ip= arg), and
// frees the slot on destroy.
func TestProvisionNetworkedBox(t *testing.T) {
	p, err := NewProvisioner("k", "r", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p.launcher = &fakeLauncher{}
	p.SetEgress(&fakeEgress{}) // managed is the default; the fake avoids touching the host
	ctx := context.Background()

	b, err := p.Provision(ctx, sandbox.CreateOptions{BoxID: "net-box"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	inst := b.(*chInstance)
	if !inst.meta.Egress || inst.meta.NetIndex != 0 {
		t.Fatalf("networked box should hold slot 0: %+v", inst.meta)
	}
	spec := p.specFor(inst.meta)
	if spec.TapName != "llmboxch0" {
		t.Errorf("spec TapName = %q, want llmboxch0", spec.TapName)
	}
	if spec.MAC == "" || !strings.Contains(spec.IPArg, "172.17.0.2") {
		t.Errorf("spec egress addressing missing: mac=%q iparg=%q", spec.MAC, spec.IPArg)
	}

	// The VM config carries the net device and the ip= arg on the cmdline.
	cfg := buildVMConfig(vmConfigParams{Kernel: "k", Rootfs: "r", VsockUDS: "v", VCPUs: 1, TapName: spec.TapName, MAC: spec.MAC, IPArg: spec.IPArg})
	if len(cfg.Net) != 1 || cfg.Net[0].Tap != "llmboxch0" {
		t.Errorf("VM config net device missing: %+v", cfg.Net)
	}
	if !strings.Contains(cfg.Payload.Cmdline, "172.17.0.2") {
		t.Errorf("VM config cmdline missing guest ip=: %q", cfg.Payload.Cmdline)
	}

	if err := b.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	p.mu.Lock()
	slots := len(p.used)
	p.mu.Unlock()
	if slots != 0 {
		t.Errorf("destroy should free the pooled slot, %d still held", slots)
	}
}

// TestResolveEgressMode maps the neutral flags to a mode and rejects the contradiction
// of --disable-egress with a non-disabled --egress-mode.
func TestResolveEgressMode(t *testing.T) {
	cases := []struct {
		opts backend.Options
		want egressMode
	}{
		{backend.Options{}, egressManaged},
		{backend.Options{DisableEgress: true}, egressDisabled},
		{backend.Options{EgressMode: "external"}, egressExternal},
		{backend.Options{EgressMode: "disabled"}, egressDisabled},
	}
	for _, c := range cases {
		got, err := resolveEgressMode(c.opts)
		if err != nil || got != c.want {
			t.Errorf("resolveEgressMode(%+v) = %v, %v; want %v", c.opts, got, err, c.want)
		}
	}
	if _, err := resolveEgressMode(backend.Options{EgressMode: "external", DisableEgress: true}); err == nil {
		t.Error("--disable-egress with --egress-mode=external should conflict")
	}
	if _, err := resolveEgressMode(backend.Options{EgressMode: "bogus"}); err == nil {
		t.Error("an unknown egress mode should error")
	}
}
