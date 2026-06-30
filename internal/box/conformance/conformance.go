package conformance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/box"
	"github.com/clems4ever/llmbox/internal/cluster"
	"github.com/clems4ever/llmbox/internal/sandbox"
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

	id, authURL, err := m.Create(ctx, sandbox.CreateOptions{BoxID: "life-box"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.Contains(authURL, "oauth/authorize") {
		t.Fatalf("authorize URL = %q", authURL)
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

// testDestroyIdempotent checks Destroy is idempotent.
//
// @arg t The test to assert under.
// @arg newProv Builds the provisioner under test.
//
// @testcase TestConformanceFake runs testDestroyIdempotent as a subtest.
func testDestroyIdempotent(t *testing.T, newProv NewProvisioner) {
	m := box.NewManager(newProv(t), box.Config{})
	ctx := opCtx(t)
	id, _, err := m.Create(ctx, sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
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
	if _, _, err := m.Create(ctx, sandbox.CreateOptions{BoxID: "a"}); err != nil {
		t.Fatalf("Create a: %v", err)
	}
	if _, _, err := m.Create(ctx, sandbox.CreateOptions{BoxID: "b"}); err != nil {
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
	if _, _, err := m.Create(opCtx(t), sandbox.CreateOptions{BoxID: "Bad_ID"}); err == nil {
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
	if _, _, err := m.Create(ctx, sandbox.CreateOptions{BoxID: "dup"}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, _, err := m.Create(ctx, sandbox.CreateOptions{BoxID: "dup"}); err == nil {
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
	if _, _, err := m.Create(ctx, sandbox.CreateOptions{}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, _, err := m.Create(ctx, sandbox.CreateOptions{}); err == nil {
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

	pendingID, _, err := m.Create(ctx, sandbox.CreateOptions{BoxID: "pending"})
	if err != nil {
		t.Fatalf("Create pending: %v", err)
	}
	readyID, _, err := m.Create(ctx, sandbox.CreateOptions{BoxID: "ready"})
	if err != nil {
		t.Fatalf("Create ready: %v", err)
	}
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
