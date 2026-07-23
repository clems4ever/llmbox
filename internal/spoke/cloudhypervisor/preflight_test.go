package cloudhypervisor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clems4ever/llmbox/internal/spoke/box/backend"
)

// TestCPUHasVirtualization detects the vmx/svm flag on a real-shaped cpuinfo flags
// line and rejects one without it.
func TestCPUHasVirtualization(t *testing.T) {
	withVMX := "processor\t: 0\nflags\t\t: fpu vme de pse tsc msr pae vmx lm\nmodel name\t: x\n"
	withSVM := "flags\t\t: fpu de pse tsc msr fxsr svm lm\n"
	without := "processor\t: 0\nflags\t\t: fpu vme de pse tsc msr pae lm\n"
	if !cpuHasVirtualization(withVMX) {
		t.Error("should detect vmx")
	}
	if !cpuHasVirtualization(withSVM) {
		t.Error("should detect svm")
	}
	if cpuHasVirtualization(without) {
		t.Error("should not detect virtualization without vmx/svm")
	}
	// A CPU flag literally named elsewhere must not false-positive: only the flags line
	// counts.
	if cpuHasVirtualization("model name\t: fake vmx cpu\n") {
		t.Error("vmx only counts on the flags line")
	}
}

// okProbes returns probes describing a fully ready host, so a test can knock out one
// dimension at a time.
func okProbes() preflightProbes {
	return preflightProbes{
		lookPath:     func(string) (string, error) { return "/usr/bin/x", nil },
		readable:     func(string) error { return nil },
		kvmReadWrite: func() error { return nil },
		cpuVirt:      func() (bool, error) { return true, nil },
		iommuActive:  func() bool { return true },
		euid:         func() int { return 0 },
	}
}

// readyConfig is a control-only spoke on a temp kernel/rootfs so the readable probe
// has real paths to accept (the fake accepts anything, but this keeps intent clear).
func readyConfig(t *testing.T) preflightConfig {
	dir := t.TempDir()
	k := filepath.Join(dir, "vmlinux")
	r := filepath.Join(dir, "rootfs.ext4")
	for _, p := range []string{k, r} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return preflightConfig{chBinary: "cloud-hypervisor", kernel: k, rootfs: r, egressMode: egressDisabled}
}

// TestPreflightValidate passes a ready host and then checks each unmet prerequisite is
// individually reported with an actionable message.
func TestPreflightValidate(t *testing.T) {
	cfg := readyConfig(t)
	if err := okProbes().validate(cfg); err != nil {
		t.Fatalf("a ready host should pass preflight: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*preflightProbes, *preflightConfig)
		want   string
	}{
		{"no binary", func(p *preflightProbes, _ *preflightConfig) {
			p.lookPath = func(string) (string, error) { return "", errors.New("not found") }
		}, "cloud-hypervisor binary"},
		{"no kvm", func(p *preflightProbes, _ *preflightConfig) {
			p.kvmReadWrite = func() error { return errors.New("permission denied") }
		}, "/dev/kvm"},
		{"no cpu virt", func(p *preflightProbes, _ *preflightConfig) {
			p.cpuVirt = func() (bool, error) { return false, nil }
		}, "hardware virtualization"},
		{"unreadable kernel", func(p *preflightProbes, _ *preflightConfig) {
			p.readable = func(string) error { return errors.New("no such file") }
		}, "guest kernel"},
		{"missing kernel path", func(_ *preflightProbes, c *preflightConfig) {
			c.kernel = ""
		}, "no guest kernel configured"},
		{"managed needs root", func(p *preflightProbes, c *preflightConfig) {
			p.euid = func() int { return 1000 }
			c.egressMode = egressManaged
		}, "needs root"},
		{"managed needs iptables", func(p *preflightProbes, c *preflightConfig) {
			c.egressMode = egressManaged
			p.lookPath = func(name string) (string, error) {
				if name == "iptables" {
					return "", errors.New("not found")
				}
				return "/usr/bin/" + name, nil
			}
		}, `needs "iptables"`},
		{"gpu needs iommu", func(p *preflightProbes, c *preflightConfig) {
			p.iommuActive = func() bool { return false }
			c.gpuCount = 1
		}, "IOMMU"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := okProbes()
			c := readyConfig(t)
			tc.mutate(&p, &c)
			err := p.validate(c)
			if err == nil {
				t.Fatalf("expected preflight to fail for %q", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q should mention %q", err.Error(), tc.want)
			}
		})
	}
}

// TestPreflightReportsAllProblems checks a wholly-unready host lists every problem in
// one error, so an operator fixes them in a single pass.
func TestPreflightReportsAllProblems(t *testing.T) {
	bad := preflightProbes{
		lookPath:     func(string) (string, error) { return "", errors.New("nope") },
		readable:     func(string) error { return errors.New("nope") },
		kvmReadWrite: func() error { return errors.New("nope") },
		cpuVirt:      func() (bool, error) { return false, nil },
		iommuActive:  func() bool { return false },
		euid:         func() int { return 1000 },
	}
	cfg := preflightConfig{chBinary: "cloud-hypervisor", kernel: "/k", rootfs: "/r", egressMode: egressManaged, gpuCount: 2}
	err := bad.validate(cfg)
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, want := range []string{"cloud-hypervisor binary", "/dev/kvm", "hardware virtualization", "guest kernel", "guest rootfs", "needs root", "IOMMU"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("aggregated error missing %q:\n%s", want, err.Error())
		}
	}
}

// TestPreflightConfigFrom maps neutral options and the GPU counts into the config.
func TestPreflightConfigFrom(t *testing.T) {
	cfg := preflightConfigFrom(backend.Options{
		KernelImagePath:       "/k",
		RootfsImagePath:       "/r",
		CloudHypervisorBinary: "/opt/ch",
		GPUPassthrough:        []string{"0000:65:00.0", "0000:b3:00.0"},
		GPUMediatedDevices:    []string{"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"},
	}, egressExternal)
	if cfg.chBinary != "/opt/ch" || cfg.kernel != "/k" || cfg.rootfs != "/r" {
		t.Fatalf("paths not mapped: %+v", cfg)
	}
	if cfg.egressMode != egressExternal || cfg.gpuCount != 3 {
		t.Fatalf("mode/gpuCount not mapped: %+v", cfg)
	}
}
