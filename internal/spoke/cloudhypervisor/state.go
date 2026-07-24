package cloudhypervisor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// boxMeta is the persisted description of one Cloud Hypervisor microVM box. Cloud
// Hypervisor has no daemon that remembers boxes, so the provisioner writes this
// alongside each box's runtime files; on startup it reloads them so List/Find/reap
// see boxes created by a previous run, and Destroy can clean up a box whose VM is
// already gone. It mirrors the Firecracker backend's boxMeta minus the jailer/chroot
// fields (phase 1 launches Cloud Hypervisor directly).
type boxMeta struct {
	// Token is the box's opaque generation token (exposed to the hub as its
	// InstanceID) and the name of its state subdirectory. It is spoke-owned and
	// never a native VM handle — the microVM has no daemon-assigned id.
	Token string `json:"token"`
	// BoxID is the caller-assigned alias, if any.
	BoxID string `json:"box_id,omitempty"`
	// Description is the caller-supplied label, if any.
	Description string `json:"description,omitempty"`
	// Image is the rootfs image the box booted.
	Image string `json:"image,omitempty"`
	// Phase is the auth phase ("pending" or "ready").
	Phase string `json:"phase"`
	// Paused is true when the box has been intentionally paused (its VM stopped to
	// free CPU/RAM while its rootfs is kept).
	Paused bool `json:"paused,omitempty"`
	// Created is the box creation time as a unix timestamp.
	Created int64 `json:"created"`
	// DiskBytes is the resolved size the box's writable rootfs was grown to.
	DiskBytes int64 `json:"disk_bytes,omitempty"`
	// GPUs holds the host PCI addresses passed through to the box by VFIO, persisted
	// so a rehydrated box reports (and a resume re-attaches) the same devices.
	GPUs []string `json:"gpus,omitempty"`
	// MDEVs holds the mediated-device refs (vGPU / MIG-backed vGPU) passed through to
	// the box, persisted so a rehydrated/resumed box re-attaches the same slices.
	MDEVs []string `json:"mdevs,omitempty"`
	// Egress is true when the box has a TAP-backed egress NIC; NetIndex is its pooled
	// network slot, persisted so a rehydrated box keeps the same TAP/IP and the slot
	// is freed only when the box is destroyed.
	Egress   bool `json:"egress,omitempty"`
	NetIndex int  `json:"net_index,omitempty"`
	// Namespace scopes the box to a provisioner namespace, mirroring the other
	// backends so two spokes sharing a host never see each other's boxes.
	Namespace string `json:"namespace,omitempty"`
}

// metaFileName is the per-box metadata file inside its state subdirectory.
const metaFileName = "meta.json"

// boxDir returns the state subdirectory for a box token; it holds the box's
// metadata, per-box rootfs copy, and live Cloud Hypervisor sockets.
//
// @arg stateDir The provisioner's state root.
// @arg token The box token.
// @return string The box's state subdirectory path.
//
// @testcase TestSaveLoadMeta round-trips metadata through a box directory.
func boxDir(stateDir, token string) string { return filepath.Join(stateDir, token) }

// apiSockPath returns the box's Cloud Hypervisor REST API socket path.
//
// @arg stateDir The provisioner's state root.
// @return string The box's API Unix-socket path.
//
// @testcase TestSaveLoadMeta derives the api socket path from the box dir.
func (m boxMeta) apiSockPath(stateDir string) string {
	return filepath.Join(boxDir(stateDir, m.Token), "ch-api.sock")
}

// vsockUDSPath returns the box's vsock UDS path, through which the host reaches the
// guest's control channel.
//
// @arg stateDir The provisioner's state root.
// @return string The box's vsock Unix-socket path.
//
// @testcase TestSaveLoadMeta derives the vsock socket path from the box dir.
func (m boxMeta) vsockUDSPath(stateDir string) string {
	return filepath.Join(boxDir(stateDir, m.Token), "vsock.sock")
}

// rootfsPath returns the box's per-box writable rootfs copy path.
//
// @arg stateDir The provisioner's state root.
// @return string The box's rootfs image path.
//
// @testcase TestSaveLoadMeta derives the rootfs path from the box dir.
func (m boxMeta) rootfsPath(stateDir string) string {
	return filepath.Join(boxDir(stateDir, m.Token), "rootfs.ext4")
}

// save writes m atomically into its box directory (creating the directory), so a
// crash mid-write never leaves a half-written meta file that fails to parse.
//
// @arg stateDir The provisioner's state root.
// @error error if the directory or file cannot be written.
//
// @testcase TestSaveLoadMeta writes then reads metadata back.
func (m boxMeta) save(stateDir string) error {
	dir := boxDir(stateDir, m.Token)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating box state dir: %w", err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshalling box meta: %w", err)
	}
	tmp := filepath.Join(dir, metaFileName+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing box meta: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, metaFileName)); err != nil {
		return fmt.Errorf("committing box meta: %w", err)
	}
	return nil
}

// loadMetas reads every box's metadata under stateDir, skipping directories with no
// (or unparseable) meta file. A missing stateDir yields no boxes and no error.
//
// @arg stateDir The provisioner's state root.
// @return []boxMeta The metadata of every persisted box.
// @error error if stateDir exists but cannot be read.
//
// @testcase TestLoadMetasSkipsJunk loads valid metas and ignores non-box entries.
func loadMetas(stateDir string) ([]boxMeta, error) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading state dir: %w", err)
	}
	var metas []boxMeta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(stateDir, e.Name(), metaFileName))
		if err != nil {
			continue
		}
		var m boxMeta
		if err := json.Unmarshal(data, &m); err != nil || m.Token == "" {
			continue
		}
		metas = append(metas, m)
	}
	return metas, nil
}

// toBox maps persisted metadata to the neutral box view. state is the runtime state
// ("running"/"stopped"/paused) the caller determined for the box.
//
// @arg state The runtime state to report.
// @return sandbox.Box The neutral view built from the metadata.
//
// @testcase TestSaveLoadMeta maps loaded metadata to a box view.
func (m boxMeta) toBox(state string) sandbox.Box {
	return sandbox.Box{
		InstanceID:  m.Token,
		Name:        namePrefix(m.Phase) + m.Token,
		BoxID:       m.BoxID,
		Description: m.Description,
		Image:       m.Image,
		State:       state,
		Phase:       m.Phase,
		Created:     m.Created,
	}
}

// namePrefix returns the instance-name prefix encoding a box's phase, mirroring the
// other backends' pending-/ready- naming so callers see a consistent shape.
//
// @arg phase The box's auth phase.
// @return string "llmbox-pending-" for a pending box, "llmbox-" otherwise.
//
// @testcase TestSaveLoadMeta derives the name prefix from the phase.
func namePrefix(phase string) string {
	if phase == "pending" {
		return "llmbox-pending-"
	}
	return "llmbox-"
}
