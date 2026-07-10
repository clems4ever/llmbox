package firecracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	dockerregistry "github.com/docker/docker/api/types/registry"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/registry/remote/auth"
)

// fakePuller returns a pull func that records the refs it was asked for and writes
// the asset's expected file into destDir (so resolve's existence check passes),
// standing in for a real registry.
func fakePuller(got *[]string) func(context.Context, string, string, auth.CredentialFunc) error {
	return func(_ context.Context, ref, destDir string, _ auth.CredentialFunc) error {
		*got = append(*got, ref)
		var fname string
		switch {
		case strings.Contains(ref, kernelAsset.repo):
			fname = kernelAsset.file
		case strings.Contains(ref, baseAsset.repo):
			fname = baseAsset.file
		case strings.Contains(ref, payloadAsset.repo):
			fname = payloadAsset.file
		default:
			return fmt.Errorf("unexpected ref %q", ref)
		}
		return os.WriteFile(filepath.Join(destDir, fname), []byte("img"), 0o644)
	}
}

// testResolver builds an assetResolver with a temp cache dir and an injected pull
// func, so resolution logic is exercised without touching a real registry.
func testResolver(t *testing.T, pull func(context.Context, string, string, auth.CredentialFunc) error) *assetResolver {
	t.Helper()
	return &assetResolver{registry: "ghcr.io/acme", tag: "latest", cacheDir: t.TempDir(), pull: pull}
}

// TestResolveImagesPullsOnlyMissing checks that supplied paths are left untouched
// and only the empty ones are pulled, each returning a cached file path.
func TestResolveImagesPullsOnlyMissing(t *testing.T) {
	var got []string
	r := testResolver(t, fakePuller(&got))

	// Nothing supplied: all three resolve.
	k, rf, pl, err := r.resolveImages(context.Background(), "", "", "")
	if err != nil {
		t.Fatalf("resolveImages: %v", err)
	}
	for _, p := range []string{k, rf, pl} {
		if p == "" {
			t.Fatalf("resolved paths = (%q, %q, %q); want all set", k, rf, pl)
		}
		if _, err := os.Stat(p); err != nil {
			t.Errorf("resolved path %q not on disk: %v", p, err)
		}
	}
	if len(got) != 3 {
		t.Fatalf("pulled %d images (%v), want 3", len(got), got)
	}

	// A supplied kernel is returned verbatim and not pulled.
	got = nil
	k, _, _, err = r.resolveImages(context.Background(), "/my/vmlinux", "", "")
	if err != nil {
		t.Fatalf("resolveImages: %v", err)
	}
	if k != "/my/vmlinux" {
		t.Errorf("kernel = %q, want the supplied /my/vmlinux", k)
	}
	for _, ref := range got {
		if strings.Contains(ref, kernelAsset.repo) {
			t.Errorf("kernel was pulled (%q) despite being supplied", ref)
		}
	}
}

// TestResolveImagesPayloadCoupledToRootfs checks the payload resolves only when the
// rootfs was auto-resolved: a caller bringing its own rootfs and no payload gets the
// all-in-one layout (empty payload), not a surprise pull.
func TestResolveImagesPayloadCoupledToRootfs(t *testing.T) {
	var got []string
	r := testResolver(t, fakePuller(&got))

	k, rf, pl, err := r.resolveImages(context.Background(), "", "/my/rootfs.ext4", "")
	if err != nil {
		t.Fatalf("resolveImages: %v", err)
	}
	if rf != "/my/rootfs.ext4" {
		t.Errorf("rootfs = %q, want the supplied path", rf)
	}
	if pl != "" {
		t.Errorf("payload = %q, want empty (all-in-one; not pulled when rootfs is supplied)", pl)
	}
	if k == "" {
		t.Error("kernel not resolved though it was empty")
	}
	for _, ref := range got {
		if strings.Contains(ref, payloadAsset.repo) {
			t.Errorf("payload was pulled (%q) despite the rootfs being supplied", ref)
		}
	}
}

// TestResolveImagesPropagatesPullError checks a failed pull surfaces as an error.
func TestResolveImagesPropagatesPullError(t *testing.T) {
	r := testResolver(t, func(context.Context, string, string, auth.CredentialFunc) error {
		return fmt.Errorf("registry down")
	})
	if _, _, _, err := r.resolveImages(context.Background(), "", "", ""); err == nil {
		t.Fatal("resolveImages should fail when a pull fails")
	}
}

// TestCredentialForMatchesHost checks a credential is produced only for a matching
// registry host and nil otherwise (anonymous pull).
func TestCredentialForMatchesHost(t *testing.T) {
	auths := map[string]dockerregistry.AuthConfig{
		"ghcr.io": {Username: "u", Password: "p"},
	}
	if credentialFor("ghcr.io/acme", auths) == nil {
		t.Error("want a credential for a configured host")
	}
	if credentialFor("ghcr.io/acme", nil) != nil {
		t.Error("want nil (anonymous) when no auths are configured")
	}
	if credentialFor("registry.example.com/acme", auths) != nil {
		t.Error("want nil for a host with no matching auth entry")
	}
	empty := map[string]dockerregistry.AuthConfig{"ghcr.io": {}}
	if credentialFor("ghcr.io/acme", empty) != nil {
		t.Error("want nil for an empty auth entry")
	}
}

// TestOrasPullRejectsBadReference checks orasPull surfaces an error for a malformed
// image reference before any network round-trip.
func TestOrasPullRejectsBadReference(t *testing.T) {
	if err := orasPull(context.Background(), "not a valid ref!!", t.TempDir(), nil); err == nil {
		t.Fatal("orasPull should error on a malformed reference")
	}
}

// TestProgressReader checks the progress wrapper is a transparent reader: it yields
// exactly the underlying bytes and tracks the full total.
func TestProgressReader(t *testing.T) {
	data := bytes.Repeat([]byte("llmbox"), 5000) // ~30 KiB, several Read calls
	pr := newProgressReader(bytes.NewReader(data), int64(len(data)))
	got, err := io.ReadAll(pr)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("progress reader altered the stream (%d bytes vs %d)", len(got), len(data))
	}
	if pr.read != int64(len(data)) {
		t.Errorf("tracked %d bytes, want %d", pr.read, len(data))
	}
}

// TestProgressTargetPush checks the wrapping target still writes the blob to the
// underlying file store (the progress wrapper does not corrupt the content).
func TestProgressTargetPush(t *testing.T) {
	dir := t.TempDir()
	fs, err := file.New(dir)
	if err != nil {
		t.Fatalf("file.New: %v", err)
	}
	defer func() { _ = fs.Close() }()

	data := []byte("a guest image blob")
	desc := ocispec.Descriptor{
		MediaType:   "application/octet-stream",
		Digest:      digest.FromBytes(data),
		Size:        int64(len(data)),
		Annotations: map[string]string{ocispec.AnnotationTitle: "thing.ext4"},
	}
	pt := &progressTarget{Store: fs, dir: dir}
	if err := pt.Push(context.Background(), desc, bytes.NewReader(data)); err != nil {
		t.Fatalf("Push: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "thing.ext4"))
	if err != nil {
		t.Fatalf("read pushed file: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("pushed content = %q, want %q", got, data)
	}
}

// TestProgressTargetPushRejectsTooLarge checks a titled layer bigger than the
// free space fails before any bytes stream (the content reader is never read),
// with an actionable error — so a too-small cache dir (e.g. the tmpfs run-dir)
// costs a second, not a multi-GiB download that a restart-on-failure service
// then repeats every retry.
func TestProgressTargetPushRejectsTooLarge(t *testing.T) {
	dir := t.TempDir()
	fs, err := file.New(dir)
	if err != nil {
		t.Fatalf("file.New: %v", err)
	}
	defer func() { _ = fs.Close() }()

	// A layer claiming more bytes than any real filesystem has free. The reader
	// panics if read, proving the check short-circuits before streaming.
	desc := ocispec.Descriptor{
		MediaType:   "application/octet-stream",
		Digest:      digest.FromString("huge"),
		Size:        math.MaxInt64,
		Annotations: map[string]string{ocispec.AnnotationTitle: "base-rootfs.ext4"},
	}
	pt := &progressTarget{Store: fs, dir: dir}
	err = pt.Push(context.Background(), desc, iotest.ErrReader(errors.New("must not read")))
	if err == nil {
		t.Fatal("Push accepted a layer larger than free space")
	}
	if !strings.Contains(err.Error(), "base-rootfs.ext4") || !strings.Contains(err.Error(), "free") {
		t.Errorf("error = %q, want it to name the layer and the free-space shortfall", err)
	}
}

// TestChooseAssetCacheDir checks the cache-dir policy for each branch: the env
// override wins first, then an explicit --state-dir, then the user cache dir,
// then (no home) a disk path under /var/lib for root, and only the tmpfs run-dir
// as an unprivileged last resort.
func TestChooseAssetCacheDir(t *testing.T) {
	noHome := errors.New("$HOME is not defined")
	for _, c := range []struct {
		name       string
		env, state string
		cache      string
		cacheErr   error
		euid       int
		want       string
	}{
		{"env override wins", "/override", "/st", "/home/.cache", nil, 0, "/override"},
		{"explicit state-dir", "", "/data/llmbox", "/home/.cache", nil, 1000, "/data/llmbox/assets"},
		{"user cache dir", "", "", "/home/u/.cache", nil, 1000, "/home/u/.cache/llmbox/firecracker"},
		{"root no home", "", "", "", noHome, 0, "/var/lib/llmbox/firecracker/assets"},
		{"nonroot no home last resort", "", "", "", noHome, 1000, filepath.Join(defaultStateDir, "assets")},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := chooseAssetCacheDir(c.env, c.state, c.cache, c.cacheErr, c.euid); got != c.want {
				t.Errorf("chooseAssetCacheDir = %q, want %q", got, c.want)
			}
		})
	}
}

// TestHumanBytes checks byte counts format in binary units across boundaries.
func TestHumanBytes(t *testing.T) {
	for _, c := range []struct {
		n    int64
		want string
	}{
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{5 * 1024 * 1024, "5.0 MiB"},
		{6 * 1024 * 1024 * 1024, "6.0 GiB"},
	} {
		if got := humanBytes(c.n); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// TestAssetCacheDirHonoursEnv checks the cache dir honours the override env var
// end to end (an explicit state-dir does not override it).
func TestAssetCacheDirHonoursEnv(t *testing.T) {
	t.Setenv("LLMBOX_FC_ASSET_CACHE", "/custom/cache")
	if got := assetCacheDir("/some/state-dir"); got != "/custom/cache" {
		t.Errorf("assetCacheDir = %q, want /custom/cache", got)
	}
}
