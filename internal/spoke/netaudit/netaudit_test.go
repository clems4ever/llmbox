package netaudit

import (
	"testing"
	"time"
)

// fixedClock returns a Recorder whose clock advances by one second per Record so
// LastSeen ordering is deterministic in tests.
func newTestRecorder(t *testing.T, perBox int) (*Recorder, func()) {
	t.Helper()
	r := NewRecorder(perBox)
	var tick int64
	r.nowFunc = func() time.Time {
		tick++
		return time.Unix(1_000_000+tick, 0)
	}
	return r, func() {}
}

func TestRecorderAttributesBySrcIP(t *testing.T) {
	r, _ := newTestRecorder(t, 0)
	r.Register("box-a", "172.16.5.2")
	r.Record(Event{Proto: "tcp", SrcIP: "172.16.5.2", SrcPort: 51000, DstIP: "140.82.121.4", DstPort: 443, State: "ESTABLISHED", BytesOut: 100, BytesIn: 200})

	flows := r.Flows("box-a")
	if len(flows) != 1 {
		t.Fatalf("want 1 flow, got %d", len(flows))
	}
	f := flows[0]
	if f.DstIP != "140.82.121.4" || f.DstPort != 443 || f.SrcPort != 51000 {
		t.Errorf("wrong tuple: %+v", f)
	}
	if f.BytesOut != 100 || f.BytesIn != 200 {
		t.Errorf("wrong bytes: out=%d in=%d", f.BytesOut, f.BytesIn)
	}
	if f.State != "ESTABLISHED" {
		t.Errorf("wrong state: %q", f.State)
	}
}

func TestRecorderDropsUnknownSource(t *testing.T) {
	r, _ := newTestRecorder(t, 0)
	r.Register("box-a", "172.16.5.2")
	// An event from an address not registered to any box must be ignored.
	r.Record(Event{Proto: "tcp", SrcIP: "10.9.9.9", DstIP: "1.1.1.1", DstPort: 443})
	if got := r.Flows("box-a"); got != nil {
		t.Fatalf("want no flows for box-a, got %v", got)
	}
}

func TestRecorderAggregatesConnection(t *testing.T) {
	r, _ := newTestRecorder(t, 0)
	r.Register("box-a", "172.16.5.2")
	base := Event{Proto: "tcp", SrcIP: "172.16.5.2", SrcPort: 51000, DstIP: "140.82.121.4", DstPort: 443}
	// NEW, then UPDATE (established), then DESTROY with final counters — one flow.
	r.Record(base)
	up := base
	up.State, up.BytesOut, up.BytesIn = "ESTABLISHED", 500, 600
	r.Record(up)
	del := base
	del.Closed, del.BytesOut, del.BytesIn = true, 1420, 5300
	r.Record(del)

	flows := r.Flows("box-a")
	if len(flows) != 1 {
		t.Fatalf("want 1 aggregated flow, got %d", len(flows))
	}
	f := flows[0]
	if f.BytesOut != 1420 || f.BytesIn != 5300 {
		t.Errorf("final counters not kept: out=%d in=%d", f.BytesOut, f.BytesIn)
	}
	if f.State != "CLOSE" && f.State != "ESTABLISHED" {
		t.Errorf("unexpected state after close: %q", f.State)
	}
}

func TestRecorderCountersNeverDecrease(t *testing.T) {
	r, _ := newTestRecorder(t, 0)
	r.Register("box-a", "172.16.5.2")
	base := Event{Proto: "udp", SrcIP: "172.16.5.2", SrcPort: 34567, DstIP: "8.8.8.8", DstPort: 53}
	hi := base
	hi.BytesOut, hi.BytesIn = 1000, 2000
	r.Record(hi)
	lo := base // a stale/out-of-order event with smaller counts
	lo.BytesOut, lo.BytesIn = 10, 20
	r.Record(lo)

	f := r.Flows("box-a")[0]
	if f.BytesOut != 1000 || f.BytesIn != 2000 {
		t.Errorf("counters regressed: out=%d in=%d", f.BytesOut, f.BytesIn)
	}
}

func TestRecorderEvictsLeastRecent(t *testing.T) {
	r, _ := newTestRecorder(t, 2)
	r.Register("box-a", "172.16.5.2")
	mk := func(port int) Event {
		return Event{Proto: "tcp", SrcIP: "172.16.5.2", SrcPort: port, DstIP: "10.0.0.1", DstPort: 443}
	}
	r.Record(mk(1)) // LastSeen 1
	r.Record(mk(2)) // LastSeen 2
	r.Record(mk(3)) // LastSeen 3 -> evicts port 1 (oldest)

	flows := r.Flows("box-a")
	if len(flows) != 2 {
		t.Fatalf("want 2 flows after eviction, got %d", len(flows))
	}
	for _, f := range flows {
		if f.SrcPort == 1 {
			t.Errorf("oldest flow (port 1) should have been evicted")
		}
	}
}

func TestRecorderFlowsSortedByLastSeen(t *testing.T) {
	r, _ := newTestRecorder(t, 0)
	r.Register("box-a", "172.16.5.2")
	r.Record(Event{Proto: "tcp", SrcIP: "172.16.5.2", SrcPort: 1, DstIP: "10.0.0.1", DstPort: 80})
	r.Record(Event{Proto: "tcp", SrcIP: "172.16.5.2", SrcPort: 2, DstIP: "10.0.0.2", DstPort: 80})
	flows := r.Flows("box-a")
	if len(flows) != 2 || flows[0].SrcPort != 2 {
		t.Fatalf("want newest (port 2) first, got %+v", flows)
	}
}

func TestRecorderUnregisterDropsFlows(t *testing.T) {
	r, _ := newTestRecorder(t, 0)
	r.Register("box-a", "172.16.5.2")
	r.Record(Event{Proto: "tcp", SrcIP: "172.16.5.2", DstIP: "1.1.1.1", DstPort: 443})
	r.Unregister("box-a")
	if got := r.Flows("box-a"); got != nil {
		t.Fatalf("want flows dropped after unregister, got %v", got)
	}
	// A later event for the freed IP must not resurrect the box's table.
	r.Record(Event{Proto: "tcp", SrcIP: "172.16.5.2", DstIP: "1.1.1.1", DstPort: 443})
	if got := r.Flows("box-a"); got != nil {
		t.Fatalf("unregistered IP should be dropped, got %v", got)
	}
}

func TestRecorderReusedSlotStartsFresh(t *testing.T) {
	r, _ := newTestRecorder(t, 0)
	r.Register("box-a", "172.16.5.2")
	r.Record(Event{Proto: "tcp", SrcIP: "172.16.5.2", SrcPort: 1, DstIP: "1.1.1.1", DstPort: 443})
	// The pool slot's IP is reassigned to a new box; box-a's flows must not leak
	// into box-b.
	r.Register("box-b", "172.16.5.2")
	if got := r.Flows("box-a"); got != nil {
		t.Errorf("old box flows should be dropped on slot reuse, got %v", got)
	}
	r.Record(Event{Proto: "tcp", SrcIP: "172.16.5.2", SrcPort: 2, DstIP: "2.2.2.2", DstPort: 443})
	if got := r.Flows("box-b"); len(got) != 1 || got[0].DstIP != "2.2.2.2" {
		t.Errorf("new box should record its own flow, got %v", got)
	}
}

func TestRecorderIgnoresEmptyRegistration(t *testing.T) {
	r, _ := newTestRecorder(t, 0)
	r.Register("", "1.2.3.4")
	r.Register("box", "")
	r.Record(Event{Proto: "tcp", SrcIP: "1.2.3.4", DstIP: "9.9.9.9", DstPort: 443})
	if got := r.Flows("box"); got != nil {
		t.Fatalf("want nothing recorded, got %v", got)
	}
}
