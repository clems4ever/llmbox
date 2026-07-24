package cloudhypervisor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCopyFile copies a file and checks the contents and 0600 mode.
func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("rootfs-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "rootfs-bytes" {
		t.Fatalf("copied contents = %q, %v", got, err)
	}
	info, _ := os.Stat(dst)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("copied mode = %v, want 0600", info.Mode().Perm())
	}
}

// TestPrepareRootfsGrows copies the base image and grows it to the requested size
// with a sparse truncate, never shrinking below the base.
func TestPrepareRootfsGrows(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "base.ext4")
	dst := filepath.Join(dir, "box-rootfs.ext4")
	if err := os.WriteFile(src, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareRootfs(src, dst, 4096); err != nil {
		t.Fatalf("prepareRootfs: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 4096 {
		t.Errorf("grown size = %d, want 4096", info.Size())
	}

	// A requested size below the base leaves the copy at the base size (grow-only).
	dst2 := filepath.Join(dir, "box-rootfs2.ext4")
	if err := prepareRootfs(src, dst2, 512); err != nil {
		t.Fatalf("prepareRootfs (small): %v", err)
	}
	info2, _ := os.Stat(dst2)
	if info2.Size() != 1024 {
		t.Errorf("size = %d, want the base 1024 (never shrink)", info2.Size())
	}
}

// TestWaitForSocket returns once a socket path appears and errors on timeout.
func TestWaitForSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "late.sock")
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = os.WriteFile(path, nil, 0o600)
	}()
	if err := waitForSocket(context.Background(), path, time.Second); err != nil {
		t.Fatalf("waitForSocket should succeed once the socket appears: %v", err)
	}
	if err := waitForSocket(context.Background(), filepath.Join(dir, "never.sock"), 50*time.Millisecond); err == nil {
		t.Fatal("waitForSocket should time out when the socket never appears")
	}
}

// TestCHLauncherAliveHalt checks the real launcher's orphan probe and halt drive the
// VMM's API socket: a live fake VMM is reported alive and receives a vmm.shutdown on
// Halt, while an absent socket is not alive.
func TestCHLauncherAliveHalt(t *testing.T) {
	f, sock := startFakeVMM(t)
	l := newCHLauncher("")
	if !l.Alive(sock) {
		t.Error("Alive should be true for a live VMM socket")
	}
	if l.Alive(filepath.Join(t.TempDir(), "absent.sock")) {
		t.Error("Alive should be false for an absent socket")
	}
	if err := l.Halt(sock); err != nil {
		t.Fatalf("Halt: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var sawShutdown bool
	for _, c := range f.calls {
		if c == "PUT /api/v1/vmm.shutdown" {
			sawShutdown = true
		}
	}
	if !sawShutdown {
		t.Errorf("Halt should send vmm.shutdown, calls = %v", f.calls)
	}
}
