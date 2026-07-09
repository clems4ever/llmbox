package firecracker

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// boxNet is the per-box network addressing derived from a small integer slot. Each
// box gets its own /30 (network, host TAP, guest, broadcast) on its own TAP, so
// boxes are on separate subnets and — with the inter-guest DROP rule the pool
// installs — cannot reach one another, only the host gateway that NATs them out.
// The guest handles control and proxy over vsock; this networking exists solely
// for the guest's outbound (egress) traffic.
//
// The TAP devices are NOT created per box: a pool of them is provisioned once at
// startup (EnsurePool) and reused, so creating or destroying a box never adds,
// removes, or reconfigures a host interface. That matters because a browser on the
// same host aborts in-flight requests with ERR_NETWORK_CHANGED whenever a network
// interface appears mid-request; keeping the interface set stable across a box's
// lifetime avoids it.
type boxNet struct {
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

const (
	// maxBoxNetIndex bounds the pool slots: 172.16.<0..255>.0/30 gives 256 isolated
	// subnets. The actual pool size is the smaller, configured value.
	maxBoxNetIndex = 255
	// guestSubnet is the aggregate the NAT and isolation rules cover; every box's
	// /30 falls inside it.
	guestSubnet = "172.16.0.0/16"
)

// tapName is the deterministic pool TAP device name for a slot. It stays within
// the 15-char interface-name limit (llmboxfc255 is 11 chars).
//
// @arg index The pool slot.
// @return string The TAP device name for the slot.
//
// @testcase TestNetFor derives the pool TAP name for a slot.
func tapName(index int) string { return fmt.Sprintf("llmboxfc%d", index) }

// netFor derives the addressing for pool slot index: HostIP 172.16.index.1 and
// GuestIP 172.16.index.2 in a /30, on the slot's pre-created TAP.
//
// @arg index The pool slot (0..maxBoxNetIndex).
// @return boxNet The slot's TAP name and host/guest addresses.
//
// @testcase TestNetFor derives distinct, well-formed /30s for different slots.
func netFor(index int) boxNet {
	return boxNet{
		Index:   index,
		TapName: tapName(index),
		HostIP:  fmt.Sprintf("172.16.%d.1", index),
		GuestIP: fmt.Sprintf("172.16.%d.2", index),
		Netmask: "255.255.255.252",
	}
}

// kernelIPArg renders the Linux kernel `ip=` boot argument that statically
// configures the guest eth0 from this addressing, with autoconf off (no DHCP).
//
// @return string The `ip=guest::gateway:mask::eth0:off` kernel argument.
//
// @testcase TestKernelIPArg renders the guest ip= argument from the addressing.
func (n boxNet) kernelIPArg() string {
	return fmt.Sprintf("ip=%s::%s:%s::eth0:off", n.GuestIP, n.HostIP, n.Netmask)
}

// egress provisions and tears down the pool of TAP devices and NAT rules that give
// boxes outbound connectivity. It is an interface so the provisioner can be
// unit-tested with a fake that records calls instead of touching the host stack.
type egress interface {
	// EnsurePool idempotently creates `size` TAP devices (llmboxfc0..size-1), each
	// with its host gateway address up, plus the shared NAT and inter-guest
	// isolation rules. It is called once, at startup, so no interface changes during
	// a box create/destroy.
	EnsurePool(ctx context.Context, size int) error
	// TeardownPool removes the `size` pool TAP devices and the shared rules. It is
	// best-effort and called at shutdown.
	TeardownPool(ctx context.Context, size int) error
}

// hostEgress is the real egress that shells out to ip(8) and iptables(8). It
// requires CAP_NET_ADMIN (typically root) and enables IPv4 forwarding.
type hostEgress struct {
	// uplink is the host interface guest traffic is masqueraded out of; empty means
	// "resolve the default-route interface".
	uplink string
	mu     sync.Mutex // serialises iptables mutations, which are not concurrency-safe
}

// EnsurePool creates the TAP pool and installs the shared NAT/isolation rules,
// idempotently, so it is safe to call on every startup (including after an unclean
// shutdown left some devices behind).
//
// @arg ctx Context for the ip/iptables invocations.
// @arg size The number of TAP slots to provision.
// @error error if forwarding cannot be enabled, the uplink cannot be resolved, or a TAP/rule cannot be created.
//
// @testcase TestHostEgressPoolSkipsWithoutRoot is skipped when not root / tools absent.
func (e *hostEgress) EnsurePool(ctx context.Context, size int) error {
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
		n := netFor(i)
		if err := e.ensureTap(ctx, n); err != nil {
			return err
		}
	}
	// Shared rules (aggregate over the whole guest subnet, so they don't grow with
	// the pool): masquerade guest egress, drop guest-to-guest, allow guest→uplink
	// and established returns.
	rules := [][]string{
		{"-t", "nat", "POSTROUTING", "-s", guestSubnet, "-o", uplink, "-j", "MASQUERADE"},
		{"FORWARD", "-s", guestSubnet, "-d", guestSubnet, "-j", "DROP"},
		{"FORWARD", "-s", guestSubnet, "-o", uplink, "-j", "ACCEPT"},
		{"FORWARD", "-d", guestSubnet, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	}
	for _, r := range rules {
		if err := e.ensureRule(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// ensureTap creates the slot's TAP (if absent), assigns its host address, and
// brings it up. Each step tolerates "already exists" so the call is idempotent.
//
// @arg ctx Context for the ip invocations.
// @arg n The slot addressing.
// @error error if the TAP cannot be created, addressed, or brought up.
//
// @testcase TestHostEgressPoolSkipsWithoutRoot is skipped when not root / tools absent.
func (e *hostEgress) ensureTap(ctx context.Context, n boxNet) error {
	if run(ctx, "ip", "link", "show", n.TapName) != nil {
		if err := run(ctx, "ip", "tuntap", "add", "dev", n.TapName, "mode", "tap"); err != nil {
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
// present (checked with -C), so EnsurePool never appends duplicates across
// restarts.
//
// @arg ctx Context for the iptables invocations.
// @arg rule The rule spec (chain and match/target args, optionally led by "-t <table>").
// @error error if the rule cannot be added.
//
// @testcase TestHostEgressPoolSkipsWithoutRoot is skipped when not root / tools absent.
func (e *hostEgress) ensureRule(ctx context.Context, rule []string) error {
	check := append([]string{"iptables"}, rule...)
	add := append([]string{"iptables"}, rule...)
	// Splice the -C / -A verb in after an optional "-t <table>" prefix.
	insertAt := 1
	if len(rule) >= 2 && rule[0] == "-t" {
		insertAt = 3
	}
	check = insertVerb(check, insertAt, "-C")
	add = insertVerb(add, insertAt, "-A")
	if run(ctx, check...) == nil {
		return nil // already present
	}
	if err := run(ctx, add...); err != nil {
		return fmt.Errorf("adding iptables rule %q: %w", strings.Join(rule, " "), err)
	}
	return nil
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

// TeardownPool removes the pool TAPs and the shared rules, best-effort.
//
// @arg ctx Context for the ip/iptables invocations.
// @arg size The number of TAP slots to remove.
// @error error is always nil; failures are best-effort and swallowed.
//
// @testcase TestHostEgressPoolSkipsWithoutRoot is skipped when not root / tools absent.
func (e *hostEgress) TeardownPool(ctx context.Context, size int) error {
	uplink, _ := e.resolveUplink(ctx)
	e.mu.Lock()
	defer e.mu.Unlock()
	rules := [][]string{
		{"-t", "nat", "POSTROUTING", "-s", guestSubnet, "-o", uplink, "-j", "MASQUERADE"},
		{"FORWARD", "-s", guestSubnet, "-d", guestSubnet, "-j", "DROP"},
		{"FORWARD", "-s", guestSubnet, "-o", uplink, "-j", "ACCEPT"},
		{"FORWARD", "-d", guestSubnet, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	}
	for _, r := range rules {
		del := insertVerb(append([]string{"iptables"}, r...), delInsertAt(r), "-D")
		_ = run(ctx, del...)
	}
	for i := 0; i < size; i++ {
		_ = run(ctx, "ip", "link", "del", tapName(i))
	}
	return nil
}

// delInsertAt returns the index to splice "-D" into a rule spec, after an optional
// "-t <table>" prefix.
//
// @arg rule The rule spec.
// @return int The insert index (1, or 3 when the rule leads with -t <table>).
//
// @testcase TestInsertVerb covers the table-prefix offset.
func delInsertAt(rule []string) int {
	if len(rule) >= 2 && rule[0] == "-t" {
		return 3
	}
	return 1
}

// resolveUplink returns the configured uplink, or the default-route interface when
// none was configured.
//
// @arg ctx Context for the ip invocation.
// @return string The uplink interface name.
// @error error if the default route cannot be resolved.
//
// @testcase TestHostEgressPoolSkipsWithoutRoot is skipped when not root / tools absent.
func (e *hostEgress) resolveUplink(ctx context.Context) (string, error) {
	if e.uplink != "" {
		return e.uplink, nil
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
