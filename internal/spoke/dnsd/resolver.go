package dnsd

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"

	"github.com/miekg/dns"
)

// defaultDNSPort is appended to a bare upstream host/IP so an operator can name a
// resolver (e.g. a Pi-hole) by address alone.
const defaultDNSPort = "53"

// NormalizeUpstream canonicalises an upstream resolver address into "host:port",
// defaulting the port to 53 when the caller gives only a host or IP. It is how
// forwarding to an external resolver like Pi-hole is configured from a simple
// address: NormalizeUpstream("10.0.0.53") -> "10.0.0.53:53". An empty or
// malformed value is an error so a bad --dns-upstream fails the spoke at startup
// rather than silently dropping every allowed lookup.
//
// @arg raw The operator-supplied upstream ("host", "host:port", "ip", or "ip:port").
// @return string The canonical "host:port".
// @error error if raw is empty or not a valid host/address.
//
// @testcase TestNormalizeUpstream defaults the port and rejects empty/malformed input.
func NormalizeUpstream(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("dns upstream is empty")
	}
	if strings.ContainsAny(s, "/ ") {
		return "", fmt.Errorf("invalid dns upstream %q", raw)
	}
	// Already host:port (or ip:port)? Accept only a numeric port and non-empty host.
	if host, port, err := net.SplitHostPort(s); err == nil {
		if host == "" || !validPort(port) {
			return "", fmt.Errorf("invalid dns upstream %q", raw)
		}
		return s, nil
	}
	// No port: a bare IPv6 literal must be bracketed before appending the default.
	if ip, err := netip.ParseAddr(s); err == nil {
		return net.JoinHostPort(ip.String(), defaultDNSPort), nil
	}
	// A bare hostname (e.g. "pihole.lan").
	return net.JoinHostPort(s, defaultDNSPort), nil
}

// validPort reports whether p is a decimal port in 1..65535.
//
// @arg p The port string.
// @return bool True when p is a valid port number.
func validPort(p string) bool {
	n, err := strconv.Atoi(p)
	return err == nil && n >= 1 && n <= 65535
}

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
