package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
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

	stopErr error
	pullErr error

	// createNotFoundUntilPull makes ContainerCreate return an image-not-found
	// error until ImagePull has been called, simulating a missing local image.
	createNotFoundUntilPull bool

	createConfig      *container.Config
	createCalls       int
	createConfigs     []*container.Config
	createHostConfigs []*container.HostConfig
	createNames       []string
	started           []string
	renames      [][2]string // {id, newName}
	resizes      []string
	stopped      []string
	removed      []string
	pulled       []string
}

// ContainerList returns the canned summaries (or error) configured on the fake.
func (f *fakeDocker) ContainerList(_ context.Context, _ container.ListOptions) ([]container.Summary, error) {
	return f.listResult, f.listErr
}

// ContainerCreate records the requested config and returns the canned response,
// or an image-not-found error until ImagePull is called when so configured.
func (f *fakeDocker) ContainerCreate(_ context.Context, config *container.Config, hostCfg *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	f.createConfig = config
	f.createConfigs = append(f.createConfigs, config)
	f.createHostConfigs = append(f.createHostConfigs, hostCfg)
	f.createNames = append(f.createNames, name)
	f.createCalls++
	if f.createNotFoundUntilPull && len(f.pulled) == 0 {
		return container.CreateResponse{}, fmt.Errorf("no such image: %w", cerrdefs.ErrNotFound)
	}
	return f.createResp, f.createErr
}

// ImagePull records the pulled reference and returns a short progress stream.
func (f *fakeDocker) ImagePull(_ context.Context, ref string, _ image.PullOptions) (io.ReadCloser, error) {
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	f.pulled = append(f.pulled, ref)
	return io.NopCloser(strings.NewReader("pulling " + ref + "\n")), nil
}

// ContainerStart records the started ID and returns the canned start error.
func (f *fakeDocker) ContainerStart(_ context.Context, id string, _ container.StartOptions) error {
	f.started = append(f.started, id)
	return f.startErr
}

// ContainerRename records the rename and returns the canned rename error.
func (f *fakeDocker) ContainerRename(_ context.Context, id, newName string) error {
	f.renames = append(f.renames, [2]string{id, newName})
	return f.renameErr
}

// ContainerResize records the resized ID and always succeeds.
func (f *fakeDocker) ContainerResize(_ context.Context, id string, _ container.ResizeOptions) error {
	f.resizes = append(f.resizes, id)
	return nil
}

// ContainerAttach returns a hijacked response over the fake's pipe, or the canned error.
func (f *fakeDocker) ContainerAttach(_ context.Context, _ string, _ container.AttachOptions) (types.HijackedResponse, error) {
	if f.attachErr != nil {
		return types.HijackedResponse{}, f.attachErr
	}
	return types.NewHijackedResponse(f.attachConn, ""), nil
}

// ContainerStop records the stopped ID and returns the canned stop error.
func (f *fakeDocker) ContainerStop(_ context.Context, id string, _ container.StopOptions) error {
	f.stopped = append(f.stopped, id)
	return f.stopErr
}

// ContainerRemove records the removed ID and always succeeds.
func (f *fakeDocker) ContainerRemove(_ context.Context, id string, _ container.RemoveOptions) error {
	f.removed = append(f.removed, id)
	return nil
}

// Close satisfies the dockerAPI interface; the fake holds no resources.
func (f *fakeDocker) Close() error { return nil }

// newTestManager builds a Manager backed by the given fake Docker client.
func newTestManager(f *fakeDocker) *Manager {
	return &Manager{cli: f, defaultImage: DefaultImage, remoteArgs: defaultRemoteArgs}
}

// TestListMapsPhaseFromName checks List maps phase, shortened ID, hostname, and description.
func TestListMapsPhaseFromName(t *testing.T) {
	f := &fakeDocker{listResult: []container.Summary{
		{ID: "aaaaaaaaaaaa1111", Names: []string{"/llmbox-pending-aaaaaaaaaaaa"}, State: "running", Status: "Up", Created: 1700000000,
			Labels: map[string]string{HostnameLabel: "web-box", DescriptionLabel: "front-end work"}},
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
	if got[0].Hostname != "web-box" || got[0].Description != "front-end work" {
		t.Errorf("box0 hostname/description = %q/%q, want web-box/front-end work", got[0].Hostname, got[0].Description)
	}
	if got[1].Hostname != "" || got[1].Description != "" {
		t.Errorf("box1 should have empty hostname/description, got %q/%q", got[1].Hostname, got[1].Description)
	}
}

// TestCreateLLMBoxCapturesURL checks the authorize URL capture, naming, and hostname/description labels.
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

	id, url, err := m.CreateLLMBox(context.Background(), CreateOptions{Hostname: "my-box", Description: "scratch box"})
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
	// Hostname is set on the container and, with the description, persisted as
	// labels so List can report them.
	if f.createConfig.Hostname != "my-box" {
		t.Errorf("hostname = %q, want my-box", f.createConfig.Hostname)
	}
	if got := f.createConfig.Labels[HostnameLabel]; got != "my-box" {
		t.Errorf("hostname label = %q, want my-box", got)
	}
	if got := f.createConfig.Labels[DescriptionLabel]; got != "scratch box" {
		t.Errorf("description label = %q, want scratch box", got)
	}
}

// TestCreateLLMBoxCleansUpOnStartFailure checks the container is removed when start fails.
func TestCreateLLMBoxCleansUpOnStartFailure(t *testing.T) {
	f := &fakeDocker{
		createResp: container.CreateResponse{ID: "doomed0000000000"},
		startErr:   errors.New("no resources"),
	}
	m := newTestManager(f)
	if _, _, err := m.CreateLLMBox(context.Background(), CreateOptions{}); err == nil {
		t.Fatal("expected error when start fails")
	}
	if len(f.removed) != 1 || f.removed[0] != "doomed0000000000" {
		t.Errorf("expected cleanup removal, got %v", f.removed)
	}
}

// TestCreateLLMBoxRejectsDuplicateHostname checks a create is refused (and no
// container made) when another box already uses the requested hostname.
func TestCreateLLMBoxRejectsDuplicateHostname(t *testing.T) {
	f := &fakeDocker{listResult: []container.Summary{
		{ID: "existing0000aaaa", Names: []string{"/llmbox-existing0000"}, Labels: map[string]string{HostnameLabel: "dup-host"}},
	}}
	m := newTestManager(f)

	// Case-insensitive: "DUP-HOST" must still collide with "dup-host".
	_, _, err := m.CreateLLMBox(context.Background(), CreateOptions{Hostname: "DUP-HOST"})
	if err == nil {
		t.Fatal("expected error for duplicate hostname")
	}
	if !strings.Contains(err.Error(), "dup-host") && !strings.Contains(err.Error(), "DUP-HOST") {
		t.Errorf("error should name the conflicting hostname: %v", err)
	}
	if f.createConfig != nil {
		t.Error("no container should be created when the hostname conflicts")
	}
}

// TestCreateLLMBoxPullsMissingImage checks that when the image is absent the
// manager pulls it and retries the create, succeeding on the second attempt.
func TestCreateLLMBoxPullsMissingImage(t *testing.T) {
	managerEnd, testEnd := net.Pipe()
	f := &fakeDocker{
		createResp:              container.CreateResponse{ID: "abcdef0123456789ffff"},
		attachConn:              managerEnd,
		createNotFoundUntilPull: true,
	}
	m := newTestManager(f)

	go func() {
		_, _ = testEnd.Write([]byte(realAuthorizeURL + "\r\nPaste code here if prompted >"))
	}()

	id, _, err := m.CreateLLMBox(context.Background(), CreateOptions{})
	if err != nil {
		t.Fatalf("CreateLLMBox: %v", err)
	}
	if id != "abcdef0123456789ffff" {
		t.Errorf("id = %q", id)
	}
	if len(f.pulled) != 1 || f.pulled[0] != DefaultImage {
		t.Errorf("expected one pull of %q, got %v", DefaultImage, f.pulled)
	}
	if f.createCalls != 2 {
		t.Errorf("expected create retried after pull (2 calls), got %d", f.createCalls)
	}
}

// TestCreateLLMBoxPullFailure checks a failed pull surfaces an error and no box.
func TestCreateLLMBoxPullFailure(t *testing.T) {
	f := &fakeDocker{
		createNotFoundUntilPull: true,
		pullErr:                 errors.New("registry unreachable"),
	}
	m := newTestManager(f)
	if _, _, err := m.CreateLLMBox(context.Background(), CreateOptions{}); err == nil {
		t.Fatal("expected error when the pull fails")
	}
	if len(f.started) != 0 {
		t.Errorf("no box should be started when the pull fails, got %v", f.started)
	}
}

// TestCreateLLMBoxStartsCapture checks that, when capture is enabled, a tcpdump
// sidecar is created sharing the box's netns, mounting the host dir, and started.
func TestCreateLLMBoxStartsCapture(t *testing.T) {
	managerEnd, testEnd := net.Pipe()
	f := &fakeDocker{
		createResp: container.CreateResponse{ID: "abcdef0123456789ffff"},
		attachConn: managerEnd,
	}
	m := &Manager{cli: f, defaultImage: DefaultImage, remoteArgs: defaultRemoteArgs, captureDir: "/caps", captureImage: "cap-img"}

	go func() { _, _ = testEnd.Write([]byte(realAuthorizeURL + "\r\n>")) }()

	id, _, err := m.CreateLLMBox(context.Background(), CreateOptions{})
	if err != nil {
		t.Fatalf("CreateLLMBox: %v", err)
	}
	short := id[:12]

	var capCfg *container.Config
	var capHost *container.HostConfig
	var capName string
	for i, c := range f.createConfigs {
		if c.Labels[CaptureForLabel] != "" {
			capCfg, capHost, capName = c, f.createHostConfigs[i], f.createNames[i]
		}
	}
	if capCfg == nil {
		t.Fatal("no capture sidecar was created")
	}
	if capCfg.Image != "cap-img" {
		t.Errorf("capture image = %q, want cap-img", capCfg.Image)
	}
	if capName != captureName(short) {
		t.Errorf("capture name = %q, want %q", capName, captureName(short))
	}
	if ep := strings.Join(capCfg.Entrypoint, " "); !strings.Contains(ep, "tcpdump") || !strings.Contains(ep, "/capture/"+short+".pcap") {
		t.Errorf("capture entrypoint = %q", ep)
	}
	if string(capHost.NetworkMode) != "container:"+id {
		t.Errorf("capture netns = %q, want container:%s", capHost.NetworkMode, id)
	}
	if len(capHost.Binds) != 1 || capHost.Binds[0] != "/caps:/capture" {
		t.Errorf("capture binds = %v, want [/caps:/capture]", capHost.Binds)
	}
	if len(capHost.CapAdd) == 0 || capHost.CapAdd[0] != "NET_RAW" {
		t.Errorf("capture caps = %v, want NET_RAW", capHost.CapAdd)
	}
}

// TestDestroyRemovesCapture checks the capture sidecar is removed when its box is.
func TestDestroyRemovesCapture(t *testing.T) {
	f := &fakeDocker{listResult: []container.Summary{
		{ID: "abcdef0123456789", Names: []string{"/llmbox-abcdef012345"}},
	}}
	m := &Manager{cli: f, defaultImage: DefaultImage, remoteArgs: defaultRemoteArgs, captureDir: "/caps", captureImage: "cap-img"}

	if err := m.Destroy(context.Background(), "abcdef012345"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	foundCap := false
	for _, r := range f.removed {
		if r == captureName("abcdef012345") {
			foundCap = true
		}
	}
	if !foundCap {
		t.Errorf("capture sidecar not removed: %v", f.removed)
	}
}

// TestSubmitCodeReturnsSessionURL checks the code is written and the session URL returned.
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

// TestSubmitCodeAttachError checks SubmitCode errors when attaching fails.
func TestSubmitCodeAttachError(t *testing.T) {
	f := &fakeDocker{attachErr: errors.New("no such container")}
	m := newTestManager(f)
	if _, err := m.SubmitCode(context.Background(), "id", "code"); err == nil {
		t.Fatal("expected error when attach fails")
	}
}

// TestDestroyStopsThenRemoves checks Destroy gracefully stops the box before
// removing it (and removes without forcing).
func TestDestroyStopsThenRemoves(t *testing.T) {
	f := &fakeDocker{listResult: []container.Summary{
		{ID: "abcdef0123456789", Names: []string{"/llmbox-pending-abcdef012345"}},
	}}
	m := newTestManager(f)
	if err := m.Destroy(context.Background(), "abcdef012345"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(f.stopped) != 1 || f.stopped[0] != "abcdef012345" {
		t.Errorf("expected graceful stop, got %v", f.stopped)
	}
	if len(f.removed) != 1 || f.removed[0] != "abcdef012345" {
		t.Errorf("expected removal, got %v", f.removed)
	}
}

// TestDestroyStopErrorAborts checks a failed stop aborts before removal so the
// box is not torn down abruptly.
func TestDestroyStopErrorAborts(t *testing.T) {
	f := &fakeDocker{
		listResult: []container.Summary{{ID: "abcdef0123456789", Names: []string{"/llmbox-abcdef012345"}}},
		stopErr:    errors.New("stop failed"),
	}
	m := newTestManager(f)
	if err := m.Destroy(context.Background(), "abcdef012345"); err == nil {
		t.Fatal("expected error when stop fails")
	}
	if len(f.removed) != 0 {
		t.Errorf("container should not be removed when stop fails, got %v", f.removed)
	}
}

// TestReapOrphans checks only old pending boxes are reaped.
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

// TestStripANSI checks ANSI escape sequences and carriage returns are removed.
func TestStripANSI(t *testing.T) {
	in := []byte("\x1b[2J\x1b[1;34mhttps://x\x1b[0m\r\n")
	if got := string(stripANSI(in)); got != "https://x\n" {
		t.Errorf("stripANSI = %q", got)
	}
}
