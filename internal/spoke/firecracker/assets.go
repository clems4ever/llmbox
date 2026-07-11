package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	dockerregistry "github.com/docker/docker/api/types/registry"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

const (
	// defaultAssetRegistry is the OCI namespace the published guest images live
	// under. The base rootfs, guest payload, and guest kernel are stored there as
	// opaque OCI artifacts (pushed with oras, not runnable images — Firecracker
	// boots the raw files). Override with LLMBOX_FC_REGISTRY, e.g. for a fork.
	defaultAssetRegistry = "ghcr.io/clems4ever"
	// defaultAssetTag is the tag pulled when the operator pins none. Override with
	// LLMBOX_FC_TAG to pin a specific published build.
	defaultAssetTag = "latest"
)

// fcAsset names one published guest image: the OCI repository it lives in (under
// the registry namespace) and the file it carries — the layer title the artifact
// was pushed with, which is also its on-disk name after a pull.
type fcAsset struct {
	repo string
	file string
}

var (
	kernelAsset  = fcAsset{repo: "llmbox-fc-kernel", file: "vmlinux"}
	baseAsset    = fcAsset{repo: "llmbox-fc-base", file: "base-rootfs.ext4"}
	payloadAsset = fcAsset{repo: "llmbox-fc-payload", file: "payload.ext4"}
)

// assetResolver downloads published Firecracker guest images (kernel, base rootfs,
// guest payload) from an OCI registry into a local cache, so a spoke can run the
// backend with no image flags — the operator need not build or host any of them.
// pull is injectable so the resolution logic is unit-testable without a registry.
type assetResolver struct {
	registry string
	tag      string
	cacheDir string
	cred     auth.CredentialFunc
	pull     func(ctx context.Context, ref, destDir string, cred auth.CredentialFunc) error
}

// newAssetResolver builds a resolver caching under cacheDir. It reads the registry
// namespace and tag from LLMBOX_FC_REGISTRY / LLMBOX_FC_TAG (falling back to the
// public defaults) and reuses a configured credential for the registry host if the
// operator supplied one, otherwise pulls anonymously (public packages).
//
// @arg cacheDir Directory the pulled images are cached in.
// @arg auths Image-pull credentials keyed by registry host; used if one matches the registry.
// @return *assetResolver A resolver wired to the real oras-backed puller.
//
// @testcase TestResolveImagesPullsOnlyMissing resolves only the unset images.
func newAssetResolver(cacheDir string, auths map[string]dockerregistry.AuthConfig) *assetResolver {
	reg := defaultAssetRegistry
	if v := os.Getenv("LLMBOX_FC_REGISTRY"); v != "" {
		reg = v
	}
	tag := defaultAssetTag
	if v := os.Getenv("LLMBOX_FC_TAG"); v != "" {
		tag = v
	}
	return &assetResolver{
		registry: reg,
		tag:      tag,
		cacheDir: cacheDir,
		cred:     credentialFor(reg, auths),
		pull:     orasPull,
	}
}

// resolve returns a host path to asset a, downloading it into the cache when it is
// not already present for the current tag digest.
//
// @arg ctx Context for the registry round-trips.
// @arg a The asset to resolve (its repo and carried file).
// @return string Host path to the cached image file.
// @error error if the pull fails or the expected file is missing afterwards.
//
// @testcase TestResolveImagesPullsOnlyMissing returns the cached path per asset.
func (r *assetResolver) resolve(ctx context.Context, a fcAsset) (string, error) {
	dest := filepath.Join(r.cacheDir, a.repo)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	ref := fmt.Sprintf("%s/%s:%s", r.registry, a.repo, r.tag)
	if err := r.pull(ctx, ref, dest, r.cred); err != nil {
		return "", fmt.Errorf("resolving %s from %s: %w", a.file, ref, err)
	}
	out := filepath.Join(dest, a.file)
	if _, err := os.Stat(out); err != nil {
		return "", fmt.Errorf("pulled %s but %s is missing: %w", ref, a.file, err)
	}
	return out, nil
}

// resolveImages fills in any of the kernel, rootfs, or payload paths the operator
// left empty by pulling the published image from the registry. The kernel and
// rootfs always resolve when empty; the payload resolves only when the rootfs was
// also auto-resolved — a caller that brought its own rootfs and no payload wants
// the all-in-one layout (guest baked into that rootfs), not a surprise payload.
//
// @arg ctx Context for the pulls.
// @arg kernel Operator-supplied kernel path, or empty to auto-resolve.
// @arg rootfs Operator-supplied rootfs path, or empty to auto-resolve.
// @arg payload Operator-supplied payload path, or empty (resolved only if rootfs was).
// @return string The resolved kernel path.
// @return string The resolved rootfs path.
// @return string The resolved payload path (may stay empty for the all-in-one layout).
// @error error if any required pull fails.
//
// @testcase TestResolveImagesPullsOnlyMissing leaves supplied paths untouched and pulls the rest.
// @testcase TestResolveImagesPayloadCoupledToRootfs skips the payload when only the rootfs is supplied.
func (r *assetResolver) resolveImages(ctx context.Context, kernel, rootfs, payload string) (string, string, string, error) {
	log.Printf("firecracker: resolving guest images from %s (tag %s); first run downloads a multi-GiB base rootfs and may take a while", r.registry, r.tag)
	var err error
	if kernel == "" {
		if kernel, err = r.resolve(ctx, kernelAsset); err != nil {
			return "", "", "", err
		}
	}
	rootfsResolved := rootfs == ""
	if rootfs == "" {
		if rootfs, err = r.resolve(ctx, baseAsset); err != nil {
			return "", "", "", err
		}
	}
	if payload == "" && rootfsResolved {
		if payload, err = r.resolve(ctx, payloadAsset); err != nil {
			return "", "", "", err
		}
	}
	return kernel, rootfs, payload, nil
}

// FetchAssets downloads every published guest image (kernel, base rootfs, and
// payload) into the on-disk cache the Firecracker backend reads from, resuming any
// partial download left by an interrupted run. It is the entry point for the
// spoke's `fetch` subcommand: warming or pre-seeding a spoke's images resiliently,
// separately from running the spoke, so a later run starts with them already
// cached. The cache directory is resolved from stateDir exactly as a run would, so
// the images land where the backend then looks for them.
//
// @arg ctx Context cancelling the downloads.
// @arg stateDir The spoke's --state-dir (empty uses the backend default); selects the cache location.
// @arg auths Image-pull credentials keyed by registry host; used if one matches the registry.
// @return string The cache directory the images were written under.
// @return []string The resolved on-disk paths of the fetched images.
// @error error if any image cannot be resolved or downloaded.
//
// @testcase TestFetchAssetsResolvesIntoCacheDir reports the cache dir and surfaces a pull error.
func FetchAssets(ctx context.Context, stateDir string, auths map[string]dockerregistry.AuthConfig) (string, []string, error) {
	r := newAssetResolver(assetCacheDir(stateDir), auths)
	log.Printf("firecracker: fetching guest images from %s (tag %s) into %s", r.registry, r.tag, r.cacheDir)
	paths, err := r.fetchAll(ctx)
	return r.cacheDir, paths, err
}

// fetchAll resolves every published guest image (kernel, base rootfs, payload)
// into the cache and returns their on-disk paths. Unlike resolveImages it pulls all
// three unconditionally — the fetch command warms the whole set rather than filling
// in only the images a run left unset — and, like resolve, uses the injectable
// puller so it is unit-testable without a registry.
//
// @arg ctx Context for the pulls.
// @return []string The resolved paths of the kernel, base rootfs, and payload.
// @error error if any pull fails.
//
// @testcase TestFetchAllResolvesEveryAsset pulls all three assets and returns their cached paths.
func (r *assetResolver) fetchAll(ctx context.Context) ([]string, error) {
	var paths []string
	for _, a := range []fcAsset{kernelAsset, baseAsset, payloadAsset} {
		p, err := r.resolve(ctx, a)
		if err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// credentialFor returns a static credential for the registry's host when auths
// carries a non-empty entry for it, or nil for an anonymous (public) pull.
//
// @arg registry The registry namespace, e.g. "ghcr.io/clems4ever".
// @arg auths Image-pull credentials keyed by registry host.
// @return auth.CredentialFunc A static credential for the host, or nil when none applies.
//
// @testcase TestCredentialForMatchesHost returns a credential for a matching host and nil otherwise.
func credentialFor(registry string, auths map[string]dockerregistry.AuthConfig) auth.CredentialFunc {
	host := registry
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	a, ok := auths[host]
	if !ok || (a.Username == "" && a.Password == "" && a.IdentityToken == "") {
		return nil
	}
	return auth.StaticCredential(host, auth.Credential{
		Username:     a.Username,
		Password:     a.Password,
		RefreshToken: a.IdentityToken,
	})
}

// orasPull downloads the OCI artifact ref into destDir, skipping the transfer when
// the cached copy already matches the tag's current manifest digest (so a moving
// tag like :latest still updates, but an unchanged one costs only a manifest HEAD).
// Each titled layer is fetched with a resumable, ranged download so a slow or flaky
// link — where a multi-GiB single stream is prone to a peer reset (HTTP/2
// PROTOCOL_ERROR) — resumes from the bytes already on disk instead of restarting
// from zero on every service retry.
//
// @arg ctx Context for the registry round-trips.
// @arg ref The full image reference, e.g. ghcr.io/owner/llmbox-fc-base:latest.
// @arg destDir Directory the artifact's file is written into.
// @arg cred Credential for the pull, or nil for anonymous.
// @error error if the reference is invalid, the manifest cannot be read, or a layer download fails.
//
// @testcase TestOrasPullRejectsBadReference errors on a malformed image reference.
func orasPull(ctx context.Context, ref, destDir string, cred auth.CredentialFunc) error {
	repo, err := remote.NewRepository(ref)
	if err != nil {
		return err
	}
	client := &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache()}
	if cred != nil {
		client.Credential = cred
	}
	repo.Client = client

	tag := repo.Reference.Reference
	desc, err := repo.Resolve(ctx, tag)
	if err != nil {
		return err
	}
	marker := filepath.Join(destDir, ".oci-digest")
	if cur, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(cur)) == desc.Digest.String() {
		log.Printf("firecracker: %s is up to date (cached)", ref)
		return nil // cache already current for this tag
	}

	rc, err := repo.Fetch(ctx, desc)
	if err != nil {
		return err
	}
	manifestBytes, err := content.ReadAll(rc, desc) // verifies the manifest's size and digest
	if err != nil {
		return err
	}
	var man ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &man); err != nil {
		return fmt.Errorf("decoding manifest for %s: %w", ref, err)
	}
	for _, layer := range man.Layers {
		title := layer.Annotations[ocispec.AnnotationTitle]
		if title == "" {
			continue // only the titled guest-image layers are materialized on disk
		}
		if err := fetchLayer(ctx, repo, layer, filepath.Join(destDir, title)); err != nil {
			return err
		}
	}
	log.Printf("firecracker: %s ready", ref)
	_ = os.WriteFile(marker, []byte(desc.Digest.String()), 0o644)
	return nil
}

// fetchLayer downloads a titled layer to dest, resuming from any partial written by
// an earlier interrupted run and verifying the content digest before the file is
// put in place. It streams into a ".part" sibling and renames it on success, so a
// half-downloaded file is never mistaken for a complete one, and refuses up front
// when the cache lacks room for the remaining bytes (the tmpfs run-dir is tiny).
//
// @arg ctx Context for the download.
// @arg repo The remote repository the layer's blob is fetched from.
// @arg layer The layer descriptor (its digest, size, and title annotation).
// @arg dest The final host path the layer's file is written to.
// @error error if the cache lacks room, the download stalls, or the content digest does not match.
//
// @testcase TestFetchLayerResumesAndVerifies downloads a titled layer over a flaky server and verifies it.
// @testcase TestFetchLayerRejectsTooLarge fails when the cache cannot hold the remaining bytes.
func fetchLayer(ctx context.Context, repo *remote.Repository, layer ocispec.Descriptor, dest string) error {
	title := layer.Annotations[ocispec.AnnotationTitle]
	dir := filepath.Dir(dest)
	part := dest + ".part"

	var offset int64
	if fi, err := os.Stat(part); err == nil {
		if fi.Size() > layer.Size {
			_ = os.Remove(part) // an oversized partial can't be trusted; start clean
		} else {
			offset = fi.Size()
		}
	}
	if err := ensureRoom(dir, layer.Size-offset, title); err != nil {
		return err
	}

	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	resume := ""
	if offset > 0 {
		resume = fmt.Sprintf(" (resuming from %s)", humanBytes(offset))
	}
	log.Printf("firecracker: downloading %s (%s)%s...", title, humanBytes(layer.Size), resume)

	scheme := "https"
	if repo.PlainHTTP {
		scheme = "http"
	}
	blobURL := fmt.Sprintf("%s://%s/v2/%s/blobs/%s", scheme, repo.Reference.Host(), repo.Reference.Repository, layer.Digest)

	if err := download(ctx, repo.Client, blobURL, f, offset, layer.Size); err != nil {
		_ = f.Close()
		return fmt.Errorf("downloading %s: %w", title, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := verifyDigest(part, layer.Digest); err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("verifying %s: %w", title, err)
	}
	return os.Rename(part, dest)
}

// ensureRoom returns an actionable error when dir has fewer than need bytes free,
// naming the layer — so a too-small cache (e.g. the tmpfs run-dir) fails in a
// second rather than after streaming gigabytes toward "no space left on device",
// which on a restart-on-failure service would re-download on every retry.
//
// @arg dir The directory the layer will be written into.
// @arg need The number of bytes still to be written.
// @arg title The layer's file name, for the error message.
// @error error if the filesystem has less than need bytes available.
//
// @testcase TestFetchLayerRejectsTooLarge relies on ensureRoom to reject an over-large layer.
// @testcase TestEnsureRoom rejects a need larger than the free space and names the layer.
func ensureRoom(dir string, need int64, title string) error {
	if free, err := freeBytes(dir); err == nil && free < uint64(need) {
		return fmt.Errorf("cannot cache %s (%s) in %s: only %s free — point --state-dir or LLMBOX_FC_ASSET_CACHE at a larger disk (the default run-dir is a small in-memory tmpfs)",
			title, humanBytes(need), dir, humanBytes(int64(free)))
	}
	return nil
}

// download streams the blob at blobURL into w starting at offset, retrying with a
// fresh ranged request whenever the transfer is interrupted (a peer reset or dropped
// connection) so it always makes forward progress from the bytes already written. It
// gives up only when repeated attempts add no bytes at all, which distinguishes a
// transient interruption from a genuinely stuck endpoint.
//
// @arg ctx Context cancelling the download and its back-off waits.
// @arg client The HTTP client (carries auth and follows the storage redirect).
// @arg blobURL The registry blob URL to range-fetch.
// @arg w The destination writer, positioned at offset (opened for append).
// @arg offset The number of bytes already written, resumed from.
// @arg size The layer's total size in bytes.
// @error error if the context is cancelled or the transfer stalls with no progress.
//
// @testcase TestFetchLayerResumesAndVerifies exercises download resuming across an interrupted transfer.
// @testcase TestDownloadStalls fails when no bytes can be transferred.
func download(ctx context.Context, client remote.Client, blobURL string, w io.Writer, offset, size int64) error {
	const maxStalls = 6
	stalls := 0
	for offset < size {
		n, err := downloadRange(ctx, client, blobURL, w, offset, size)
		offset += n
		if offset >= size {
			return nil
		}
		if n > 0 {
			stalls = 0
		} else {
			stalls++
			if stalls > maxStalls {
				if err == nil {
					err = io.ErrUnexpectedEOF
				}
				return fmt.Errorf("stalled at %s/%s after %d attempts with no progress: %w",
					humanBytes(offset), humanBytes(size), stalls, err)
			}
		}
		delay := backoff(stalls)
		log.Printf("firecracker:   interrupted at %s/%s (%v); resuming in %s",
			humanBytes(offset), humanBytes(size), err, delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil
}

// downloadRange issues a single ranged GET for the bytes from offset onward and
// copies them into w through a progress reader, returning how many it transferred
// before the response ended (cleanly or with an error). Its partial return lets the
// caller resume from wherever the stream stopped.
//
// @arg ctx Context for the request.
// @arg client The HTTP client.
// @arg blobURL The registry blob URL.
// @arg w The destination writer.
// @arg offset The byte offset to request from (0 for a full GET).
// @arg size The layer's total size, for progress rendering.
// @return int64 The number of bytes transferred by this request.
// @error error if the request cannot be built, the status is unexpected, or the copy fails.
//
// @testcase TestFetchLayerResumesAndVerifies exercises downloadRange for full and ranged requests.
func downloadRange(ctx context.Context, client remote.Client, blobURL string, w io.Writer, offset, size int64) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, blobURL, nil)
	if err != nil {
		return 0, err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case offset == 0 && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent):
	case offset > 0 && resp.StatusCode == http.StatusPartialContent:
	default:
		return 0, fmt.Errorf("unexpected status %s fetching bytes from %d", resp.Status, offset)
	}
	return io.Copy(w, newProgressReader(resp.Body, size, offset))
}

// verifyDigest checks the file at path hashes to want, so a resumed download that
// stitched together several ranges is proven byte-for-byte correct before the file
// is put in place.
//
// @arg path The file to hash.
// @arg want The expected content digest.
// @error error if the file cannot be read or its digest does not match want.
//
// @testcase TestVerifyDigest accepts a matching file and rejects a corrupted one.
func verifyDigest(path string, want digest.Digest) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	v := want.Verifier()
	if _, err := io.Copy(v, f); err != nil {
		return err
	}
	if !v.Verified() {
		return fmt.Errorf("content digest mismatch (want %s)", want)
	}
	return nil
}

// resumeBackoffBase is the first resume-retry delay, exposed as a variable only so
// tests can shrink it; production keeps the one-second default.
var resumeBackoffBase = time.Second

// backoff returns the wait before the next resume attempt: an exponential delay
// starting at resumeBackoffBase and capped at thirty seconds, so a flaky link is
// retried promptly at first but a persistently failing one is not hammered.
//
// @arg attempt The count of consecutive no-progress attempts.
// @return time.Duration The wait before the next attempt.
//
// @testcase TestBackoff grows the delay and caps it.
func backoff(attempt int) time.Duration {
	const max = 30 * time.Second
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 30 {
		return max
	}
	if d := resumeBackoffBase << attempt; d > 0 && d < max {
		return d
	}
	return max
}

// freeBytes returns the bytes available for a new file under dir. A root process
// may use the filesystem-reserved blocks (Bfree); an unprivileged one may not
// (Bavail), so the check never rejects space the caller could actually use.
//
// @arg dir An existing directory on the target filesystem.
// @return uint64 The bytes available to this process on that filesystem.
// @error error if the filesystem cannot be stat'd.
//
// @testcase TestEnsureRoom relies on freeBytes to size the cache filesystem.
func freeBytes(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	avail := st.Bavail
	if os.Geteuid() == 0 {
		avail = st.Bfree
	}
	return avail * uint64(st.Bsize), nil
}

// progressReader counts bytes read from an underlying reader and renders download
// progress. On a terminal it draws an in-place bar (updated at most ~10×/s); when
// stderr is redirected it logs a line at each new 10% so redirected output still
// shows progress without carriage-return spam.
type progressReader struct {
	r          io.Reader
	total      int64
	read       int64
	tty        bool
	lastRender time.Time
	lastDecile int
}

// newProgressReader wraps r to render progress toward total bytes, choosing a live
// bar on a terminal or decile log lines otherwise. start seeds the byte count so a
// resumed download renders absolute progress (e.g. picking up at 40%) rather than
// restarting the bar from zero for each range.
//
// @arg r The underlying content reader.
// @arg total The expected total byte count.
// @arg start The bytes already transferred before this reader (0 for a fresh download).
// @return *progressReader A reader that renders progress as it is consumed.
//
// @testcase TestProgressReader passes bytes through unchanged while tracking progress.
func newProgressReader(r io.Reader, total, start int64) *progressReader {
	tty := false
	if fi, err := os.Stderr.Stat(); err == nil {
		tty = fi.Mode()&os.ModeCharDevice != 0
	}
	return &progressReader{r: r, total: total, read: start, tty: tty}
}

// Read reads from the underlying reader, updating the progress display. It returns
// exactly what the underlying reader returns, so it is a transparent wrapper.
//
// @arg b The read buffer.
// @return int The number of bytes read.
// @error error the underlying reader's error (including io.EOF).
//
// @testcase TestProgressReader passes bytes through unchanged while tracking progress.
func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.read += int64(n)
	}
	done := err == io.EOF
	frac := 1.0
	if p.total > 0 && p.read < p.total {
		frac = float64(p.read) / float64(p.total)
	}
	if p.tty {
		if done || time.Since(p.lastRender) >= 100*time.Millisecond {
			p.lastRender = time.Now()
			const w = 30
			filled := int(frac * w)
			if filled > w {
				filled = w
			}
			fmt.Fprintf(os.Stderr, "\r  [%s%s] %5.1f%% (%s / %s)",
				strings.Repeat("=", filled), strings.Repeat(" ", w-filled),
				frac*100, humanBytes(p.read), humanBytes(p.total))
			if done {
				fmt.Fprintln(os.Stderr)
			}
		}
	} else if d := int(frac * 10); d > p.lastDecile {
		p.lastDecile = d
		log.Printf("firecracker:   %d%% (%s / %s)", d*10, humanBytes(p.read), humanBytes(p.total))
	}
	return n, err
}

// humanBytes formats a byte count in binary units (KiB/MiB/GiB) for progress logs.
//
// @arg n A byte count.
// @return string A human-readable size like "5.9 GiB".
//
// @testcase TestHumanBytes formats bytes across unit boundaries.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// assetCacheDir is where auto-resolved guest images are cached. The base rootfs
// is multi-GiB and the cache must survive reboots, so it must never land on the
// tmpfs-backed run-state dir (RAM: typically too small — the download dies
// half-way with "no space left on device" — and wiped on reboot). In priority:
// an explicit LLMBOX_FC_ASSET_CACHE; then an explicit --state-dir (stateDir),
// since an operator who pointed that at a disk location to move a spoke's files
// off the run-dir means the images too; then the user cache dir; then, only when
// no home resolves (the normal state of a root systemd service, which gets no
// $HOME), a disk path under /var/lib for a root process. A non-root process with
// no home and no explicit dir has nowhere better than the run-dir default.
//
// @arg stateDir The operator's explicit --state-dir, or empty for the default.
// @return string The directory pulled images are cached under.
//
// @testcase TestAssetCacheDirHonoursEnv honours LLMBOX_FC_ASSET_CACHE end to end.
func assetCacheDir(stateDir string) string {
	cache, cacheErr := os.UserCacheDir()
	return chooseAssetCacheDir(os.Getenv("LLMBOX_FC_ASSET_CACHE"), stateDir, cache, cacheErr, os.Geteuid())
}

// chooseAssetCacheDir is the pure policy behind assetCacheDir, with its inputs
// injected so every branch is testable. It never returns the tmpfs run-dir when
// a better location exists, in priority order: the env override; an explicit
// --state-dir; the user cache dir; and, only when no home resolves (a root
// systemd service has no $HOME) for a root process, a disk path under /var/lib.
// The run-dir default remains only the last resort for an unprivileged process
// with no home and no explicit dir.
//
// @arg envOverride The LLMBOX_FC_ASSET_CACHE value, or empty.
// @arg stateDir The operator's explicit --state-dir, or empty.
// @arg userCache The os.UserCacheDir result (used only when userCacheErr is nil).
// @arg userCacheErr The error from os.UserCacheDir (non-nil when no home resolves).
// @arg euid The effective user ID (0 selects the /var/lib fallback).
// @return string The chosen cache directory.
//
// @testcase TestChooseAssetCacheDir covers the override, state-dir, user-cache, root, and run-dir branches.
func chooseAssetCacheDir(envOverride, stateDir, userCache string, userCacheErr error, euid int) string {
	if envOverride != "" {
		return envOverride
	}
	if stateDir != "" {
		return filepath.Join(stateDir, "assets")
	}
	if userCacheErr == nil {
		return filepath.Join(userCache, "llmbox", "firecracker")
	}
	if euid == 0 {
		return "/var/lib/llmbox/firecracker/assets"
	}
	return filepath.Join(defaultStateDir, "assets")
}
