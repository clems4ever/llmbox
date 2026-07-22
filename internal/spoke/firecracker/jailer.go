package firecracker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	fcsdk "github.com/firecracker-microvm/firecracker-go-sdk"
)

// This file holds the jailer launch policy. Every production Firecracker box is
// started through the official `jailer`, which chroots the VMM, drops it to an
// unprivileged per-VM UID, sets up its own /dev/{kvm,net/tun,urandom} nodes, and
// places it in a cgroup — a defense-in-depth boundary around an untrusted workload.
// There is deliberately NO flag to launch firecracker directly: jailing is the only
// path, and a host missing the prerequisites fails closed (see checkJailerPrereqs)
// rather than silently running unjailed.

const (
	// defaultJailerBin is the jailer executable, resolved from PATH when the operator
	// configures no explicit path. fetch-firecracker.sh installs it next to
	// firecracker, version-matched.
	defaultJailerBin = "jailer"

	// defaultChrootSubdir is the jailer chroot base created under the state dir when
	// the operator configures none. Keeping it under the state dir keeps it on the
	// same filesystem as each box's rootfs copy and the shared asset cache, so the
	// jailer's chroot strategy can hard-link the kernel, rootfs, and payload into the
	// chroot (a hard link cannot cross filesystems).
	defaultChrootSubdir = "chroot"

	// defaultUIDMin and defaultUIDMax bound the range unique per-VM UIDs are drawn
	// from. The range sits in the conventional subordinate-uid space so it does not
	// collide with real host accounts. Each live box holds one UID; it is freed only
	// once the box is destroyed.
	defaultUIDMin = 100000
	defaultUIDMax = 165535

	// defaultFcGID is the shared group every jailed VMM runs under. It owns the
	// pooled TAP devices (created with `ip tuntap add ... group <gid>`), so a jailed,
	// unprivileged Firecracker can attach to its assigned TAP without CAP_NET_ADMIN.
	// Per-box files stay owner-only (mode 0700 dirs, 0600 rootfs), so sharing this
	// group across boxes leaks no box state. It is separate from the per-box UID
	// precisely so filesystem isolation (UID) and TAP access (GID) are decoupled.
	defaultFcGID = 100000

	// defaultNumaNode is the NUMA node the jailer binds each VMM to; single-node hosts
	// (the common case) use 0.
	defaultNumaNode = 0
)

// jailerConfig groups the host-wide jailer settings resolved once for the
// provisioner: the binaries, chroot base, UID range, shared GID, and cgroup
// version. Per-VM values (the unique UID and the box's ID) are layered on at boot.
type jailerConfig struct {
	// jailerBin is the jailer executable (path or PATH-resolved name).
	jailerBin string
	// execFile is the absolute path to the firecracker binary the jailer exec-s.
	execFile string
	// chrootBase is the jailer chroot base directory.
	chrootBase string
	// uidMin/uidMax bound the per-VM UID allocation range (inclusive).
	uidMin, uidMax int
	// gid is the shared fc-net group every jailed VMM runs under.
	gid int
	// cgroupVersion is the cgroup filesystem version ("1" or "2") the jailer uses.
	cgroupVersion string
}

// execBase is the basename of the firecracker binary, which the jailer uses as the
// chroot subdirectory (<chrootBase>/<execBase>/<id>/root). Path computation must
// match it exactly, so it is derived from the resolved exec file.
//
// @return string The firecracker binary basename (e.g. "firecracker").
//
// @testcase TestChrootPaths derives the chroot path from the exec basename.
func (jc jailerConfig) execBase() string { return filepath.Base(jc.execFile) }

// defaultJailerConfig returns the jailer settings for a provisioner whose chroot
// base defaults under stateDir. Binary paths are left unresolved here (a bare
// "firecracker"/"jailer"); checkJailerPrereqs resolves and validates them, so
// construction never touches the host.
//
// @arg stateDir The provisioner's state root; the chroot base defaults under it.
// @return jailerConfig The default jailer settings.
//
// @testcase TestDefaultJailerConfig sets the chroot base under the state dir.
func defaultJailerConfig(stateDir string) jailerConfig {
	return jailerConfig{
		jailerBin:     defaultJailerBin,
		execFile:      defaultFirecrackerBin,
		chrootBase:    filepath.Join(stateDir, defaultChrootSubdir),
		uidMin:        defaultUIDMin,
		uidMax:        defaultUIDMax,
		gid:           defaultFcGID,
		cgroupVersion: detectCgroupVersion(),
	}
}

// detectCgroupVersion reports the host cgroup filesystem version the jailer should
// place VMMs in: "2" on a unified (cgroup v2) host, "1" otherwise. The unified
// hierarchy exposes /sys/fs/cgroup/cgroup.controllers, which v1 does not.
//
// @return string "2" on a cgroup v2 host, "1" otherwise.
//
// @testcase TestDetectCgroupVersion returns a valid version string.
func detectCgroupVersion() string {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		return "2"
	}
	return "1"
}

// jailerCfgFor builds the SDK JailerConfig for one box: its unique UID and ID plus
// the shared host settings, with the naive chroot strategy that hard-links the
// kernel and drives into the chroot and Daemonize set so the jailer setsid()s the
// VMM into its own session — detaching it from the spoke so it survives a spoke
// restart (the pre-jailer code did this with Setpgid). The kernelImage is the host
// path the strategy links in.
//
// @arg uid The box's unique unprivileged UID.
// @arg id The box's jailer ID (its token).
// @arg kernelImage The host kernel path the chroot strategy hard-links into the jail.
// @return *fcsdk.JailerConfig The per-box jailer configuration.
//
// @testcase TestJailerCfgFor builds a per-box jailer config with the shared settings.
func (jc jailerConfig) jailerCfgFor(uid int, id, kernelImage string) *fcsdk.JailerConfig {
	gid := jc.gid
	numa := defaultNumaNode
	return &fcsdk.JailerConfig{
		UID:            fcsdk.Int(uid),
		GID:            fcsdk.Int(gid),
		ID:             id,
		NumaNode:       fcsdk.Int(numa),
		ExecFile:       jc.execFile,
		JailerBinary:   jc.jailerBin,
		ChrootBaseDir:  jc.chrootBase,
		CgroupVersion:  jc.cgroupVersion,
		Daemonize:      true,
		ChrootStrategy: fcsdk.NewNaiveChrootStrategy(kernelImage),
	}
}

// checkJailerPrereqs validates, at spoke startup, that this host can launch jailed
// VMMs and returns an actionable error listing every problem rather than failing
// later mid-boot. It resolves the firecracker and jailer binaries to absolute paths
// (jailer needs an absolute exec-file), and requires root (jailer must chroot,
// mknod, chown, and setuid) and a present /dev/kvm. It never falls back to direct
// launch — a failure here fails the spoke closed. netEnabled additionally requires
// the shared GID to be usable for TAP ownership. On success the resolved absolute
// binary paths are written back into jc.
//
// @arg netEnabled Whether egress networking (and thus TAP-group ownership) is in use.
// @error error naming every missing prerequisite (binaries, privilege, /dev/kvm, UID range).
//
// @testcase TestCheckJailerPrereqsReportsProblems reports a missing binary / non-root / no kvm.
func (jc *jailerConfig) checkJailerPrereqs(netEnabled bool) error {
	var problems []string

	fcPath, err := exec.LookPath(jc.execFile)
	if err != nil {
		problems = append(problems, fmt.Sprintf("firecracker binary %q not found on PATH (install it, e.g. scripts/firecracker/fetch-firecracker.sh): %v", jc.execFile, err))
	} else {
		jc.execFile = fcPath
	}

	jailerPath, err := exec.LookPath(jc.jailerBin)
	if err != nil {
		problems = append(problems, fmt.Sprintf("jailer binary %q not found on PATH — jailed launch is mandatory, install a firecracker-matched jailer (scripts/firecracker/fetch-firecracker.sh): %v", jc.jailerBin, err))
	} else {
		jc.jailerBin = jailerPath
	}

	if os.Geteuid() != 0 {
		problems = append(problems, "jailer requires root (it must chroot, create device nodes, and drop to an unprivileged per-VM UID); run the firecracker spoke as root")
	}

	if _, err := os.Stat("/dev/kvm"); err != nil {
		problems = append(problems, fmt.Sprintf("/dev/kvm is not available (%v); Firecracker needs KVM (enable virtualization / load the kvm module)", err))
	}

	if jc.uidMin <= 0 || jc.uidMax < jc.uidMin {
		problems = append(problems, fmt.Sprintf("invalid per-VM UID range [%d,%d]", jc.uidMin, jc.uidMax))
	}
	if jc.gid <= 0 {
		problems = append(problems, fmt.Sprintf("invalid fc-net GID %d", jc.gid))
	}

	if len(problems) > 0 {
		return fmt.Errorf("firecracker jailer prerequisites not met:\n  - %s", joinProblems(problems))
	}
	return nil
}

// ensureAssetsReadable makes the shared, read-only guest assets (the kernel and the
// optional payload image) readable by the fc-net group, so every jailed VMM — which
// runs under that group and gets these files hard-linked into its chroot — can read
// them, while they are not world-readable. The per-box writable rootfs is handled
// separately (chowned to the box's own UID). It is a no-op when not root — jailing
// requires root (validated by checkJailerPrereqs) — so it only skips in unprivileged
// unit tests. Empty paths are ignored.
//
// @arg gid The shared fc-net GID the assets are made group-readable for.
// @arg paths The shared asset paths (kernel, payload); empty entries are skipped.
// @error error if an asset's ownership or mode cannot be set while running as root.
//
// @testcase TestEnsureAssetsReadableSkipsUnprivileged is a no-op when not root.
func ensureAssetsReadable(gid int, paths ...string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat guest asset %s: %w", path, err)
		}
		if err := os.Chown(path, -1, gid); err != nil {
			return fmt.Errorf("setting group of guest asset %s to %d: %w", path, gid, err)
		}
		// Add group-read (0040) to whatever mode the asset already carries.
		if err := os.Chmod(path, info.Mode().Perm()|0o040); err != nil {
			return fmt.Errorf("making guest asset %s group-readable: %w", path, err)
		}
	}
	return nil
}

// joinProblems renders a prerequisite problem list as newline-and-bullet-separated
// text for a single actionable error.
//
// @arg problems The problem descriptions.
// @return string The joined, bulleted problem list.
//
// @testcase TestCheckJailerPrereqsReportsProblems checks the joined message lists each problem.
func joinProblems(problems []string) string {
	out := ""
	for i, p := range problems {
		if i > 0 {
			out += "\n  - "
		}
		out += p
	}
	return out
}
