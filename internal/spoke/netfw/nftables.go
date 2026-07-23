package netfw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
)

// tableName is the dedicated nftables table (inet family, so one table covers
// both IPv4 and IPv6) that holds every box's isolation rules. Keeping it separate
// from any host ruleset means Teardown/flush never touches unrelated firewalling.
const tableName = "llmbox_isolation"

// Runner runs an `nft` invocation. It is injected so the nftables Programmer can
// be unit-tested by capturing the commands it emits instead of mutating a real
// firewall (which needs root). The default runner execs the nft binary.
type Runner func(ctx context.Context, args ...string) error

// execRunner runs nft for real via os/exec, surfacing its stderr on failure.
//
// @arg ctx Cancels the invocation.
// @arg args The nft arguments.
// @error error if nft exits non-zero (its output is included).
func execRunner(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "nft", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nft %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// NFTables is a Programmer backed by the `nft` command. Each box gets, in the
// shared inet table, two named sets (`a4_<h>` for IPv4 and `a6_<h>` for IPv6
// destinations, `<h>` a hash of the box id) and rules in a per-box chain that,
// for packets from the box's source prefix, accept DNS to the resolver and
// traffic to a pinned destination, and drop everything else. Allow/Revoke just
// add/remove set elements, so opening or closing a resolved IP is a single set
// operation rather than a rule rebuild.
//
// The struct is safe for concurrent use; nft itself serialises ruleset changes,
// and a mutex here keeps the one-time table creation race-free.
type NFTables struct {
	run Runner

	mu           sync.Mutex
	tableEnsured bool
}

// NewNFTables builds an nftables Programmer. Pass nil to exec the real nft
// binary; tests pass a capturing Runner.
//
// @arg run The nft runner, or nil for the real exec runner.
// @return *NFTables A ready programmer.
//
// @testcase TestNFTablesBaselineEmitsRules records the commands a baseline emits.
func NewNFTables(run Runner) *NFTables {
	if run == nil {
		run = execRunner
	}
	return &NFTables{run: run}
}

// boxHash derives a short, nft-identifier-safe token from a box id (nft names are
// limited and can't hold arbitrary characters), so two boxes never collide on a
// set or chain name.
//
// @arg boxID The box id.
// @return string A 12-hex-char token derived from the id.
func boxHash(boxID string) string {
	sum := sha256.Sum256([]byte(boxID))
	return hex.EncodeToString(sum[:])[:12]
}

func chainName(boxID string) string { return "box_" + boxHash(boxID) }
func set4Name(boxID string) string  { return "a4_" + boxHash(boxID) }
func set6Name(boxID string) string  { return "a6_" + boxHash(boxID) }

// ensureTable creates the shared inet table once. It is idempotent (`add table`
// is a no-op if the table exists), guarded so concurrent Baselines create it once.
//
// @arg ctx Cancels the invocation.
// @error error if creating the table fails.
func (n *NFTables) ensureTable(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.tableEnsured {
		return nil
	}
	if err := n.run(ctx, "add", "table", "inet", tableName); err != nil {
		return err
	}
	n.tableEnsured = true
	return nil
}

// Baseline creates the box's sets and chain and its deny-by-default rules.
//
// @arg boxID The box.
// @arg spec The box's source prefix and DNS resolver.
// @error error if any nft command fails.
//
// @testcase TestNFTablesBaselineEmitsRules asserts the emitted set/chain/rule commands.
func (n *NFTables) Baseline(boxID string, spec BoxSpec) error {
	ctx := context.Background()
	if err := n.ensureTable(ctx); err != nil {
		return err
	}
	chain, s4, s6 := chainName(boxID), set4Name(boxID), set6Name(boxID)
	src := spec.Source.String()
	dns := spec.DNS.String()
	// A hook chain per box keeps its rules isolated and teardown a single delete.
	cmds := [][]string{
		{"add", "set", "inet", tableName, s4, "{ type ipv4_addr; flags interval; }"},
		{"add", "set", "inet", tableName, s6, "{ type ipv6_addr; flags interval; }"},
		{"add", "chain", "inet", tableName, chain,
			"{ type filter hook forward priority 0; policy accept; }"},
		// Only this box's source traffic is judged by the rules below; other
		// traffic falls through the accept policy untouched.
		{"add", "rule", "inet", tableName, chain, "ip", "saddr", src, "udp", "dport", "53", "ip", "daddr", dns, "accept"},
		{"add", "rule", "inet", tableName, chain, "ip", "saddr", src, "tcp", "dport", "53", "ip", "daddr", dns, "accept"},
		{"add", "rule", "inet", tableName, chain, "ip", "saddr", src, "ip", "daddr", "@" + s4, "accept"},
		{"add", "rule", "inet", tableName, chain, "ip6", "saddr", src, "ip6", "daddr", "@" + s6, "accept"},
		// Everything else from the box is dropped: deny-by-default egress.
		{"add", "rule", "inet", tableName, chain, "ip", "saddr", src, "drop"},
	}
	for _, c := range cmds {
		if err := n.run(ctx, c...); err != nil {
			return err
		}
	}
	return nil
}

// Allow adds ip to the box's allow set (IPv4 or IPv6 depending on the address).
//
// @arg boxID The box.
// @arg ip The resolved IP to open.
// @error error if the nft add-element fails.
//
// @testcase TestNFTablesAllowRevokePicksFamily adds to the right family set.
func (n *NFTables) Allow(boxID string, ip netip.Addr) error {
	set := set4Name(boxID)
	if ip.Is6() {
		set = set6Name(boxID)
	}
	return n.run(context.Background(), "add", "element", "inet", tableName, set, "{ "+ip.String()+" }")
}

// Revoke removes ip from the box's allow set.
//
// @arg boxID The box.
// @arg ip The IP to close.
// @error error if the nft delete-element fails.
//
// @testcase TestNFTablesAllowRevokePicksFamily deletes from the right family set.
func (n *NFTables) Revoke(boxID string, ip netip.Addr) error {
	set := set4Name(boxID)
	if ip.Is6() {
		set = set6Name(boxID)
	}
	return n.run(context.Background(), "delete", "element", "inet", tableName, set, "{ "+ip.String()+" }")
}

// Teardown deletes the box's chain and sets. Deleting a missing object is treated
// as success so teardown is idempotent even if Baseline half-failed.
//
// @arg boxID The box.
// @error error if a delete fails for a reason other than "not found".
//
// @testcase TestNFTablesTeardownDeletes deletes the chain and both sets.
func (n *NFTables) Teardown(boxID string) error {
	ctx := context.Background()
	chain, s4, s6 := chainName(boxID), set4Name(boxID), set6Name(boxID)
	for _, c := range [][]string{
		{"delete", "chain", "inet", tableName, chain},
		{"delete", "set", "inet", tableName, s4},
		{"delete", "set", "inet", tableName, s6},
	} {
		if err := n.run(ctx, c...); err != nil && !isNotFound(err) {
			return err
		}
	}
	return nil
}

// isNotFound reports whether an nft error is a benign "No such file or directory"
// (the object was already gone), so teardown stays idempotent.
//
// @arg err The nft error.
// @return bool True when the error indicates a missing object.
func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "No such file or directory")
}
