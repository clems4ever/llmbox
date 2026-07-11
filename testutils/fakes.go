// Package testutils holds test doubles shared across the llmbox test suites. It
// depends only on the box- and tool-layer interfaces (internal/cluster,
// internal/sandbox, internal/mcpserver) and never on internal/server, so both the
// in-package server tests and the external e2e suite can import it without an
// import cycle.
package testutils

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/cluster"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// FakeMgr is a stand-in for *docker.Manager: it records the calls it receives
// and returns canned results, satisfying cluster.BoxManager.
type FakeMgr struct {
	mu sync.Mutex

	CreateID               string
	CreateURL              string
	CreateErr              error
	CreateInitScriptFailed bool
	CreateInitScriptOutput string

	SubmitURL string
	SubmitErr error
	GotCode   string

	ListResult []sandbox.Box
	// created models box existence: Create appends, Destroy removes, and List
	// returns these on top of ListResult — so a box created through the fake shows
	// up in the box list, like a real spoke.
	created    []sandbox.Box
	listCalls  int
	Destroyed  []string
	DestroyErr error
	Reaped     []string

	Paused    []string
	PauseErr  error
	Resumed   []string
	ResumeURL string
	ResumeErr error

	LogsResult string
	LogsErr    error
	GotLogsID  string
	GotLogsN   int

	ExecResult sandbox.ExecResult
	ExecErr    error
	GotExecID  string
	GotExecCmd []string

	GotOpts sandbox.CreateOptions
}

// Create records the requested options and returns the canned result/error. On
// success it also records the box so it appears in List, modelling a real spoke;
// the canned CreateInitScriptFailed/Output model a spoke that kept a broken box.
//
// @arg ctx Context (unused by the fake).
// @arg opts The create options, recorded into GotOpts.
// @return sandbox.CreateResult The canned create result (ID, URL, and any init-script failure).
// @error error The canned create error, if any.
//
// @testcase TestFakeMgr checks each verb records its inputs and returns the canned results.
func (f *FakeMgr) Create(ctx context.Context, opts sandbox.CreateOptions) (sandbox.CreateResult, error) {
	f.mu.Lock()
	f.GotOpts = opts
	if f.CreateErr == nil {
		f.created = append(f.created, sandbox.Box{BoxID: opts.BoxID, InstanceID: f.CreateID})
	}
	f.mu.Unlock()
	return sandbox.CreateResult{
		InstanceID:       f.CreateID,
		AuthorizeURL:     f.CreateURL,
		InitScriptFailed: f.CreateInitScriptFailed,
		InitScriptOutput: f.CreateInitScriptOutput,
	}, f.CreateErr
}

// SubmitCode records the submitted code and returns the canned URL/error.
//
// @arg ctx Context (unused by the fake).
// @arg idOrName The box identifier (ignored).
// @arg code The submitted OAuth code, recorded into GotCode.
// @return string The canned session URL.
// @error error The canned submit error, if any.
//
// @testcase TestFakeMgr checks each verb records its inputs and returns the canned results.
func (f *FakeMgr) SubmitCode(ctx context.Context, idOrName, code string) (string, error) {
	f.mu.Lock()
	f.GotCode = code
	f.mu.Unlock()
	return f.SubmitURL, f.SubmitErr
}

// List returns the canned ListResult plus any boxes created (and not destroyed)
// through the fake, so box existence tracks Create/Destroy like a real spoke.
//
// @arg ctx Context (unused by the fake).
// @return []sandbox.Box ListResult followed by the still-live created boxes.
// @error error Always nil.
//
// @testcase TestFakeMgr checks each verb records its inputs and returns the canned results.
func (f *FakeMgr) List(ctx context.Context) ([]sandbox.Box, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	out := append([]sandbox.Box{}, f.ListResult...)
	return append(out, f.created...), nil
}

// ListCalls reports how many times List was called, so a test can assert a code
// path did (or did not) consult the spoke's live inventory.
//
// @return int The number of List calls received so far.
//
// @testcase TestFakeMgr checks each verb records its inputs and returns the canned results.
func (f *FakeMgr) ListCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

// Destroy records the destroyed ID and returns the canned DestroyErr (nil by
// default), letting a test simulate a spoke whose box is already gone.
//
// @arg ctx Context (unused by the fake).
// @arg id The identifier to destroy, appended to Destroyed.
// @error error The canned DestroyErr, if any.
//
// @testcase TestFakeMgr checks Destroy records the ID and surfaces the canned DestroyErr.
func (f *FakeMgr) Destroy(ctx context.Context, id string) error {
	f.mu.Lock()
	f.Destroyed = append(f.Destroyed, id)
	var kept []sandbox.Box
	for _, b := range f.created {
		if b.BoxID != id && b.InstanceID != id {
			kept = append(kept, b)
		}
	}
	f.created = kept
	f.mu.Unlock()
	return f.DestroyErr
}

// Pause records the paused ID and returns the canned PauseErr.
//
// @arg ctx Context (unused by the fake).
// @arg id The identifier to pause, appended to Paused.
// @error error The canned PauseErr, if any.
//
// @testcase TestFakeMgr checks Pause records the ID and surfaces the canned PauseErr.
func (f *FakeMgr) Pause(ctx context.Context, id string) error {
	f.mu.Lock()
	f.Paused = append(f.Paused, id)
	f.mu.Unlock()
	return f.PauseErr
}

// Resume records the resumed ID and returns the canned session URL/error.
//
// @arg ctx Context (unused by the fake).
// @arg id The identifier to resume, appended to Resumed.
// @return string The canned ResumeURL.
// @error error The canned ResumeErr, if any.
//
// @testcase TestFakeMgr checks Resume records the ID and surfaces the canned session URL and error.
func (f *FakeMgr) Resume(ctx context.Context, id string) (string, error) {
	f.mu.Lock()
	f.Resumed = append(f.Resumed, id)
	f.mu.Unlock()
	return f.ResumeURL, f.ResumeErr
}

// Logs records the requested box ID and tail and returns the canned output/error.
//
// @arg ctx Context (unused by the fake).
// @arg id The box identifier, recorded into GotLogsID.
// @arg tail The requested tail count, recorded into GotLogsN.
// @return string The canned LogsResult.
// @error error The canned logs error, if any.
//
// @testcase TestFakeMgr checks each verb records its inputs and returns the canned results.
func (f *FakeMgr) Logs(ctx context.Context, id string, tail int) (string, error) {
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
// @return sandbox.ExecResult The canned ExecResult.
// @error error The canned exec error, if any.
//
// @testcase TestFakeMgr checks each verb records its inputs and returns the canned results.
func (f *FakeMgr) Exec(ctx context.Context, id string, cmd []string) (sandbox.ExecResult, error) {
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
//
// @testcase TestFakeMgr checks each verb records its inputs and returns the canned results.
func (f *FakeMgr) ReapOrphans(ctx context.Context, ttl time.Duration) ([]string, error) {
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
//
// @testcase TestFakeHub checks Spoke returns injected spokes and reports missing ones.
func (h *FakeHub) Spoke(name string) (cluster.BoxManager, bool) {
	bm, ok := h.Connected[name]
	return bm, ok
}

// Spokes returns the connected spokes.
//
// @return map[string]cluster.BoxManager The connected spokes keyed by name.
//
// @testcase TestFakeHub checks Spokes returns the injected spokes.
func (h *FakeHub) Spokes() map[string]cluster.BoxManager { return h.Connected }

// ConnectHandler is a no-op; tests inject spokes directly.
//
// @arg w The response writer (unused).
// @arg r The request (unused).
//
// @testcase TestFakeHub checks ConnectHandler is inert.
func (h *FakeHub) ConnectHandler(w http.ResponseWriter, r *http.Request) {}

// Disconnect records the name so tests can assert a spoke was kicked.
//
// @arg name The spoke name, appended to Disconnected.
//
// @testcase TestFakeHub checks Disconnect records the kicked spoke name.
func (h *FakeHub) Disconnect(name string) { h.Disconnected = append(h.Disconnected, name) }
