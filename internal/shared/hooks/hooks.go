// Package hooks runs external hook executables at box lifecycle events. A hook is
// any program llmbox runs as a subprocess, exchanging JSON over stdin/stdout per
// the hookproto contract. This keeps llmbox free of any knowledge of what a hook
// does — for example, a granular hook (in its own repo) mints a subject token and
// installs the granular CLIs into each box, while llmbox only injects the files
// the hook returns and replays the hook's opaque state on destroy.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/clems4ever/llmbox/hookproto"
)

// BoxInfo describes a box to a hook. It aliases the wire type so callers need not
// import hookproto just to build one.
type BoxInfo = hookproto.Box

// File is a resolved file a hook asked to inject into a box: its content already
// decoded and its mode already parsed, ready to hand to the Docker layer.
type File struct {
	Path    string
	Content []byte
	Mode    int64
	UID     int
	GID     int
}

// Runner runs a fixed list of hook executables for every event. A nil *Runner is
// a no-op (no hooks configured), so callers can hold one without nil-checking
// every call site.
type Runner struct {
	execs []string
}

// New builds a Runner over the given hook executable paths, or returns nil when
// the list is empty so the integration stays opt-in (a nil *Runner is a no-op).
//
// @arg execs The hook executable paths to run for every lifecycle event.
// @return *Runner A Runner over the executables, or nil when execs is empty.
//
// @testcase TestNewEmptyIsNil returns nil for an empty executable list.
// @testcase TestOnCreateInjectsFilesAndState runs a configured hook executable.
func New(execs []string) *Runner {
	if len(execs) == 0 {
		return nil
	}
	return &Runner{execs: execs}
}

// OnCreate runs every hook's box.create event, returning the files to inject into
// the box and a map from hook executable to the opaque state it returned (to be
// persisted and replayed on destroy). A nil Runner returns nothing. The first
// hook that fails aborts and returns its error along with the state gathered so
// far, so the caller can run OnDestroy to undo partial work.
//
// @arg ctx Context for the hook subprocess calls.
// @arg box The box the create event concerns.
// @return files The files every hook asked to inject into the box.
// @return state A map from hook executable to the opaque state it returned.
// @error error if a hook fails to run or returns an unusable response.
//
// @testcase TestOnCreateNilIsNoop returns nothing for a nil Runner.
// @testcase TestOnCreateInjectsFilesAndState aggregates files and keys state by hook.
// @testcase TestOnCreateFailingHookErrors surfaces a failing hook's error and partial state.
func (r *Runner) OnCreate(ctx context.Context, box BoxInfo) (files []File, state map[string]string, err error) {
	if r == nil {
		return nil, nil, nil
	}
	state = make(map[string]string, len(r.execs))
	for _, h := range r.execs {
		resp, rerr := r.run(ctx, h, hookproto.Request{Event: hookproto.EventBoxCreate, Box: box})
		if rerr != nil {
			return files, state, fmt.Errorf("hook %s: %w", h, rerr)
		}
		if resp.State != "" {
			state[h] = resp.State
		}
		for _, wf := range resp.Files {
			content, berr := wf.Bytes()
			if berr != nil {
				return files, state, fmt.Errorf("hook %s: %w", h, berr)
			}
			mode, merr := wf.FileMode()
			if merr != nil {
				return files, state, fmt.Errorf("hook %s: %w", h, merr)
			}
			files = append(files, File{Path: wf.Path, Content: content, Mode: mode, UID: wf.UID, GID: wf.GID})
		}
	}
	return files, state, nil
}

// OnDestroy runs every hook's box.destroy event, replaying the opaque state each
// hook returned at create time (from the map keyed by hook executable). It is
// best-effort: it runs all hooks and returns the first error, so a caller can log
// it while still tearing the box down. A nil Runner or empty state is a no-op.
//
// @arg ctx Context for the hook subprocess calls.
// @arg box The box the destroy event concerns.
// @arg state The per-hook state captured by OnCreate.
// @error error if any hook fails to run; the remaining hooks still run.
//
// @testcase TestOnDestroyNilIsNoop does nothing for a nil Runner or empty state.
// @testcase TestOnDestroyReplaysState passes each hook back its own create-time state.
func (r *Runner) OnDestroy(ctx context.Context, box BoxInfo, state map[string]string) error {
	if r == nil || len(state) == 0 {
		return nil
	}
	var firstErr error
	for _, h := range r.execs {
		st, ok := state[h]
		if !ok {
			continue
		}
		if _, err := r.run(ctx, h, hookproto.Request{Event: hookproto.EventBoxDestroy, Box: box, State: st}); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("hook %s: %w", h, err)
		}
	}
	return firstErr
}

// run invokes one hook executable, writing req as JSON to its stdin and decoding
// its stdout as a hookproto.Response. A non-zero exit is an error carrying the
// hook's stderr, so a failing hook's message reaches the caller.
//
// @arg ctx Context for the subprocess.
// @arg execPath The hook executable to run.
// @arg req The request to write to the hook's stdin.
// @return hookproto.Response The response decoded from the hook's stdout.
// @error error if the request cannot be encoded, the hook exits non-zero, or its output cannot be decoded.
//
// @testcase TestOnCreateInjectsFilesAndState drives run via OnCreate against a real hook script.
// @testcase TestOnCreateFailingHookErrors checks run surfaces a non-zero exit and stderr.
func (r *Runner) run(ctx context.Context, execPath string, req hookproto.Request) (hookproto.Response, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return hookproto.Response{}, fmt.Errorf("encoding request: %w", err)
	}
	cmd := exec.CommandContext(ctx, execPath)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return hookproto.Response{}, fmt.Errorf("%w: %s", err, msg)
		}
		return hookproto.Response{}, err
	}
	if stdout.Len() == 0 {
		return hookproto.Response{}, nil
	}
	var resp hookproto.Response
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return hookproto.Response{}, fmt.Errorf("decoding response: %w", err)
	}
	return resp, nil
}
