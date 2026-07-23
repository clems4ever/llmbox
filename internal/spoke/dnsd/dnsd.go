package dnsd

import (
	"context"
	"net"
	"net/netip"
	"time"

	"github.com/miekg/dns"
)

// Resolver forwards an allowed query to an upstream and returns the response.
// The default implementation forwards to a configured resolver; a Pi-hole or any
// other DNS server is just a different Resolver, which is how forwarding is added
// later without touching the enforcement flow. Implementations must be safe for
// concurrent use.
type Resolver interface {
	// Resolve sends q upstream and returns the reply, or an error on failure.
	Resolve(ctx context.Context, q *dns.Msg) (*dns.Msg, error)
}

// Pinner opens a resolved IP in a box's firewall for a TTL. *netfw.Pinner
// satisfies it; the narrow interface keeps dnsd independent of the firewall
// backend.
type Pinner interface {
	// Pin opens ip for boxID until ttl elapses, refreshing an existing pin.
	Pin(boxID string, ip netip.Addr, ttl time.Duration) error
}

// BoxResolver maps a DNS client's source address to the box it belongs to. In the
// llmbox model each box has its own network with a known guest address, so the
// query's source IP identifies the box. An unknown source is refused.
type BoxResolver interface {
	// Box returns the box id owning clientIP; ok is false for an unknown source.
	Box(clientIP netip.Addr) (boxID string, ok bool)
}

// Verdict is how a lookup was resolved, recorded for audit.
type Verdict string

const (
	// VerdictAllowed marks a lookup for an allowlisted domain that was resolved
	// and whose IPs were pinned.
	VerdictAllowed Verdict = "allowed"
	// VerdictBlocked marks a lookup for a domain not on the box's allowlist: it was
	// answered NXDOMAIN and no IP was opened.
	VerdictBlocked Verdict = "blocked"
	// VerdictError marks a lookup that failed upstream (audited, no pin).
	VerdictError Verdict = "error"
	// VerdictRefused marks a query from an unrecognised source (no box).
	VerdictRefused Verdict = "refused"
)

// Event is one recorded DNS lookup for the audit trail (phase 3 surfaces these in
// the UI). IPs is the resolved, pinned addresses for an allowed lookup.
type Event struct {
	BoxID   string
	QName   string
	QType   uint16
	Verdict Verdict
	IPs     []netip.Addr
	Time    time.Time
}

// AuditSink receives every lookup event. The default is a log sink; a hub-stream
// sink (phase 3) implements the same interface. Record must not block the query
// path meaningfully and must be safe for concurrent use.
type AuditSink interface {
	Record(Event)
}

// Server is the enforcing DNS resolver. It composes the injected pieces and, for
// each query, identifies the box, consults the Policy, and either resolves +
// pins (allowed) or answers NXDOMAIN (blocked) — auditing every lookup. It
// implements dns.Handler.
type Server struct {
	boxes    BoxResolver
	policy   Policy
	resolver Resolver
	pinner   Pinner
	audit    AuditSink
	now      func() time.Time
}

// Config wires a Server's collaborators.
type Config struct {
	Boxes    BoxResolver
	Policy   Policy
	Resolver Resolver
	Pinner   Pinner
	Audit    AuditSink
	// Now supplies the current time for audit events; nil means time.Now.
	Now func() time.Time
}

// NewServer builds a Server from cfg.
//
// @arg cfg The wired collaborators.
// @return *Server A ready DNS handler.
//
// @testcase TestServerAllowsAndPins resolves an allowed domain and pins its IPs.
// @testcase TestServerBlocksUnlisted answers NXDOMAIN and pins nothing for a blocked domain.
func NewServer(cfg Config) *Server {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	audit := cfg.Audit
	if audit == nil {
		audit = nopSink{}
	}
	return &Server{
		boxes: cfg.Boxes, policy: cfg.Policy, resolver: cfg.Resolver,
		pinner: cfg.Pinner, audit: audit, now: now,
	}
}

// ServeDNS implements dns.Handler: it derives the client IP from the transport
// and writes the computed reply. It is the network adapter over Handle.
//
// @arg w The DNS response writer.
// @arg r The incoming query.
func (s *Server) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	clientIP := clientAddr(w.RemoteAddr())
	_ = w.WriteMsg(s.Handle(clientIP, r))
}

// Handle computes the reply for a query from clientIP without any network I/O of
// its own (beyond the injected resolver), so it is directly unit-testable. It
// handles exactly the first question, which is the shape of essentially every
// real DNS query.
//
// @arg clientIP The query's source address (identifies the box).
// @arg r The query message.
// @return *dns.Msg The reply to send.
//
// @testcase TestServerAllowsAndPins returns an answer for an allowed domain.
// @testcase TestServerBlocksUnlisted returns NXDOMAIN for a blocked domain.
// @testcase TestServerRefusesUnknownSource returns REFUSED for an unknown client.
func (s *Server) Handle(clientIP netip.Addr, r *dns.Msg) *dns.Msg {
	if len(r.Question) == 0 {
		return reply(r, dns.RcodeFormatError)
	}
	q := r.Question[0]
	qname := q.Name

	boxID, ok := s.boxes.Box(clientIP)
	if !ok {
		s.record(Event{QName: qname, QType: q.Qtype, Verdict: VerdictRefused})
		return reply(r, dns.RcodeRefused)
	}

	ttl, allowed := s.policy.Allowed(boxID, qname)
	if !allowed {
		s.record(Event{BoxID: boxID, QName: qname, QType: q.Qtype, Verdict: VerdictBlocked})
		return reply(r, dns.RcodeNameError) // NXDOMAIN: the box gets no IP to dial
	}

	resp, err := s.resolver.Resolve(context.Background(), r)
	if err != nil || resp == nil {
		s.record(Event{BoxID: boxID, QName: qname, QType: q.Qtype, Verdict: VerdictError})
		return reply(r, dns.RcodeServerFailure)
	}

	ips := answerIPs(resp)
	for _, ip := range ips {
		if s.pinner != nil {
			// A pin failure must not leak the answer to a box whose firewall was not
			// opened, so drop the whole reply to SERVFAIL if any pin fails.
			if err := s.pinner.Pin(boxID, ip, ttl); err != nil {
				s.record(Event{BoxID: boxID, QName: qname, QType: q.Qtype, Verdict: VerdictError})
				return reply(r, dns.RcodeServerFailure)
			}
		}
	}
	s.record(Event{BoxID: boxID, QName: qname, QType: q.Qtype, Verdict: VerdictAllowed, IPs: ips})

	resp.Id = r.Id // keep the client's transaction id
	return resp
}

// record stamps and forwards an audit event.
//
// @arg ev The event (Time is filled in).
func (s *Server) record(ev Event) {
	ev.Time = s.now()
	s.audit.Record(ev)
}

// answerIPs extracts the A/AAAA addresses from a response, in order, deduplicated,
// as the set to pin.
//
// @arg m The upstream response.
// @return []netip.Addr The resolved addresses.
func answerIPs(m *dns.Msg) []netip.Addr {
	var out []netip.Addr
	seen := map[netip.Addr]bool{}
	add := func(ip net.IP) {
		a, ok := netip.AddrFromSlice(ip)
		if !ok {
			return
		}
		a = a.Unmap()
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, rr := range m.Answer {
		switch v := rr.(type) {
		case *dns.A:
			add(v.A)
		case *dns.AAAA:
			add(v.AAAA)
		}
	}
	return out
}

// reply builds an empty response to r with the given rcode.
//
// @arg r The query.
// @arg rcode The response code.
// @return *dns.Msg The response.
func reply(r *dns.Msg, rcode int) *dns.Msg {
	m := new(dns.Msg)
	m.SetRcode(r, rcode)
	return m
}

// clientAddr extracts the IP from a DNS client's transport address, dropping the
// port and any IPv4-mapped IPv6 wrapper so it matches the box's real address.
//
// @arg addr The client's transport address.
// @return netip.Addr The client IP (zero value when it cannot be parsed).
func clientAddr(addr net.Addr) netip.Addr {
	var ipStr string
	switch a := addr.(type) {
	case *net.UDPAddr:
		ipStr = a.IP.String()
	case *net.TCPAddr:
		ipStr = a.IP.String()
	default:
		host, _, err := net.SplitHostPort(addr.String())
		if err != nil {
			return netip.Addr{}
		}
		ipStr = host
	}
	ip, err := netip.ParseAddr(ipStr)
	if err != nil {
		return netip.Addr{}
	}
	return ip.Unmap()
}

// nopSink is the default audit sink: it drops events.
type nopSink struct{}

// Record discards the event.
//
// @arg _ The dropped event.
func (nopSink) Record(_ Event) {}
