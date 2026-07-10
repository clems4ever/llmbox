package firecracker

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	dockerregistry "github.com/docker/docker/api/types/registry"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
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
//
// @arg ctx Context for the registry round-trips.
// @arg ref The full image reference, e.g. ghcr.io/owner/llmbox-fc-base:latest.
// @arg destDir Directory the artifact's file is written into.
// @arg cred Credential for the pull, or nil for anonymous.
// @error error if the reference is invalid or the registry copy fails.
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

	fs, err := file.New(destDir)
	if err != nil {
		return err
	}
	defer func() { _ = fs.Close() }()
	// Wrap the destination so each carried file (the layers with a title) streams
	// through a progress bar as it is written — a first-run multi-GiB download is
	// visibly moving rather than a single frozen "downloading…" line — and so a
	// layer larger than the free space fails up front instead of half-written.
	if _, err := oras.Copy(ctx, repo, tag, &progressTarget{Store: fs, dir: destDir}, tag, oras.DefaultCopyOptions); err != nil {
		return err
	}
	log.Printf("firecracker: %s ready", ref)
	_ = os.WriteFile(marker, []byte(desc.Digest.String()), 0o644)
	return nil
}

// progressTarget wraps an oras file store so that pushing a titled layer (a guest
// image file, as opposed to the small manifest/config blobs) renders a download
// progress bar as the bytes are written, and so a layer that cannot fit in the
// cache's free space fails before any bytes are streamed.
type progressTarget struct {
	*file.Store
	dir string // the cache directory the store writes into, for the free-space check
}

// Push writes desc's content to the underlying store, streaming a titled layer
// through a progress bar so a large download is visibly in progress. Before a
// titled layer streams it checks that dir has room for it, so a too-small cache
// (e.g. the tmpfs run-dir) fails in a second with an actionable error rather
// than downloading gigabytes only to hit "no space left on device" — which, on
// a restart-on-failure service, re-downloads on every retry.
//
// @arg ctx Context for the push.
// @arg desc The descriptor being written; a title annotation marks a guest image file.
// @arg content The content reader.
// @error error if the cache lacks room for the layer or the underlying store push fails.
//
// @testcase TestProgressTargetPush writes a titled blob through the progress wrapper.
// @testcase TestProgressTargetPushRejectsTooLarge fails a titled layer larger than the free space.
func (t *progressTarget) Push(ctx context.Context, desc ocispec.Descriptor, content io.Reader) error {
	if title := desc.Annotations[ocispec.AnnotationTitle]; title != "" && desc.Size > 0 {
		if free, err := freeBytes(t.dir); err == nil && free < uint64(desc.Size) {
			return fmt.Errorf("cannot cache %s (%s) in %s: only %s free — point --state-dir or LLMBOX_FC_ASSET_CACHE at a larger disk (the default run-dir is a small in-memory tmpfs)",
				title, humanBytes(desc.Size), t.dir, humanBytes(int64(free)))
		}
		log.Printf("firecracker: downloading %s (%s)...", title, humanBytes(desc.Size))
		content = newProgressReader(content, desc.Size)
	}
	return t.Store.Push(ctx, desc, content)
}

// freeBytes returns the bytes available for a new file under dir. A root process
// may use the filesystem-reserved blocks (Bfree); an unprivileged one may not
// (Bavail), so the check never rejects space the caller could actually use.
//
// @arg dir An existing directory on the target filesystem.
// @return uint64 The bytes available to this process on that filesystem.
// @error error if the filesystem cannot be stat'd.
//
// @testcase TestProgressTargetPushRejectsTooLarge relies on freeBytes to size the cache filesystem.
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
// bar on a terminal or decile log lines otherwise.
//
// @arg r The underlying content reader.
// @arg total The expected total byte count.
// @return *progressReader A reader that renders progress as it is consumed.
//
// @testcase TestProgressReader passes bytes through unchanged while tracking progress.
func newProgressReader(r io.Reader, total int64) *progressReader {
	tty := false
	if fi, err := os.Stderr.Stat(); err == nil {
		tty = fi.Mode()&os.ModeCharDevice != 0
	}
	return &progressReader{r: r, total: total, tty: tty}
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
