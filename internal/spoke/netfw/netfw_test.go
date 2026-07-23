package netfw

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

// TestPinnerPinAndExpire checks a pin opens through the programmer and a sweep
// after its TTL revokes it.
func TestPinnerPinAndExpire(t *testing.T) {
	prog := NewRecordingProgrammer()
	now := time.Unix(1000, 0)
	p := NewPinner(prog, func() time.Time { return now })

	if err := p.Pin("web", addr("1.2.3.4"), 30*time.Second); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if !prog.Allowed("web", addr("1.2.3.4")) {
		t.Fatal("IP not opened after Pin")
	}

	// Before expiry, a sweep leaves it.
	now = now.Add(20 * time.Second)
	if err := p.Sweep(); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if !prog.Allowed("web", addr("1.2.3.4")) {
		t.Fatal("IP revoked before its TTL elapsed")
	}

	// After expiry, a sweep revokes it.
	now = now.Add(11 * time.Second)
	if err := p.Sweep(); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if prog.Allowed("web", addr("1.2.3.4")) {
		t.Fatal("IP still open after its TTL elapsed")
	}
}

// TestPinnerRefreshExtends checks re-pinning an open IP extends its deadline
// without re-calling Allow, and keeps it alive past the original TTL.
func TestPinnerRefreshExtends(t *testing.T) {
	prog := &countingProgrammer{RecordingProgrammer: NewRecordingProgrammer()}
	now := time.Unix(1000, 0)
	p := NewPinner(prog, func() time.Time { return now })

	_ = p.Pin("web", addr("1.2.3.4"), 30*time.Second)
	now = now.Add(25 * time.Second)
	_ = p.Pin("web", addr("1.2.3.4"), 30*time.Second) // refresh: new deadline now+30
	if prog.allows != 1 {
		t.Fatalf("Allow called %d times, want 1 (refresh must not re-open)", prog.allows)
	}

	// 10s later the original deadline (1030) has passed but the refreshed one (1055)
	// has not, so the pin survives.
	now = now.Add(10 * time.Second)
	_ = p.Sweep()
	if !prog.Allowed("web", addr("1.2.3.4")) {
		t.Fatal("refreshed pin was revoked early")
	}
}

// TestPinnerSweepKeepsLive checks a sweep never revokes a still-live pin.
func TestPinnerSweepKeepsLive(t *testing.T) {
	prog := NewRecordingProgrammer()
	now := time.Unix(1000, 0)
	p := NewPinner(prog, func() time.Time { return now })
	_ = p.Pin("web", addr("9.9.9.9"), time.Minute)
	if err := p.Sweep(); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if !prog.Allowed("web", addr("9.9.9.9")) {
		t.Fatal("live pin revoked by sweep")
	}
}

// TestPinnerForget checks forgetting a box drops its pins so a later sweep never
// revokes them (the box's rules were torn down wholesale).
func TestPinnerForget(t *testing.T) {
	prog := &countingProgrammer{RecordingProgrammer: NewRecordingProgrammer()}
	now := time.Unix(1000, 0)
	p := NewPinner(prog, func() time.Time { return now })
	_ = p.Pin("web", addr("1.2.3.4"), time.Second)
	p.Forget("web")

	now = now.Add(time.Hour)
	if err := p.Sweep(); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if prog.revokes != 0 {
		t.Fatalf("Revoke called %d times after Forget, want 0", prog.revokes)
	}
}

// TestNFTablesBaselineEmitsRules checks the baseline emits the table, sets, chain,
// the DNS-allow rules, the allow-set rules, and the final drop.
func TestNFTablesBaselineEmitsRules(t *testing.T) {
	var cmds []string
	run := func(_ context.Context, args ...string) error {
		cmds = append(cmds, strings.Join(args, " "))
		return nil
	}
	n := NewNFTables(run)
	spec := BoxSpec{Source: netip.MustParsePrefix("172.16.0.4/30"), DNS: addr("172.16.0.1")}
	if err := n.Baseline("web-box", spec); err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	joined := strings.Join(cmds, "\n")
	for _, want := range []string{
		"add table inet llmbox_isolation",
		"type filter hook forward",
		"udp dport 53 ip daddr 172.16.0.1 accept",
		"172.16.0.4/30 drop",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("baseline commands missing %q; got:\n%s", want, joined)
		}
	}
	// The drop must be emitted after the accepts, or it would shadow them.
	if idxDrop, idxAccept := lastIndex(cmds, "drop"), lastIndex(cmds, "accept"); idxDrop < idxAccept {
		t.Errorf("drop rule (%d) emitted before the last accept (%d)", idxDrop, idxAccept)
	}
}

// TestNFTablesAllowRevokePicksFamily checks Allow/Revoke target the v4 set for an
// IPv4 address and the v6 set for an IPv6 address.
func TestNFTablesAllowRevokePicksFamily(t *testing.T) {
	var last string
	run := func(_ context.Context, args ...string) error {
		last = strings.Join(args, " ")
		return nil
	}
	n := NewNFTables(run)

	_ = n.Allow("web", addr("1.2.3.4"))
	if !strings.Contains(last, "add element inet llmbox_isolation a4_") || !strings.Contains(last, "1.2.3.4") {
		t.Errorf("IPv4 Allow = %q", last)
	}
	_ = n.Allow("web", addr("2606:4700::1111"))
	if !strings.Contains(last, "a6_") || !strings.Contains(last, "2606:4700::1111") {
		t.Errorf("IPv6 Allow = %q", last)
	}
	_ = n.Revoke("web", addr("1.2.3.4"))
	if !strings.Contains(last, "delete element inet llmbox_isolation a4_") {
		t.Errorf("IPv4 Revoke = %q", last)
	}
}

// TestNFTablesTeardownDeletes checks teardown deletes the chain and both sets and
// swallows a benign "No such file or directory".
func TestNFTablesTeardownDeletes(t *testing.T) {
	var cmds []string
	run := func(_ context.Context, args ...string) error {
		cmds = append(cmds, strings.Join(args, " "))
		return nil
	}
	if err := NewNFTables(run).Teardown("web"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	joined := strings.Join(cmds, "\n")
	for _, want := range []string{"delete chain inet llmbox_isolation box_", "delete set inet llmbox_isolation a4_", "delete set inet llmbox_isolation a6_"} {
		if !strings.Contains(joined, want) {
			t.Errorf("teardown missing %q; got:\n%s", want, joined)
		}
	}

	// A missing-object error is ignored.
	miss := func(_ context.Context, _ ...string) error { return errNotFound }
	if err := NewNFTables(miss).Teardown("web"); err != nil {
		t.Errorf("Teardown with missing objects = %v, want nil", err)
	}
}

// errNotFound mimics nft's error when deleting an absent object.
var errNotFound = &nftError{"Error: No such file or directory"}

type nftError struct{ s string }

func (e *nftError) Error() string { return e.s }

// countingProgrammer counts Allow/Revoke calls on top of the recording behaviour.
type countingProgrammer struct {
	*RecordingProgrammer
	allows  int
	revokes int
}

func (c *countingProgrammer) Allow(boxID string, ip netip.Addr) error {
	c.allows++
	return c.RecordingProgrammer.Allow(boxID, ip)
}

func (c *countingProgrammer) Revoke(boxID string, ip netip.Addr) error {
	c.revokes++
	return c.RecordingProgrammer.Revoke(boxID, ip)
}

// lastIndex returns the index of the last command containing sub, or -1.
func lastIndex(cmds []string, sub string) int {
	idx := -1
	for i, c := range cmds {
		if strings.Contains(c, sub) {
			idx = i
		}
	}
	return idx
}
