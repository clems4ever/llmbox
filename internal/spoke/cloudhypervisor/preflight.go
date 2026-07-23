package cloudhypervisor

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/clems4ever/llmbox/internal/spoke/box/backend"
)

// preflightConfig is what a spoke wants to run, distilled to the facts the host must
// satisfy: which cloud-hypervisor binary, which guest kernel/rootfs, which egress
// mode, and whether any GPU passthrough is requested (which additionally needs an
// IOMMU).
type preflightConfig struct {
	chBinary   string
	kernel     string
	rootfs     string
	egressMode egressMode
	// gpuCount is the number of full-GPU + mediated-device passthroughs requested; when
	// non-zero the host must have an active IOMMU.
	gpuCount int
}

// preflightProbes are the host-touching checks a preflight run performs, injected so
// the validator is fully unit-testable with fakes (no real /dev/kvm, CPU flags, or
// binaries needed in CI). realProbes wires the production implementations.
type preflightProbes struct {
	// lookPath resolves an executable (exec.LookPath).
	lookPath func(string) (string, error)
	// readable reports whether a path can be opened for reading.
	readable func(string) error
	// kvmReadWrite reports whether /dev/kvm exists and is open-able read/write by us.
	kvmReadWrite func() error
	// cpuVirt reports whether the CPU exposes hardware virtualization.
	cpuVirt func() (bool, error)
	// iommuActive reports whether the kernel has an active IOMMU (groups present).
	iommuActive func() bool
	// euid returns the effective UID (os.Geteuid).
	euid func() int
}

// realProbes returns the production host probes.
//
// @return preflightProbes The probes backed by the real host (PATH, /dev/kvm, /proc, /sys).
//
// @testcase TestPreflightValidate uses injected fakes instead of these.
func realProbes() preflightProbes {
	return preflightProbes{
		lookPath: exec.LookPath,
		readable: func(p string) error {
			f, err := os.Open(p)
			if err == nil {
				_ = f.Close()
			}
			return err
		},
		kvmReadWrite: func() error {
			f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
			if err != nil {
				return err
			}
			_ = f.Close()
			return nil
		},
		cpuVirt: func() (bool, error) {
			// arm64 KVM has no vmx/svm cpuinfo flag; /dev/kvm access (checked separately)
			// is the real gate there, so don't flag a missing flag on arm64.
			if runtime.GOARCH == "arm64" {
				return true, nil
			}
			data, err := os.ReadFile("/proc/cpuinfo")
			if err != nil {
				return false, err
			}
			return cpuHasVirtualization(string(data)), nil
		},
		iommuActive: func() bool {
			entries, err := os.ReadDir("/sys/kernel/iommu_groups")
			return err == nil && len(entries) > 0
		},
		euid: os.Geteuid,
	}
}

// cpuHasVirtualization reports whether /proc/cpuinfo content advertises hardware
// virtualization: the Intel "vmx" or AMD "svm" CPU flag.
//
// @arg cpuinfo The contents of /proc/cpuinfo.
// @return bool True when a vmx or svm flag is present.
//
// @testcase TestCPUHasVirtualization detects vmx/svm and rejects a plain flag line.
func cpuHasVirtualization(cpuinfo string) bool {
	for _, line := range strings.Split(cpuinfo, "\n") {
		// The relevant line is "flags" (x86); tolerate leading whitespace.
		if !strings.HasPrefix(strings.TrimSpace(line), "flags") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if f == "vmx" || f == "svm" {
				return true
			}
		}
	}
	return false
}

// validate runs every preflight check against the host and returns a single error
// listing ALL problems found (so an operator fixes them in one pass) or nil when the
// host is ready. It is the fail-fast gate the spoke runs at startup before serving, so
// a misconfigured host produces one clear, actionable message instead of a confusing
// failure on the first box create.
//
// @arg cfg What the spoke wants to run (binary, kernel/rootfs, egress mode, GPU count).
// @error error listing every unmet prerequisite, or nil when the host is ready.
//
// @testcase TestPreflightValidate passes a ready host and reports each unmet prerequisite.
func (pr preflightProbes) validate(cfg preflightConfig) error {
	var problems []string
	add := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	bin := cfg.chBinary
	if bin == "" {
		bin = defaultCHBinary
	}
	if _, err := pr.lookPath(bin); err != nil {
		add("cloud-hypervisor binary %q not found on PATH or not executable (%v); install it or set --cloud-hypervisor", bin, err)
	}

	if err := pr.kvmReadWrite(); err != nil {
		add("/dev/kvm is not available for read+write (%v); load the kvm module and give this user access (e.g. add it to the kvm group), or enable (nested) virtualization on this host/instance", err)
	}

	if ok, err := pr.cpuVirt(); err != nil {
		add("could not determine CPU virtualization support (%v)", err)
	} else if !ok {
		add("the CPU does not expose hardware virtualization (no vmx/svm flag); enable Intel VT-x / AMD-V in firmware, or run on a virtualization-capable host/instance")
	}

	if cfg.kernel == "" {
		add("no guest kernel configured; set --kernel to the guest vmlinux path")
	} else if err := pr.readable(cfg.kernel); err != nil {
		add("guest kernel %q is not readable (%v)", cfg.kernel, err)
	}
	if cfg.rootfs == "" {
		add("no guest rootfs configured; set --rootfs to the guest rootfs image path")
	} else if err := pr.readable(cfg.rootfs); err != nil {
		add("guest rootfs %q is not readable (%v)", cfg.rootfs, err)
	}

	switch cfg.egressMode {
	case egressManaged:
		if pr.euid() != 0 {
			add("--egress-mode=managed provisions host TAP/NAT and needs root / CAP_NET_ADMIN; run the spoke as root, or use --egress-mode=external (attach to a pre-provisioned pool) or --disable-egress")
		}
		for _, tool := range []string{"ip", "iptables"} {
			if _, err := pr.lookPath(tool); err != nil {
				add("managed egress needs %q on PATH (%v); install iproute2/iptables or use --disable-egress", tool, err)
			}
		}
	case egressExternal:
		if _, err := pr.lookPath("ip"); err != nil {
			add("external egress validation needs %q on PATH (%v); install iproute2", "ip", err)
		}
	}

	if cfg.gpuCount > 0 && !pr.iommuActive() {
		add("GPU passthrough is configured but no IOMMU groups are present; enable the IOMMU on the host kernel command line (intel_iommu=on / amd_iommu=on) and bind the device(s) to vfio-pci")
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("cloud-hypervisor spoke preflight failed — the host is not ready to run microVM boxes:\n  - %s", strings.Join(problems, "\n  - "))
}

// preflightConfigFrom builds the preflight config from neutral backend options and the
// resolved egress mode.
//
// @arg opts The neutral backend options.
// @arg mode The resolved egress mode.
// @return preflightConfig The distilled host requirements.
//
// @testcase TestPreflightConfigFrom maps options and GPU counts into the config.
func preflightConfigFrom(opts backend.Options, mode egressMode) preflightConfig {
	return preflightConfig{
		chBinary:   opts.CloudHypervisorBinary,
		kernel:     opts.KernelImagePath,
		rootfs:     opts.RootfsImagePath,
		egressMode: mode,
		gpuCount:   len(opts.GPUPassthrough) + len(opts.GPUMediatedDevices),
	}
}
