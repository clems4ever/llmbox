package pcap

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcapgo"
)

// TestSummarizeNoFiles checks an absent capture yields an empty, error-free summary.
func TestSummarizeNoFiles(t *testing.T) {
	s, err := Summarize(t.TempDir(), "deadbeef0000")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if s.Files != 0 || s.Packets != 0 || len(s.Destinations) != 0 || len(s.Domains) != 0 {
		t.Errorf("expected empty summary, got %+v", s)
	}
}

// TestSummarizeReadsCapture writes a synthetic pcap (one HTTPS ClientHello with
// SNI, one DNS query) and checks the extracted destinations and domains.
func TestSummarizeReadsCapture(t *testing.T) {
	dir := t.TempDir()
	id := "abcdef012345"
	path := filepath.Join(dir, id+".pcap")

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	w := pcapgo.NewWriter(f)
	if err := w.WriteFileHeader(65536, layers.LinkTypeEthernet); err != nil {
		t.Fatalf("header: %v", err)
	}
	ts := time.Unix(1700000000, 0)
	writePacket(t, w, ts, tcpPacket(t, net.IP{10, 0, 0, 2}, 50000, net.IP{1, 2, 3, 4}, 443, clientHello("example.com")))
	writePacket(t, w, ts.Add(time.Second), udpDNSPacket(t, net.IP{10, 0, 0, 2}, 50001, net.IP{8, 8, 8, 8}, 53, "api.example.com"))
	_ = f.Close()

	s, err := Summarize(dir, id)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if s.Files != 1 || s.Packets != 2 {
		t.Fatalf("files/packets = %d/%d, want 1/2", s.Files, s.Packets)
	}
	if s.Bytes == 0 || s.FirstSeen.IsZero() || !s.LastSeen.After(s.FirstSeen) {
		t.Errorf("unexpected totals/timespan: %+v", s)
	}
	if !contains(s.Domains, "example.com") || !contains(s.Domains, "api.example.com") {
		t.Errorf("domains = %v, want example.com and api.example.com", s.Domains)
	}
	// The 443 destination should carry the SNI hostname.
	var https *Destination
	for i := range s.Destinations {
		if s.Destinations[i].Port == 443 {
			https = &s.Destinations[i]
		}
	}
	if https == nil {
		t.Fatalf("no :443 destination in %+v", s.Destinations)
	}
	if https.IP != "1.2.3.4" || https.Proto != "tcp" || https.Hostname != "example.com" {
		t.Errorf("https destination = %+v, want 1.2.3.4 tcp example.com", *https)
	}
}

// TestSNINonClientHello checks non-handshake payloads yield no SNI.
func TestSNINonClientHello(t *testing.T) {
	if _, ok := sniFromClientHello([]byte("GET / HTTP/1.1\r\n")); ok {
		t.Error("plain HTTP should not parse as a ClientHello")
	}
	if _, ok := sniFromClientHello(nil); ok {
		t.Error("empty payload should not parse as a ClientHello")
	}
}

// TestRemoteEndpoint checks the server side of a flow is chosen.
func TestRemoteEndpoint(t *testing.T) {
	// Outbound: high port -> 443. Remote is the :443 server.
	if ip, port := remoteEndpoint(net.IP{10, 0, 0, 2}, 50000, net.IP{1, 2, 3, 4}, 443); ip != "1.2.3.4" || port != 443 {
		t.Errorf("outbound remote = %s:%d, want 1.2.3.4:443", ip, port)
	}
	// Inbound response: from :443 to high port. Remote is still the :443 server.
	if ip, port := remoteEndpoint(net.IP{1, 2, 3, 4}, 443, net.IP{10, 0, 0, 2}, 50000); ip != "1.2.3.4" || port != 443 {
		t.Errorf("inbound remote = %s:%d, want 1.2.3.4:443", ip, port)
	}
}

// --- helpers ---

// contains reports whether want is in s.
func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

// writePacket writes one raw packet to a pcap writer at time ts.
func writePacket(t *testing.T, w *pcapgo.Writer, ts time.Time, data []byte) {
	t.Helper()
	if err := w.WritePacket(gopacket.CaptureInfo{Timestamp: ts, CaptureLength: len(data), Length: len(data)}, data); err != nil {
		t.Fatalf("write packet: %v", err)
	}
}

// tcpPacket builds a serialized Ethernet/IPv4/TCP packet with the given payload.
func tcpPacket(t *testing.T, src net.IP, sport int, dst net.IP, dport int, payload []byte) []byte {
	t.Helper()
	eth := &layers.Ethernet{SrcMAC: mac(1), DstMAC: mac(2), EthernetType: layers.EthernetTypeIPv4}
	ip := &layers.IPv4{Version: 4, IHL: 5, TTL: 64, Protocol: layers.IPProtocolTCP, SrcIP: src, DstIP: dst}
	tcp := &layers.TCP{SrcPort: layers.TCPPort(sport), DstPort: layers.TCPPort(dport)}
	_ = tcp.SetNetworkLayerForChecksum(ip)
	return serialize(t, eth, ip, tcp, gopacket.Payload(payload))
}

// udpDNSPacket builds a serialized Ethernet/IPv4/UDP DNS query for qname.
func udpDNSPacket(t *testing.T, src net.IP, sport int, dst net.IP, dport int, qname string) []byte {
	t.Helper()
	eth := &layers.Ethernet{SrcMAC: mac(1), DstMAC: mac(2), EthernetType: layers.EthernetTypeIPv4}
	ip := &layers.IPv4{Version: 4, IHL: 5, TTL: 64, Protocol: layers.IPProtocolUDP, SrcIP: src, DstIP: dst}
	udp := &layers.UDP{SrcPort: layers.UDPPort(sport), DstPort: layers.UDPPort(dport)}
	_ = udp.SetNetworkLayerForChecksum(ip)
	dns := &layers.DNS{ID: 1, Questions: []layers.DNSQuestion{{Name: []byte(qname), Type: layers.DNSTypeA, Class: layers.DNSClassIN}}}
	return serialize(t, eth, ip, udp, dns)
}

// serialize serializes layers into raw packet bytes.
func serialize(t *testing.T, ls ...gopacket.SerializableLayer) []byte {
	t.Helper()
	buf := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buf, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}, ls...); err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return buf.Bytes()
}

// mac returns a test MAC address ending in b.
func mac(b byte) net.HardwareAddr { return net.HardwareAddr{0, 0, 0, 0, 0, b} }

// clientHello builds a minimal TLS ClientHello record carrying an SNI host.
func clientHello(sni string) []byte {
	name := []byte(sni)
	hostEntry := append([]byte{0x00, byte(len(name) >> 8), byte(len(name))}, name...) // type host_name + len + name
	snList := append([]byte{byte(len(hostEntry) >> 8), byte(len(hostEntry))}, hostEntry...)
	ext := append([]byte{0x00, 0x00, byte(len(snList) >> 8), byte(len(snList))}, snList...) // ext type 0 + len + list
	exts := append([]byte{byte(len(ext) >> 8), byte(len(ext))}, ext...)

	body := []byte{0x03, 0x03}                      // client_version
	body = append(body, make([]byte, 32)...)        // random
	body = append(body, 0x00)                       // session_id len
	body = append(body, 0x00, 0x02, 0x00, 0x00)     // cipher_suites len + one suite
	body = append(body, 0x01, 0x00)                 // compression_methods len + null
	body = append(body, exts...)                    // extensions

	hs := append([]byte{0x01, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}, body...) // ClientHello
	rec := append([]byte{0x16, 0x03, 0x01, byte(len(hs) >> 8), byte(len(hs))}, hs...)                 // handshake record
	return rec
}
