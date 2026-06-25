// Package testutils holds test doubles shared across the llmbox test suites. It
// deliberately depends only on the box-layer interfaces (internal/cluster and
// internal/docker) and never on internal/server, so both the in-package server
// tests and the external e2e suite can import it without an import cycle.
package testutils

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/clems4ever/llmbox/internal/cluster"
	"github.com/clems4ever/llmbox/internal/docker"
)

// FakeMgr is a stand-in for *docker.Manager: it records the calls it receives
// and returns canned results, satisfying cluster.BoxManager.
type FakeMgr struct {
	mu sync.Mutex

	CreateID  string
	CreateURL string
	CreateErr error

	SubmitURL string
	SubmitErr error
	GotCode   string

	ListResult []docker.Box
	Destroyed  []string
	Reaped     []string

	LogsResult string
	LogsErr    error
	GotLogsID  string
	GotLogsN   int

	ExecResult docker.ExecResult
	ExecErr    error
	GotExecID  string
	GotExecCmd []string

	GotOpts docker.CreateOptions
}

// Create records the requested options and returns the canned ID/URL/error.
//
// @arg ctx Context (unused by the fake).
// @arg opts The create options, recorded into GotOpts.
// @return string The canned container ID.
// @return string The canned authorize URL.
// @error error The canned create error, if any.
func (f *FakeMgr) Create(_ context.Context, opts docker.CreateOptions) (string, string, error) {
	f.mu.Lock()
	f.GotOpts = opts
	f.mu.Unlock()
	return f.CreateID, f.CreateURL, f.CreateErr
}

// SubmitCode records the submitted code and returns the canned URL/error.
//
// @arg ctx Context (unused by the fake).
// @arg idOrName The box identifier (ignored).
// @arg code The submitted OAuth code, recorded into GotCode.
// @return string The canned session URL.
// @error error The canned submit error, if any.
func (f *FakeMgr) SubmitCode(_ context.Context, _, code string) (string, error) {
	f.mu.Lock()
	f.GotCode = code
	f.mu.Unlock()
	return f.SubmitURL, f.SubmitErr
}

// List returns the canned boxes.
//
// @arg ctx Context (unused by the fake).
// @return []docker.Box The canned ListResult slice.
// @error error Always nil.
func (f *FakeMgr) List(context.Context) ([]docker.Box, error) { return f.ListResult, nil }

// Destroy records the destroyed ID and always succeeds.
//
// @arg ctx Context (unused by the fake).
// @arg id The identifier to destroy, appended to Destroyed.
// @error error Always nil.
func (f *FakeMgr) Destroy(_ context.Context, id string) error {
	f.Destroyed = append(f.Destroyed, id)
	return nil
}

// Logs records the requested box ID and tail and returns the canned output/error.
//
// @arg ctx Context (unused by the fake).
// @arg id The box identifier, recorded into GotLogsID.
// @arg tail The requested tail count, recorded into GotLogsN.
// @return string The canned LogsResult.
// @error error The canned logs error, if any.
func (f *FakeMgr) Logs(_ context.Context, id string, tail int) (string, error) {
	f.mu.Lock()
	f.GotLogsID = id
	f.GotLogsN = tail
	f.mu.Unlock()
	return f.LogsResult, f.LogsErr
}

// Exec records the requested box ID and command and returns the canned result/error.
//
// @arg ctx Context (unused by the fake).
// @arg id The box identifier, recorded into GotExecID.
// @arg cmd The command, recorded into GotExecCmd.
// @return docker.ExecResult The canned ExecResult.
// @error error The canned exec error, if any.
func (f *FakeMgr) Exec(_ context.Context, id string, cmd []string) (docker.ExecResult, error) {
	f.mu.Lock()
	f.GotExecID = id
	f.GotExecCmd = cmd
	f.mu.Unlock()
	return f.ExecResult, f.ExecErr
}

// ReapOrphans returns the canned reaped IDs.
//
// @arg ctx Context (unused by the fake).
// @arg ttl The orphan TTL (ignored).
// @return []string The canned Reaped slice.
// @error error Always nil.
func (f *FakeMgr) ReapOrphans(context.Context, time.Duration) ([]string, error) {
	return f.Reaped, nil
}

// FakeHub is a stand-in for the server's spoke hub: tests inject connected
// spokes directly and assert which ones were disconnected.
type FakeHub struct {
	Connected    map[string]cluster.BoxManager // spokes injected by tests, keyed by name
	Disconnected []string                      // names passed to Disconnect, for assertions
}

// Spoke returns the connected spoke with the given name.
//
// @arg name The spoke name to look up.
// @return cluster.BoxManager The connected spoke's box manager, if present.
// @return bool True when a spoke with that name is connected.
func (h *FakeHub) Spoke(name string) (cluster.BoxManager, bool) {
	bm, ok := h.Connected[name]
	return bm, ok
}

// Spokes returns the connected spokes.
//
// @return map[string]cluster.BoxManager The connected spokes keyed by name.
func (h *FakeHub) Spokes() map[string]cluster.BoxManager { return h.Connected }

// ConnectHandler is a no-op; tests inject spokes directly.
//
// @arg w The response writer (unused).
// @arg r The request (unused).
func (h *FakeHub) ConnectHandler(http.ResponseWriter, *http.Request) {}

// Disconnect records the name so tests can assert a spoke was kicked.
//
// @arg name The spoke name, appended to Disconnected.
func (h *FakeHub) Disconnect(name string) { h.Disconnected = append(h.Disconnected, name) }
