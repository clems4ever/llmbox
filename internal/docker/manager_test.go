package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/pkg/stdcopy"
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

	logsBody string // raw bytes ContainerLogs hands back
	logsErr  error
	logsTail string // Tail option recorded from the last ContainerLogs call

	execStream     []byte // stdcopy-multiplexed bytes ContainerExecAttach replays
	execExitCode   int    // exit code ContainerExecInspect reports
	execCreateErr  error
	execAttachErr  error
	execInspectErr error
	gotExecCmd     []string // Cmd recorded from the last ContainerExecCreate call

	// createNotFoundUntilPull makes ContainerCreate return an image-not-found
	// error until ImagePull has been called, simulating a missing local image.
	createNotFoundUntilPull bool

	createConfig *container.Config
	createCalls  int
	started      []string
	renames      [][2]string // {id, newName}
	resizes      []string
	stopped      []string
	removed      []string
	pulled       []string

	copyErr     error
	copyToCalls []copyToCall // recorded CopyToContainer invocations

	networkCreateErr error
	networksCreated  []string    // network names passed to NetworkCreate
	netConnects      [][2]string // {network, container} per NetworkConnect
	netDisconnects   [][2]string // {network, container} per NetworkDisconnect
	networksRemoved  []string    // network names passed to NetworkRemove
}

// copyToCall records one CopyToContainer invocation: the destination path and
// the raw tar bytes streamed to it.
type copyToCall struct {
	dst     string
	archive []byte
}

// ContainerList returns the canned summaries (or error) configured on the fake.
func (f *fakeDocker) ContainerList(_ context.Context, _ container.ListOptions) ([]container.Summary, error) {
	return f.listResult, f.listErr
}

// ContainerCreate records the requested config and returns the canned response,
// or an image-not-found error until ImagePull is called when so configured.
func (f *fakeDocker) ContainerCreate(_ context.Context, config *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
	f.createConfig = config
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

// CopyToContainer records the destination and archive bytes, returning the canned error.
func (f *fakeDocker) CopyToContainer(_ context.Context, _, dst string, content io.Reader, _ container.CopyToContainerOptions) error {
	if f.copyErr != nil {
		return f.copyErr
	}
	b, _ := io.ReadAll(content)
	f.copyToCalls = append(f.copyToCalls, copyToCall{dst: dst, archive: b})
	return nil
}

// NetworkCreate records the created network name and returns the canned error.
func (f *fakeDocker) NetworkCreate(_ context.Context, name string, _ network.CreateOptions) (network.CreateResponse, error) {
	if f.networkCreateErr != nil {
		return network.CreateResponse{}, f.networkCreateErr
	}
	f.networksCreated = append(f.networksCreated, name)
	return network.CreateResponse{ID: name}, nil
}

// NetworkConnect records the {network, container} pair and always succeeds.
func (f *fakeDocker) NetworkConnect(_ context.Context, networkID, containerID string, _ *network.EndpointSettings) error {
	f.netConnects = append(f.netConnects, [2]string{networkID, containerID})
	return nil
}

// NetworkDisconnect records the {network, container} pair and always succeeds.
func (f *fakeDocker) NetworkDisconnect(_ context.Context, networkID, containerID string, _ bool) error {
	f.netDisconnects = append(f.netDisconnects, [2]string{networkID, containerID})
	return nil
}

// NetworkRemove records the removed network name and always succeeds.
func (f *fakeDocker) NetworkRemove(_ context.Context, networkID string) error {
	f.networksRemoved = append(f.networksRemoved, networkID)
	return nil
}

// ContainerLogs records the requested Tail and returns the canned log body or error.
func (f *fakeDocker) ContainerLogs(_ context.Context, _ string, opts container.LogsOptions) (io.ReadCloser, error) {
	if f.logsErr != nil {
		return nil, f.logsErr
	}
	f.logsTail = opts.Tail
	return io.NopCloser(strings.NewReader(f.logsBody)), nil
}

// ContainerExecCreate records the requested command and returns a canned exec ID.
func (f *fakeDocker) ContainerExecCreate(_ context.Context, _ string, opts container.ExecOptions) (container.ExecCreateResponse, error) {
	if f.execCreateErr != nil {
		return container.ExecCreateResponse{}, f.execCreateErr
	}
	f.gotExecCmd = opts.Cmd
	return container.ExecCreateResponse{ID: "exec-1"}, nil
}

// ContainerExecAttach replays the canned multiplexed stream as the exec output.
func (f *fakeDocker) ContainerExecAttach(_ context.Context, _ string, _ container.ExecAttachOptions) (types.HijackedResponse, error) {
	if f.execAttachErr != nil {
		return types.HijackedResponse{}, f.execAttachErr
	}
	return types.NewHijackedResponse(readConn{bytes.NewReader(f.execStream)}, ""), nil
}

// ContainerExecInspect returns the canned exit code (or error).
func (f *fakeDocker) ContainerExecInspect(_ context.Context, _ string) (container.ExecInspect, error) {
	if f.execInspectErr != nil {
		return container.ExecInspect{}, f.execInspectErr
	}
	return container.ExecInspect{ExitCode: f.execExitCode}, nil
}

// Close satisfies the dockerAPI interface; the fake holds no resources.
func (f *fakeDocker) Close() error { return nil }

// readConn adapts an io.Reader to a net.Conn so it can back a HijackedResponse:
// reads come from the reader, writes are discarded, and the rest are no-ops.
type readConn struct{ io.Reader }

// Write discards the bytes, reporting them all written (the fake conn is read-only).
func (readConn) Write(p []byte) (int, error) { return len(p), nil }

// Close is a no-op; the fake conn holds no resources.
func (readConn) Close() error { return nil }

// LocalAddr returns nil; the fake conn has no address.
func (readConn) LocalAddr() net.Addr { return nil }

// RemoteAddr returns nil; the fake conn has no address.
func (readConn) RemoteAddr() net.Addr { return nil }

// SetDeadline is a no-op; the fake conn ignores deadlines.
func (readConn) SetDeadline(time.Time) error { return nil }

// SetReadDeadline is a no-op; the fake conn ignores deadlines.
func (readConn) SetReadDeadline(time.Time) error { return nil }

// SetWriteDeadline is a no-op; the fake conn ignores deadlines.
func (readConn) SetWriteDeadline(time.Time) error { return nil }

// testClaudeBin is the path to a stand-in Claude binary written once by TestMain,
// so the always-on injection in CreateLLMBox has a real file to read.
var testClaudeBin string

// testClaudeBinContent is the stand-in binary's bytes, asserted on as the
// injected payload.
var testClaudeBinContent = []byte("#!/bin/sh\necho fake-claude\n")

// TestMain writes a stand-in Claude binary to a temp file the whole package
// shares, then runs the tests.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "llmbox-test-claude")
	if err != nil {
		panic(err)
	}
	testClaudeBin = filepath.Join(dir, "claude")
	if err := os.WriteFile(testClaudeBin, testClaudeBinContent, 0o755); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// newTestManager builds a Manager backed by the given fake Docker client, with
// the stand-in Claude binary wired in so box creation can inject it.
func newTestManager(f *fakeDocker) *Manager {
	return &Manager{cli: f, defaultImage: DefaultImage, remoteArgs: defaultRemoteArgs, claudeBinSrc: testClaudeBin}
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
	// Login is guarded so a restart with credentials already on disk skips
	// re-authentication instead of prompting the user again.
	if !strings.Contains(ep, ".claude/.credentials.json") {
		t.Errorf("entrypoint missing credentials guard for restart: %q", ep)
	}
	// The two post-login gates (workspace trust and "Enable Remote Control?") are
	// pre-answered by the injected ~/.claude.json seed now, not the entrypoint, so
	// the entrypoint stays node-free. See TestCreateLLMBoxInjectsClaude.
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

// TestLogsReturnsTail checks Logs resolves a box by ID, requests the given tail,
// and returns its output with ANSI escape sequences stripped.
func TestLogsReturnsTail(t *testing.T) {
	f := &fakeDocker{
		listResult: []container.Summary{
			{ID: "abcdef0123456789", Names: []string{"/llmbox-abcdef012345"}},
		},
		logsBody: "\x1b[2J\x1b[1;32mReady\x1b[0m\r\nlistening\r\n",
	}
	m := newTestManager(f)
	out, err := m.Logs(context.Background(), "abcdef012345", 50)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if out != "Ready\nlistening\n" {
		t.Errorf("logs = %q, want ANSI-stripped output", out)
	}
	if f.logsTail != "50" {
		t.Errorf("tail = %q, want 50", f.logsTail)
	}
}

// TestLogsDefaultsTail checks a non-positive tail falls back to the default count.
func TestLogsDefaultsTail(t *testing.T) {
	f := &fakeDocker{
		listResult: []container.Summary{{ID: "abcdef0123456789", Names: []string{"/llmbox-abcdef012345"}}},
		logsBody:   "hello\r\n",
	}
	m := newTestManager(f)
	if _, err := m.Logs(context.Background(), "abcdef012345", 0); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if f.logsTail != strconv.Itoa(defaultLogTail) {
		t.Errorf("tail = %q, want default %d", f.logsTail, defaultLogTail)
	}
}

// TestLogsUnknownBox checks Logs errors when no managed box matches.
func TestLogsUnknownBox(t *testing.T) {
	m := newTestManager(&fakeDocker{})
	if _, err := m.Logs(context.Background(), "missing", 10); err == nil {
		t.Fatal("expected error for unknown box")
	}
}

// muxStream builds a stdcopy-multiplexed stream carrying the given stdout and
// stderr payloads, as Docker's exec attach endpoint returns for a non-TTY exec.
func muxStream(t *testing.T, stdout, stderr string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if stdout != "" {
		if _, err := stdcopy.NewStdWriter(&buf, stdcopy.Stdout).Write([]byte(stdout)); err != nil {
			t.Fatalf("writing stdout frame: %v", err)
		}
	}
	if stderr != "" {
		if _, err := stdcopy.NewStdWriter(&buf, stdcopy.Stderr).Write([]byte(stderr)); err != nil {
			t.Fatalf("writing stderr frame: %v", err)
		}
	}
	return buf.Bytes()
}

// TestExecCapturesOutput checks Exec resolves a box, forwards the command, and
// returns its demultiplexed stdout, stderr, and exit code.
func TestExecCapturesOutput(t *testing.T) {
	f := &fakeDocker{
		listResult:   []container.Summary{{ID: "abcdef0123456789", Names: []string{"/llmbox-abcdef012345"}}},
		execStream:   muxStream(t, "hello\n", "oops\n"),
		execExitCode: 3,
	}
	m := newTestManager(f)
	cmd := []string{"/bin/sh", "-c", "echo hello"}
	res, err := m.Exec(context.Background(), "abcdef012345", cmd)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Stdout != "hello\n" || res.Stderr != "oops\n" || res.ExitCode != 3 {
		t.Errorf("unexpected result: %+v", res)
	}
	if !reflect.DeepEqual(f.gotExecCmd, cmd) {
		t.Errorf("manager ran cmd %v, want %v", f.gotExecCmd, cmd)
	}
}

// TestExecCapsOutput checks output past the cap is truncated and marked.
func TestExecCapsOutput(t *testing.T) {
	big := strings.Repeat("a", maxExecOutput+1000)
	f := &fakeDocker{
		listResult: []container.Summary{{ID: "abcdef0123456789", Names: []string{"/llmbox-abcdef012345"}}},
		execStream: muxStream(t, big, ""),
	}
	m := newTestManager(f)
	res, err := m.Exec(context.Background(), "abcdef012345", []string{"/bin/sh", "-c", "yes"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.HasSuffix(res.Stdout, "[output truncated]") {
		t.Errorf("expected truncation marker, got %d bytes ending %q", len(res.Stdout), res.Stdout[len(res.Stdout)-20:])
	}
	if len(res.Stdout) > maxExecOutput+len("\n... [output truncated]") {
		t.Errorf("output not capped: %d bytes", len(res.Stdout))
	}
}

// TestExecUnknownBox checks Exec errors when no managed box matches.
func TestExecUnknownBox(t *testing.T) {
	m := newTestManager(&fakeDocker{})
	if _, err := m.Exec(context.Background(), "missing", []string{"true"}); err == nil {
		t.Fatal("expected error for unknown box")
	}
}

// TestSetupBoxNetworkConnectsPeers checks each box gets its own network, named
// after the box, with the box and every resource-server peer connected to it.
func TestSetupBoxNetworkConnectsPeers(t *testing.T) {
	managerEnd, testEnd := net.Pipe()
	f := &fakeDocker{
		createResp: container.CreateResponse{ID: "abcdef0123456789ffff"},
		attachConn: managerEnd,
	}
	m := &Manager{cli: f, defaultImage: DefaultImage, remoteArgs: defaultRemoteArgs, claudeBinSrc: testClaudeBin, peers: []string{"peer-svc"}}
	go func() { _, _ = testEnd.Write([]byte(realAuthorizeURL + "\r\n")) }()

	id, _, err := m.CreateLLMBox(context.Background(), CreateOptions{})
	if err != nil {
		t.Fatalf("CreateLLMBox: %v", err)
	}
	// The box is created on no shared network, then attached only to its own.
	if f.createConfig == nil {
		t.Fatal("no container created")
	}
	wantNet := boxNetworkName(id)
	if len(f.networksCreated) != 1 || f.networksCreated[0] != wantNet {
		t.Errorf("networksCreated = %v, want [%s]", f.networksCreated, wantNet)
	}
	wantConnects := [][2]string{{wantNet, id}, {wantNet, "peer-svc"}}
	if !reflect.DeepEqual(f.netConnects, wantConnects) {
		t.Errorf("netConnects = %v, want %v", f.netConnects, wantConnects)
	}
	// The box is detached from the default bridge it was created on, so it lives
	// only on its own network (rather than being created in "none" mode, which
	// Docker forbids connecting to any other network).
	if len(f.netDisconnects) != 1 || f.netDisconnects[0] != [2]string{defaultBridgeNetwork, id} {
		t.Errorf("netDisconnects = %v, want [{%s %s}]", f.netDisconnects, defaultBridgeNetwork, id)
	}
}

// TestDestroyRemovesBoxNetwork checks destroy disconnects the peers and removes
// the box's dedicated network.
func TestDestroyRemovesBoxNetwork(t *testing.T) {
	f := &fakeDocker{listResult: []container.Summary{
		{ID: "abcdef0123456789", Names: []string{"/llmbox-pending-abcdef012345"}},
	}}
	m := &Manager{cli: f, defaultImage: DefaultImage, remoteArgs: defaultRemoteArgs, claudeBinSrc: testClaudeBin, peers: []string{"peer-svc"}}
	if err := m.Destroy(context.Background(), "abcdef012345"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	wantNet := boxNetworkName("abcdef0123456789")
	if len(f.netDisconnects) != 1 || f.netDisconnects[0] != [2]string{wantNet, "peer-svc"} {
		t.Errorf("netDisconnects = %v, want [{%s peer-svc}]", f.netDisconnects, wantNet)
	}
	if len(f.networksRemoved) != 1 || f.networksRemoved[0] != wantNet {
		t.Errorf("networksRemoved = %v, want [%s]", f.networksRemoved, wantNet)
	}
}

// TestStripANSI checks ANSI escape sequences and carriage returns are removed.
func TestStripANSI(t *testing.T) {
	in := []byte("\x1b[2J\x1b[1;34mhttps://x\x1b[0m\r\n")
	if got := string(stripANSI(in)); got != "https://x\n" {
		t.Errorf("stripANSI = %q", got)
	}
}

// tarEntries parses a tar archive into a name->header+content map for assertions.
func tarEntries(t *testing.T, b []byte) map[string]struct {
	hdr  *tar.Header
	body []byte
} {
	t.Helper()
	out := map[string]struct {
		hdr  *tar.Header
		body []byte
	}{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		body, _ := io.ReadAll(tr)
		out[hdr.Name] = struct {
			hdr  *tar.Header
			body []byte
		}{hdr, body}
	}
	return out
}

// TestCreateLLMBoxInjectsFiles checks injected files are copied into the box, to
// the container root, before it starts.
func TestCreateLLMBoxInjectsFiles(t *testing.T) {
	managerEnd, testEnd := net.Pipe()
	f := &fakeDocker{
		createResp: container.CreateResponse{ID: "abcdef0123456789ffff"},
		attachConn: managerEnd,
	}
	m := newTestManager(f)

	go func() {
		_, _ = testEnd.Write([]byte(realAuthorizeURL + "\r\nPaste code here if prompted >"))
	}()

	_, _, err := m.CreateLLMBox(context.Background(), CreateOptions{
		Files: []InjectFile{{
			Path:    "/home/node/.secrets/token",
			Content: []byte("sekret-123"),
			Mode:    0o600,
			UID:     1000,
			GID:     1000,
		}},
	})
	if err != nil {
		t.Fatalf("CreateLLMBox: %v", err)
	}
	if len(f.copyToCalls) != 1 {
		t.Fatalf("want 1 CopyToContainer call, got %d", len(f.copyToCalls))
	}
	call := f.copyToCalls[0]
	if call.dst != "/" {
		t.Errorf("copy dst = %q, want /", call.dst)
	}
	entries := tarEntries(t, call.archive)
	file, ok := entries["home/node/.secrets/token"]
	if !ok {
		t.Fatalf("subject token not in archive: %v keys", entries)
	}
	if string(file.body) != "sekret-123" {
		t.Errorf("token content = %q, want sekret-123", file.body)
	}
	if file.hdr.Uid != 1000 || file.hdr.Gid != 1000 {
		t.Errorf("token owner = %d:%d, want 1000:1000", file.hdr.Uid, file.hdr.Gid)
	}
}

// TestCreateLLMBoxInjectsClaude checks the standalone Claude binary and the
// ~/.claude.json seed are injected into every box, and that the box is forced to
// run as root with a fixed HOME/WorkingDir.
func TestCreateLLMBoxInjectsClaude(t *testing.T) {
	managerEnd, testEnd := net.Pipe()
	f := &fakeDocker{
		createResp: container.CreateResponse{ID: "abcdef0123456789ffff"},
		attachConn: managerEnd,
	}
	m := newTestManager(f)

	go func() {
		_, _ = testEnd.Write([]byte(realAuthorizeURL + "\r\nPaste code here if prompted >"))
	}()

	if _, _, err := m.CreateLLMBox(context.Background(), CreateOptions{}); err != nil {
		t.Fatalf("CreateLLMBox: %v", err)
	}

	// Box runs as root with a deterministic HOME/WorkingDir.
	if f.createConfig.User != "0:0" {
		t.Errorf("user = %q, want 0:0", f.createConfig.User)
	}
	if f.createConfig.WorkingDir != boxWorkdir {
		t.Errorf("workdir = %q, want %q", f.createConfig.WorkingDir, boxWorkdir)
	}
	if !slicesContains(f.createConfig.Env, "HOME="+boxHome) {
		t.Errorf("env missing HOME=%s: %v", boxHome, f.createConfig.Env)
	}
	// Entrypoint is node-free (the seed file answers the gates, not a node step).
	ep := strings.Join(f.createConfig.Entrypoint, " ")
	if strings.Contains(ep, "node ") {
		t.Errorf("entrypoint should not use node: %q", ep)
	}

	if len(f.copyToCalls) != 1 {
		t.Fatalf("want 1 CopyToContainer call, got %d", len(f.copyToCalls))
	}
	entries := tarEntries(t, f.copyToCalls[0].archive)

	// The Claude binary lands on PATH, executable, owned by root.
	bin, ok := entries[strings.TrimPrefix(claudeBinTarget, "/")]
	if !ok {
		t.Fatalf("claude binary not in archive: %v keys", entries)
	}
	if !bytes.Equal(bin.body, testClaudeBinContent) {
		t.Errorf("claude binary content mismatch")
	}
	if bin.hdr.Mode != 0o755 {
		t.Errorf("claude binary mode = %o, want 0755", bin.hdr.Mode)
	}
	if bin.hdr.Uid != 0 || bin.hdr.Gid != 0 {
		t.Errorf("claude binary owner = %d:%d, want 0:0", bin.hdr.Uid, bin.hdr.Gid)
	}

	// The ~/.claude.json seed pre-answers the trust and remote-control gates.
	seed, ok := entries[strings.TrimPrefix(boxHome, "/")+"/.claude.json"]
	if !ok {
		t.Fatalf("claude config seed not in archive: %v keys", entries)
	}
	body := string(seed.body)
	for _, want := range []string{"hasTrustDialogAccepted", "remoteDialogSeen", boxWorkdir} {
		if !strings.Contains(body, want) {
			t.Errorf("seed missing %q: %s", want, body)
		}
	}
}

// slicesContains reports whether s contains v.
func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestCreateLLMBoxMissingClaudeBinary checks create fails cleanly, without making
// a container, when the Claude binary to inject cannot be read.
func TestCreateLLMBoxMissingClaudeBinary(t *testing.T) {
	f := &fakeDocker{createResp: container.CreateResponse{ID: "doomed0000000000"}}
	m := &Manager{cli: f, defaultImage: DefaultImage, remoteArgs: defaultRemoteArgs, claudeBinSrc: filepath.Join(t.TempDir(), "does-not-exist")}

	_, _, err := m.CreateLLMBox(context.Background(), CreateOptions{})
	if err == nil {
		t.Fatal("expected an error when the claude binary is unreadable")
	}
	if f.createCalls != 0 {
		t.Errorf("no container should be created on a bad binary path, got %d creates", f.createCalls)
	}
}

// TestTarFilesCreatesParentDirs checks tarFiles emits an owned parent directory
// entry for each file and strips the leading slash from absolute paths.
func TestTarFilesCreatesParentDirs(t *testing.T) {
	r, err := tarFiles([]InjectFile{{
		Path:    "/home/node/.secrets/token",
		Content: []byte("tok"),
		UID:     1000,
		GID:     1000,
	}})
	if err != nil {
		t.Fatalf("tarFiles: %v", err)
	}
	b, _ := io.ReadAll(r)
	entries := tarEntries(t, b)

	dir, ok := entries["home/node/.secrets/"]
	if !ok {
		t.Fatalf("parent dir entry missing: %v", entries)
	}
	if dir.hdr.Typeflag != tar.TypeDir || dir.hdr.Uid != 1000 || dir.hdr.Gid != 1000 {
		t.Errorf("dir entry = type %c owner %d:%d, want dir 1000:1000", dir.hdr.Typeflag, dir.hdr.Uid, dir.hdr.Gid)
	}
	file, ok := entries["home/node/.secrets/token"]
	if !ok {
		t.Fatalf("file entry missing: %v", entries)
	}
	if file.hdr.Mode != 0o600 {
		t.Errorf("default mode = %o, want 600", file.hdr.Mode)
	}
}
