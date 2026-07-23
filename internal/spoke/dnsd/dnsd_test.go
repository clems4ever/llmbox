package dnsd

import (
	"context"
	"net"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// newUDPConn opens a UDP listener on an ephemeral loopback port for the
// in-process upstream DNS server the forward-resolver test exercises.
func newUDPConn() (net.PacketConn, error) {
	return net.ListenPacket("udp", "127.0.0.1:0")
}

// --- matcher / policy ---

// TestMatchDomain covers exact, wildcard sub-domain, apex, and non-match.
func TestMatchDomain(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"github.com", "github.com", true},
		{"github.com", "api.github.com", false},
		{"*.github.com", "api.github.com", true},
		{"*.github.com", "a.b.github.com", true},
		{"*.github.com", "github.com", false}, // apex is not a sub-domain
		{"*.github.com", "notgithub.com", false},
		{"GitHub.com", "github.com", true}, // case-insensitive
	}
	for _, c := range cases {
		if got := matchDomain(c.pattern, normalizeName(c.name)); got != c.want {
			t.Errorf("matchDomain(%q,%q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

// TestStaticPolicyMatches checks a box's rules match exact and wildcard domains,
// return the rule's TTL, and block everything else (incl. unknown boxes).
func TestStaticPolicyMatches(t *testing.T) {
	p := NewStaticPolicy()
	p.Set("web", []Rule{
		{Pattern: "github.com", TTL: 30 * time.Second},
		{Pattern: "*.githubusercontent.com", TTL: 45 * time.Second},
	})

	if ttl, ok := p.Allowed("web", "github.com."); !ok || ttl != 30*time.Second {
		t.Errorf("exact match = (%v,%v), want (30s,true)", ttl, ok)
	}
	if ttl, ok := p.Allowed("web", "raw.githubusercontent.com"); !ok || ttl != 45*time.Second {
		t.Errorf("wildcard match = (%v,%v), want (45s,true)", ttl, ok)
	}
	if _, ok := p.Allowed("web", "evil.com"); ok {
		t.Error("evil.com allowed, want blocked")
	}
	if _, ok := p.Allowed("other", "github.com"); ok {
		t.Error("unknown box allowed, want blocked")
	}

	// Clearing rules blocks everything again.
	p.Remove("web")
	if _, ok := p.Allowed("web", "github.com"); ok {
		t.Error("github.com allowed after Remove, want blocked")
	}
}

// --- server ---

// stubResolver returns a fixed answer for any query.
type stubResolver struct {
	ips []string
	err error
}

func (s stubResolver) Resolve(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
	if s.err != nil {
		return nil, s.err
	}
	m := new(dns.Msg)
	m.SetReply(q)
	for _, ip := range s.ips {
		rr, _ := dns.NewRR(q.Question[0].Name + " 300 IN A " + ip)
		m.Answer = append(m.Answer, rr)
	}
	return m, nil
}

// recSink records audit events.
type recSink struct{ events []Event }

func (r *recSink) Record(ev Event) { r.events = append(r.events, ev) }

// recPinner records pin calls.
type recPinner struct{ pins []pinCall }
type pinCall struct {
	box string
	ip  netip.Addr
	ttl time.Duration
}

func (p *recPinner) Pin(box string, ip netip.Addr, ttl time.Duration) error {
	p.pins = append(p.pins, pinCall{box, ip, ttl})
	return nil
}

func query(name string) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	return m
}

func newTestServer(policy Policy, res Resolver, pin Pinner, sink AuditSink) *Server {
	boxes := NewMapBoxes()
	boxes.Set(netip.MustParseAddr("172.16.0.2"), "web")
	return NewServer(Config{
		Boxes: boxes, Policy: policy, Resolver: res, Pinner: pin, Audit: sink,
		Now: func() time.Time { return time.Unix(1000, 0) },
	})
}

// TestServerAllowsAndPins checks an allowed domain is resolved, its IPs pinned
// with the rule TTL, and the lookup audited "allowed".
func TestServerAllowsAndPins(t *testing.T) {
	policy := NewStaticPolicy()
	policy.Set("web", []Rule{{Pattern: "github.com", TTL: 30 * time.Second}})
	pin := &recPinner{}
	sink := &recSink{}
	s := newTestServer(policy, stubResolver{ips: []string{"140.82.121.3", "140.82.121.4"}}, pin, sink)

	resp := s.Handle(netip.MustParseAddr("172.16.0.2"), query("github.com"))
	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %d, want success", resp.Rcode)
	}
	if len(resp.Answer) != 2 {
		t.Fatalf("answers = %d, want 2", len(resp.Answer))
	}
	want := []pinCall{
		{"web", netip.MustParseAddr("140.82.121.3"), 30 * time.Second},
		{"web", netip.MustParseAddr("140.82.121.4"), 30 * time.Second},
	}
	if !reflect.DeepEqual(pin.pins, want) {
		t.Errorf("pins = %+v, want %+v", pin.pins, want)
	}
	if len(sink.events) != 1 || sink.events[0].Verdict != VerdictAllowed {
		t.Fatalf("audit = %+v, want one allowed", sink.events)
	}
}

// TestServerBlocksUnlisted checks a domain off the allowlist is answered NXDOMAIN,
// nothing is pinned, and the lookup is audited "blocked".
func TestServerBlocksUnlisted(t *testing.T) {
	policy := NewStaticPolicy()
	policy.Set("web", []Rule{{Pattern: "github.com", TTL: 30 * time.Second}})
	pin := &recPinner{}
	sink := &recSink{}
	s := newTestServer(policy, stubResolver{ips: []string{"1.2.3.4"}}, pin, sink)

	resp := s.Handle(netip.MustParseAddr("172.16.0.2"), query("evil.com"))
	if resp.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode = %d, want NXDOMAIN", resp.Rcode)
	}
	if len(pin.pins) != 0 {
		t.Errorf("pinned %d IPs for a blocked domain, want 0", len(pin.pins))
	}
	if len(sink.events) != 1 || sink.events[0].Verdict != VerdictBlocked {
		t.Fatalf("audit = %+v, want one blocked", sink.events)
	}
}

// TestServerRefusesUnknownSource checks a query from an unmapped client is
// REFUSED and audited, never reaching the policy or resolver.
func TestServerRefusesUnknownSource(t *testing.T) {
	policy := NewStaticPolicy()
	sink := &recSink{}
	s := newTestServer(policy, stubResolver{}, &recPinner{}, sink)

	resp := s.Handle(netip.MustParseAddr("10.0.0.9"), query("github.com"))
	if resp.Rcode != dns.RcodeRefused {
		t.Fatalf("rcode = %d, want REFUSED", resp.Rcode)
	}
	if len(sink.events) != 1 || sink.events[0].Verdict != VerdictRefused {
		t.Fatalf("audit = %+v, want one refused", sink.events)
	}
}

// TestServerResolverErrorIsServfail checks an upstream failure yields SERVFAIL,
// no pin, and an "error" audit — the box gets no IP it could use unpinned.
func TestServerResolverErrorIsServfail(t *testing.T) {
	policy := NewStaticPolicy()
	policy.Set("web", []Rule{{Pattern: "github.com", TTL: time.Second}})
	pin := &recPinner{}
	sink := &recSink{}
	s := newTestServer(policy, stubResolver{err: context.DeadlineExceeded}, pin, sink)

	resp := s.Handle(netip.MustParseAddr("172.16.0.2"), query("github.com"))
	if resp.Rcode != dns.RcodeServerFailure {
		t.Fatalf("rcode = %d, want SERVFAIL", resp.Rcode)
	}
	if len(pin.pins) != 0 {
		t.Errorf("pinned on resolver error, want 0")
	}
	if len(sink.events) != 1 || sink.events[0].Verdict != VerdictError {
		t.Fatalf("audit = %+v, want one error", sink.events)
	}
}

// TestForwardResolverForwards checks the default resolver forwards a query to an
// upstream and returns its answer, exercising the real miekg/dns client against
// an in-process upstream server.
func TestForwardResolverForwards(t *testing.T) {
	// Spin up an in-process upstream that answers everything with 1.2.3.4.
	pc, err := newUDPConn()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		rr, _ := dns.NewRR(r.Question[0].Name + " 300 IN A 1.2.3.4")
		m.Answer = append(m.Answer, rr)
		_ = w.WriteMsg(m)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	defer func() { _ = srv.Shutdown() }()

	f := NewForwardResolver(pc.LocalAddr().String())
	resp, err := f.Resolve(context.Background(), query("example.com"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("answers = %d, want 1", len(resp.Answer))
	}
}
