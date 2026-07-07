package firecracker

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	dockerregistry "github.com/docker/docker/api/types/registry"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

const (
	// defaultAssetRegistry is the OCI namespace the published guest images live
	// under. The base rootfs, agent payload, and guest kernel are stored there as
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
// agent payload) from an OCI registry into a local cache, so a spoke can run the
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
// the all-in-one layout (agent baked into that rootfs), not a surprise payload.
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
	// Log each carried file (the layers with a title) as it starts, with its size,
	// so a first-run multi-GiB download is visibly in progress rather than a hang.
	opts := oras.DefaultCopyOptions
	opts.PreCopy = func(_ context.Context, d ocispec.Descriptor) error {
		if title := d.Annotations[ocispec.AnnotationTitle]; title != "" {
			log.Printf("firecracker: downloading %s (%s) from %s ...", title, humanBytes(d.Size), ref)
		}
		return nil
	}
	if _, err := oras.Copy(ctx, repo, tag, fs, tag, opts); err != nil {
		return err
	}
	log.Printf("firecracker: %s ready", ref)
	_ = os.WriteFile(marker, []byte(desc.Digest.String()), 0o644)
	return nil
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

// assetCacheDir is where auto-resolved guest images are cached. It must survive
// reboots (the base rootfs is multi-GiB), so it defaults under the user cache dir
// rather than the tmpfs-backed run-state dir; override with LLMBOX_FC_ASSET_CACHE.
//
// @return string The directory pulled images are cached under.
//
// @testcase TestAssetCacheDirHonoursEnv honours LLMBOX_FC_ASSET_CACHE when set.
func assetCacheDir() string {
	if v := os.Getenv("LLMBOX_FC_ASSET_CACHE"); v != "" {
		return v
	}
	if base, err := os.UserCacheDir(); err == nil {
		return filepath.Join(base, "llmbox", "firecracker")
	}
	return filepath.Join(defaultStateDir, "assets")
}
