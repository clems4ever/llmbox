package firecracker

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// writeMeta persists a box meta under stateDir so the operator functions (which read
// on-disk state, not a live provisioner) have something to list and destroy.
func writeMeta(t *testing.T, stateDir string, m boxMeta) {
	t.Helper()
	if err := m.save(stateDir); err != nil {
		t.Fatalf("save meta %s: %v", m.Token, err)
	}
}

// TestListVMs checks ListVMs returns every persisted box, oldest first, with a
// running state derived from probing each box's VMM API socket.
func TestListVMs(t *testing.T) {
	stateDir := t.TempDir()
	writeMeta(t, stateDir, boxMeta{Token: "bbbbbbbbbbbb", BoxID: "second", Phase: "ready", Created: 200, NetIndex: 1})
	writeMeta(t, stateDir, boxMeta{Token: "aaaaaaaaaaaa", BoxID: "first", Phase: "pending", Created: 100, NetIndex: 0})

	// A live VMM answers on its fc.sock; give the first box a listening socket so its
	// probe reports running, and leave the second box's socket absent (stopped).
	ln, err := net.Listen("unix", filepath.Join(boxDir(stateDir, "aaaaaaaaaaaa"), "fc.sock"))
	if err != nil {
		t.Fatalf("listen fc.sock: %v", err)
	}
	defer func() { _ = ln.Close() }()

	vms, err := ListVMs(stateDir)
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(vms) != 2 {
		t.Fatalf("ListVMs len = %d, want 2", len(vms))
	}
	// Oldest first: the Created=100 box leads.
	if vms[0].Token != "aaaaaaaaaaaa" || vms[1].Token != "bbbbbbbbbbbb" {
		t.Fatalf("order = %q,%q, want aaaaaaaaaaaa,bbbbbbbbbbbb", vms[0].Token, vms[1].Token)
	}
	if !vms[0].Running {
		t.Fatal("box with a live fc.sock reported not running")
	}
	if vms[1].Running {
		t.Fatal("box with no fc.sock reported running")
	}
	if vms[0].BoxID != "first" || vms[0].Phase != "pending" {
		t.Fatalf("unexpected snapshot: %+v", vms[0])
	}
}

// TestListVMsEmpty checks ListVMs on a missing/empty state dir yields no boxes and no
// error, so `vm list` on a fresh host prints cleanly rather than failing.
func TestListVMsEmpty(t *testing.T) {
	vms, err := ListVMs(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("ListVMs on empty: %v", err)
	}
	if len(vms) != 0 {
		t.Fatalf("ListVMs len = %d, want 0", len(vms))
	}
}

// TestDestroyVM checks DestroyVM removes the matched box's state directory while
// leaving every other box untouched.
func TestDestroyVM(t *testing.T) {
	stateDir := t.TempDir()
	writeMeta(t, stateDir, boxMeta{Token: "keepkeepkeep", BoxID: "keep", Phase: "ready", Created: 1})
	writeMeta(t, stateDir, boxMeta{Token: "goneg0neg0ne", BoxID: "gone", Phase: "ready", Created: 2})

	// Resolve by exact box id.
	v, err := DestroyVM(stateDir, "gone")
	if err != nil {
		t.Fatalf("DestroyVM: %v", err)
	}
	if v.Token != "goneg0neg0ne" {
		t.Fatalf("destroyed token = %q, want goneg0neg0ne", v.Token)
	}
	if _, err := os.Stat(boxDir(stateDir, "goneg0neg0ne")); !os.IsNotExist(err) {
		t.Fatalf("destroyed box dir still present: %v", err)
	}
	if _, err := os.Stat(boxDir(stateDir, "keepkeepkeep")); err != nil {
		t.Fatalf("untargeted box dir removed: %v", err)
	}
}

// TestDestroyVMByPrefix checks a unique token prefix resolves to the one box.
func TestDestroyVMByPrefix(t *testing.T) {
	stateDir := t.TempDir()
	writeMeta(t, stateDir, boxMeta{Token: "abc123abc123", Phase: "ready", Created: 1})
	if _, err := DestroyVM(stateDir, "abc1"); err != nil {
		t.Fatalf("DestroyVM by prefix: %v", err)
	}
	if _, err := os.Stat(boxDir(stateDir, "abc123abc123")); !os.IsNotExist(err) {
		t.Fatalf("box dir still present after prefix destroy: %v", err)
	}
}

// TestDestroyVMUnknown checks DestroyVM reports ErrBoxNotFound when nothing matches.
func TestDestroyVMUnknown(t *testing.T) {
	stateDir := t.TempDir()
	writeMeta(t, stateDir, boxMeta{Token: "abc123abc123", Phase: "ready", Created: 1})
	if _, err := DestroyVM(stateDir, "nope"); !errors.Is(err, sandbox.ErrBoxNotFound) {
		t.Fatalf("DestroyVM err = %v, want ErrBoxNotFound", err)
	}
}

// TestDestroyAllVMs checks DestroyAllVMs removes every persisted box and reports the
// ones it destroyed.
func TestDestroyAllVMs(t *testing.T) {
	stateDir := t.TempDir()
	writeMeta(t, stateDir, boxMeta{Token: "aaaaaaaaaaaa", BoxID: "a", Phase: "ready", Created: 1})
	writeMeta(t, stateDir, boxMeta{Token: "bbbbbbbbbbbb", BoxID: "b", Phase: "ready", Created: 2})

	destroyed, err := DestroyAllVMs(stateDir)
	if err != nil {
		t.Fatalf("DestroyAllVMs: %v", err)
	}
	if len(destroyed) != 2 {
		t.Fatalf("destroyed %d boxes, want 2", len(destroyed))
	}
	// Oldest first, so the report is deterministic.
	if destroyed[0].Token != "aaaaaaaaaaaa" || destroyed[1].Token != "bbbbbbbbbbbb" {
		t.Fatalf("destroy order = %q,%q, want aaaaaaaaaaaa,bbbbbbbbbbbb", destroyed[0].Token, destroyed[1].Token)
	}
	if left, _ := ListVMs(stateDir); len(left) != 0 {
		t.Fatalf("%d boxes remain after destroy-all, want 0", len(left))
	}
}

// TestDestroyAllVMsEmpty checks DestroyAllVMs on a clean host destroys nothing and
// returns no error.
func TestDestroyAllVMsEmpty(t *testing.T) {
	destroyed, err := DestroyAllVMs(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("DestroyAllVMs on empty: %v", err)
	}
	if len(destroyed) != 0 {
		t.Fatalf("destroyed %d boxes on a clean host, want 0", len(destroyed))
	}
}

// TestResolveMetaAmbiguousPrefix checks a prefix matching several boxes errors rather
// than silently picking one, so an operator never destroys the wrong box.
func TestResolveMetaAmbiguousPrefix(t *testing.T) {
	metas := []boxMeta{{Token: "abcd00000000"}, {Token: "abce00000000"}}
	if _, err := resolveMeta(metas, "abc"); err == nil {
		t.Fatal("resolveMeta accepted an ambiguous prefix")
	} else if errors.Is(err, sandbox.ErrBoxNotFound) {
		t.Fatalf("ambiguous prefix reported as not-found: %v", err)
	}
}
