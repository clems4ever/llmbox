package dnsd

import (
	"context"
	"log/slog"
	"net/netip"
	"sync"

	"github.com/miekg/dns"
)

// ForwardResolver is the default Resolver: it forwards an allowed query to a
// single upstream DNS server over UDP (falling back to TCP on a truncated reply).
// Pointing Upstream at a Pi-hole or any other resolver is how forwarding to an
// external service is configured — no other change needed.
type ForwardResolver struct {
	// Upstream is the "host:port" of the resolver to forward to (e.g. "1.1.1.1:53").
	Upstream string
	client   *dns.Client
	tcp      *dns.Client
	once     sync.Once
}

// NewForwardResolver builds a forwarder to upstream ("host:port").
//
// @arg upstream The upstream resolver address.
// @return *ForwardResolver A ready resolver.
func NewForwardResolver(upstream string) *ForwardResolver {
	return &ForwardResolver{Upstream: upstream}
}

// Resolve forwards q to the upstream, retrying over TCP if the UDP reply is
// truncated so large answers still get through.
//
// @arg ctx Cancels the exchange.
// @arg q The query to forward.
// @return *dns.Msg The upstream reply.
// @error error if the exchange fails.
//
// @testcase TestForwardResolverForwards forwards a query to an in-process upstream.
func (f *ForwardResolver) Resolve(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
	f.once.Do(func() {
		f.client = &dns.Client{Net: "udp"}
		f.tcp = &dns.Client{Net: "tcp"}
	})
	resp, _, err := f.client.ExchangeContext(ctx, q, f.Upstream)
	if err != nil {
		return nil, err
	}
	if resp.Truncated {
		if tcpResp, _, terr := f.tcp.ExchangeContext(ctx, q, f.Upstream); terr == nil {
			return tcpResp, nil
		}
	}
	return resp, nil
}

// MapBoxes is a concurrent BoxResolver backed by an IP→box map the spoke keeps in
// step with box lifecycle (a box's guest address maps to its id).
type MapBoxes struct {
	mu sync.RWMutex
	m  map[netip.Addr]string
}

// NewMapBoxes builds an empty box map.
//
// @return *MapBoxes A ready resolver.
func NewMapBoxes() *MapBoxes {
	return &MapBoxes{m: map[netip.Addr]string{}}
}

// Set maps a box's client IP to its id.
//
// @arg ip The box's guest address.
// @arg boxID The box id.
func (b *MapBoxes) Set(ip netip.Addr, boxID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.m[ip] = boxID
}

// Remove drops a box's mapping.
//
// @arg ip The box's guest address.
func (b *MapBoxes) Remove(ip netip.Addr) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.m, ip)
}

// Box resolves a client IP to its box id.
//
// @arg clientIP The query source.
// @return string The box id when known.
// @return bool True when the IP maps to a box.
//
// @testcase TestServerAllowsAndPins resolves the seeded client IP to its box.
func (b *MapBoxes) Box(clientIP netip.Addr) (string, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	id, ok := b.m[clientIP]
	return id, ok
}

// LogSink is an AuditSink that logs each lookup at debug level. It is the default
// until the hub-stream sink (phase 3) is wired in.
type LogSink struct {
	// Logger is the destination; nil uses slog.Default.
	Logger *slog.Logger
}

// Record logs the event.
//
// @arg ev The lookup event.
func (l LogSink) Record(ev Event) {
	logger := l.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Debug("dns lookup",
		"box", ev.BoxID, "qname", ev.QName, "verdict", string(ev.Verdict), "ips", len(ev.IPs))
}
