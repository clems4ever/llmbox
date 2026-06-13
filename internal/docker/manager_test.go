package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// fakeDocker is an in-memory stand-in for the Docker client. Each field lets a
// test stub a method's behaviour; every call is recorded for assertions.
type fakeDocker struct {
	// stubbed returns
	listResult []container.Summary
	listErr    error
	createResp container.CreateResponse
	createErr  error
	startErr   error
	removeErr  error

	// recorded calls
	listOpts     []container.ListOptions
	createConfig *container.Config
	createHost   *container.HostConfig
	createName   string
	started      []string
	removed      []string
	removeOpts   []container.RemoveOptions
}

func (f *fakeDocker) ContainerList(_ context.Context, opts container.ListOptions) ([]container.Summary, error) {
	f.listOpts = append(f.listOpts, opts)
	return f.listResult, f.listErr
}

func (f *fakeDocker) ContainerCreate(_ context.Context, config *container.Config, hostConfig *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	f.createConfig = config
	f.createHost = hostConfig
	f.createName = name
	return f.createResp, f.createErr
}

func (f *fakeDocker) ContainerStart(_ context.Context, id string, _ container.StartOptions) error {
	f.started = append(f.started, id)
	return f.startErr
}

func (f *fakeDocker) ContainerRemove(_ context.Context, id string, opts container.RemoveOptions) error {
	f.removed = append(f.removed, id)
	f.removeOpts = append(f.removeOpts, opts)
	return f.removeErr
}

func (f *fakeDocker) Close() error { return nil }

// newTestManager wires a Manager to a fake Docker client.
func newTestManager(f *fakeDocker, defaultImage string) *Manager {
	if defaultImage == "" {
		defaultImage = DefaultImage
	}
	return &Manager{cli: f, defaultImage: defaultImage}
}

func TestNewManagerDefaultImage(t *testing.T) {
	// NewManager talks to the daemon lazily, so it succeeds without one.
	m, err := NewManager("")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()
	if m.defaultImage != DefaultImage {
		t.Errorf("defaultImage = %q, want %q", m.defaultImage, DefaultImage)
	}

	m2, err := NewManager("my/image:tag")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m2.Close()
	if m2.defaultImage != "my/image:tag" {
		t.Errorf("defaultImage = %q, want override", m2.defaultImage)
	}
}

func TestList(t *testing.T) {
	f := &fakeDocker{
		listResult: []container.Summary{
			{
				ID:      "abcdef0123456789",
				Names:   []string{"/claude-box-1"},
				Image:   "claude-remote",
				State:   "running",
				Status:  "Up 2 minutes",
				Created: 1700000000,
				Ports: []container.Port{
					{IP: "0.0.0.0", PrivatePort: 8080, PublicPort: 32768, Type: "tcp"},
					{PrivatePort: 9090, Type: "tcp"}, // not published -> skipped
				},
			},
		},
	}
	m := newTestManager(f, "")

	got, err := m.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 container, got %d", len(got))
	}
	c := got[0]
	if c.ID != "abcdef012345" {
		t.Errorf("ID = %q, want short 12-char ID", c.ID)
	}
	if c.Name != "claude-box-1" {
		t.Errorf("Name = %q, want leading slash trimmed", c.Name)
	}
	if len(c.Ports) != 1 || c.Ports[0] != "0.0.0.0:32768->8080/tcp" {
		t.Errorf("Ports = %v, want only the published mapping", c.Ports)
	}

	// List must be scoped to the managed label.
	if len(f.listOpts) != 1 || !f.listOpts[0].All {
		t.Fatalf("expected one ListOptions with All=true, got %+v", f.listOpts)
	}
	if !f.listOpts[0].Filters.ExactMatch("label", ManagedLabel+"=true") {
		t.Errorf("List not scoped to managed label; filters = %v", f.listOpts[0].Filters)
	}
}

func TestListError(t *testing.T) {
	f := &fakeDocker{listErr: errors.New("daemon down")}
	m := newTestManager(f, "")
	if _, err := m.List(context.Background()); err == nil {
		t.Fatal("expected error when ContainerList fails")
	}
}

func TestCreateDefaults(t *testing.T) {
	f := &fakeDocker{createResp: container.CreateResponse{ID: "newid000000000000"}}
	m := newTestManager(f, "custom-image")

	id, err := m.Create(context.Background(), CreateOptions{Env: []string{"FOO=bar"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id != "newid000000000000" {
		t.Errorf("returned id = %q", id)
	}
	if f.createConfig.Image != "custom-image" {
		t.Errorf("Image = %q, want the manager's default image", f.createConfig.Image)
	}
	if f.createConfig.Labels[ManagedLabel] != "true" {
		t.Errorf("managed label not set: %v", f.createConfig.Labels)
	}
	if !f.createConfig.Tty || !f.createConfig.OpenStdin {
		t.Error("Tty/OpenStdin must be set for remote-control")
	}
	if len(f.createConfig.Env) != 1 || f.createConfig.Env[0] != "FOO=bar" {
		t.Errorf("Env = %v, want passthrough", f.createConfig.Env)
	}
	if len(f.started) != 1 || f.started[0] != id {
		t.Errorf("container not started: %v", f.started)
	}
}

func TestCreateExplicitImage(t *testing.T) {
	f := &fakeDocker{createResp: container.CreateResponse{ID: "id1234567890abcd"}}
	m := newTestManager(f, "default-image")

	if _, err := m.Create(context.Background(), CreateOptions{Image: "explicit:tag", Name: "mybox"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.createConfig.Image != "explicit:tag" {
		t.Errorf("Image = %q, want explicit override", f.createConfig.Image)
	}
	if f.createName != "mybox" {
		t.Errorf("name = %q, want mybox", f.createName)
	}
}

func TestCreateStartFailureCleansUp(t *testing.T) {
	f := &fakeDocker{
		createResp: container.CreateResponse{ID: "doomed0000000000"},
		startErr:   errors.New("oom"),
	}
	m := newTestManager(f, "")

	if _, err := m.Create(context.Background(), CreateOptions{}); err == nil {
		t.Fatal("expected error when start fails")
	}
	// The created-but-not-started container must be removed.
	if len(f.removed) != 1 || f.removed[0] != "doomed0000000000" {
		t.Errorf("expected cleanup removal of created container, removed = %v", f.removed)
	}
}

func TestCreateError(t *testing.T) {
	f := &fakeDocker{createErr: errors.New("no such image")}
	m := newTestManager(f, "")
	if _, err := m.Create(context.Background(), CreateOptions{}); err == nil {
		t.Fatal("expected error when ContainerCreate fails")
	}
	if len(f.started) != 0 {
		t.Error("must not start when create fails")
	}
}

func TestDestroyByName(t *testing.T) {
	f := &fakeDocker{listResult: []container.Summary{
		{ID: "abcdef0123456789", Names: []string{"/target"}, State: "running"},
	}}
	m := newTestManager(f, "")

	if err := m.Destroy(context.Background(), "target"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(f.removed) != 1 || f.removed[0] != "abcdef012345" {
		t.Errorf("removed = %v, want the short ID", f.removed)
	}
	if !f.removeOpts[0].Force {
		t.Error("Destroy must force-remove so running containers are stopped")
	}
}

func TestDestroyByIDPrefix(t *testing.T) {
	f := &fakeDocker{listResult: []container.Summary{
		{ID: "abcdef0123456789", Names: []string{"/box"}},
	}}
	m := newTestManager(f, "")

	// Caller passes a longer/full ID; short stored ID is a prefix of it.
	if err := m.Destroy(context.Background(), "abcdef0123456789"); err != nil {
		t.Fatalf("Destroy by full ID: %v", err)
	}
	if len(f.removed) != 1 {
		t.Fatalf("expected removal, got %v", f.removed)
	}
}

func TestDestroyUnknown(t *testing.T) {
	f := &fakeDocker{listResult: []container.Summary{
		{ID: "abcdef0123456789", Names: []string{"/box"}},
	}}
	m := newTestManager(f, "")

	if err := m.Destroy(context.Background(), "does-not-exist"); err == nil {
		t.Fatal("expected error for unknown container")
	}
	if len(f.removed) != 0 {
		t.Error("must not remove anything when no managed container matches")
	}
}
