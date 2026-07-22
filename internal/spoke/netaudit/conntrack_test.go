package netaudit

import (
	"context"
	"os/exec"
	"sync"
	"testing"
	"time"
)

func TestParseConntrackTCPDestroy(t *testing.T) {
	line := "[DESTROY] tcp 6 src=172.16.5.2 dst=140.82.121.4 sport=51000 dport=443 packets=10 bytes=1420 src=140.82.121.4 dst=203.0.113.9 sport=443 dport=51000 packets=8 bytes=5300 [ASSURED]"
	ev, ok := parseConntrackLine(line)
	if !ok {
		t.Fatalf("expected line to parse")
	}
	if ev.Proto != "tcp" || ev.SrcIP != "172.16.5.2" || ev.DstIP != "140.82.121.4" {
		t.Errorf("wrong tuple: %+v", ev)
	}
	if ev.SrcPort != 51000 || ev.DstPort != 443 {
		t.Errorf("wrong ports: %+v", ev)
	}
	if ev.BytesOut != 1420 || ev.BytesIn != 5300 {
		t.Errorf("wrong byte counters: out=%d in=%d", ev.BytesOut, ev.BytesIn)
	}
	if !ev.Closed {
		t.Errorf("DESTROY should mark the event closed")
	}
}

func TestParseConntrackTCPEstablished(t *testing.T) {
	line := "[UPDATE] tcp 6 432000 ESTABLISHED src=172.16.5.2 dst=140.82.121.4 sport=51000 dport=443 src=140.82.121.4 dst=203.0.113.9 sport=443 dport=51000 [ASSURED]"
	ev, ok := parseConntrackLine(line)
	if !ok {
		t.Fatalf("expected line to parse")
	}
	if ev.State != "ESTABLISHED" {
		t.Errorf("wrong state: %q", ev.State)
	}
	if ev.SrcIP != "172.16.5.2" || ev.DstPort != 443 {
		t.Errorf("wrong tuple: %+v", ev)
	}
}

func TestParseConntrackUDPNew(t *testing.T) {
	line := "[NEW] udp 17 30 src=172.16.7.2 dst=8.8.8.8 sport=34567 dport=53 [UNREPLIED] src=8.8.8.8 dst=203.0.113.9 sport=53 dport=34567"
	ev, ok := parseConntrackLine(line)
	if !ok {
		t.Fatalf("expected line to parse")
	}
	if ev.Proto != "udp" || ev.DstIP != "8.8.8.8" || ev.DstPort != 53 {
		t.Errorf("wrong udp flow: %+v", ev)
	}
	if ev.State != "" {
		t.Errorf("udp should have no tcp state, got %q", ev.State)
	}
}

func TestParseConntrackICMP(t *testing.T) {
	line := "[NEW] icmp 1 30 type=8 code=0 id=1234 src=172.16.9.2 dst=8.8.8.8 [UNREPLIED] src=8.8.8.8 dst=203.0.113.9 type=0 code=0 id=1234"
	ev, ok := parseConntrackLine(line)
	if !ok {
		t.Fatalf("expected icmp line to parse")
	}
	if ev.Proto != "icmp" || ev.SrcIP != "172.16.9.2" || ev.DstIP != "8.8.8.8" {
		t.Errorf("wrong icmp flow: %+v", ev)
	}
	if ev.SrcPort != 0 || ev.DstPort != 0 {
		t.Errorf("icmp should have no ports: %+v", ev)
	}
}

func TestParseConntrackIgnoresUnknown(t *testing.T) {
	for _, line := range []string{
		"",
		"   ",
		"[NEW] unknown 2 600 src=172.16.5.2 dst=224.0.0.1",
		"a totally unrelated log line",
		"[UPDATE] tcp 6 120 SYN_SENT",        // no tuple
		"conntrack v1.4.6 (conntrack-tools)", // banner
	} {
		if _, ok := parseConntrackLine(line); ok {
			t.Errorf("expected %q to be ignored", line)
		}
	}
}

func TestParseConntrackNoAccounting(t *testing.T) {
	// Without nf_conntrack_acct there are no bytes= fields; the flow still parses,
	// just with zero counters.
	line := "[NEW] tcp 6 120 SYN_SENT src=172.16.5.2 dst=1.1.1.1 sport=5000 dport=443 [UNREPLIED] src=1.1.1.1 dst=203.0.113.9 sport=443 dport=5000"
	ev, ok := parseConntrackLine(line)
	if !ok {
		t.Fatalf("expected line to parse")
	}
	if ev.BytesOut != 0 || ev.BytesIn != 0 {
		t.Errorf("want zero counters without accounting, got out=%d in=%d", ev.BytesOut, ev.BytesIn)
	}
	if ev.State != "SYN_SENT" {
		t.Errorf("wrong state: %q", ev.State)
	}
}

// TestConntrackSourceStreamsFakeCommand drives the whole Source path (command →
// streamLines → parse → emit) with a fake command that prints recorded conntrack
// lines, so no real conntrack or privilege is needed.
func TestConntrackSourceStreamsFakeCommand(t *testing.T) {
	const feed = `[NEW] tcp 6 120 SYN_SENT src=172.16.5.2 dst=140.82.121.4 sport=51000 dport=443 [UNREPLIED] src=140.82.121.4 dst=203.0.113.9 sport=443 dport=51000
[DESTROY] tcp 6 src=172.16.5.2 dst=140.82.121.4 sport=51000 dport=443 packets=10 bytes=1420 src=140.82.121.4 dst=203.0.113.9 sport=443 dport=51000 packets=8 bytes=5300
[NEW] udp 17 30 src=172.16.7.2 dst=8.8.8.8 sport=34567 dport=53 [UNREPLIED] src=8.8.8.8 dst=203.0.113.9 sport=53 dport=34567
`
	src := &ConntrackSource{
		command: func(ctx context.Context) *exec.Cmd {
			return exec.CommandContext(ctx, "printf", "%s", feed)
		},
		enableAcct: func(context.Context) {},
	}

	var mu sync.Mutex
	var got []Event
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = src.Run(ctx, func(ev Event) {
			mu.Lock()
			got = append(got, ev)
			mu.Unlock()
		})
		close(done)
	}()

	// Wait until the fake feed's three events have been decoded, then stop.
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out; decoded %d/3 events", n)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	// Feed the events through a recorder to assert end-to-end attribution.
	r := NewRecorder(0)
	r.Register("box-a", "172.16.5.2")
	r.Register("box-b", "172.16.7.2")
	for _, ev := range got[:3] {
		r.Record(ev)
	}
	a := r.Flows("box-a")
	if len(a) != 1 || a[0].DstPort != 443 || a[0].BytesIn != 5300 {
		t.Errorf("box-a flow wrong: %+v", a)
	}
	b := r.Flows("box-b")
	if len(b) != 1 || b[0].DstIP != "8.8.8.8" {
		t.Errorf("box-b flow wrong: %+v", b)
	}
}

func TestParseConntrackOriginalDirectionOnly(t *testing.T) {
	// The destination must come from the ORIGINAL tuple (the box's outbound target),
	// never the reply tuple (which is the host's NAT address). Guards against
	// picking up the second dst=.
	line := "[UPDATE] tcp 6 432000 ESTABLISHED src=172.16.5.2 dst=140.82.121.4 sport=51000 dport=443 src=140.82.121.4 dst=203.0.113.9 sport=443 dport=51000"
	ev, _ := parseConntrackLine(line)
	if ev.DstIP != "140.82.121.4" {
		t.Errorf("dst should be the original target, got %q", ev.DstIP)
	}
	if ev.DstIP == "203.0.113.9" {
		t.Errorf("dst leaked the reply/NAT address")
	}
}
