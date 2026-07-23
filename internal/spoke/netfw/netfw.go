// Package netfw is the packet-layer egress firewall for network isolation: it
// makes a box's outbound traffic deny-by-default and opens only the IPs a DNS
// lookup for an allowlisted domain resolved to, for a short TTL. It is split into
// a backend-agnostic Programmer interface (the thing that mutates host firewall
// state), a pure TTL-tracking Pinner (which IPs are open, and until when), and
// concrete Programmer implementations (nftables today; a recording fake for
// tests). The TTL bookkeeping lives here, off the syscall path, so it is unit-
// testable without root or a real firewall.
package netfw

import (
	"context"
	"net/netip"
	"sort"
	"sync"
	"time"
)

// BoxSpec is a box's network identity for the firewall: the source prefix its
// packets come from (how the firewall tells this box's traffic apart) and the DNS
// resolver address it is allowed to reach even under deny-by-default.
type BoxSpec struct {
	// Source is the box's source subnet/address; egress rules match packets from it.
	Source netip.Prefix
	// DNS is the resolver (llmbox-dnsd) the box may reach on port 53 by default.
	DNS netip.Addr
}

// Programmer mutates the host firewall to enforce one box's egress policy. A box
// is identified by an opaque id the caller also uses elsewhere (its box id). All
// methods must be idempotent — re-applying an existing rule, or removing an
// absent one, is a no-op success — so a crash-and-recover never double-adds or
// errors on a missing rule. Implementations must be safe for concurrent use.
type Programmer interface {
	// Baseline installs the deny-by-default egress policy for a box: from the
	// box's source prefix, DROP all egress except DNS (UDP/TCP 53) to spec.DNS.
	// Called once when a box starts.
	Baseline(boxID string, spec BoxSpec) error
	// Allow opens egress to ip for the box (adds it to the box's allow set).
	Allow(boxID string, ip netip.Addr) error
	// Revoke closes egress to ip for the box (removes it from the allow set).
	Revoke(boxID string, ip netip.Addr) error
	// Teardown removes every rule for the box, called when it is destroyed.
	Teardown(boxID string) error
}

// pinKey identifies one pinned (box, ip) pair.
type pinKey struct {
	box string
	ip  netip.Addr
}

// Pinner tracks which resolved IPs are currently open for each box and for how
// long, calling the Programmer to open a pin and to close it once its TTL
// elapses. It is the TTL brain of enforcement: dnsd calls Pin after resolving an
// allowlisted domain, and a periodic Sweep (or the background Run loop) revokes
// pins that have expired. Refreshing an existing pin (a repeat lookup) extends
// its deadline without re-touching the firewall.
type Pinner struct {
	prog Programmer
	now  func() time.Time

	mu   sync.Mutex
	pins map[pinKey]time.Time // pin -> expiry
}

// NewPinner builds a Pinner over prog. now supplies the current time (injectable
// for tests); pass nil for time.Now.
//
// @arg prog The firewall programmer the Pinner drives.
// @arg now The clock, or nil for time.Now.
// @return *Pinner A ready pinner.
//
// @testcase TestPinnerPinAndExpire pins an IP and sweeps it away once expired.
func NewPinner(prog Programmer, now func() time.Time) *Pinner {
	if now == nil {
		now = time.Now
	}
	return &Pinner{prog: prog, now: now, pins: map[pinKey]time.Time{}}
}

// Pin opens ip for boxID until now+ttl, extending the deadline if it is already
// open. The firewall is only touched on the first pin for a (box, ip) pair; a
// refresh just moves the in-memory deadline, so a busy domain does not churn the
// rule set.
//
// @arg boxID The box the pin is for.
// @arg ip The resolved IP to open.
// @arg ttl How long the pin stays open from now.
// @error error if the underlying Allow fails.
//
// @testcase TestPinnerPinAndExpire opens a new pin through the programmer.
// @testcase TestPinnerRefreshExtends refreshes a pin without re-calling Allow.
func (p *Pinner) Pin(boxID string, ip netip.Addr, ttl time.Duration) error {
	key := pinKey{box: boxID, ip: ip}
	deadline := p.now().Add(ttl)

	p.mu.Lock()
	_, exists := p.pins[key]
	p.pins[key] = deadline
	p.mu.Unlock()

	if exists {
		return nil // already open; only the deadline moved
	}
	if err := p.prog.Allow(boxID, ip); err != nil {
		// Roll back the bookkeeping so a failed open is retried on the next lookup.
		p.mu.Lock()
		delete(p.pins, key)
		p.mu.Unlock()
		return err
	}
	return nil
}

// Sweep revokes every pin whose TTL has elapsed. It returns the first Programmer
// error, having attempted the rest, so one failing revoke does not strand the
// others. Call it periodically (Run does this) to keep the firewall in step with
// the deadlines.
//
// @error error the first revoke error, if any.
//
// @testcase TestPinnerPinAndExpire revokes an expired pin.
// @testcase TestPinnerSweepKeepsLive leaves a still-live pin in place.
func (p *Pinner) Sweep() error {
	now := p.now()
	p.mu.Lock()
	var expired []pinKey
	for k, deadline := range p.pins {
		if !deadline.After(now) {
			expired = append(expired, k)
		}
	}
	for _, k := range expired {
		delete(p.pins, k)
	}
	p.mu.Unlock()

	// Revoke outside the lock; order the keys for deterministic behaviour in tests.
	sort.Slice(expired, func(i, j int) bool {
		if expired[i].box != expired[j].box {
			return expired[i].box < expired[j].box
		}
		return expired[i].ip.Less(expired[j].ip)
	})
	var firstErr error
	for _, k := range expired {
		if err := p.prog.Revoke(k.box, k.ip); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Forget drops all pins for a box without touching the firewall — used after the
// box's rules are torn down wholesale (Programmer.Teardown), so the Pinner does
// not later try to revoke pins for a box that no longer exists.
//
// @arg boxID The box whose pins to forget.
//
// @testcase TestPinnerForget drops a box's pins so a later sweep ignores them.
func (p *Pinner) Forget(boxID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.pins {
		if k.box == boxID {
			delete(p.pins, k)
		}
	}
}

// Run sweeps every interval until ctx is done, the background driver for TTL
// expiry. Errors are returned via the onErr callback (nil to ignore) so a
// transient revoke failure is observable without stopping the loop.
//
// @arg ctx Cancels the loop.
// @arg interval How often to sweep.
// @arg onErr Called with each sweep error, or nil to ignore.
func (p *Pinner) Run(ctx context.Context, interval time.Duration, onErr func(error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := p.Sweep(); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}
