package cloudhypervisor

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestValidatePCIAddress accepts well-formed PCI addresses and rejects malformed
// ones, since a bad address must fail the spoke at startup rather than at box boot.
func TestValidatePCIAddress(t *testing.T) {
	good := []string{"0000:65:00.0", "0000:00:1f.7", "abcd:ef:0a.3"}
	for _, a := range good {
		if !validatePCIAddress(a) {
			t.Errorf("validatePCIAddress(%q) = false, want true", a)
		}
	}
	bad := []string{"", "0000:65:00", "65:00.0", "0000:65:00.8", "0000:65:0g.0", "0000:65:00.0/", "all"}
	for _, a := range bad {
		if validatePCIAddress(a) {
			t.Errorf("validatePCIAddress(%q) = true, want false", a)
		}
	}
}

// TestBuildVMConfigBasics checks the core VmConfig wiring: kernel/cmdline payload,
// the rootfs as the first writable virtio-block disk, the vsock control channel, and
// the CPU/memory sizing. It also asserts the cmdline keeps PCI enabled — the whole
// reason to use Cloud Hypervisor here — so a future edit can't silently re-add
// pci=off and break GPU passthrough.
func TestBuildVMConfigBasics(t *testing.T) {
	cfg := buildVMConfig(vmConfigParams{
		Kernel:      "/k/vmlinux",
		Rootfs:      "/boxes/t1/rootfs.ext4",
		VsockUDS:    "/boxes/t1/vsock.sock",
		VCPUs:       2,
		MemoryBytes: 1 << 30,
	})
	if cfg.Payload.Kernel != "/k/vmlinux" {
		t.Errorf("kernel = %q", cfg.Payload.Kernel)
	}
	if strings.Contains(cfg.Payload.Cmdline, "pci=off") {
		t.Errorf("cmdline must NOT disable PCI (GPU passthrough needs it): %q", cfg.Payload.Cmdline)
	}
	if len(cfg.Disks) != 1 || cfg.Disks[0].Path != "/boxes/t1/rootfs.ext4" || cfg.Disks[0].Readonly {
		t.Errorf("rootfs disk not wired writable: %+v", cfg.Disks)
	}
	if cfg.Vsock.Socket != "/boxes/t1/vsock.sock" || cfg.Vsock.CID != guestCID {
		t.Errorf("vsock not wired: %+v", cfg.Vsock)
	}
	if cfg.CPUs.BootVCPUs != 2 || cfg.CPUs.MaxVCPUs != 2 || cfg.Memory.Size != 1<<30 {
		t.Errorf("sizing not applied: cpus=%+v mem=%d", cfg.CPUs, cfg.Memory.Size)
	}
}

// TestBuildVMConfigGPUPassthrough checks each requested GPU PCI address becomes a
// VFIO passthrough device pointing at the kernel's sysfs path — the feature this
// backend exists for.
func TestBuildVMConfigGPUPassthrough(t *testing.T) {
	cfg := buildVMConfig(vmConfigParams{
		Kernel:   "/k/vmlinux",
		Rootfs:   "/r.ext4",
		VsockUDS: "/v.sock",
		VCPUs:    1,
		GPUs:     []string{"0000:65:00.0", "0000:b3:00.0"},
	})
	if len(cfg.Devices) != 2 {
		t.Fatalf("want 2 VFIO devices, got %d: %+v", len(cfg.Devices), cfg.Devices)
	}
	want := []string{"/sys/bus/pci/devices/0000:65:00.0/", "/sys/bus/pci/devices/0000:b3:00.0/"}
	for i, d := range cfg.Devices {
		if d.Path != want[i] {
			t.Errorf("device[%d].Path = %q, want %q", i, d.Path, want[i])
		}
	}
	// The devices must survive JSON marshalling under the "devices" key Cloud
	// Hypervisor expects.
	blob, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"devices":[{"path":"/sys/bus/pci/devices/0000:65:00.0/"}`) {
		t.Errorf("marshalled config missing VFIO devices: %s", blob)
	}
}

// TestValidateMediatedDevice accepts mdev UUIDs and absolute /sys paths and rejects
// anything else, since a bad ref must fail the spoke at startup.
func TestValidateMediatedDevice(t *testing.T) {
	good := []string{"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "/sys/bus/mdev/devices/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}
	for _, s := range good {
		if !validateMediatedDevice(s) {
			t.Errorf("validateMediatedDevice(%q) = false, want true", s)
		}
	}
	bad := []string{"", "not-a-uuid", "0000:65:00.0", "/etc/passwd", "aaaaaaaa-bbbb-cccc-dddd"}
	for _, s := range bad {
		if validateMediatedDevice(s) {
			t.Errorf("validateMediatedDevice(%q) = true, want false", s)
		}
	}
}

// TestBuildVMConfigMediatedDevices checks vGPU/MIG mdev refs become VFIO devices
// pointing at their sysfs mdev path (UUID resolved under the mdev bus; absolute /sys
// path used as-is), and that they coexist with full-GPU passthrough devices.
func TestBuildVMConfigMediatedDevices(t *testing.T) {
	cfg := buildVMConfig(vmConfigParams{
		Kernel: "/k", Rootfs: "/r", VsockUDS: "/v", VCPUs: 1,
		GPUs:  []string{"0000:65:00.0"},
		MDEVs: []string{"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "/sys/devices/custom/mdev0"},
	})
	if len(cfg.Devices) != 3 {
		t.Fatalf("want 3 VFIO devices (1 GPU + 2 mdev), got %d: %+v", len(cfg.Devices), cfg.Devices)
	}
	want := []string{
		"/sys/bus/pci/devices/0000:65:00.0/",
		"/sys/bus/mdev/devices/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"/sys/devices/custom/mdev0",
	}
	for i, d := range cfg.Devices {
		if d.Path != want[i] {
			t.Errorf("device[%d].Path = %q, want %q", i, d.Path, want[i])
		}
	}
}

// TestBuildVMConfigNoGPUsOmitsDevices checks the devices key is omitted entirely when
// no GPU is requested, so a plain box's config carries no empty passthrough array.
func TestBuildVMConfigNoGPUsOmitsDevices(t *testing.T) {
	cfg := buildVMConfig(vmConfigParams{Kernel: "/k", Rootfs: "/r", VsockUDS: "/v", VCPUs: 1})
	if cfg.Devices != nil {
		t.Errorf("Devices = %+v, want nil for a box with no GPU", cfg.Devices)
	}
	blob, _ := json.Marshal(cfg)
	if strings.Contains(string(blob), "devices") {
		t.Errorf("marshalled config should omit devices when empty: %s", blob)
	}
}

// TestBuildVMConfigDefaultCmdline checks an empty cmdline falls back to the default
// (which keeps PCI on and points root at /dev/vda).
func TestBuildVMConfigDefaultCmdline(t *testing.T) {
	cfg := buildVMConfig(vmConfigParams{Kernel: "/k", Rootfs: "/r", VsockUDS: "/v", VCPUs: 1})
	if cfg.Payload.Cmdline != defaultKernelCmdline {
		t.Errorf("cmdline = %q, want default %q", cfg.Payload.Cmdline, defaultKernelCmdline)
	}
	custom := buildVMConfig(vmConfigParams{Kernel: "/k", Rootfs: "/r", VsockUDS: "/v", VCPUs: 1, Cmdline: "custom args"})
	if custom.Payload.Cmdline != "custom args" {
		t.Errorf("custom cmdline not honoured: %q", custom.Payload.Cmdline)
	}
}
