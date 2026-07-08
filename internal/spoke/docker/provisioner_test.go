package docker

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/registry"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// fakeDocker is a recording stand-in for the Docker client. On ContainerStart it
// stands up a real Unix listener at the box's bind-mounted socket path, so the
// agent socket Provision waits for actually appears and Control can dial it.
type fakeDocker struct {
	mu sync.Mutex

	createCfg  *container.Config
	createHost *container.HostConfig
	createID   string
	createErr  error
	// notFoundOnce makes the first ContainerCreate report the image missing, so
	// the pull-and-retry path runs.
	notFoundOnce bool
	createCalls  int

	startErr   error
	startID    string
	skipSocket bool // when set, ContainerStart does not create the agent socket

	renames     [][2]string
	renameErr   error
	stopped     []string
	removed     []string
	stopErr     error
	removeErr   error
	stopMissing bool

	netCreated    []string
	netCreateErr  error
	netConnect    [][2]string
	netDisconn    [][2]string
	netDisconnErr error
	netRemoved    []string
	netRemoveErr  error
	pulled        []string
	pullErr       error
	pullAuthSeen  string

	listResult      []container.Summary
	listErr         error
	netConnectCalls int
	peerConnErr     error

	mountSource string
	listeners   []net.Listener
}

// ContainerCreate records the create/host config and returns a canned ID, reporting the image missing once when notFoundOnce is set.
func (f *fakeDocker) ContainerCreate(_ context.Context, cfg *container.Config, host *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	if f.notFoundOnce && f.createCalls == 1 {
		return container.CreateResponse{}, errdefs.ErrNotFound.WithMessage("no such image")
	}
	if f.createErr != nil {
		return container.CreateResponse{}, f.createErr
	}
	f.createCfg, f.createHost = cfg, host
	if len(host.Mounts) > 0 {
		f.mountSource = host.Mounts[0].Source
	}
	id := f.createID
	if id == "" {
		id = "0123456789abcdeffull"
	}
	return container.CreateResponse{ID: id}, nil
}

// ContainerStart records the start and stands up a real Unix listener at the box's bind-mounted socket path so Provision's socket wait and Control succeed.
func (f *fakeDocker) ContainerStart(_ context.Context, id string, _ container.StartOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startID = id
	if f.startErr != nil {
		return f.startErr
	}
	// Mimic the agent creating its control socket once it is listening.
	if f.mountSource != "" && !f.skipSocket {
		ln, err := net.Listen("unix", filepath.Join(f.mountSource, socketFileName))
		if err == nil {
			f.listeners = append(f.listeners, ln)
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					_ = c.Close()
				}
			}()
		}
	}
	return nil
}

// closeListeners closes the sockets ContainerStart opened.
func (f *fakeDocker) closeListeners() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ln := range f.listeners {
		_ = ln.Close()
	}
}

// ContainerStop records the stop, or reports the container missing when stopMissing is set.
func (f *fakeDocker) ContainerStop(_ context.Context, id string, _ container.StopOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopMissing {
		return errdefs.ErrNotFound.WithMessage("no such container")
	}
	f.stopped = append(f.stopped, id)
	return f.stopErr
}

// ContainerRemove records the removed container ID.
func (f *fakeDocker) ContainerRemove(_ context.Context, id string, _ container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	return f.removeErr
}

// ContainerRename records the rename (old, new).
func (f *fakeDocker) ContainerRename(_ context.Context, id, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renames = append(f.renames, [2]string{id, name})
	return f.renameErr
}

// ImagePull records the pulled ref and the encoded auth header it carried.
func (f *fakeDocker) ImagePull(_ context.Context, ref string, opts image.PullOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulled = append(f.pulled, ref)
	f.pullAuthSeen = opts.RegistryAuth
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	return io.NopCloser(strings.NewReader("")), nil
}

// ContainerList returns the canned container summaries.
func (f *fakeDocker) ContainerList(_ context.Context, _ container.ListOptions) ([]container.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listResult, f.listErr
}

// NetworkCreate records the created network name.
func (f *fakeDocker) NetworkCreate(_ context.Context, name string, _ network.CreateOptions) (network.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.netCreated = append(f.netCreated, name)
	return network.CreateResponse{}, f.netCreateErr
}

// NetworkConnect records the (network, container) connection.
func (f *fakeDocker) NetworkConnect(_ context.Context, net, container string, _ *network.EndpointSettings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.netConnect = append(f.netConnect, [2]string{net, container})
	f.netConnectCalls++
	if f.netConnectCalls > 1 {
		return f.peerConnErr
	}
	return nil
}

// NetworkDisconnect records the (network, container) disconnection.
func (f *fakeDocker) NetworkDisconnect(_ context.Context, net, container string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.netDisconn = append(f.netDisconn, [2]string{net, container})
	return f.netDisconnErr
}

// NetworkRemove records the removed network name.
func (f *fakeDocker) NetworkRemove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.netRemoved = append(f.netRemoved, name)
	return f.netRemoveErr
}

// Close is a no-op for the fake.
func (f *fakeDocker) Close() error { return nil }

// newTestProvisioner builds a Provisioner over the fake with a temp socket dir.
func newTestProvisioner(t *testing.T, f *fakeDocker) *Provisioner {
	t.Helper()
	t.Cleanup(f.closeListeners)
	return &Provisioner{cli: f, defaultImage: "test-image", socketDir: t.TempDir()}
}

// TestProvisionCreatesAgentBox creates an agent-entrypoint box with the socket
// mount and restart policy, names it pending, and exposes a dialable control
// socket.
func TestProvisionCreatesAgentBox(t *testing.T) {
	f := &fakeDocker{}
	p := newTestProvisioner(t, f)

	inst, err := p.Provision(context.Background(), sandbox.CreateOptions{BoxID: "my-box"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if got := f.createCfg.Entrypoint; len(got) < 4 || got[0] != "tini" || got[3] != "llmbox-agent" {
		t.Fatalf("entrypoint = %v, want tini ... llmbox-agent", got)
	}
	if f.createHost.RestartPolicy.Name != container.RestartPolicyUnlessStopped {
		t.Fatalf("restart policy = %q, want unless-stopped", f.createHost.RestartPolicy.Name)
	}
	if len(f.createHost.Mounts) != 1 || f.createHost.Mounts[0].Target != socketMountTarget {
		t.Fatalf("mounts = %+v, want one bind to %s", f.createHost.Mounts, socketMountTarget)
	}
	if inst.Meta().Phase != "pending" || inst.Meta().BoxID != "my-box" {
		t.Fatalf("meta = %+v, want pending my-box", inst.Meta())
	}

	conn, err := inst.Control(context.Background())
	if err != nil {
		t.Fatalf("Control: %v", err)
	}
	_ = conn.Close()
}

// TestProvisionSetsNamespaceLabel stamps a namespaced provisioner's namespace on
// the box container and its network, and leaves an unscoped provisioner's box
// carrying no NamespaceLabel.
func TestProvisionSetsNamespaceLabel(t *testing.T) {
	f := &fakeDocker{}
	p := newTestProvisioner(t, f)
	p.SetNamespace("spoke-a")

	if _, err := p.Provision(context.Background(), sandbox.CreateOptions{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if got := f.createCfg.Labels[NamespaceLabel]; got != "spoke-a" {
		t.Fatalf("box %s label = %q, want %q", NamespaceLabel, got, "spoke-a")
	}

	f2 := &fakeDocker{}
	p2 := newTestProvisioner(t, f2)
	if _, err := p2.Provision(context.Background(), sandbox.CreateOptions{}); err != nil {
		t.Fatalf("Provision (unscoped): %v", err)
	}
	if _, ok := f2.createCfg.Labels[NamespaceLabel]; ok {
		t.Fatalf("unscoped box should carry no %s label, got %v", NamespaceLabel, f2.createCfg.Labels)
	}
}

// TestManagedFilterScopesByNamespace adds the namespace label to list/find
// filters only when the provisioner is namespaced.
func TestManagedFilterScopesByNamespace(t *testing.T) {
	unscoped := (&Provisioner{}).managedFilter()
	if got := unscoped.Get("label"); len(got) != 1 || got[0] != ManagedLabel+"=true" {
		t.Fatalf("unscoped filter labels = %v, want just the managed label", got)
	}

	scoped := (&Provisioner{namespace: "spoke-b"}).managedFilter()
	labels := scoped.Get("label")
	var sawManaged, sawNamespace bool
	for _, l := range labels {
		switch l {
		case ManagedLabel + "=true":
			sawManaged = true
		case NamespaceLabel + "=spoke-b":
			sawNamespace = true
		}
	}
	if !sawManaged || !sawNamespace {
		t.Fatalf("scoped filter labels = %v, want both managed and namespace labels", labels)
	}
}

// TestProvisionCleansUpOnStartFailure removes the container, network, and socket
// dir when start fails.
func TestProvisionCleansUpOnStartFailure(t *testing.T) {
	f := &fakeDocker{startErr: errors.New("boom")}
	p := newTestProvisioner(t, f)

	_, err := p.Provision(context.Background(), sandbox.CreateOptions{})
	if err == nil {
		t.Fatal("Provision should fail when start fails")
	}
	if len(f.removed) == 0 {
		t.Fatal("container should be removed on start failure")
	}
	entries, _ := os.ReadDir(p.socketDir)
	if len(entries) != 0 {
		t.Fatalf("socket dir should be cleaned up, found %d entries", len(entries))
	}
}

// TestProvisionAppliesLimits applies the configured resource caps and
// no-new-privileges.
func TestProvisionAppliesLimits(t *testing.T) {
	f := &fakeDocker{}
	p := newTestProvisioner(t, f)
	pids := int64(256)
	p.SetPerBoxLimits(sandbox.Limits{MemoryBytes: 512 << 20, NanoCPUs: 1_500_000_000, PidsLimit: pids})

	if _, err := p.Provision(context.Background(), sandbox.CreateOptions{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	h := f.createHost
	if h.Memory != 512<<20 || h.NanoCPUs != 1_500_000_000 || h.PidsLimit == nil || *h.PidsLimit != pids {
		t.Fatalf("limits not applied: mem=%d cpu=%d pids=%v", h.Memory, h.NanoCPUs, h.PidsLimit)
	}
	if len(h.SecurityOpt) != 1 || h.SecurityOpt[0] != "no-new-privileges" {
		t.Fatalf("security opt = %v, want no-new-privileges", h.SecurityOpt)
	}
}

// TestProvisionAppliesGPUs attaches the configured GPU device requests.
func TestProvisionAppliesGPUs(t *testing.T) {
	f := &fakeDocker{}
	p := newTestProvisioner(t, f)
	if err := p.SetBoxGPUs("all"); err != nil {
		t.Fatalf("SetBoxGPUs: %v", err)
	}
	if _, err := p.Provision(context.Background(), sandbox.CreateOptions{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if len(f.createHost.DeviceRequests) != 1 || f.createHost.DeviceRequests[0].Count != -1 {
		t.Fatalf("device requests = %+v, want one all-GPU request", f.createHost.DeviceRequests)
	}
}

// TestProvisionLaunchesDefaultImage checks a create launches the spoke's own
// configured image — the request carries none, so the box always runs the
// provisioner's default.
func TestProvisionLaunchesDefaultImage(t *testing.T) {
	f := &fakeDocker{}
	p := newTestProvisioner(t, f)
	p.defaultImage = "ghcr.io/clems4ever/llmbox-box:pinned"
	if _, err := p.Provision(context.Background(), sandbox.CreateOptions{BoxID: "b1"}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if f.createCfg == nil || f.createCfg.Image != "ghcr.io/clems4ever/llmbox-box:pinned" {
		t.Fatalf("container created with image %v, want the spoke's configured default", f.createCfg)
	}
}

// TestProvisionPullsMissingImage pulls the image then retries when it is absent.
func TestProvisionPullsMissingImage(t *testing.T) {
	f := &fakeDocker{notFoundOnce: true}
	p := newTestProvisioner(t, f)
	p.defaultImage = "ghcr.io/x/y:latest"
	if _, err := p.Provision(context.Background(), sandbox.CreateOptions{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if len(f.pulled) != 1 || f.pulled[0] != "ghcr.io/x/y:latest" {
		t.Fatalf("pulled = %v, want the missing image pulled once", f.pulled)
	}
	if f.createCalls != 2 {
		t.Fatalf("create calls = %d, want 2 (initial + retry)", f.createCalls)
	}
}

// TestProvisionPullsWithRegistryAuth matches an image to its registry
// credentials when pulling.
func TestProvisionPullsWithRegistryAuth(t *testing.T) {
	f := &fakeDocker{notFoundOnce: true}
	p := newTestProvisioner(t, f)
	p.defaultImage = "ghcr.io/x/y:latest"
	p.SetRegistryAuths(map[string]registry.AuthConfig{"ghcr.io": {Username: "u", Password: "p"}})
	if _, err := p.Provision(context.Background(), sandbox.CreateOptions{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if f.pullAuthSeen == "" {
		t.Fatal("pull should carry an encoded registry auth header")
	}
}

// TestProvisionConnectsPeers creates the box network, connects the box and peers,
// and detaches the default bridge.
func TestProvisionConnectsPeers(t *testing.T) {
	f := &fakeDocker{}
	p := newTestProvisioner(t, f)
	p.peers = []string{"resource-a"}
	if _, err := p.Provision(context.Background(), sandbox.CreateOptions{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if len(f.netCreated) != 1 {
		t.Fatalf("networks created = %v, want one", f.netCreated)
	}
	var connectedPeer bool
	for _, c := range f.netConnect {
		if c[1] == "resource-a" {
			connectedPeer = true
		}
	}
	if !connectedPeer {
		t.Fatalf("peer not connected: %v", f.netConnect)
	}
	var detached bool
	for _, d := range f.netDisconn {
		if d[0] == defaultBridgeNetwork {
			detached = true
		}
	}
	if !detached {
		t.Fatalf("box not detached from default bridge: %v", f.netDisconn)
	}
}

// TestListMapsManagedContainers checks phase, container ID, box ID, and the
// managed filter mapping from a container summary.
func TestListMapsManagedContainers(t *testing.T) {
	f := &fakeDocker{listResult: []container.Summary{{
		ID:     "abcdef0123456789",
		Names:  []string{"/" + readyPrefix + "abcdef012345"},
		Labels: map[string]string{ManagedLabel: "true", BoxIDLabel: "b1", socketLabel: "tok1"},
		Image:  "img",
		State:  "running",
	}}}
	p := newTestProvisioner(t, f)
	insts, err := p.List(context.Background())
	if err != nil || len(insts) != 1 {
		t.Fatalf("List = %v, %v", insts, err)
	}
	b := insts[0].Meta()
	if b.BoxID != "b1" || b.Phase != "ready" || b.InstanceID != "abcdef012345" {
		t.Fatalf("meta = %+v", b)
	}
}

// TestFindResolvesByIDAndBoxID resolves a box by its short id and by its box id.
func TestFindResolvesByIDAndBoxID(t *testing.T) {
	f := &fakeDocker{listResult: []container.Summary{{
		ID:     "abcdef0123456789",
		Names:  []string{"/" + pendingPrefix + "abcdef012345"},
		Labels: map[string]string{ManagedLabel: "true", BoxIDLabel: "mybox", socketLabel: "tok"},
	}}}
	p := newTestProvisioner(t, f)
	if _, err := p.Find(context.Background(), "abcdef012345"); err != nil {
		t.Fatalf("Find by id: %v", err)
	}
	if _, err := p.Find(context.Background(), "mybox"); err != nil {
		t.Fatalf("Find by box id: %v", err)
	}
}

// TestFindUnknownBox errors with sandbox.ErrBoxNotFound when no managed box matches.
func TestFindUnknownBox(t *testing.T) {
	p := newTestProvisioner(t, &fakeDocker{})
	if _, err := p.Find(context.Background(), "nope"); !errors.Is(err, sandbox.ErrBoxNotFound) {
		t.Fatalf("err = %v, want sandbox.ErrBoxNotFound", err)
	}
}

// TestSetBoxGPUsParsesSpec accepts all/count/device-list specs and rejects bad
// ones.
func TestSetBoxGPUsParsesSpec(t *testing.T) {
	p := newTestProvisioner(t, &fakeDocker{})
	for _, spec := range []string{"", "all", "2", "device=0,1", "GPU-abc123"} {
		if err := p.SetBoxGPUs(spec); err != nil {
			t.Errorf("SetBoxGPUs(%q) = %v, want nil", spec, err)
		}
	}
	for _, spec := range []string{"0", "-1"} {
		if err := p.SetBoxGPUs(spec); err == nil {
			t.Errorf("SetBoxGPUs(%q) = nil, want error", spec)
		}
	}
}

// TestMarkReadyRenamesContainer renames the box to the ready prefix.
func TestMarkReadyRenamesContainer(t *testing.T) {
	f := &fakeDocker{}
	p := newTestProvisioner(t, f)
	inst := &dockerInstance{prov: p, box: sandbox.Box{InstanceID: "abcdef012345"}}
	if err := inst.MarkReady(context.Background()); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	if len(f.renames) != 1 || f.renames[0][1] != readyPrefix+"abcdef012345" {
		t.Fatalf("renames = %v, want rename to ready prefix", f.renames)
	}
}

// TestDestroyRemovesNetworkAndSocket stops the box, removes its network, and
// deletes its socket dir.
func TestDestroyRemovesNetworkAndSocket(t *testing.T) {
	f := &fakeDocker{}
	p := newTestProvisioner(t, f)
	tokenDir := filepath.Join(p.socketDir, "tok9")
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	inst := &dockerInstance{prov: p, box: sandbox.Box{InstanceID: "abcdef012345"}, socketToken: "tok9"}
	if err := inst.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(f.stopped) != 1 || len(f.removed) != 1 {
		t.Fatalf("want one stop and one remove, got stop=%v remove=%v", f.stopped, f.removed)
	}
	if len(f.netRemoved) != 1 {
		t.Fatalf("network not removed: %v", f.netRemoved)
	}
	if _, err := os.Stat(tokenDir); !os.IsNotExist(err) {
		t.Fatalf("socket dir should be removed, stat err = %v", err)
	}
}

// TestDestroyAlreadyGone reports sandbox.ErrBoxNotFound when the container is missing.
func TestDestroyAlreadyGone(t *testing.T) {
	f := &fakeDocker{stopMissing: true}
	p := newTestProvisioner(t, f)
	inst := &dockerInstance{prov: p, box: sandbox.Box{InstanceID: "abcdef012345"}}
	if err := inst.Destroy(context.Background()); !errors.Is(err, sandbox.ErrBoxNotFound) {
		t.Fatalf("err = %v, want sandbox.ErrBoxNotFound", err)
	}
}

// TestIsNotFound recognizes the sentinel, a wrapped error, a wire string, and
// rejects others.
func TestIsNotFound(t *testing.T) {
	if !IsNotFound(sandbox.ErrBoxNotFound) {
		t.Error("sentinel should be not-found")
	}
	if !IsNotFound(errors.New(sandbox.ErrBoxNotFound.Error() + " \"x\"")) {
		t.Error("wire string should be not-found")
	}
	if IsNotFound(errors.New("other")) {
		t.Error("unrelated error should not be not-found")
	}
	if IsNotFound(nil) {
		t.Error("nil should not be not-found")
	}
}

// TestNewProvisionerDefaults applies the default image and socket dir, and Close
// releases the client.
func TestNewProvisionerDefaults(t *testing.T) {
	p, err := NewProvisioner("", "", nil)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	if p.defaultImage != DefaultImage || p.socketDir != DefaultSocketDir {
		t.Fatalf("defaults not applied: image=%q dir=%q", p.defaultImage, p.socketDir)
	}
	if p.logger() == nil {
		t.Fatal("logger should never be nil")
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestProvisionNetworkSetupFailure cleans up the container and socket dir when
// the box network cannot be created.
func TestProvisionNetworkSetupFailure(t *testing.T) {
	f := &fakeDocker{netCreateErr: errors.New("net boom")}
	p := newTestProvisioner(t, f)
	if _, err := p.Provision(context.Background(), sandbox.CreateOptions{}); err == nil {
		t.Fatal("Provision should fail when the network cannot be created")
	}
	if len(f.removed) == 0 {
		t.Fatal("container should be removed on network failure")
	}
	if entries, _ := os.ReadDir(p.socketDir); len(entries) != 0 {
		t.Fatalf("socket dir should be cleaned up, found %d entries", len(entries))
	}
}

// TestProvisionRenameFailure cleans up when the box cannot be named pending.
func TestProvisionRenameFailure(t *testing.T) {
	f := &fakeDocker{renameErr: errors.New("rename boom")}
	p := newTestProvisioner(t, f)
	if _, err := p.Provision(context.Background(), sandbox.CreateOptions{}); err == nil {
		t.Fatal("Provision should fail when the rename fails")
	}
	if len(f.removed) == 0 {
		t.Fatal("container should be removed on rename failure")
	}
}

// TestProvisionSocketTimeout cleans up when the agent socket never appears.
func TestProvisionSocketTimeout(t *testing.T) {
	f := &fakeDocker{skipSocket: true}
	p := newTestProvisioner(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := p.Provision(ctx, sandbox.CreateOptions{}); err == nil {
		t.Fatal("Provision should fail when the agent socket never appears")
	}
	if len(f.removed) == 0 {
		t.Fatal("container should be removed when the socket never appears")
	}
}

// TestProvisionPullFailure surfaces a pull error from the missing-image path.
func TestProvisionPullFailure(t *testing.T) {
	f := &fakeDocker{notFoundOnce: true, pullErr: errors.New("pull boom")}
	p := newTestProvisioner(t, f)
	p.defaultImage = "ghcr.io/x/y:latest"
	if _, err := p.Provision(context.Background(), sandbox.CreateOptions{}); err == nil {
		t.Fatal("Provision should fail when the image pull fails")
	}
	if entries, _ := os.ReadDir(p.socketDir); len(entries) != 0 {
		t.Fatalf("socket dir should be cleaned up, found %d entries", len(entries))
	}
}

// TestDestroyRemoveError surfaces a non-not-found remove error.
func TestDestroyRemoveError(t *testing.T) {
	f := &fakeDocker{removeErr: errors.New("remove boom")}
	p := newTestProvisioner(t, f)
	inst := &dockerInstance{prov: p, box: sandbox.Box{InstanceID: "abcdef012345"}}
	if err := inst.Destroy(context.Background()); err == nil {
		t.Fatal("Destroy should surface a remove error")
	}
}

// TestDestroyToleratesNetworkErrors removes the box even when peer disconnect and
// network removal fail (best-effort), and still deletes the socket dir.
func TestDestroyToleratesNetworkErrors(t *testing.T) {
	f := &fakeDocker{netDisconnErr: errors.New("disc boom"), netRemoveErr: errors.New("rm boom")}
	p := newTestProvisioner(t, f)
	p.peers = []string{"peer"}
	tokenDir := filepath.Join(p.socketDir, "tokX")
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	inst := &dockerInstance{prov: p, box: sandbox.Box{InstanceID: "abcdef012345"}, socketToken: "tokX"}
	if err := inst.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy should tolerate network errors, got %v", err)
	}
	if _, err := os.Stat(tokenDir); !os.IsNotExist(err) {
		t.Fatalf("socket dir should be removed, stat err = %v", err)
	}
}

// TestMarkReadyError surfaces a rename error.
func TestMarkReadyError(t *testing.T) {
	f := &fakeDocker{renameErr: errors.New("rename boom")}
	p := newTestProvisioner(t, f)
	inst := &dockerInstance{prov: p, box: sandbox.Box{InstanceID: "abcdef012345"}}
	if err := inst.MarkReady(context.Background()); err == nil {
		t.Fatal("MarkReady should surface a rename error")
	}
}

// TestRegistryAuthFor matches by host, returns false when none configured, and
// returns false for an unparseable reference.
func TestRegistryAuthFor(t *testing.T) {
	p := newTestProvisioner(t, &fakeDocker{})
	if _, ok := p.registryAuthFor("ghcr.io/x/y"); ok {
		t.Fatal("no auths configured should not match")
	}
	p.SetRegistryAuths(map[string]registry.AuthConfig{"ghcr.io": {Username: "u"}})
	if _, ok := p.registryAuthFor("ghcr.io/x/y:latest"); !ok {
		t.Fatal("should match the configured ghcr.io host")
	}
	if _, ok := p.registryAuthFor("not a valid ref!!"); ok {
		t.Fatal("an unparseable ref should not match")
	}
}

// TestListError surfaces a Docker list error through List and Find.
func TestListError(t *testing.T) {
	f := &fakeDocker{listErr: errors.New("list boom")}
	p := newTestProvisioner(t, f)
	if _, err := p.List(context.Background()); err == nil {
		t.Fatal("List should surface the Docker error")
	}
	if _, err := p.Find(context.Background(), "x"); err == nil {
		t.Fatal("Find should surface the Docker list error")
	}
}

// TestProvisionToleratesPeerConnectFailure provisions the box even when a peer
// cannot be connected to its network (best-effort).
func TestProvisionToleratesPeerConnectFailure(t *testing.T) {
	f := &fakeDocker{peerConnErr: errors.New("peer boom")}
	p := newTestProvisioner(t, f)
	p.peers = []string{"peer"}
	if _, err := p.Provision(context.Background(), sandbox.CreateOptions{}); err != nil {
		t.Fatalf("Provision should tolerate a peer connect failure, got %v", err)
	}
}

// TestWaitForSocketCancel returns the context error when ctx is cancelled before
// the socket appears.
func TestWaitForSocketCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForSocket(ctx, filepath.Join(t.TempDir(), "nope.sock"), time.Minute); err == nil {
		t.Fatal("waitForSocket should fail on a cancelled context")
	}
}
