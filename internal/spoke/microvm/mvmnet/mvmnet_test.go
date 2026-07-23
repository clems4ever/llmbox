package mvmnet

import (
	"context"
	"os"
	"strings"
	"testing"
)

// fcCfg / chCfg mirror the two backends' addressing so the tests prove they don't
// collide.
var (
	fcCfg = Config{TapPrefix: "llmboxfc", SubnetBase: "172.16"}
	chCfg = Config{TapPrefix: "llmboxch", SubnetBase: "172.17"}
)

// TestNetFor derives distinct, well-formed /30s per slot, and shows the two backend
// configs occupy different TAP names and subnets.
func TestNetFor(t *testing.T) {
	n := fcCfg.NetFor(5)
	if n.TapName != "llmboxfc5" || n.HostIP != "172.16.5.1" || n.GuestIP != "172.16.5.2" || n.Netmask != "255.255.255.252" {
		t.Fatalf("fc NetFor(5) = %+v", n)
	}
	c := chCfg.NetFor(5)
	if c.TapName != "llmboxch5" || c.HostIP != "172.17.5.1" {
		t.Fatalf("ch NetFor(5) = %+v", c)
	}
	if fcCfg.aggregateSubnet() != "172.16.0.0/16" || chCfg.aggregateSubnet() != "172.17.0.0/16" {
		t.Fatalf("aggregate subnets collide: %s %s", fcCfg.aggregateSubnet(), chCfg.aggregateSubnet())
	}
}

// TestKernelIPArg renders the guest ip= argument from the addressing.
func TestKernelIPArg(t *testing.T) {
	got := fcCfg.NetFor(3).KernelIPArg()
	want := "ip=172.16.3.2::172.16.3.1:255.255.255.252::eth0:off"
	if got != want {
		t.Fatalf("KernelIPArg = %q, want %q", got, want)
	}
}

// TestMACForIndex derives a stable, locally-administered, unique MAC per slot.
func TestMACForIndex(t *testing.T) {
	if got := MACForIndex(1); got != "02:00:00:00:00:01" {
		t.Errorf("MACForIndex(1) = %q", got)
	}
	if got := MACForIndex(258); got != "02:00:00:00:01:02" {
		t.Errorf("MACForIndex(258) = %q", got)
	}
	if MACForIndex(1) == MACForIndex(2) {
		t.Error("MACs must differ per slot")
	}
}

// TestParseEgressMode accepts every mode (and empty=managed) and rejects an unknown
// one, round-tripping through String.
func TestParseEgressMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want EgressMode
	}{{"", EgressManaged}, {"managed", EgressManaged}, {"EXTERNAL", EgressExternal}, {" disabled ", EgressDisabled}} {
		got, err := ParseEgressMode(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseEgressMode(%q) = %v, %v", tc.in, got, err)
		}
	}
	if _, err := ParseEgressMode("bridged"); err == nil {
		t.Error("ParseEgressMode should reject an unknown mode")
	}
	if EgressExternal.String() != "external" {
		t.Errorf("String = %q", EgressExternal.String())
	}
}

// TestInsertVerb splices the verb after an optional table prefix.
func TestInsertVerb(t *testing.T) {
	got := insertVerb([]string{"iptables", "FORWARD", "-j", "DROP"}, ruleInsertAt([]string{"FORWARD", "-j", "DROP"}), "-A")
	if strings.Join(got, " ") != "iptables -A FORWARD -j DROP" {
		t.Errorf("no-table insert = %v", got)
	}
	rule := []string{"-t", "nat", "POSTROUTING", "-j", "MASQUERADE"}
	got = insertVerb(append([]string{"iptables"}, rule...), ruleInsertAt(rule), "-C")
	if strings.Join(got, " ") != "iptables -t nat -C POSTROUTING -j MASQUERADE" {
		t.Errorf("table insert = %v", got)
	}
}

// TestLinkFlagsUp reads the administrative UP flag from an interface flag block.
func TestLinkFlagsUp(t *testing.T) {
	if !linkFlagsUp("2: llmboxfc0: <BROADCAST,MULTICAST,UP> mtu 1500") {
		t.Error("should detect UP")
	}
	if linkFlagsUp("2: llmboxfc0: <BROADCAST,MULTICAST> mtu 1500") {
		t.Error("should not detect UP when absent")
	}
	if linkFlagsUp("no brackets here") {
		t.Error("malformed line should be not-up")
	}
}

// TestRunReportsFailure returns the command's output on a non-zero exit.
func TestRunReportsFailure(t *testing.T) {
	err := run(context.Background(), "sh", "-c", "echo boom >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("run should carry the command output: %v", err)
	}
}

// TestHostEgressValidatePool reports the missing pool TAPs (there are none on the
// test host, so a size-2 pool is entirely missing).
func TestHostEgressValidatePool(t *testing.T) {
	e := NewHostEgress(Config{TapPrefix: "llmboxtest-absent-", SubnetBase: "172.31"})
	e.SetTapGroup(1234)
	err := e.ValidatePool(context.Background(), 2)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("ValidatePool should report missing TAPs: %v", err)
	}
}

// TestHostEgressPoolSkipsWithoutRoot exercises EnsurePool only as root (it mutates
// host networking); otherwise it is skipped so the suite runs unprivileged.
func TestHostEgressPoolSkipsWithoutRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("EnsurePool mutates host networking; run as root to exercise it")
	}
	e := NewHostEgress(Config{TapPrefix: "llmboxtest", SubnetBase: "172.30", TapGroup: 0})
	ctx := context.Background()
	if err := e.EnsurePool(ctx, 1); err != nil {
		t.Fatalf("EnsurePool: %v", err)
	}
	t.Cleanup(func() { _ = e.TeardownPool(ctx, 1) })
}
