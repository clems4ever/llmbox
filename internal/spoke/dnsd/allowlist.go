// Package dnsd is the per-runner DNS resolver that enforces network isolation: a
// box is configured to use it, and for each lookup it audits the query, checks
// the box's effective allowlist, and — only for an allowed domain — resolves
// upstream and pins the answer IPs into the box's firewall for a short TTL. A
// blocked domain is answered NXDOMAIN and no IP is opened, so the box cannot reach
// it. The allowlist matching and the query flow live here (unit-testable without
// sockets or root); the upstream resolver, the firewall pinner, and the audit
// sink are injected interfaces so a real deployment, a test, or a Pi-hole
// forwarder all compose the same core.
package dnsd

import (
	"strings"
	"sync"
	"time"
)

// Policy decides whether a box may reach a domain, and for how long a resolved IP
// stays pinned. It is the enforcement view of the phase-1 allowlist groups: the
// hub computes each box's effective domain set and TTLs and feeds them in.
// Implementations must be safe for concurrent use.
type Policy interface {
	// Allowed reports whether boxID may reach qname (a query name, with or without
	// a trailing dot), and the TTL to pin the resolved IPs for. ok is false for a
	// blocked domain.
	Allowed(boxID, qname string) (ttl time.Duration, ok bool)
}

// Rule is one allowlist entry: a domain pattern and the pin TTL to apply when it
// matches. Pattern is an exact host ("github.com") or a single leading-wildcard
// ("*.github.com", matching any sub-domain but not the bare apex).
type Rule struct {
	Pattern string
	TTL     time.Duration
}

// StaticPolicy is an in-memory Policy: a fixed set of rules per box. It is what
// the spoke feeds from the hub-pushed effective allowlist (and what tests use).
// Safe for concurrent use; Set replaces a box's rules atomically.
type StaticPolicy struct {
	mu    sync.RWMutex
	rules map[string][]Rule
}

// NewStaticPolicy builds an empty policy.
//
// @return *StaticPolicy A ready policy with no boxes.
func NewStaticPolicy() *StaticPolicy {
	return &StaticPolicy{rules: map[string][]Rule{}}
}

// Set replaces the rules for a box. Passing no rules leaves the box fully denied
// (every domain blocked), the deny-by-default state.
//
// @arg boxID The box whose rules to set.
// @arg rules The box's allowlist rules.
//
// @testcase TestStaticPolicyMatches sets rules and matches exact and wildcard domains.
func (p *StaticPolicy) Set(boxID string, rules []Rule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(rules) == 0 {
		delete(p.rules, boxID)
		return
	}
	cp := make([]Rule, len(rules))
	copy(cp, rules)
	p.rules[boxID] = cp
}

// Remove drops a box's rules entirely (e.g. when the box is destroyed).
//
// @arg boxID The box to remove.
func (p *StaticPolicy) Remove(boxID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.rules, boxID)
}

// Allowed matches qname against the box's rules, returning the TTL of the first
// matching rule. An unknown box or an unmatched name is blocked.
//
// @arg boxID The querying box.
// @arg qname The query name (trailing dot and case are normalised).
// @return time.Duration The pin TTL of the matching rule (zero when blocked).
// @return bool True when a rule matched.
//
// @testcase TestStaticPolicyMatches matches exact and wildcard, and blocks the rest.
func (p *StaticPolicy) Allowed(boxID, qname string) (time.Duration, bool) {
	name := normalizeName(qname)
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, r := range p.rules[boxID] {
		if matchDomain(r.Pattern, name) {
			return r.TTL, true
		}
	}
	return 0, false
}

// normalizeName lowercases a query name and strips a trailing dot, so matching is
// case- and root-dot-insensitive.
//
// @arg qname The raw query name.
// @return string The normalised name.
func normalizeName(qname string) string {
	return strings.TrimSuffix(strings.ToLower(qname), ".")
}

// matchDomain reports whether an already-normalised name matches a pattern. An
// exact pattern matches only itself; a "*." pattern matches any strict
// sub-domain (one or more labels) but not the bare apex.
//
// @arg pattern The allowlist pattern (exact or leading-wildcard).
// @arg name The normalised query name.
// @return bool True on a match.
//
// @testcase TestMatchDomain covers exact, wildcard sub-domain, apex, and non-match.
func matchDomain(pattern, name string) bool {
	pattern = normalizeName(pattern)
	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		// "*.github.com" matches "api.github.com" (and deeper) but not "github.com".
		return strings.HasSuffix(name, "."+suffix)
	}
	return name == pattern
}
