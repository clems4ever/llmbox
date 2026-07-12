package firecracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	dockerregistry "github.com/docker/docker/api/types/registry"
	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
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

// TestFetchAllResolvesEveryAsset checks fetchAll pulls all three published images
// unconditionally and returns their cached paths.
func TestFetchAllResolvesEveryAsset(t *testing.T) {
	var got []string
	r := testResolver(t, fakePuller(&got))

	paths, err := r.fetchAll(context.Background())
	if err != nil {
		t.Fatalf("fetchAll: %v", err)
	}
	if len(paths) != 3 {
		t.Fatalf("got %d paths (%v), want 3", len(paths), paths)
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("path %q not on disk: %v", p, err)
		}
	}
	if len(got) != 3 {
		t.Errorf("pulled %d refs (%v), want 3 (kernel, base, payload)", len(got), got)
	}
}

// TestFetchAssetsResolvesIntoCacheDir checks FetchAssets reports the resolved cache
// directory (honouring the override env) and surfaces a pull failure.
func TestFetchAssetsResolvesIntoCacheDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMBOX_FC_ASSET_CACHE", dir)
	t.Setenv("LLMBOX_FC_REGISTRY", "example.invalid/x")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled up front: the pull fails fast without touching the network

	cacheDir, _, err := FetchAssets(ctx, "", nil)
	if cacheDir != dir {
		t.Errorf("cacheDir = %q, want the override %q", cacheDir, dir)
	}
	if err == nil {
		t.Error("FetchAssets should fail when the pull cannot complete")
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

// TestDecompressLayerZstd checks a .zst layer is decoded to its suffix-stripped
// sibling (the raw file the backend boots) and the compressed copy is reclaimed.
func TestDecompressLayerZstd(t *testing.T) {
	dir := t.TempDir()
	want := bytes.Repeat([]byte("ext4-rootfs-bytes-"), 4096) // ~72 KiB, several frames
	comp := filepath.Join(dir, "base-rootfs.ext4.zst")
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	if _, err := enc.Write(want); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("encode close: %v", err)
	}
	if err := os.WriteFile(comp, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write compressed: %v", err)
	}

	if err := decompressLayer(comp); err != nil {
		t.Fatalf("decompressLayer: %v", err)
	}
	raw := filepath.Join(dir, "base-rootfs.ext4")
	got, err := os.ReadFile(raw)
	if err != nil {
		t.Fatalf("read decoded file: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("decoded %d bytes, want %d", len(got), len(want))
	}
	if _, err := os.Stat(comp); !os.IsNotExist(err) {
		t.Fatalf("compressed copy not reclaimed (stat err = %v)", err)
	}
}

// TestDecompressLayerPassesThroughRaw checks a layer with no known compression
// suffix (the raw kernel/payload) is left exactly as fetched.
func TestDecompressLayerPassesThroughRaw(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "vmlinux")
	if err := os.WriteFile(raw, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := decompressLayer(raw); err != nil {
		t.Fatalf("decompressLayer: %v", err)
	}
	got, err := os.ReadFile(raw)
	if err != nil || string(got) != "kernel" {
		t.Fatalf("raw layer altered: %q, %v", got, err)
	}
}

// TestProgressReader checks the progress wrapper is a transparent reader: it yields
// exactly the underlying bytes and tracks the full total.
func TestProgressReader(t *testing.T) {
	data := bytes.Repeat([]byte("llmbox"), 5000) // ~30 KiB, several Read calls
	pr := newProgressReader(bytes.NewReader(data), int64(len(data)), 0)
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

// rangeServer serves data over HTTP with Range support, optionally aborting the
// first response part-way through to simulate a peer reset on a flaky link. It
// records how many requests it received so a test can assert a resume happened.
type rangeServer struct {
	data     []byte
	abortsAt int   // if >0, abort the first response after this many body bytes
	requests int32 // total requests served (atomic via mutex below)
	mu       sync.Mutex
}

// handler serves the data with Range support, resetting the connection part-way
// through the first response when abortsAt is set to simulate a flaky link.
func (s *rangeServer) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests++
	first := s.requests == 1
	s.mu.Unlock()

	var start int64
	if rg := r.Header.Get("Range"); rg != "" {
		fmt.Sscanf(rg, "bytes=%d-", &start)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(s.data)-1, len(s.data)))
		w.WriteHeader(http.StatusPartialContent)
	}
	body := s.data[start:]
	if first && s.abortsAt > 0 && s.abortsAt < len(body) {
		_, _ = w.Write(body[:s.abortsAt])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler) // reset the connection mid-stream
	}
	_, _ = w.Write(body)
}

// blobRepo builds a remote.Repository pointing at a test server so fetchLayer's
// URL construction and ranged download run against it over plain HTTP.
func blobRepo(t *testing.T, srv *httptest.Server) *remote.Repository {
	t.Helper()
	host := strings.TrimPrefix(srv.URL, "http://")
	repo, err := remote.NewRepository(host + "/owner/img")
	if err != nil {
		t.Fatalf("NewRepository: %v", err)
	}
	repo.PlainHTTP = true
	repo.Client = srv.Client()
	return repo
}

// TestFetchLayerResumesAndVerifies checks that a titled layer whose first transfer
// is cut off mid-stream is resumed from the bytes already written, ends up complete
// and digest-verified on disk, and leaves no ".part" behind.
func TestFetchLayerResumesAndVerifies(t *testing.T) {
	t.Cleanup(func(orig time.Duration) func() {
		return func() { resumeBackoffBase = orig }
	}(resumeBackoffBase))
	resumeBackoffBase = time.Millisecond

	data := bytes.Repeat([]byte("firecracker-rootfs"), 4096) // ~72 KiB, several reads
	s := &rangeServer{data: data, abortsAt: len(data) / 3}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()

	repo := blobRepo(t, srv)
	dir := t.TempDir()
	dest := filepath.Join(dir, "base-rootfs.ext4")
	layer := ocispec.Descriptor{
		Digest:      digest.FromBytes(data),
		Size:        int64(len(data)),
		Annotations: map[string]string{ocispec.AnnotationTitle: "base-rootfs.ext4"},
	}
	if err := fetchLayer(context.Background(), repo, layer, dest); err != nil {
		t.Fatalf("fetchLayer: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("downloaded content differs (%d bytes vs %d)", len(got), len(data))
	}
	if s.requests < 2 {
		t.Errorf("server saw %d requests, want >=2 (the transfer should have resumed)", s.requests)
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Errorf(".part file should be gone after a successful download, stat err = %v", err)
	}
}

// TestFetchLayerRejectsTooLarge checks a layer that cannot fit in the cache fails
// with an actionable error before any download, naming the layer.
func TestFetchLayerRejectsTooLarge(t *testing.T) {
	repo, err := remote.NewRepository("example.com/owner/img")
	if err != nil {
		t.Fatalf("NewRepository: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "base-rootfs.ext4")
	layer := ocispec.Descriptor{
		Digest:      digest.FromString("huge"),
		Size:        math.MaxInt64,
		Annotations: map[string]string{ocispec.AnnotationTitle: "base-rootfs.ext4"},
	}
	err = fetchLayer(context.Background(), repo, layer, dest)
	if err == nil {
		t.Fatal("fetchLayer accepted a layer larger than free space")
	}
	if !strings.Contains(err.Error(), "base-rootfs.ext4") || !strings.Contains(err.Error(), "free") {
		t.Errorf("error = %q, want it to name the layer and the free-space shortfall", err)
	}
}

// TestEnsureRoom checks the free-space guard rejects an impossible need (naming the
// layer) and admits one that fits.
func TestEnsureRoom(t *testing.T) {
	dir := t.TempDir()
	if err := ensureRoom(dir, math.MaxInt64, "base-rootfs.ext4"); err == nil {
		t.Error("ensureRoom admitted a need larger than any filesystem")
	} else if !strings.Contains(err.Error(), "base-rootfs.ext4") || !strings.Contains(err.Error(), "free") {
		t.Errorf("error = %q, want it to name the layer and the free-space shortfall", err)
	}
	if err := ensureRoom(dir, 0, "tiny"); err != nil {
		t.Errorf("ensureRoom rejected a zero-byte need: %v", err)
	}
}

// TestDownloadStalls checks download gives up with a clear error when repeated
// attempts transfer no bytes at all.
func TestDownloadStalls(t *testing.T) {
	t.Cleanup(func(orig time.Duration) func() {
		return func() { resumeBackoffBase = orig }
	}(resumeBackoffBase))
	resumeBackoffBase = time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := download(context.Background(), srv.Client(), srv.URL+"/blob", io.Discard, 0, 100)
	if err == nil {
		t.Fatal("download should fail when no bytes can be transferred")
	}
	if !strings.Contains(err.Error(), "stalled") {
		t.Errorf("error = %q, want it to mention the stall", err)
	}
}

// TestVerifyDigest checks a matching file passes and a corrupted one is rejected.
func TestVerifyDigest(t *testing.T) {
	data := []byte("guest image bytes")
	path := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := verifyDigest(path, digest.FromBytes(data)); err != nil {
		t.Errorf("verifyDigest rejected a matching file: %v", err)
	}
	if err := verifyDigest(path, digest.FromString("something else")); err == nil {
		t.Error("verifyDigest accepted a file whose digest does not match")
	}
}

// TestBackoff checks the resume delay grows with consecutive failures and caps out.
func TestBackoff(t *testing.T) {
	t.Cleanup(func(orig time.Duration) func() {
		return func() { resumeBackoffBase = orig }
	}(resumeBackoffBase))
	resumeBackoffBase = time.Second

	if got := backoff(0); got != time.Second {
		t.Errorf("backoff(0) = %v, want 1s", got)
	}
	if got := backoff(3); got != 8*time.Second {
		t.Errorf("backoff(3) = %v, want 8s", got)
	}
	if got := backoff(100); got != 30*time.Second {
		t.Errorf("backoff(100) = %v, want the 30s cap", got)
	}
	if got := backoff(-1); got != time.Second {
		t.Errorf("backoff(-1) = %v, want 1s (floored at 0)", got)
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
