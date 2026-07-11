package conformance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/cluster"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/box"
)

// box.Manager must satisfy the cluster box-verb surface for every backend.
var _ cluster.BoxManager = (*box.Manager)(nil)

// NewProvisioner builds a fresh provisioner for one subtest. A backend supplies
// one of these to Run; the Fake's is conformance.NewFake.
type NewProvisioner func(t testing.TB) box.Provisioner

// Run executes the backend-neutral behavioural contract against a Manager built
// over newProv. Every backend (the in-process Fake, the Docker provisioner) must
// pass exactly these assertions, so the two isolators are proven to behave
// identically. Each subtest gets its own provisioner so state never leaks between
// them.
//
// @arg t The test to run the contract under.
// @arg newProv Builds a fresh provisioner per subtest.
//
// @testcase TestConformanceFake runs the contract against the Fake provisioner.
func Run(t *testing.T, newProv NewProvisioner) {
	t.Run("Lifecycle", func(t *testing.T) { testLifecycle(t, newProv) })
	t.Run("DestroyIdempotent", func(t *testing.T) { testDestroyIdempotent(t, newProv) })
	t.Run("ListAndFind", func(t *testing.T) { testListAndFind(t, newProv) })
	t.Run("InvalidBoxID", func(t *testing.T) { testInvalidBoxID(t, newProv) })
	t.Run("DuplicateBoxID", func(t *testing.T) { testDuplicateBoxID(t, newProv) })
	t.Run("MaxBoxes", func(t *testing.T) { testMaxBoxes(t, newProv) })
	t.Run("ReapOrphans", func(t *testing.T) { testReapOrphans(t, newProv) })
	t.Run("InitScript", func(t *testing.T) { testInitScript(t, newProv) })
	t.Run("InitScriptFailure", func(t *testing.T) { testInitScriptFailure(t, newProv) })
	t.Run("PauseResume", func(t *testing.T) { testPauseResume(t, newProv) })
}

// opCtx returns a context bounded to a generous per-operation timeout, cancelled
// when the test ends.
//
// @arg t The test the context cancellation is scoped to.
// @return context.Context A per-operation context.
//
// @testcase TestConformanceFake uses opCtx to bound its operations.
func opCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// testLifecycle runs the full create/login/exec/logs lifecycle.
//
// @arg t The test to assert under.
// @arg newProv Builds the provisioner under test.
//
// @testcase TestConformanceFake runs testLifecycle as a subtest.
func testLifecycle(t *testing.T, newProv NewProvisioner) {
	m := box.NewManager(newProv(t), box.Config{})
	ctx := opCtx(t)

	created, err := m.Create(ctx, sandbox.CreateOptions{BoxID: "life-box"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := created.InstanceID
	if !strings.Contains(created.AuthorizeURL, "oauth/authorize") {
		t.Fatalf("authorize URL = %q", created.AuthorizeURL)
	}

	session, err := m.SubmitCode(ctx, id, "the-code")
	if err != nil {
		t.Fatalf("SubmitCode: %v", err)
	}
	if !strings.HasPrefix(session, "https://claude.ai/") {
		t.Fatalf("session URL = %q", session)
	}

	boxes, err := m.List(ctx)
	if err != nil || len(boxes) != 1 {
		t.Fatalf("List = %v, %v", boxes, err)
	}
	if boxes[0].Phase != "ready" {
		t.Fatalf("phase = %q, want ready after SubmitCode", boxes[0].Phase)
	}

	res, err := m.Exec(ctx, id, []string{"echo", "hello-box"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "hello-box" || res.ExitCode != 0 {
		t.Fatalf("Exec = %+v", res)
	}

	logs, err := m.Logs(ctx, id, 0)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !strings.Contains(logs, "Remote control session ready") {
		t.Fatalf("logs missing remote-control banner:\n%s", logs)
	}
}

// testInitScript checks a host-provided init script runs inside the box during
// Create, before claude starts, as the box user: its side effect (a file written
// into the box) is observable afterwards via Exec. It is part of the shared
// contract so every backend proves the provisioning hook fires in a real box.
//
// @arg t The test to assert under.
// @arg newProv Builds the provisioner under test.
//
// @testcase TestConformanceFake runs testInitScript as a subtest.
func testInitScript(t *testing.T, newProv NewProvisioner) {
	script := "#!/bin/sh\necho box-was-provisioned > \"$HOME/init-marker\"\n"
	m := box.NewManager(newProv(t), box.Config{InitScript: []byte(script)})
	ctx := opCtx(t)

	created, err := m.Create(ctx, sandbox.CreateOptions{BoxID: "init-box"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := created.InstanceID
	res, err := m.Exec(ctx, id, []string{"sh", "-c", "cat \"$HOME/init-marker\""})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "box-was-provisioned" || res.ExitCode != 0 {
		t.Fatalf("init script side effect missing: %+v", res)
	}
}

// testInitScriptFailure checks a non-zero init script does not fail Create but
// keeps the box for inspection, reporting the failure and the script's output in
// the CreateResult. The box is still listed (spared from the reaper) and remains
// reachable, so an operator can debug why provisioning broke rather than facing a
// vanished box.
//
// @arg t The test to assert under.
// @arg newProv Builds the provisioner under test.
//
// @testcase TestConformanceFake runs testInitScriptFailure as a subtest.
func testInitScriptFailure(t *testing.T, newProv NewProvisioner) {
	m := box.NewManager(newProv(t), box.Config{InitScript: []byte("#!/bin/sh\necho boom-in-init >&2\nexit 9\n")})
	ctx := opCtx(t)

	created, err := m.Create(ctx, sandbox.CreateOptions{BoxID: "bad-init"})
	if err != nil {
		t.Fatalf("Create should keep a broken box, not error: %v", err)
	}
	if !created.InitScriptFailed {
		t.Fatal("CreateResult should report InitScriptFailed for a non-zero init script")
	}
	if !strings.Contains(created.InitScriptOutput, "boom-in-init") {
		t.Fatalf("InitScriptOutput = %q, want it to carry the script output", created.InitScriptOutput)
	}
	boxes, err := m.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(boxes) != 1 {
		t.Fatalf("failed init left %d boxes, want the broken box kept for inspection", len(boxes))
	}
	// The box was provisioned and its guest is up, so it stays reachable for debugging.
	if res, err := m.Exec(ctx, created.InstanceID, []string{"echo", "still-here"}); err != nil || strings.TrimSpace(res.Stdout) != "still-here" {
		t.Fatalf("broken box should stay reachable: err=%v res=%+v", err, res)
	}
}

// testPauseResume checks the pause/resume lifecycle end to end on a real box:
// a ready box is paused (freeing compute — its guest goes away, so it reports
// paused and Exec no longer reaches it), then resumed (its compute reboots from the
// kept disk and claude relaunches to a fresh session URL, no re-login, since the
// creds seeded on disk survive). It is part of the shared contract so every backend
// proves pause frees compute while keeping disk, and resume brings the box back.
//
// @arg t The test to assert under.
// @arg newProv Builds the provisioner under test.
//
// @testcase TestConformanceFake runs testPauseResume as a subtest.
func testPauseResume(t *testing.T, newProv NewProvisioner) {
	m := box.NewManager(newProv(t), box.Config{})
	ctx := opCtx(t)

	created, err := m.Create(ctx, sandbox.CreateOptions{BoxID: "pause-box"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := created.InstanceID
	if _, err := m.SubmitCode(ctx, id, "the-code"); err != nil {
		t.Fatalf("SubmitCode: %v", err)
	}
	// Seed a credentials file on the box's disk so that, after a resume reboots the
	// box from that disk, claude comes straight up to a session without re-login —
	// exactly as a real authenticated box would.
	if res, err := m.Exec(ctx, id, []string{"sh", "-c", `mkdir -p "$HOME/.claude" && printf '{"t":"x"}' > "$HOME/.claude/.credentials.json"`}); err != nil || res.ExitCode != 0 {
		t.Fatalf("seeding creds: err=%v res=%+v", err, res)
	}

	if err := m.Pause(ctx, id); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	boxes, err := m.List(ctx)
	if err != nil {
		t.Fatalf("List after pause: %v", err)
	}
	if s := findState(boxes, id); s != sandbox.StatePaused {
		t.Fatalf("state after pause = %q, want %q", s, sandbox.StatePaused)
	}
	// Compute is gone: the guest is down, so Exec can no longer reach the box.
	if _, err := m.Exec(ctx, id, []string{"echo", "hi"}); err == nil {
		t.Fatal("Exec should fail on a paused box (its guest is stopped)")
	}

	sessionURL, err := m.Resume(ctx, id)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !strings.HasPrefix(sessionURL, "https://claude.ai/") {
		t.Fatalf("resume session URL = %q, want a claude.ai session URL (box should come up authenticated)", sessionURL)
	}
	boxes, err = m.List(ctx)
	if err != nil {
		t.Fatalf("List after resume: %v", err)
	}
	if s := findState(boxes, id); s != "running" {
		t.Fatalf("state after resume = %q, want running", s)
	}
	// Compute is back: Exec reaches the box again.
	if res, err := m.Exec(ctx, id, []string{"echo", "back"}); err != nil || strings.TrimSpace(res.Stdout) != "back" {
		t.Fatalf("Exec after resume: err=%v res=%+v", err, res)
	}
}

// findState returns the reported State of the box with the given instance id, or ""
// when absent.
//
// @arg boxes The boxes to search.
// @arg id The instance id to match.
// @return string The matched box's State, or "" when not found.
//
// @testcase TestConformanceFake relies on findState to read a box's state.
func findState(boxes []sandbox.Box, id string) string {
	for _, b := range boxes {
		if b.InstanceID == id {
			return b.State
		}
	}
	return ""
}

// testDestroyIdempotent checks Destroy is idempotent.
//
// @arg t The test to assert under.
// @arg newProv Builds the provisioner under test.
//
// @testcase TestConformanceFake runs testDestroyIdempotent as a subtest.
func testDestroyIdempotent(t *testing.T, newProv NewProvisioner) {
	m := box.NewManager(newProv(t), box.Config{})
	ctx := opCtx(t)
	created, err := m.Create(ctx, sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := created.InstanceID
	if err := m.Destroy(ctx, id); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if err := m.Destroy(ctx, id); err != nil {
		t.Fatalf("second Destroy should be a no-op, got %v", err)
	}
	if err := m.Destroy(ctx, "never-existed"); err != nil {
		t.Fatalf("Destroy of unknown box should be a no-op, got %v", err)
	}
}

// testListAndFind checks List and unknown-box resolution.
//
// @arg t The test to assert under.
// @arg newProv Builds the provisioner under test.
//
// @testcase TestConformanceFake runs testListAndFind as a subtest.
func testListAndFind(t *testing.T, newProv NewProvisioner) {
	m := box.NewManager(newProv(t), box.Config{})
	ctx := opCtx(t)
	if _, err := m.Create(ctx, sandbox.CreateOptions{BoxID: "a"}); err != nil {
		t.Fatalf("Create a: %v", err)
	}
	if _, err := m.Create(ctx, sandbox.CreateOptions{BoxID: "b"}); err != nil {
		t.Fatalf("Create b: %v", err)
	}
	boxes, err := m.List(ctx)
	if err != nil || len(boxes) != 2 {
		t.Fatalf("List = %v (%d), %v", boxes, len(boxes), err)
	}
	if _, err := m.Logs(ctx, "no-such-box", 0); !errors.Is(err, sandbox.ErrBoxNotFound) {
		t.Fatalf("Logs of unknown box err = %v, want ErrBoxNotFound", err)
	}
}

// testInvalidBoxID checks a malformed box id is rejected.
//
// @arg t The test to assert under.
// @arg newProv Builds the provisioner under test.
//
// @testcase TestConformanceFake runs testInvalidBoxID as a subtest.
func testInvalidBoxID(t *testing.T, newProv NewProvisioner) {
	m := box.NewManager(newProv(t), box.Config{})
	if _, err := m.Create(opCtx(t), sandbox.CreateOptions{BoxID: "Bad_ID"}); err == nil {
		t.Fatal("Create with an invalid box id should fail")
	}
}

// testDuplicateBoxID checks a duplicate box id is rejected.
//
// @arg t The test to assert under.
// @arg newProv Builds the provisioner under test.
//
// @testcase TestConformanceFake runs testDuplicateBoxID as a subtest.
func testDuplicateBoxID(t *testing.T, newProv NewProvisioner) {
	m := box.NewManager(newProv(t), box.Config{})
	ctx := opCtx(t)
	if _, err := m.Create(ctx, sandbox.CreateOptions{BoxID: "dup"}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, err := m.Create(ctx, sandbox.CreateOptions{BoxID: "dup"}); err == nil {
		t.Fatal("second Create with the same box id should fail")
	}
}

// testMaxBoxes checks the box-count cap.
//
// @arg t The test to assert under.
// @arg newProv Builds the provisioner under test.
//
// @testcase TestConformanceFake runs testMaxBoxes as a subtest.
func testMaxBoxes(t *testing.T, newProv NewProvisioner) {
	m := box.NewManager(newProv(t), box.Config{MaxBoxes: 1})
	ctx := opCtx(t)
	if _, err := m.Create(ctx, sandbox.CreateOptions{}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, err := m.Create(ctx, sandbox.CreateOptions{}); err == nil {
		t.Fatal("Create past MaxBoxes should fail")
	}
}

// testReapOrphans checks orphan reaping by phase and age.
//
// @arg t The test to assert under.
// @arg newProv Builds the provisioner under test.
//
// @testcase TestConformanceFake runs testReapOrphans as a subtest.
func testReapOrphans(t *testing.T, newProv NewProvisioner) {
	m := box.NewManager(newProv(t), box.Config{})
	ctx := opCtx(t)

	pending, err := m.Create(ctx, sandbox.CreateOptions{BoxID: "pending"})
	if err != nil {
		t.Fatalf("Create pending: %v", err)
	}
	pendingID := pending.InstanceID
	ready, err := m.Create(ctx, sandbox.CreateOptions{BoxID: "ready"})
	if err != nil {
		t.Fatalf("Create ready: %v", err)
	}
	readyID := ready.InstanceID
	if _, err := m.SubmitCode(ctx, readyID, "code"); err != nil {
		t.Fatalf("SubmitCode: %v", err)
	}

	// A fresh pending box is within any positive ttl, so nothing is reaped.
	if reaped, err := m.ReapOrphans(ctx, time.Hour); err != nil || len(reaped) != 0 {
		t.Fatalf("ReapOrphans(1h) = %v, %v, want none reaped", reaped, err)
	}

	// A negative ttl puts the cutoff in the future, so every *pending* box is
	// stale; the ready box must still be spared.
	reaped, err := m.ReapOrphans(ctx, -time.Hour)
	if err != nil {
		t.Fatalf("ReapOrphans(-1h): %v", err)
	}
	if len(reaped) != 1 || reaped[0] != pendingID {
		t.Fatalf("reaped = %v, want only the pending box %q", reaped, pendingID)
	}
	boxes, _ := m.List(ctx)
	if len(boxes) != 1 || boxes[0].BoxID != "ready" {
		t.Fatalf("after reap, boxes = %v, want only the ready box", boxes)
	}
}
