// Package isolation is the composition root for network isolation on a spoke: it
// runs the enforcing DNS resolver (dnsd) and the firewall pin sweeper (netfw)
// together, and exposes a small per-box API — Configure when a box starts,
// Release when it is destroyed — that the box provisioner calls. It turns the
// hub-computed effective allowlist for a box into a running deny-by-default
// firewall plus a resolver that only opens the IPs an allowed lookup resolved.
//
// This package is the seam the spoke lifecycle wires into; the actual
// resolv.conf injection and the hub→spoke policy push are layered on top of the
// Configure/Release calls.
package isolation

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/dnsd"
	"github.com/clems4ever/llmbox/internal/spoke/netfw"
)

// defaultSweepInterval is how often expired firewall pins are swept. It is well
// below a typical pin TTL (30s) so a closed IP never lingers long past its
// deadline, while staying cheap.
const defaultSweepInterval = 5 * time.Second

// Config wires an Enforcer.
type Config struct {
	// ListenAddr is where the resolver listens for box queries ("host:port"),
	// e.g. "172.16.0.1:53" — the address boxes are pointed at.
	ListenAddr string
	// DNSAddr is that same resolver as boxes address it, written into each box's
	// firewall baseline as the one always-allowed destination.
	DNSAddr netip.Addr
	// Programmer applies the firewall rules (nftables in production, a recording
	// fake in tests). Required.
	Programmer netfw.Programmer
	// Resolver forwards allowed queries upstream; nil uses a forwarder to Upstream.
	Resolver dnsd.Resolver
	// Upstream is the upstream resolver address used when Resolver is nil.
	Upstream string
	// Audit receives lookup events; nil logs them.
	Audit dnsd.AuditSink
	// SweepInterval overrides the pin-expiry sweep cadence (0 uses the default).
	SweepInterval time.Duration
	// Now injects the clock (tests); nil uses time.Now.
	Now func() time.Time
}

// Enforcer runs the resolver + sweeper and holds the per-box policy and box-IP
// map. It is safe for concurrent use.
type Enforcer struct {
	cfg     Config
	policy  *dnsd.StaticPolicy
	boxes   *dnsd.MapBoxes
	pinner  *netfw.Pinner
	prog    netfw.Programmer
	server  *dnsd.Server
	dnsSrvs []*dns.Server

	mu       sync.Mutex
	started  bool
	guestIPs map[string]netip.Addr // boxID -> DNS client address, for Release
}

// New builds an Enforcer from cfg. Programmer is required.
//
// @arg cfg The wiring; Programmer must be set.
// @return *Enforcer A ready, unstarted enforcer.
// @error error if the config is incomplete.
//
// @testcase TestEnforcerEndToEnd configures a box and resolves through the running server.
func New(cfg Config) (*Enforcer, error) {
	if cfg.Programmer == nil {
		return nil, fmt.Errorf("isolation: Programmer is required")
	}
	resolver := cfg.Resolver
	if resolver == nil {
		if cfg.Upstream == "" {
			return nil, fmt.Errorf("isolation: Resolver or Upstream is required")
		}
		resolver = dnsd.NewForwardResolver(cfg.Upstream)
	}
	policy := dnsd.NewStaticPolicy()
	boxes := dnsd.NewMapBoxes()
	pinner := netfw.NewPinner(cfg.Programmer, cfg.Now)
	server := dnsd.NewServer(dnsd.Config{
		Boxes: boxes, Policy: policy, Resolver: resolver, Pinner: pinner,
		Audit: cfg.Audit, Now: cfg.Now,
	})
	return &Enforcer{
		cfg: cfg, policy: policy, boxes: boxes, pinner: pinner,
		prog: cfg.Programmer, server: server, guestIPs: map[string]netip.Addr{},
	}, nil
}

// rulesFromPolicy converts a hub NetworkPolicy into dnsd rules. A disabled policy
// yields no rules (the box is fully denied by the resolver, which is the safe
// state until enforcement is turned off at the firewall level).
//
// @arg policy The hub-pushed policy.
// @return []dnsd.Rule The matching resolver rules.
func rulesFromPolicy(policy sandbox.NetworkPolicy) []dnsd.Rule {
	if !policy.Enabled {
		return nil
	}
	rules := make([]dnsd.Rule, 0, len(policy.Rules))
	for _, r := range policy.Rules {
		rules = append(rules, dnsd.Rule{Pattern: r.Pattern, TTL: time.Duration(r.TTLSeconds) * time.Second})
	}
	return rules
}

// SetNetworkPolicy applies a hub-pushed policy to a box's live allowlist without
// re-baselining the firewall — the update path when an operator changes a box's
// groups. The box keeps its firewall baseline and open pins; only which domains
// the resolver will honour changes.
//
// @arg boxID The box.
// @arg policy The box's effective policy.
// @error error Always nil (kept for the applier interface).
//
// @testcase TestEnforcerSetNetworkPolicy updates a box's rules live.
func (e *Enforcer) SetNetworkPolicy(boxID string, policy sandbox.NetworkPolicy) error {
	e.policy.Set(boxID, rulesFromPolicy(policy))
	return nil
}

// Start binds the resolver on UDP and TCP and launches the pin sweeper. It
// returns once the listeners are up; Stop (via ctx cancellation) tears them down.
//
// @arg ctx Cancels the sweeper and, on cancel, shuts the listeners down.
// @error error if a listener cannot be bound.
//
// @testcase TestEnforcerEndToEnd starts the resolver and serves a real query.
func (e *Enforcer) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return fmt.Errorf("isolation: already started")
	}
	// UDP is the primary DNS transport; TCP covers truncated/large answers.
	pc, err := net.ListenPacket("udp", e.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("isolation: binding udp %s: %w", e.cfg.ListenAddr, err)
	}
	ln, err := net.Listen("tcp", e.cfg.ListenAddr)
	if err != nil {
		_ = pc.Close()
		return fmt.Errorf("isolation: binding tcp %s: %w", e.cfg.ListenAddr, err)
	}
	for _, srv := range []*dns.Server{
		{PacketConn: pc, Handler: e.server},
		{Listener: ln, Handler: e.server},
	} {
		s := srv
		e.dnsSrvs = append(e.dnsSrvs, s)
		go func() { _ = s.ActivateAndServe() }()
	}

	interval := e.cfg.SweepInterval
	if interval == 0 {
		interval = defaultSweepInterval
	}
	go e.pinner.Run(ctx, interval, nil)
	go func() {
		<-ctx.Done()
		e.shutdown()
	}()
	e.started = true
	return nil
}

// shutdown stops the DNS listeners; the sweeper stops with the context.
func (e *Enforcer) shutdown() {
	e.mu.Lock()
	srvs := e.dnsSrvs
	e.dnsSrvs = nil
	e.mu.Unlock()
	for _, s := range srvs {
		_ = s.Shutdown()
	}
}

// Addr returns the resolver's actual UDP address, useful when ListenAddr used
// port 0 (tests). It returns "" before Start.
//
// @return string The bound UDP address, or "" if not started.
func (e *Enforcer) Addr() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range e.dnsSrvs {
		if s.PacketConn != nil {
			return s.PacketConn.LocalAddr().String()
		}
	}
	return ""
}

// Configure installs (or updates) a box's isolation: the deny-by-default firewall
// baseline, its client-IP → box-id mapping, and its allowlist rules. Calling it
// again with new rules replaces the box's policy live (a hub push).
//
// @arg boxID The box.
// @arg spec The box's source prefix and DNS resolver.
// @arg guestIP The box's DNS client address (its query source).
// @arg rules The box's effective allowlist rules.
// @error error if the firewall baseline fails.
//
// @testcase TestEnforcerEndToEnd configures a box before resolving for it.
func (e *Enforcer) Configure(boxID string, spec netfw.BoxSpec, guestIP netip.Addr, rules []dnsd.Rule) error {
	if err := e.prog.Baseline(boxID, spec); err != nil {
		return fmt.Errorf("isolation: baseline for %s: %w", boxID, err)
	}
	e.boxes.Set(guestIP, boxID)
	e.policy.Set(boxID, rules)
	e.mu.Lock()
	e.guestIPs[boxID] = guestIP
	e.mu.Unlock()
	return nil
}

// Release removes a box's isolation: tears down its firewall rules, forgets its
// pins, and drops its policy and IP mapping. Idempotent; safe to call for a box
// that was never Configured (only its policy is dropped).
//
// @arg boxID The box.
// @error error if the firewall teardown fails.
//
// @testcase TestEnforcerReleaseTearsDown releases a box and blocks its later queries.
func (e *Enforcer) Release(boxID string) error {
	e.policy.Remove(boxID)
	e.mu.Lock()
	guestIP, hadIP := e.guestIPs[boxID]
	delete(e.guestIPs, boxID)
	e.mu.Unlock()
	if hadIP {
		e.boxes.Remove(guestIP)
	}
	e.pinner.Forget(boxID)
	if err := e.prog.Teardown(boxID); err != nil {
		return fmt.Errorf("isolation: teardown for %s: %w", boxID, err)
	}
	return nil
}
