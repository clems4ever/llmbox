package docker

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// realAuthorizeURL is a representative URL as printed by `claude auth login`.
const realAuthorizeURL = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Aprofile&code_challenge=AQik-DVceTlD_9L9AsSLxtyWlxB3uP_0Hm58tFJQnBI&code_challenge_method=S256&state=IygRnzxDK7vb0gQg86PUBoEeEzZyxLY5XK1IUXb_Lnw"

// fakeDocker is an in-memory stand-in for the Docker client.
type fakeDocker struct {
	listResult []container.Summary
	listErr    error
	createResp container.CreateResponse
	createErr  error
	startErr   error
	renameErr  error
	attachErr  error

	// attachConn is the manager-side end of a net.Pipe handed to the manager on
	// ContainerAttach; the test drives the other end.
	attachConn net.Conn

	createConfig *container.Config
	started      []string
	renames      [][2]string // {id, newName}
	resizes      []string
	removed      []string
}

func (f *fakeDocker) ContainerList(_ context.Context, _ container.ListOptions) ([]container.Summary, error) {
	return f.listResult, f.listErr
}

func (f *fakeDocker) ContainerCreate(_ context.Context, config *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
	f.createConfig = config
	return f.createResp, f.createErr
}

func (f *fakeDocker) ContainerStart(_ context.Context, id string, _ container.StartOptions) error {
	f.started = append(f.started, id)
	return f.startErr
}

func (f *fakeDocker) ContainerRename(_ context.Context, id, newName string) error {
	f.renames = append(f.renames, [2]string{id, newName})
	return f.renameErr
}

func (f *fakeDocker) ContainerResize(_ context.Context, id string, _ container.ResizeOptions) error {
	f.resizes = append(f.resizes, id)
	return nil
}

func (f *fakeDocker) ContainerAttach(_ context.Context, _ string, _ container.AttachOptions) (types.HijackedResponse, error) {
	if f.attachErr != nil {
		return types.HijackedResponse{}, f.attachErr
	}
	return types.NewHijackedResponse(f.attachConn, ""), nil
}

func (f *fakeDocker) ContainerRemove(_ context.Context, id string, _ container.RemoveOptions) error {
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeDocker) Close() error { return nil }

func newTestManager(f *fakeDocker) *Manager {
	return &Manager{cli: f, defaultImage: DefaultImage, remoteArgs: defaultRemoteArgs}
}

func TestListMapsPhaseFromName(t *testing.T) {
	f := &fakeDocker{listResult: []container.Summary{
		{ID: "aaaaaaaaaaaa1111", Names: []string{"/llmbox-pending-aaaaaaaaaaaa"}, State: "running", Status: "Up", Created: 1700000000},
		{ID: "bbbbbbbbbbbb2222", Names: []string{"/llmbox-bbbbbbbbbbbb"}, State: "running", Status: "Up", Created: 1700000001},
	}}
	m := newTestManager(f)
	got, err := m.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 boxes, got %d", len(got))
	}
	if got[0].Phase != "pending" {
		t.Errorf("box0 phase = %q, want pending", got[0].Phase)
	}
	if got[1].Phase != "ready" {
		t.Errorf("box1 phase = %q, want ready", got[1].Phase)
	}
	if got[0].ID != "aaaaaaaaaaaa" {
		t.Errorf("ID not shortened: %q", got[0].ID)
	}
}

func TestCreateLLMBoxCapturesURL(t *testing.T) {
	managerEnd, testEnd := net.Pipe()
	f := &fakeDocker{
		createResp: container.CreateResponse{ID: "abcdef0123456789ffff"},
		attachConn: managerEnd,
	}
	m := newTestManager(f)

	// Feed TTY output (with ANSI noise and line wrapping) from the box.
	go func() {
		_, _ = testEnd.Write([]byte("\x1b[2J\x1b[HOpening browser...\r\n"))
		_, _ = testEnd.Write([]byte("Browser didn't open? Use the url below (c to copy)\r\n"))
		_, _ = testEnd.Write([]byte(realAuthorizeURL + "\r\nPaste code here if prompted >"))
	}()

	id, url, err := m.CreateLLMBox(context.Background(), "")
	if err != nil {
		t.Fatalf("CreateLLMBox: %v", err)
	}
	if id != "abcdef0123456789ffff" {
		t.Errorf("id = %q", id)
	}
	if url != realAuthorizeURL {
		t.Errorf("captured URL mismatch:\n got %q\nwant %q", url, realAuthorizeURL)
	}
	// Named pending, started, resized wide.
	if len(f.renames) != 1 || f.renames[0][1] != pendingPrefix+"abcdef012345" {
		t.Errorf("expected rename to pending name, got %v", f.renames)
	}
	if len(f.started) != 1 {
		t.Errorf("box not started: %v", f.started)
	}
	if len(f.resizes) != 1 {
		t.Errorf("tty not resized: %v", f.resizes)
	}
	// Entrypoint runs login then remote-control.
	ep := strings.Join(f.createConfig.Entrypoint, " ")
	if !strings.Contains(ep, "claude auth login") || !strings.Contains(ep, "remote-control") {
		t.Errorf("entrypoint missing login/remote-control: %q", ep)
	}
	// Must pre-answer the two post-login gates between login and remote-control,
	// else a fresh box aborts on "Workspace not trusted" or blocks on the
	// "Enable Remote Control? (y/n)" prompt.
	if !strings.Contains(ep, "hasTrustDialogAccepted") {
		t.Errorf("entrypoint missing workspace-trust accept: %q", ep)
	}
	if !strings.Contains(ep, "remoteDialogSeen") {
		t.Errorf("entrypoint missing remote-control dialog accept: %q", ep)
	}
	if !f.createConfig.Tty || !f.createConfig.OpenStdin {
		t.Error("box needs Tty and OpenStdin")
	}
}

func TestCreateLLMBoxCleansUpOnStartFailure(t *testing.T) {
	f := &fakeDocker{
		createResp: container.CreateResponse{ID: "doomed0000000000"},
		startErr:   errors.New("no resources"),
	}
	m := newTestManager(f)
	if _, _, err := m.CreateLLMBox(context.Background(), ""); err == nil {
		t.Fatal("expected error when start fails")
	}
	if len(f.removed) != 1 || f.removed[0] != "doomed0000000000" {
		t.Errorf("expected cleanup removal, got %v", f.removed)
	}
}

func TestSubmitCodeReturnsSessionURL(t *testing.T) {
	managerEnd, testEnd := net.Pipe()
	f := &fakeDocker{attachConn: managerEnd}
	m := newTestManager(f)

	const sessionURL = "https://claude.ai/code/session/abc123"
	got := make(chan string, 1)
	go func() {
		// Read whatever the manager writes (the code), then emit the session URL.
		buf := make([]byte, 256)
		n, _ := testEnd.Read(buf)
		got <- string(buf[:n])
		_, _ = testEnd.Write([]byte("Login successful!\r\n✓ Ready\r\n" + sessionURL + "\r\n"))
	}()

	url, err := m.SubmitCode(context.Background(), "abcdef0123456789", "MYCODE")
	if err != nil {
		t.Fatalf("SubmitCode: %v", err)
	}
	if url != sessionURL {
		t.Errorf("session URL = %q, want %q", url, sessionURL)
	}
	if code := <-got; strings.TrimSpace(code) != "MYCODE" {
		t.Errorf("code written to box = %q, want MYCODE (+CR)", code)
	}
	// Renamed pending -> ready.
	if len(f.renames) != 1 || f.renames[0][1] != readyPrefix+"abcdef012345" {
		t.Errorf("expected rename to ready, got %v", f.renames)
	}
}

func TestSubmitCodeAttachError(t *testing.T) {
	f := &fakeDocker{attachErr: errors.New("no such container")}
	m := newTestManager(f)
	if _, err := m.SubmitCode(context.Background(), "id", "code"); err == nil {
		t.Fatal("expected error when attach fails")
	}
}

func TestDestroyForceRemoves(t *testing.T) {
	f := &fakeDocker{listResult: []container.Summary{
		{ID: "abcdef0123456789", Names: []string{"/llmbox-pending-abcdef012345"}},
	}}
	m := newTestManager(f)
	if err := m.Destroy(context.Background(), "abcdef012345"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(f.removed) != 1 {
		t.Errorf("expected removal, got %v", f.removed)
	}
}

func TestReapOrphans(t *testing.T) {
	old := time.Now().Add(-10 * time.Minute).Unix()
	recent := time.Now().Add(-1 * time.Minute).Unix()
	f := &fakeDocker{listResult: []container.Summary{
		{ID: "old111111111aaaa", Names: []string{"/llmbox-pending-old111111111"}, Created: old},    // reap
		{ID: "new222222222bbbb", Names: []string{"/llmbox-pending-new222222222"}, Created: recent}, // too new
		{ID: "rdy333333333cccc", Names: []string{"/llmbox-rdy333333333"}, Created: old},            // authenticated, keep
	}}
	m := newTestManager(f)
	reaped, err := m.ReapOrphans(context.Background(), 5*time.Minute)
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != "old111111111" {
		t.Errorf("reaped = %v, want [old111111111]", reaped)
	}
	if len(f.removed) != 1 || f.removed[0] != "old111111111" {
		t.Errorf("removed = %v, want only the old pending box", f.removed)
	}
}

func TestStripANSI(t *testing.T) {
	in := []byte("\x1b[2J\x1b[1;34mhttps://x\x1b[0m\r\n")
	if got := string(stripANSI(in)); got != "https://x\n" {
		t.Errorf("stripANSI = %q", got)
	}
}
