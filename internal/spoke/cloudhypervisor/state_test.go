package cloudhypervisor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// TestSaveLoadMeta round-trips a box's metadata through its state directory and
// checks the derived socket/rootfs paths and the neutral box view.
func TestSaveLoadMeta(t *testing.T) {
	dir := t.TempDir()
	m := boxMeta{
		Token: "abc123", BoxID: "mybox", Description: "d", Image: "/r.ext4",
		Phase: "ready", Created: 42, DiskBytes: 1 << 30,
		GPUs: []string{"0000:65:00.0"}, Namespace: "ns1",
	}
	if err := m.save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}
	metas, err := loadMetas(dir)
	if err != nil {
		t.Fatalf("loadMetas: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("loadMetas = %d metas, want 1", len(metas))
	}
	got := metas[0]
	if got.Token != "abc123" || got.BoxID != "mybox" || got.DiskBytes != 1<<30 || len(got.GPUs) != 1 {
		t.Fatalf("round-trip lost fields: %+v", got)
	}
	if got.apiSockPath(dir) != filepath.Join(dir, "abc123", "ch-api.sock") {
		t.Errorf("apiSockPath = %q", got.apiSockPath(dir))
	}
	if got.vsockUDSPath(dir) != filepath.Join(dir, "abc123", "vsock.sock") {
		t.Errorf("vsockUDSPath = %q", got.vsockUDSPath(dir))
	}
	if got.rootfsPath(dir) != filepath.Join(dir, "abc123", "rootfs.ext4") {
		t.Errorf("rootfsPath = %q", got.rootfsPath(dir))
	}

	box := got.toBox(sandbox.StatePaused)
	if box.InstanceID != "abc123" || box.BoxID != "mybox" || box.State != sandbox.StatePaused {
		t.Errorf("toBox = %+v", box)
	}
	if box.Name != "llmbox-abc123" {
		t.Errorf("ready box name = %q, want llmbox-abc123", box.Name)
	}
	pending := boxMeta{Token: "t", Phase: "pending"}.toBox("running")
	if pending.Name != "llmbox-pending-t" {
		t.Errorf("pending box name = %q, want llmbox-pending-t", pending.Name)
	}
}

// TestLoadMetasSkipsJunk loads valid box metadata and ignores non-box entries: a
// plain file, a directory with no meta, and a directory whose meta is corrupt.
func TestLoadMetasSkipsJunk(t *testing.T) {
	dir := t.TempDir()
	if err := (boxMeta{Token: "good", Phase: "ready", Created: 1}).save(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "loose-file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "corrupt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "corrupt", metaFileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	metas, err := loadMetas(dir)
	if err != nil {
		t.Fatalf("loadMetas: %v", err)
	}
	if len(metas) != 1 || metas[0].Token != "good" {
		t.Fatalf("loadMetas = %+v, want only the good box", metas)
	}
}

// TestLoadMetasMissingDir treats a missing state dir as no boxes and no error, so a
// first run before any box exists starts clean.
func TestLoadMetasMissingDir(t *testing.T) {
	metas, err := loadMetas(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("loadMetas of a missing dir should not error: %v", err)
	}
	if len(metas) != 0 {
		t.Fatalf("loadMetas of a missing dir = %+v, want none", metas)
	}
}
