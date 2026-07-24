// Package mvmnet is the VMM-agnostic microVM egress network layer shared by the
// Firecracker and Cloud Hypervisor backends. A microVM box reaches its control and
// proxy planes over vsock; this package provides the other half — outbound (egress)
// connectivity — as a pool of pre-created host TAP devices plus shared NAT and
// inter-guest isolation rules.
//
// The pool is provisioned once (EnsurePool) and reused across boxes, so creating or
// destroying a box never adds, removes, or reconfigures a host interface (a same-host
// browser aborts in-flight requests with ERR_NETWORK_CHANGED whenever an interface
// appears mid-request; a stable interface set avoids that).
//
// A Config parameterises the addressing so two backends can run without colliding:
// each picks its own TAP-name prefix and /16 base (Firecracker keeps its historical
// "llmboxfc" / 172.16, Cloud Hypervisor uses "llmboxch" / 172.17). The logic is
// otherwise identical, which is the point of sharing it.
package mvmnet

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// MaxIndex bounds the pool slots: <base>.<0..255>.0/30 gives 256 isolated subnets.
// The actual pool size is the smaller, configured value.
const MaxIndex = 255

// Config parameterises a backend's egress addressing and host plumbing. Two backends
// with distinct TapPrefix and SubnetBase share a host without colliding.
type Config struct {
	// TapPrefix is the host TAP device name prefix (e.g. "llmboxfc"). It must be
	// short enough that TapPrefix+index stays within the 15-char interface-name limit.
	TapPrefix string
	// SubnetBase is the first two octets of the guest address space (e.g. "172.16"),
	// so box slot i uses <SubnetBase>.<i>.0/30 and the aggregate is
	// <SubnetBase>.0.0/16.
	SubnetBase string
	// TapGroup is the GID the pooled TAP devices are owned by, so an unprivileged
	// (e.g. jailed) VMM running under that group can open its TAP without
	// CAP_NET_ADMIN. 0 leaves the TAP root-owned.
	TapGroup int
	// Uplink is the host interface guest traffic is masqueraded out of; empty resolves
	// the default-route interface.
	Uplink string
}

// aggregateSubnet returns the /16 the NAT and isolation rules cover; every box's /30
// falls inside it.
//
// @return string The aggregate guest subnet in CIDR form (e.g. "172.16.0.0/16").
//
// @testcase TestNetFor derives addresses inside the aggregate subnet.
func (c Config) aggregateSubnet() string { return c.SubnetBase + ".0.0/16" }

// BoxNet is the per-box network addressing derived from a small integer slot. Each
// box gets its own /30 (network, host TAP, guest, broadcast) on its own TAP, so boxes
// are on separate subnets and — with the inter-guest DROP rule the pool installs —
// cannot reach one another, only the host gateway that NATs them out.
type BoxNet struct {
	// Index is the pool slot the addressing is derived from.
	Index int
	// TapName is the pre-created host TAP device backing the guest NIC.
	TapName string
	// HostIP is the host-side gateway address on the TAP (the guest's default route).
	HostIP string
	// GuestIP is the address assigned to the guest's eth0 via the kernel ip= arg.
	GuestIP string
	// Netmask is the /30 mask shared by HostIP and GuestIP.
	Netmask string
}

// TapName is the deterministic pool TAP device name for a slot.
//
// @arg index The pool slot.
// @return string The TAP device name for the slot.
//
// @testcase TestNetFor derives the pool TAP name for a slot.
func (c Config) TapName(index int) string { return fmt.Sprintf("%s%d", c.TapPrefix, index) }

// NetFor derives the addressing for pool slot index: HostIP <base>.index.1 and
// GuestIP <base>.index.2 in a /30, on the slot's pre-created TAP.
//
// @arg index The pool slot (0..MaxIndex).
// @return BoxNet The slot's TAP name and host/guest addresses.
//
// @testcase TestNetFor derives distinct, well-formed /30s for different slots.
func (c Config) NetFor(index int) BoxNet {
	return BoxNet{
		Index:   index,
		TapName: c.TapName(index),
		HostIP:  fmt.Sprintf("%s.%d.1", c.SubnetBase, index),
		GuestIP: fmt.Sprintf("%s.%d.2", c.SubnetBase, index),
		Netmask: "255.255.255.252",
	}
}

// MACForIndex derives a deterministic locally-administered MAC for a box slot, so a
// box's NIC address is stable across boots without a global allocator.
//
// @arg index The pool slot.
// @return string The locally-administered unicast MAC for the slot.
//
// @testcase TestMACForIndex derives a stable locally-administered MAC per slot.
func MACForIndex(index int) string {
	// 02:.. is a locally-administered, unicast prefix; the low bytes carry the index.
	return fmt.Sprintf("02:00:00:00:%02x:%02x", (index>>8)&0xff, index&0xff)
}

// KernelIPArg renders the Linux kernel `ip=` boot argument that statically configures
// the guest eth0 from this addressing, with autoconf off (no DHCP).
//
// @return string The `ip=guest::gateway:mask::eth0:off` kernel argument.
//
// @testcase TestKernelIPArg renders the guest ip= argument from the addressing.
func (n BoxNet) KernelIPArg() string {
	return fmt.Sprintf("ip=%s::%s:%s::eth0:off", n.GuestIP, n.HostIP, n.Netmask)
}

// EgressMode selects who owns the host-side TAP/NAT egress plumbing, decoupling "does
// the guest get an egress NIC" from "does the long-running spoke mutate host
// networking". It lets the privileged, CAP_NET_ADMIN work move out of the spoke into
// a boot-time provisioning step, so the spoke can attach to a pre-provisioned pool
// without ever running sysctl/ip/iptables.
type EgressMode int

const (
	// EgressManaged is the default: the spoke itself creates the TAP pool and NAT
	// rules at startup (EnsurePool) and therefore needs CAP_NET_ADMIN / root.
	EgressManaged EgressMode = iota
	// EgressExternal gives the guest an egress NIC but the spoke never mutates host
	// networking: it attaches to a pre-provisioned pool and only validates (read-only)
	// that each required TAP exists.
	EgressExternal
	// EgressDisabled boots control-only boxes (loopback + vsock, no TAP/NAT).
	EgressDisabled
)

// egressModeNames maps each mode to its --egress-mode flag spelling.
var egressModeNames = map[EgressMode]string{
	EgressManaged:  "managed",
	EgressExternal: "external",
	EgressDisabled: "disabled",
}

// String renders the mode's flag spelling.
//
// @return string The flag spelling ("managed", "external", or "disabled").
//
// @testcase TestParseEgressMode round-trips every mode through its name.
func (m EgressMode) String() string { return egressModeNames[m] }

// ParseEgressMode resolves an --egress-mode value (case-insensitive), treating the
// empty string as the default managed mode so an unset flag keeps default behaviour.
//
// @arg s The flag value ("managed", "external", "disabled", or empty).
// @return EgressMode The parsed mode.
// @error error if s names no known mode.
//
// @testcase TestParseEgressMode accepts every mode and rejects an unknown one.
func ParseEgressMode(s string) (EgressMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "managed":
		return EgressManaged, nil
	case "external":
		return EgressExternal, nil
	case "disabled":
		return EgressDisabled, nil
	default:
		return EgressManaged, fmt.Errorf("invalid egress mode %q (want managed, external, or disabled)", s)
	}
}

// Egress provisions and tears down the pool of TAP devices and NAT rules that give
// boxes outbound connectivity. It is an interface so a provisioner can be unit-tested
// with a fake that records calls instead of touching the host stack.
type Egress interface {
	// EnsurePool idempotently creates `size` TAP devices, each with its host gateway
	// address up, plus the shared NAT and inter-guest isolation rules.
	EnsurePool(ctx context.Context, size int) error
	// TeardownPool removes the `size` pool TAP devices and the shared rules
	// (best-effort).
	TeardownPool(ctx context.Context, size int) error
	// ValidatePool read-only checks that each of the `size` pool TAP devices exists
	// and is up, without creating or mutating anything.
	ValidatePool(ctx context.Context, size int) error
}

// HostEgress is the real Egress that shells out to ip(8) and iptables(8). It requires
// CAP_NET_ADMIN (typically root) and enables IPv4 forwarding.
type HostEgress struct {
	cfg Config
	mu  sync.Mutex // serialises iptables mutations, which are not concurrency-safe
}

// NewHostEgress builds a HostEgress for cfg (TAP prefix, subnet base, owning GID,
// uplink).
//
// @arg cfg The egress addressing/plumbing config.
// @return *HostEgress A host egress ready to provision or validate the pool.
//
// @testcase TestHostEgressValidatePool builds a host egress and validates a pool.
func NewHostEgress(cfg Config) *HostEgress { return &HostEgress{cfg: cfg} }

// SetTapGroup updates the GID the pool TAPs are created under before EnsurePool, so a
// provisioner can pin the group to its jailer identity resolved after construction.
//
// @arg gid The owning GID for created TAP devices.
//
// @testcase TestHostEgressValidatePool sets the TAP group before provisioning.
func (e *HostEgress) SetTapGroup(gid int) { e.cfg.TapGroup = gid }

// EnsurePool creates the TAP pool and installs the shared NAT/isolation rules,
// idempotently, so it is safe to call on every startup (including after an unclean
// shutdown left some devices behind).
//
// @arg ctx Context for the ip/iptables invocations.
// @arg size The number of TAP slots to provision.
// @error error if forwarding cannot be enabled, the uplink cannot be resolved, or a TAP/rule cannot be created.
//
// @testcase TestHostEgressPoolSkipsWithoutRoot is skipped when not root / tools absent.
func (e *HostEgress) EnsurePool(ctx context.Context, size int) error {
	uplink, err := e.resolveUplink(ctx)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	if err := run(ctx, "sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return fmt.Errorf("enabling ip forwarding: %w", err)
	}
	for i := 0; i < size; i++ {
		if err := e.ensureTap(ctx, e.cfg.NetFor(i)); err != nil {
			return err
		}
	}
	for _, r := range e.sharedRules(uplink) {
		if err := e.ensureRule(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// sharedRules returns the aggregate NAT/isolation rules (they cover the whole guest
// subnet, so they don't grow with the pool): masquerade guest egress, drop
// guest-to-guest, allow guest→uplink and established returns.
//
// @arg uplink The resolved uplink interface.
// @return [][]string The iptables rule specs.
//
// @testcase TestHostEgressPoolSkipsWithoutRoot is skipped when not root / tools absent.
func (e *HostEgress) sharedRules(uplink string) [][]string {
	sub := e.cfg.aggregateSubnet()
	return [][]string{
		{"-t", "nat", "POSTROUTING", "-s", sub, "-o", uplink, "-j", "MASQUERADE"},
		{"FORWARD", "-s", sub, "-d", sub, "-j", "DROP"},
		{"FORWARD", "-s", sub, "-o", uplink, "-j", "ACCEPT"},
		{"FORWARD", "-d", sub, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	}
}

// ensureTap creates the slot's TAP (if absent), assigns its host address, and brings
// it up. Each step tolerates "already exists" so the call is idempotent.
//
// @arg ctx Context for the ip invocations.
// @arg n The slot addressing.
// @error error if the TAP cannot be created, addressed, or brought up.
//
// @testcase TestHostEgressPoolSkipsWithoutRoot is skipped when not root / tools absent.
func (e *HostEgress) ensureTap(ctx context.Context, n BoxNet) error {
	if run(ctx, "ip", "link", "show", n.TapName) != nil {
		add := []string{"ip", "tuntap", "add", "dev", n.TapName, "mode", "tap"}
		if e.cfg.TapGroup > 0 {
			add = append(add, "group", fmt.Sprintf("%d", e.cfg.TapGroup))
		}
		if err := run(ctx, add...); err != nil {
			return fmt.Errorf("creating tap %s: %w", n.TapName, err)
		}
	}
	// addr add is a no-op-with-error when the address already exists; ignore that.
	_ = run(ctx, "ip", "addr", "add", n.HostIP+"/30", "dev", n.TapName)
	if err := run(ctx, "ip", "link", "set", "dev", n.TapName, "up"); err != nil {
		return fmt.Errorf("bringing up tap %s: %w", n.TapName, err)
	}
	return nil
}

// ensureRule installs one iptables rule only if an identical one is not already
// present (checked with -C), so EnsurePool never appends duplicates across restarts.
//
// @arg ctx Context for the iptables invocations.
// @arg rule The rule spec (chain and match/target args, optionally led by "-t <table>").
// @error error if the rule cannot be added.
//
// @testcase TestHostEgressPoolSkipsWithoutRoot is skipped when not root / tools absent.
func (e *HostEgress) ensureRule(ctx context.Context, rule []string) error {
	check := insertVerb(append([]string{"iptables"}, rule...), ruleInsertAt(rule), "-C")
	add := insertVerb(append([]string{"iptables"}, rule...), ruleInsertAt(rule), "-A")
	if run(ctx, check...) == nil {
		return nil // already present
	}
	if err := run(ctx, add...); err != nil {
		return fmt.Errorf("adding iptables rule %q: %w", strings.Join(rule, " "), err)
	}
	return nil
}

// ValidatePool checks, read-only, that every pool TAP an external provisioner was
// meant to create exists and is up. It never runs mutating commands, so it is safe
// from an unprivileged spoke; it fails closed with an actionable error naming the
// missing or down devices.
//
// @arg ctx Context for the ip invocations.
// @arg size The number of pool TAP slots that must be present.
// @error error listing every TAP slot that is missing or not up.
//
// @testcase TestHostEgressValidatePool reports the missing pool TAPs.
func (e *HostEgress) ValidatePool(ctx context.Context, size int) error {
	var missing []string
	for i := 0; i < size; i++ {
		name := e.cfg.TapName(i)
		out, err := exec.CommandContext(ctx, "ip", "-o", "link", "show", name).CombinedOutput()
		if err != nil {
			missing = append(missing, fmt.Sprintf("%s (not found)", name))
			continue
		}
		// Check the administrative UP flag, NOT the operational `state`: a correctly
		// provisioned TAP with no VMM attached yet is admin-UP but carrier-DOWN, so
		// keying off state would wrongly reject a valid pool.
		if !linkFlagsUp(string(out)) {
			missing = append(missing, fmt.Sprintf("%s (administratively down)", name))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("externally managed egress pool is incomplete; provision it first: %s", strings.Join(missing, ", "))
	}
	return nil
}

// TeardownPool removes the pool TAPs and the shared rules, best-effort.
//
// @arg ctx Context for the ip/iptables invocations.
// @arg size The number of TAP slots to remove.
// @error error is always nil; failures are best-effort and swallowed.
//
// @testcase TestHostEgressPoolSkipsWithoutRoot is skipped when not root / tools absent.
func (e *HostEgress) TeardownPool(ctx context.Context, size int) error {
	uplink, _ := e.resolveUplink(ctx)
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range e.sharedRules(uplink) {
		del := insertVerb(append([]string{"iptables"}, r...), ruleInsertAt(r), "-D")
		_ = run(ctx, del...)
	}
	for i := 0; i < size; i++ {
		_ = run(ctx, "ip", "link", "del", e.cfg.TapName(i))
	}
	return nil
}

// resolveUplink returns the configured uplink, or the default-route interface when
// none was configured.
//
// @arg ctx Context for the ip invocation.
// @return string The uplink interface name.
// @error error if the default route cannot be resolved.
//
// @testcase TestHostEgressPoolSkipsWithoutRoot is skipped when not root / tools absent.
func (e *HostEgress) resolveUplink(ctx context.Context) (string, error) {
	if e.cfg.Uplink != "" {
		return e.cfg.Uplink, nil
	}
	out, err := exec.CommandContext(ctx, "ip", "route", "show", "default").Output()
	if err != nil {
		return "", fmt.Errorf("resolving default uplink: %w", err)
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", fmt.Errorf("no default-route interface found in %q", strings.TrimSpace(string(out)))
}

// ruleInsertAt returns the index to splice an iptables verb into a rule spec, after an
// optional "-t <table>" prefix.
//
// @arg rule The rule spec.
// @return int The insert index (1, or 3 when the rule leads with -t <table>).
//
// @testcase TestInsertVerb covers the table-prefix offset.
func ruleInsertAt(rule []string) int {
	if len(rule) >= 2 && rule[0] == "-t" {
		return 3
	}
	return 1
}

// insertVerb returns args with verb spliced in at position i.
//
// @arg args The base argv.
// @arg i The index to insert at.
// @arg verb The verb to insert (e.g. "-C" or "-A").
// @return []string The argv with verb inserted.
//
// @testcase TestInsertVerb splices the verb after an optional table prefix.
func insertVerb(args []string, i int, verb string) []string {
	out := make([]string, 0, len(args)+1)
	out = append(out, args[:i]...)
	out = append(out, verb)
	out = append(out, args[i:]...)
	return out
}

// linkFlagsUp reports whether an `ip -o link show` line carries the administrative UP
// flag in its `<...>` flag block.
//
// @arg line One `ip -o link show` output line.
// @return bool Whether the UP flag is present.
//
// @testcase TestLinkFlagsUp reads the UP flag from the interface flag block.
func linkFlagsUp(line string) bool {
	lt := strings.IndexByte(line, '<')
	gt := strings.IndexByte(line, '>')
	if lt < 0 || gt < lt {
		return false
	}
	for _, f := range strings.Split(line[lt+1:gt], ",") {
		if f == "UP" {
			return true
		}
	}
	return false
}

// run executes a command, wrapping a non-zero exit with its combined output so
// failures carry the tool's own diagnostics.
//
// @arg ctx Context for the command.
// @arg args The command and its arguments.
// @error error if the command cannot start or exits non-zero (with its output).
//
// @testcase TestRunReportsFailure returns the command output on a non-zero exit.
func run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
