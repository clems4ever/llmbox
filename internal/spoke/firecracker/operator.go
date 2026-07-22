package firecracker

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// VMStatus is an operator-facing snapshot of one persisted microVM box, built by
// ListVMs straight from the on-disk state (no running provisioner). Running
// reflects a live probe of the box's VMM API socket at snapshot time, so it
// distinguishes a box still executing from one whose VMM is gone.
type VMStatus struct {
	// Token is the box's generation token (its InstanceID and state subdir name).
	Token string
	// BoxID is the caller-assigned alias, if any.
	BoxID string
	// Namespace scopes the box to a spoke namespace, if any.
	Namespace string
	// Phase is the box's auth phase ("pending" or "ready").
	Phase string
	// Paused is true when the box was intentionally paused (VM stopped, rootfs kept).
	Paused bool
	// Running is true when the box's VMM still answers on its API socket.
	Running bool
	// NetIndex is the box's pooled network slot.
	NetIndex int
	// Created is the box creation time as a unix timestamp.
	Created int64
}

// ListVMs reports every microVM box persisted under stateDir, probing each one's
// VMM to say whether it is still running. It reads only on-disk state plus a
// best-effort liveness probe — never booting or stopping anything — so an operator
// can inspect a host's boxes without (or alongside) a running spoke. The boxes are
// returned oldest-first. An empty stateDir uses the backend default.
//
// @arg stateDir The spoke's per-box state directory; empty uses defaultStateDir.
// @return []VMStatus One snapshot per persisted box, oldest first.
// @error error if stateDir exists but cannot be read.
//
// @testcase TestListVMs lists persisted boxes with their probed running state.
func ListVMs(stateDir string) ([]VMStatus, error) {
	if stateDir == "" {
		stateDir = defaultStateDir
	}
	metas, err := loadMetas(stateDir)
	if err != nil {
		return nil, err
	}
	out := make([]VMStatus, 0, len(metas))
	for _, m := range metas {
		out = append(out, VMStatus{
			Token:     m.Token,
			BoxID:     m.BoxID,
			Namespace: m.Namespace,
			Phase:     m.Phase,
			Paused:    m.Paused,
			Running:   vmmAlive(m.apiSockPath(stateDir)),
			NetIndex:  m.NetIndex,
			Created:   m.Created,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created < out[j].Created })
	return out, nil
}

// DestroyVM stops and removes a single microVM box by exact token, exact box id, or
// unambiguous token prefix, directly against stateDir. A still-running VMM is asked
// to shut down over its API socket (best-effort) before its state directory (rootfs,
// sockets, metadata) is removed, so the rootfs is never deleted under a live VM. It
// is the operator escape hatch for reaping a box a crashed or detached spoke left
// behind; the box's pooled network slot frees itself once the metadata is gone. An
// empty stateDir uses the backend default.
//
// @arg stateDir The spoke's per-box state directory; empty uses defaultStateDir.
// @arg idOrToken The box to destroy: exact token, exact box id, or unique token prefix.
// @return VMStatus The identity of the box that was destroyed.
// @error error wrapping sandbox.ErrBoxNotFound if nothing matches, if the match is ambiguous, or if the state cannot be removed.
//
// @testcase TestDestroyVM halts and removes a matched box, leaving others intact.
// @testcase TestDestroyVMUnknown errors when no box matches the id.
func DestroyVM(stateDir, idOrToken string) (VMStatus, error) {
	if stateDir == "" {
		stateDir = defaultStateDir
	}
	metas, err := loadMetas(stateDir)
	if err != nil {
		return VMStatus{}, err
	}
	m, err := resolveMeta(metas, idOrToken)
	if err != nil {
		return VMStatus{}, err
	}
	if err := destroyBox(stateDir, m); err != nil {
		return VMStatus{}, err
	}
	return VMStatus{Token: m.Token, BoxID: m.BoxID, Namespace: m.Namespace, Phase: m.Phase}, nil
}

// DestroyAllVMs stops and removes every microVM box persisted under stateDir,
// halting each live VMM over its API socket before deleting its state. It is the
// operator sledgehammer for wiping a host clean — e.g. before decommissioning a
// spoke or after a bad rollout left orphaned boxes behind. It is best-effort per
// box: a box that fails to halt or delete is reported in the joined error while the
// rest are still destroyed, so one stuck VMM never blocks the sweep. The returned
// slice holds the boxes actually destroyed. An empty stateDir uses the backend
// default.
//
// @arg stateDir The spoke's per-box state directory; empty uses defaultStateDir.
// @return []VMStatus The boxes that were destroyed.
// @error error joining the failures of any boxes that could not be halted or removed.
//
// @testcase TestDestroyAllVMs destroys every persisted box and reports them.
// @testcase TestDestroyAllVMsEmpty destroys nothing and errors nil on a clean host.
func DestroyAllVMs(stateDir string) ([]VMStatus, error) {
	if stateDir == "" {
		stateDir = defaultStateDir
	}
	metas, err := loadMetas(stateDir)
	if err != nil {
		return nil, err
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Created < metas[j].Created })
	var destroyed []VMStatus
	var errs []error
	for _, m := range metas {
		if err := destroyBox(stateDir, m); err != nil {
			errs = append(errs, err)
			continue
		}
		destroyed = append(destroyed, VMStatus{Token: m.Token, BoxID: m.BoxID, Namespace: m.Namespace, Phase: m.Phase})
	}
	return destroyed, errors.Join(errs...)
}

// destroyBox stops box m and removes its state directory: a still-running VMM is
// asked to shut down over its API socket first, so the rootfs is never deleted
// under a live VM. It is the shared teardown behind DestroyVM and DestroyAllVMs.
//
// @arg stateDir The spoke's per-box state directory.
// @arg m The box to destroy.
// @error error if the box's VMM cannot be halted or its state cannot be removed.
//
// @testcase TestDestroyVM tears a box down through destroyBox.
func destroyBox(stateDir string, m boxMeta) error {
	apiSock := m.apiSockPath(stateDir)
	if vmmAlive(apiSock) {
		if err := haltVMM(apiSock); err != nil {
			return fmt.Errorf("halting VMM for box %s: %w", m.Token, err)
		}
	}
	if err := os.RemoveAll(boxDir(stateDir, m.Token)); err != nil {
		return fmt.Errorf("removing box %s state: %w", m.Token, err)
	}
	// Remove the jailer chroot too (empty for a legacy direct box), so destroying a
	// jailed box leaks no chroot/socket state.
	if chroot := m.chrootInstanceDir(); chroot != "" {
		if err := os.RemoveAll(chroot); err != nil {
			return fmt.Errorf("removing box %s chroot: %w", m.Token, err)
		}
	}
	return nil
}

// resolveMeta finds the single box matching idOrToken. An exact token or box-id
// match wins immediately; otherwise a token prefix must match exactly one box, so
// an operator can use a short prefix without silently hitting the wrong box.
//
// @arg metas The persisted box metadata to search.
// @arg idOrToken The token, box id, or token prefix to resolve.
// @return boxMeta The single matched box.
// @error error wrapping sandbox.ErrBoxNotFound if none match, or a plain error if a prefix is ambiguous.
//
// @testcase TestDestroyVM resolves a box by exact id.
// @testcase TestResolveMetaAmbiguousPrefix errors when a prefix matches several boxes.
func resolveMeta(metas []boxMeta, idOrToken string) (boxMeta, error) {
	var prefixed []boxMeta
	for _, m := range metas {
		if m.Token == idOrToken || (m.BoxID != "" && m.BoxID == idOrToken) {
			return m, nil
		}
		if strings.HasPrefix(m.Token, idOrToken) {
			prefixed = append(prefixed, m)
		}
	}
	switch len(prefixed) {
	case 1:
		return prefixed[0], nil
	case 0:
		return boxMeta{}, fmt.Errorf("%w %q", sandbox.ErrBoxNotFound, idOrToken)
	default:
		return boxMeta{}, fmt.Errorf("box id %q is ambiguous: matches %d boxes", idOrToken, len(prefixed))
	}
}
