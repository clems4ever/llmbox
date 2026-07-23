package isolation

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/dnsd"
	"github.com/clems4ever/llmbox/internal/spoke/netfw"
)

// fixedResolver answers every query with a fixed A record, standing in for the
// upstream so the end-to-end test needs no real network.
type fixedResolver struct{ ip string }

func (f fixedResolver) Resolve(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
	m := new(dns.Msg)
	m.SetReply(q)
	rr, _ := dns.NewRR(q.Question[0].Name + " 300 IN A " + f.ip)
	m.Answer = append(m.Answer, rr)
	return m, nil
}

// startEnforcer builds and starts an Enforcer on an ephemeral loopback port with
// a recording firewall and a fixed upstream, returning it plus the programmer.
func startEnforcer(t *testing.T, resolver dnsd.Resolver) (*Enforcer, *netfw.RecordingProgrammer, netip.Addr) {
	t.Helper()
	prog := netfw.NewRecordingProgrammer()
	e, err := New(Config{
		ListenAddr: "127.0.0.1:0",
		DNSAddr:    netip.MustParseAddr("127.0.0.1"),
		Programmer: prog,
		Resolver:   resolver,
		Audit:      dnsd.LogSink{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := e.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The DNS client's source IP is 127.0.0.1, so map the box to that.
	guest := netip.MustParseAddr("127.0.0.1")
	return e, prog, guest
}

// ask sends an A query for name to the enforcer's resolver and returns the reply.
func ask(t *testing.T, addr, name string) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	c := &dns.Client{Net: "udp", Timeout: 3 * time.Second}
	resp, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("dns exchange for %s: %v", name, err)
	}
	return resp
}

// waitAddr polls until the enforcer reports its bound address.
func waitAddr(t *testing.T, e *Enforcer) string {
	t.Helper()
	for i := 0; i < 100; i++ {
		if a := e.Addr(); a != "" {
			return a
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("enforcer never reported a bound address")
	return ""
}

// TestEnforcerEndToEnd drives a real DNS query through the running resolver: an
// allowed domain resolves and its IP is opened in the firewall, while a blocked
// domain is NXDOMAIN with nothing opened.
func TestEnforcerEndToEnd(t *testing.T) {
	e, prog, guest := startEnforcer(t, fixedResolver{ip: "140.82.121.3"})
	addr := waitAddr(t, e)

	spec := netfw.BoxSpec{Source: netip.MustParsePrefix("127.0.0.1/32"), DNS: netip.MustParseAddr("127.0.0.1")}
	if err := e.Configure("web", spec, guest, []dnsd.Rule{{Pattern: "github.com", TTL: 30 * time.Second}}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	// Allowed: resolves and the answer IP is pinned into the firewall.
	resp := ask(t, addr, "github.com")
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
		t.Fatalf("allowed query rcode=%d answers=%d", resp.Rcode, len(resp.Answer))
	}
	if !prog.Allowed("web", netip.MustParseAddr("140.82.121.3")) {
		t.Fatal("resolved IP was not opened in the firewall")
	}

	// Blocked: NXDOMAIN and nothing opened.
	resp = ask(t, addr, "evil.com")
	if resp.Rcode != dns.RcodeNameError {
		t.Fatalf("blocked query rcode=%d, want NXDOMAIN", resp.Rcode)
	}
	if len(prog.AllowList("web")) != 1 {
		t.Fatalf("blocked query changed the allow set: %v", prog.AllowList("web"))
	}

	// The baseline was recorded with the box's DNS resolver.
	if s, ok := prog.Spec("web"); !ok || s.DNS != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("baseline spec = %+v ok=%v", s, ok)
	}
}

// TestEnforcerReleaseTearsDown checks releasing a box tears down its firewall and
// blocks its subsequent queries (the box is now unknown to the resolver).
func TestEnforcerReleaseTearsDown(t *testing.T) {
	e, prog, guest := startEnforcer(t, fixedResolver{ip: "1.2.3.4"})
	addr := waitAddr(t, e)
	spec := netfw.BoxSpec{Source: netip.MustParsePrefix("127.0.0.1/32"), DNS: netip.MustParseAddr("127.0.0.1")}
	_ = e.Configure("web", spec, guest, []dnsd.Rule{{Pattern: "github.com", TTL: time.Second}})

	if err := e.Release("web"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	_ = guest
	if _, ok := prog.Spec("web"); ok {
		t.Fatal("box firewall still present after Release")
	}
	// A query from the now-unmapped box source is refused.
	if resp := ask(t, addr, "github.com"); resp.Rcode != dns.RcodeRefused {
		t.Fatalf("post-release query rcode=%d, want REFUSED", resp.Rcode)
	}
}

// TestEnforcerSetNetworkPolicy checks a live policy push changes which domains the
// resolver honours for a box, without a re-baseline.
func TestEnforcerSetNetworkPolicy(t *testing.T) {
	e, _, guest := startEnforcer(t, fixedResolver{ip: "5.6.7.8"})
	addr := waitAddr(t, e)
	spec := netfw.BoxSpec{Source: netip.MustParsePrefix("127.0.0.1/32"), DNS: netip.MustParseAddr("127.0.0.1")}
	_ = e.Configure("web", spec, guest, nil) // start fully denied

	if resp := ask(t, addr, "github.com"); resp.Rcode != dns.RcodeNameError {
		t.Fatalf("pre-policy rcode=%d, want NXDOMAIN", resp.Rcode)
	}
	// Push a policy that allows github.com.
	if err := e.SetNetworkPolicy("web", sandbox.NetworkPolicy{
		Enabled: true, Rules: []sandbox.DomainRule{{Pattern: "github.com", TTLSeconds: 30}},
	}); err != nil {
		t.Fatalf("SetNetworkPolicy: %v", err)
	}
	if resp := ask(t, addr, "github.com"); resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("post-policy rcode=%d, want success", resp.Rcode)
	}
}

// TestNewRejectsMissingProgrammer checks the composition root validates its input.
func TestNewRejectsMissingProgrammer(t *testing.T) {
	if _, err := New(Config{Upstream: "1.1.1.1:53"}); err == nil {
		t.Fatal("New without a Programmer succeeded, want error")
	}
}
