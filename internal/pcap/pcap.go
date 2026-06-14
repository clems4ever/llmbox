// Package pcap summarizes a box's captured network traffic into metadata —
// destinations, byte volumes, and domains (TLS SNI + DNS) — without exposing
// payloads. It reads classic pcap files with the pure-Go gopacket reader, so it
// needs no libpcap and builds with CGO disabled.
package pcap

import (
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcapgo"
)

// maxPackets bounds how many packets a single Summarize call decodes, so an
// enormous capture can't stall the admin page. Summary.Truncated reports a hit.
const maxPackets = 2_000_000

// Destination is one remote endpoint the box exchanged traffic with.
type Destination struct {
	IP       string // remote IP address
	Hostname string // resolved name (DNS answer or TLS SNI), if known
	Port     int    // remote port
	Proto    string // "tcp" or "udp"
	Packets  int    // packets to+from this endpoint
	Bytes    int64  // bytes to+from this endpoint
}

// Summary is the metadata extracted from a box's capture files.
type Summary struct {
	Files        int           // number of pcap files read
	Packets      int           // total packets decoded
	Bytes        int64         // total bytes (original lengths)
	FirstSeen    time.Time     // earliest packet timestamp
	LastSeen     time.Time     // latest packet timestamp
	Destinations []Destination // remote endpoints, busiest first
	Domains      []string      // unique hostnames seen (SNI + DNS), sorted
	Truncated    bool          // true if maxPackets was reached
}

// Available reports whether any capture file exists for a box in dir.
//
// @arg dir The capture directory.
// @arg boxID The box's short ID (capture file stem).
// @return bool True if at least one matching pcap file exists.
//
// @testcase TestSummarizeReadsCapture finds the written capture file.
func Available(dir, boxID string) bool {
	files, _ := filepath.Glob(filepath.Join(dir, boxID+".pcap*"))
	return len(files) > 0
}

// Summarize reads every capture file for a box (`<boxID>.pcap*` in dir) and
// returns the aggregated traffic metadata. It returns a zero Summary (no error)
// when no capture files exist.
//
// @arg dir The capture directory.
// @arg boxID The box's short ID (capture file stem).
// @return Summary The aggregated traffic metadata.
// @error error if a capture file exists but cannot be read.
//
// @testcase TestSummarizeReadsCapture summarizes a synthetic capture's destinations and domains.
// @testcase TestSummarizeNoFiles returns an empty summary when nothing was captured.
func Summarize(dir, boxID string) (Summary, error) {
	files, err := filepath.Glob(filepath.Join(dir, boxID+".pcap*"))
	if err != nil {
		return Summary{}, err
	}
	sort.Strings(files)

	agg := newAggregator()
	for _, path := range files {
		if err := agg.readFile(path); err != nil {
			return Summary{}, err
		}
		agg.files++
		if agg.truncated {
			break
		}
	}
	return agg.summary(), nil
}

// aggregator accumulates per-endpoint and per-domain state across files.
type aggregator struct {
	files     int
	packets   int
	bytes     int64
	first     time.Time
	last      time.Time
	truncated bool

	dests   map[string]*Destination // key: proto|ip|port
	ipNames map[string]string       // ip -> hostname (DNS answers, SNI)
	domains map[string]struct{}     // every hostname seen
}

// newAggregator returns an empty aggregator with its maps initialized.
//
// @return *aggregator A ready-to-use aggregator.
//
// @testcase TestSummarizeReadsCapture aggregates a synthetic capture.
func newAggregator() *aggregator {
	return &aggregator{
		dests:   map[string]*Destination{},
		ipNames: map[string]string{},
		domains: map[string]struct{}{},
	}
}

// readFile decodes one pcap file into the aggregator, stopping at maxPackets.
//
// @arg path The pcap file path.
// @error error if the file cannot be opened or its header is invalid.
//
// @testcase TestSummarizeReadsCapture reads a synthetic pcap file.
func (a *aggregator) readFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	r, err := pcapgo.NewReader(f)
	if err != nil {
		return err
	}
	src := gopacket.NewPacketSource(r, r.LinkType())
	src.Lazy = true
	src.NoCopy = true
	for packet := range src.Packets() {
		if a.packets >= maxPackets {
			a.truncated = true
			return nil
		}
		a.consume(packet)
	}
	return nil
}

// consume folds a single packet's IP/transport/DNS/TLS-SNI information into the
// aggregator.
//
// @arg packet The decoded packet.
//
// @testcase TestSummarizeReadsCapture consumes synthetic packets.
func (a *aggregator) consume(packet gopacket.Packet) {
	net := packet.NetworkLayer()
	if net == nil {
		return
	}
	srcIP, dstIP := net.NetworkFlow().Endpoints()

	var proto string
	var srcPort, dstPort int
	switch t := packet.TransportLayer().(type) {
	case *layers.TCP:
		proto, srcPort, dstPort = "tcp", int(t.SrcPort), int(t.DstPort)
		if sni, ok := sniFromClientHello(t.Payload); ok {
			a.note(dstIP.String(), sni)
		}
	case *layers.UDP:
		proto, srcPort, dstPort = "udp", int(t.SrcPort), int(t.DstPort)
	default:
		return
	}

	length := int64(packet.Metadata().Length)
	ts := packet.Metadata().Timestamp
	a.packets++
	a.bytes += length
	if a.first.IsZero() || ts.Before(a.first) {
		a.first = ts
	}
	if ts.After(a.last) {
		a.last = ts
	}

	if dns, ok := packet.Layer(layers.LayerTypeDNS).(*layers.DNS); ok {
		a.consumeDNS(dns)
	}

	rIP, rPort := remoteEndpoint(srcIP.Raw(), srcPort, dstIP.Raw(), dstPort)
	key := proto + "|" + rIP + "|" + itoa(rPort)
	d := a.dests[key]
	if d == nil {
		d = &Destination{IP: rIP, Port: rPort, Proto: proto}
		a.dests[key] = d
	}
	d.Packets++
	d.Bytes += length
}

// consumeDNS records DNS question names and A/AAAA answer name→IP mappings.
//
// @arg dns The decoded DNS layer.
//
// @testcase TestSummarizeReadsCapture records DNS names from a synthetic query.
func (a *aggregator) consumeDNS(dns *layers.DNS) {
	for _, q := range dns.Questions {
		if len(q.Name) > 0 {
			a.domains[string(q.Name)] = struct{}{}
		}
	}
	for _, ans := range dns.Answers {
		if (ans.Type == layers.DNSTypeA || ans.Type == layers.DNSTypeAAAA) && ans.IP != nil {
			a.note(ans.IP.String(), string(ans.Name))
		}
	}
}

// note records an ip→hostname association and the hostname as a seen domain.
//
// @arg ip The remote IP.
// @arg name The hostname associated with it.
//
// @testcase TestSummarizeReadsCapture notes the SNI host for a destination.
func (a *aggregator) note(ip, name string) {
	if name == "" {
		return
	}
	a.domains[name] = struct{}{}
	if _, ok := a.ipNames[ip]; !ok {
		a.ipNames[ip] = name
	}
}

// summary finalizes the aggregator into a Summary: it attaches hostnames to
// destinations, sorts destinations by bytes and domains alphabetically.
//
// @return Summary The finalized traffic metadata.
//
// @testcase TestSummarizeReadsCapture produces a sorted, hostname-annotated summary.
func (a *aggregator) summary() Summary {
	dests := make([]Destination, 0, len(a.dests))
	for _, d := range a.dests {
		d.Hostname = a.ipNames[d.IP]
		dests = append(dests, *d)
	}
	sort.Slice(dests, func(i, j int) bool {
		if dests[i].Bytes != dests[j].Bytes {
			return dests[i].Bytes > dests[j].Bytes
		}
		return dests[i].IP < dests[j].IP
	})

	domains := make([]string, 0, len(a.domains))
	for d := range a.domains {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	return Summary{
		Files:        a.files,
		Packets:      a.packets,
		Bytes:        a.bytes,
		FirstSeen:    a.first,
		LastSeen:     a.last,
		Destinations: dests,
		Domains:      domains,
		Truncated:    a.truncated,
	}
}

// remoteEndpoint picks which side of a flow is the remote server, by preferring
// the well-known (server) port, else the public IP, else the lower port.
//
// @arg src The source IP bytes.
// @arg srcPort The source port.
// @arg dst The destination IP bytes.
// @arg dstPort The destination port.
// @return string The remote IP as a string.
// @return int The remote port.
//
// @testcase TestRemoteEndpoint picks the server side of a flow.
func remoteEndpoint(src []byte, srcPort int, dst []byte, dstPort int) (string, int) {
	srcServer := isServerPort(srcPort)
	dstServer := isServerPort(dstPort)
	switch {
	case dstServer && !srcServer:
		return net.IP(dst).String(), dstPort
	case srcServer && !dstServer:
		return net.IP(src).String(), srcPort
	case isPrivate(net.IP(src)) && !isPrivate(net.IP(dst)):
		return net.IP(dst).String(), dstPort
	case isPrivate(net.IP(dst)) && !isPrivate(net.IP(src)):
		return net.IP(src).String(), srcPort
	case dstPort <= srcPort:
		return net.IP(dst).String(), dstPort
	default:
		return net.IP(src).String(), srcPort
	}
}

// isServerPort reports whether p is a typical server-side port.
//
// @arg p The port number.
// @return bool True for well-known/server ports.
//
// @testcase TestRemoteEndpoint relies on server-port detection.
func isServerPort(p int) bool {
	switch p {
	case 443, 80, 53, 8080, 8443, 22, 25, 587, 993:
		return true
	}
	return p > 0 && p < 1024
}

// isPrivate reports whether ip is loopback, link-local, or RFC1918/ULA private.
//
// @arg ip The IP address.
// @return bool True if the address is private/local.
//
// @testcase TestRemoteEndpoint distinguishes private from public addresses.
func isPrivate(ip net.IP) bool {
	return ip != nil && (ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsPrivate())
}

// itoa renders a non-negative int without importing strconv at call sites.
//
// @arg n The non-negative integer.
// @return string The decimal representation.
//
// @testcase TestSummarizeReadsCapture builds destination keys with itoa.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// sniFromClientHello extracts the SNI host from a TLS ClientHello at the start of
// a TCP payload. It returns ("", false) for anything that isn't a ClientHello
// carrying a server_name extension.
//
// @arg b The TCP payload bytes.
// @return string The SNI hostname.
// @return bool True if an SNI host was found.
//
// @testcase TestSummarizeReadsCapture extracts SNI from a synthetic ClientHello.
// @testcase TestSNINonClientHello returns false for non-handshake payloads.
func sniFromClientHello(b []byte) (string, bool) {
	// TLS record header: type(1)=22 handshake, version(2), length(2).
	if len(b) < 5 || b[0] != 0x16 {
		return "", false
	}
	hs := b[5:]
	// Handshake header: type(1)=1 ClientHello, length(3).
	if len(hs) < 4 || hs[0] != 0x01 {
		return "", false
	}
	p := 4
	// client_version(2) + random(32).
	p += 2 + 32
	if p+1 > len(hs) {
		return "", false
	}
	// session_id. p now points at the cipher_suites length field.
	p += 1 + int(hs[p])
	return sniFromExtensions(hs, p)
}

// sniFromExtensions walks the ClientHello body from the cipher-suites length
// field onward and returns the SNI host if present.
//
// @arg hs The handshake bytes (from the ClientHello header).
// @arg pCipherLen Offset of the 2-byte cipher-suites length field.
// @return string The SNI hostname.
// @return bool True if an SNI host was found.
//
// @testcase TestSummarizeReadsCapture parses extensions to find SNI.
func sniFromExtensions(hs []byte, pCipherLen int) (string, bool) {
	p := pCipherLen
	if p+2 > len(hs) {
		return "", false
	}
	p += 2 + (int(hs[p])<<8 | int(hs[p+1])) // skip cipher_suites
	if p+1 > len(hs) {
		return "", false
	}
	p += 1 + int(hs[p]) // skip compression_methods
	if p+2 > len(hs) {
		return "", false
	}
	extEnd := p + 2 + (int(hs[p])<<8 | int(hs[p+1]))
	p += 2
	for p+4 <= len(hs) && p+4 <= extEnd {
		et := int(hs[p])<<8 | int(hs[p+1])
		el := int(hs[p+2])<<8 | int(hs[p+3])
		p += 4
		if et == 0 { // server_name
			return sniFromServerNameExt(hs, p, el)
		}
		p += el
	}
	return "", false
}

// sniFromServerNameExt reads a host_name from a server_name extension body.
//
// @arg hs The handshake bytes.
// @arg p Offset of the extension body.
// @arg el Length of the extension body.
// @return string The SNI hostname.
// @return bool True if a host_name entry was found.
//
// @testcase TestSummarizeReadsCapture reads the host_name from the SNI extension.
func sniFromServerNameExt(hs []byte, p, el int) (string, bool) {
	if p+el > len(hs) || el < 5 {
		return "", false
	}
	body := hs[p : p+el]
	// server_name_list length(2), then entries: type(1), name_len(2), name.
	nameType := body[2]
	nameLen := int(body[3])<<8 | int(body[4])
	if nameType != 0 || 5+nameLen > len(body) {
		return "", false
	}
	return string(body[5 : 5+nameLen]), true
}
