package firecracker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	pt := &progressTarget{Store: fs}
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

// TestAssetCacheDirHonoursEnv checks the cache dir honours the override env var.
func TestAssetCacheDirHonoursEnv(t *testing.T) {
	t.Setenv("LLMBOX_FC_ASSET_CACHE", "/custom/cache")
	if got := assetCacheDir(); got != "/custom/cache" {
		t.Errorf("assetCacheDir = %q, want /custom/cache", got)
	}
}
