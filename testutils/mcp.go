package testutils

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/clems4ever/llmbox/internal/mcpserver"
	"github.com/clems4ever/llmbox/internal/shared/api"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// FakeBackend is a stand-in for the server's box-control backend: it records the
// calls it receives and returns canned results, satisfying api.Backend. Pair it
// with api.NewHandler (to serve the HTTP API) or ConnectMCP (to drive the MCP
// tools) without Docker, a store, or a cluster.
type FakeBackend struct {
	mu sync.Mutex

	// Canned results.
	CreateSess        api.BoxSession
	CreateErr         error
	Sessions          map[string]api.BoxSession // LookupByBoxID source, keyed by lowercased box ID
	Boxes             []api.BoxView
	ListErr           error
	Spokes            []api.SpokeStatus
	SpokesErr         error
	CreateSpokeResult api.SpokeEnrollment
	CreateSpokeErr    error
	DropSpokeErr      error
	SetDefaultErr     error
	JoinTokens        []api.JoinTokenInfo
	JoinTokensErr     error
	RevokeTokenErr    error
	RegenTokenResult  api.SpokeEnrollment
	RegenTokenErr     error
	DestroyErr        error
	PauseErr          error
	ResumeErr         error
	ExecResult        sandbox.ExecResult
	ExecErr           error
	ProxyOn           bool
	CreateProxyResult api.ProxyInfo
	CreateProxyErr    error
	Proxies           []api.ProxyInfo
	ListProxiesErr    error
	DeleteProxyErr    error

	// Recorded inputs.
	GotCreate         sandbox.CreateOptions
	GotLookup         string
	GotCreateSpoke    string
	GotCreateSpokeBk  string
	GotCreateSpokeTTL time.Duration
	GotDropSpoke      string
	GotDefaultSpoke   string
	GotRevokeToken    string
	GotRegenToken     string
	GotDestroyID      string
	GotPauseID        string
	GotResumeID       string
	GotExecID         string
	GotExecCmd        string
	GotProxyBoxID     string
	GotProxyPort      int
	GotProxyDesc      string
	GotDeleteBoxID    string
	GotDeletePort     int
	GotListBoxID      string
}

// CreateBox records the options into GotCreate and returns the canned session/error.
//
// @arg ctx Context (unused by the fake).
// @arg opts The create options, recorded into GotCreate.
// @return api.BoxSession The canned CreateSess.
// @error error The canned CreateErr, if any.
//
// @testcase TestFakeBackend checks each method records its inputs and returns the canned results.
func (f *FakeBackend) CreateBox(ctx context.Context, opts sandbox.CreateOptions) (api.BoxSession, error) {
	f.mu.Lock()
	f.GotCreate = opts
	f.mu.Unlock()
	return f.CreateSess, f.CreateErr
}

// LookupByBoxID records the box ID and returns the canned session from Sessions
// (case-insensitive); ok is false when none matches.
//
// @arg boxID The box ID to look up, recorded into GotLookup.
// @return api.BoxSession The matching canned session (zero value when absent).
// @return bool Whether a session with that box ID exists in Sessions.
//
// @testcase TestFakeBackend checks LookupByBoxID resolves from Sessions and misses unknown IDs.
func (f *FakeBackend) LookupByBoxID(boxID string) (api.BoxSession, bool) {
	f.mu.Lock()
	f.GotLookup = boxID
	f.mu.Unlock()
	sess, ok := f.Sessions[strings.ToLower(boxID)]
	return sess, ok
}

// ListBoxes returns the canned boxes/error.
//
// @arg ctx Context (unused by the fake).
// @return []api.BoxView The canned Boxes slice.
// @error error The canned ListErr, if any.
//
// @testcase TestFakeBackend checks each method records its inputs and returns the canned results.
func (f *FakeBackend) ListBoxes(ctx context.Context) ([]api.BoxView, error) {
	return f.Boxes, f.ListErr
}

// SpokeStatuses returns the canned spokes/error.
//
// @arg ctx Context (unused by the fake).
// @return []api.SpokeStatus The canned Spokes slice.
// @error error The canned SpokesErr, if any.
//
// @testcase TestFakeBackend checks each method records its inputs and returns the canned results.
func (f *FakeBackend) SpokeStatuses(ctx context.Context) ([]api.SpokeStatus, error) {
	return f.Spokes, f.SpokesErr
}

// CreateSpoke records the name/backend/ttl and returns the canned enrollment/error.
//
// @arg ctx Context (unused by the fake).
// @arg name The spoke name, recorded into GotCreateSpoke.
// @arg backend The backend, recorded into GotCreateSpokeBk.
// @arg ttl The token TTL, recorded into GotCreateSpokeTTL.
// @return api.SpokeEnrollment The canned CreateSpokeResult.
// @error error The canned CreateSpokeErr, if any.
//
// @testcase TestFakeBackend checks each method records its inputs and returns the canned results.
func (f *FakeBackend) CreateSpoke(ctx context.Context, name, backend string, ttl time.Duration) (api.SpokeEnrollment, error) {
	f.mu.Lock()
	f.GotCreateSpoke = name
	f.GotCreateSpokeBk = backend
	f.GotCreateSpokeTTL = ttl
	f.mu.Unlock()
	return f.CreateSpokeResult, f.CreateSpokeErr
}

// DropSpoke records the name and returns the canned DropSpokeErr.
//
// @arg ctx Context (unused by the fake).
// @arg name The spoke name, recorded into GotDropSpoke.
// @error error The canned DropSpokeErr, if any.
//
// @testcase TestFakeBackend checks each method records its inputs and returns the canned results.
func (f *FakeBackend) DropSpoke(ctx context.Context, name string) error {
	f.mu.Lock()
	f.GotDropSpoke = name
	f.mu.Unlock()
	return f.DropSpokeErr
}

// SetDefaultSpoke records the name and returns the canned SetDefaultErr.
//
// @arg ctx Context (unused by the fake).
// @arg name The spoke name, recorded into GotDefaultSpoke.
// @error error The canned SetDefaultErr, if any.
//
// @testcase TestFakeBackend checks each method records its inputs and returns the canned results.
func (f *FakeBackend) SetDefaultSpoke(ctx context.Context, name string) error {
	f.mu.Lock()
	f.GotDefaultSpoke = name
	f.mu.Unlock()
	return f.SetDefaultErr
}

// ListJoinTokens returns the canned join tokens/error.
//
// @arg ctx Context (unused by the fake).
// @return []api.JoinTokenInfo The canned JoinTokens slice.
// @error error The canned JoinTokensErr, if any.
//
// @testcase TestFakeBackend checks each method records its inputs and returns the canned results.
func (f *FakeBackend) ListJoinTokens(ctx context.Context) ([]api.JoinTokenInfo, error) {
	return f.JoinTokens, f.JoinTokensErr
}

// RevokeJoinToken records the id and returns the canned RevokeTokenErr.
//
// @arg ctx Context (unused by the fake).
// @arg id The token ID, recorded into GotRevokeToken.
// @error error The canned RevokeTokenErr, if any.
//
// @testcase TestFakeBackend checks each method records its inputs and returns the canned results.
func (f *FakeBackend) RevokeJoinToken(ctx context.Context, id string) error {
	f.mu.Lock()
	f.GotRevokeToken = id
	f.mu.Unlock()
	return f.RevokeTokenErr
}

// RegenerateJoinToken records the id and returns the canned enrollment/error.
//
// @arg ctx Context (unused by the fake).
// @arg id The token ID, recorded into GotRegenToken.
// @return api.SpokeEnrollment The canned RegenTokenResult.
// @error error The canned RegenTokenErr, if any.
//
// @testcase TestFakeBackend checks each method records its inputs and returns the canned results.
func (f *FakeBackend) RegenerateJoinToken(ctx context.Context, id string) (api.SpokeEnrollment, error) {
	f.mu.Lock()
	f.GotRegenToken = id
	f.mu.Unlock()
	return f.RegenTokenResult, f.RegenTokenErr
}

// DestroyBox records the box ID and returns the canned DestroyErr.
//
// @arg ctx Context (unused by the fake).
// @arg boxID The box ID to destroy, recorded into GotDestroyID.
// @error error The canned DestroyErr, if any.
//
// @testcase TestFakeBackend checks each method records its inputs and returns the canned results.
func (f *FakeBackend) DestroyBox(ctx context.Context, boxID string) error {
	f.mu.Lock()
	f.GotDestroyID = boxID
	f.mu.Unlock()
	return f.DestroyErr
}

// PauseBox records the box ID and returns the canned PauseErr.
//
// @arg ctx Context (unused by the fake).
// @arg boxID The box ID to pause, recorded into GotPauseID.
// @error error The canned PauseErr, if any.
//
// @testcase TestFakeBackend checks each method records its inputs and returns the canned results.
func (f *FakeBackend) PauseBox(ctx context.Context, boxID string) error {
	f.mu.Lock()
	f.GotPauseID = boxID
	f.mu.Unlock()
	return f.PauseErr
}

// ResumeBox records the box ID and returns the canned ResumeErr.
//
// @arg ctx Context (unused by the fake).
// @arg boxID The box ID to resume, recorded into GotResumeID.
// @error error The canned ResumeErr, if any.
//
// @testcase TestFakeBackend checks each method records its inputs and returns the canned results.
func (f *FakeBackend) ResumeBox(ctx context.Context, boxID string) error {
	f.mu.Lock()
	f.GotResumeID = boxID
	f.mu.Unlock()
	return f.ResumeErr
}

// BoxExec records the box ID and command and returns the canned result/error.
//
// @arg ctx Context (unused by the fake).
// @arg boxID The box ID, recorded into GotExecID.
// @arg command The command, recorded into GotExecCmd.
// @return sandbox.ExecResult The canned ExecResult.
// @error error The canned ExecErr, if any.
//
// @testcase TestFakeBackend checks each method records its inputs and returns the canned results.
func (f *FakeBackend) BoxExec(ctx context.Context, boxID, command string) (sandbox.ExecResult, error) {
	f.mu.Lock()
	f.GotExecID = boxID
	f.GotExecCmd = command
	f.mu.Unlock()
	return f.ExecResult, f.ExecErr
}

// ProxyEnabled reports the canned ProxyOn.
//
// @return bool The canned ProxyOn.
//
// @testcase TestFakeBackend checks ProxyEnabled reports the canned flag.
func (f *FakeBackend) ProxyEnabled() bool { return f.ProxyOn }

// CreateProxy records the box ID, port, and description and returns the canned proxy/error.
//
// @arg ctx Context (unused by the fake).
// @arg boxID The box ID, recorded into GotProxyBoxID.
// @arg port The port, recorded into GotProxyPort.
// @arg description The description, recorded into GotProxyDesc.
// @return api.ProxyInfo The canned CreateProxyResult.
// @error error The canned CreateProxyErr, if any.
//
// @testcase TestFakeBackend checks each method records its inputs and returns the canned results.
func (f *FakeBackend) CreateProxy(ctx context.Context, boxID string, port int, description string) (api.ProxyInfo, error) {
	f.mu.Lock()
	f.GotProxyBoxID = boxID
	f.GotProxyPort = port
	f.GotProxyDesc = description
	f.mu.Unlock()
	return f.CreateProxyResult, f.CreateProxyErr
}

// DeleteProxy records the box ID and port and returns the canned DeleteProxyErr.
//
// @arg ctx Context (unused by the fake).
// @arg boxID The box ID, recorded into GotDeleteBoxID.
// @arg port The port, recorded into GotDeletePort.
// @error error The canned DeleteProxyErr, if any.
//
// @testcase TestFakeBackend checks each method records its inputs and returns the canned results.
func (f *FakeBackend) DeleteProxy(ctx context.Context, boxID string, port int) error {
	f.mu.Lock()
	f.GotDeleteBoxID = boxID
	f.GotDeletePort = port
	f.mu.Unlock()
	return f.DeleteProxyErr
}

// ListProxies records the box-ID filter and returns the canned proxies/error.
//
// @arg ctx Context (unused by the fake).
// @arg boxID The box-ID filter, recorded into GotListBoxID.
// @return []api.ProxyInfo The canned Proxies slice.
// @error error The canned ListProxiesErr, if any.
//
// @testcase TestFakeBackend checks each method records its inputs and returns the canned results.
func (f *FakeBackend) ListProxies(ctx context.Context, boxID string) ([]api.ProxyInfo, error) {
	f.mu.Lock()
	f.GotListBoxID = boxID
	f.mu.Unlock()
	return f.Proxies, f.ListProxiesErr
}

// ConnectMCP builds an MCP server over backend and returns an in-memory-connected
// client session, so a test can drive the real MCP tools end to end. The session
// is closed automatically when the test finishes. Pass an api.Client as the
// backend to exercise the full stand-alone path (MCP tools → HTTP → server).
//
// @arg t The test the session's lifetime is tied to.
// @arg backend The backend the MCP tools run against.
// @arg name The MCP server implementation name.
// @arg version The MCP server implementation version.
// @return *mcp.ClientSession A connected MCP client session, closed on test cleanup.
//
// @testcase TestConnectMCP lists the registered tools over the returned session.
func ConnectMCP(t testing.TB, backend api.Backend, name, version string) *mcp.ClientSession {
	t.Helper()
	srv := mcpserver.NewServer(backend, name, version)
	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), serverT, nil); err != nil {
		t.Fatalf("mcp server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), clientT, nil)
	if err != nil {
		t.Fatalf("mcp client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}
