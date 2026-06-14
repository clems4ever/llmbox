package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcapgo"

	"github.com/clems4ever/llmbox-mcp/internal/docker"
)

// TestAdminAuth checks the admin pages 404 when no token is set and 401 without
// valid Basic Auth credentials.
func TestAdminAuth(t *testing.T) {
	s := newTestServer(&fakeMgr{})
	h := s.Handler(s.MCPServer("t", "v"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boxes", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("disabled admin should 404, got %d", rec.Code)
	}

	s.EnableAdmin("", "secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boxes", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing auth should 401, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	bad := httptest.NewRequest(http.MethodGet, "/boxes", nil)
	bad.SetBasicAuth("admin", "wrong")
	h.ServeHTTP(rec, bad)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong password should 401, got %d", rec.Code)
	}
}

// TestAdminPages checks the box list, the per-box traffic page (with a synthetic
// capture), and the embedded stylesheet render for an authenticated request.
func TestAdminPages(t *testing.T) {
	dir := t.TempDir()
	const id = "abcdef012345"
	writeTinyPcap(t, filepath.Join(dir, id+".pcap"))

	f := &fakeMgr{listResult: []docker.Box{{ID: id, Hostname: "web-box", Phase: "ready", State: "running", Image: "img"}}}
	s := newTestServer(f)
	s.EnableAdmin(dir, "secret")
	h := s.Handler(s.MCPServer("t", "v"))

	// List.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/boxes", nil)
	req.SetBasicAuth("admin", "secret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "web-box") {
		t.Error("box hostname missing from list")
	}

	// Detail with captured traffic.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/boxes/"+id, nil)
	req.SetBasicAuth("admin", "secret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Traffic metadata") {
		t.Error("traffic section missing")
	}
	if !strings.Contains(body, "8.8.8.8") || !strings.Contains(body, "example.com") {
		t.Error("captured destination/domain missing from detail page")
	}

	// Stylesheet.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/style.css", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/css") {
		t.Errorf("stylesheet response: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

// TestHumanBytes checks the human-readable formatting helpers.
func TestHumanBytes(t *testing.T) {
	for _, c := range []struct {
		n    int64
		want string
	}{{512, "512 B"}, {1024, "1.0 KB"}, {1536, "1.5 KB"}, {1048576, "1.0 MB"}} {
		if got := humanBytes(c.n); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
	if got := humanSpan(time.Time{}, time.Time{}); got != "—" {
		t.Errorf("zero span = %q, want —", got)
	}
	if got := humanSpan(time.Unix(0, 0), time.Unix(90, 0)); got != "1m" {
		t.Errorf("90s span = %q, want 1m", got)
	}
	if got := relativeUnix(0); got != "—" {
		t.Errorf("zero time = %q, want —", got)
	}
}

// writeTinyPcap writes a one-packet (DNS query to 8.8.8.8) capture file.
func writeTinyPcap(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create pcap: %v", err)
	}
	defer f.Close()
	w := pcapgo.NewWriter(f)
	if err := w.WriteFileHeader(65536, layers.LinkTypeEthernet); err != nil {
		t.Fatalf("pcap header: %v", err)
	}
	eth := &layers.Ethernet{SrcMAC: net.HardwareAddr{0, 0, 0, 0, 0, 1}, DstMAC: net.HardwareAddr{0, 0, 0, 0, 0, 2}, EthernetType: layers.EthernetTypeIPv4}
	ip := &layers.IPv4{Version: 4, IHL: 5, TTL: 64, Protocol: layers.IPProtocolUDP, SrcIP: net.IP{10, 0, 0, 2}, DstIP: net.IP{8, 8, 8, 8}}
	udp := &layers.UDP{SrcPort: 50000, DstPort: 53}
	_ = udp.SetNetworkLayerForChecksum(ip)
	dns := &layers.DNS{ID: 1, Questions: []layers.DNSQuestion{{Name: []byte("example.com"), Type: layers.DNSTypeA, Class: layers.DNSClassIN}}}
	buf := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buf, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}, eth, ip, udp, dns); err != nil {
		t.Fatalf("serialize: %v", err)
	}
	data := buf.Bytes()
	if err := w.WritePacket(gopacket.CaptureInfo{Timestamp: time.Unix(1700000000, 0), CaptureLength: len(data), Length: len(data)}, data); err != nil {
		t.Fatalf("write packet: %v", err)
	}
}
