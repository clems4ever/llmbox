package firecracker

import (
	"context"
	"testing"

	"github.com/clems4ever/llmbox/internal/spoke/box"
	"github.com/clems4ever/llmbox/internal/spoke/netaudit"
)

// TestFirecrackerNetworkFlows checks a box instance surfaces the flows the
// provisioner's recorder attributed to its guest IP, via the optional
// box.NetworkAuditor capability.
func TestFirecrackerNetworkFlows(t *testing.T) {
	p := &Provisioner{recorder: netaudit.NewRecorder(0), netEnabled: true}
	// Slot 5's guest IP is what egress traffic from this box is sourced from.
	n := netFor(5)
	p.recorder.Register("box-a", n.GuestIP)
	p.recorder.Record(netaudit.Event{
		Proto: "tcp", SrcIP: n.GuestIP, SrcPort: 51000,
		DstIP: "140.82.121.4", DstPort: 443, State: "ESTABLISHED",
		BytesOut: 1420, BytesIn: 5300,
	})

	inst := &fcInstance{prov: p, meta: boxMeta{Token: "tok", BoxID: "box-a", NetIndex: 5}}
	auditor, ok := box.Instance(inst).(box.NetworkAuditor)
	if !ok {
		t.Fatal("fcInstance should implement box.NetworkAuditor")
	}
	flows, err := auditor.NetworkFlows(context.Background())
	if err != nil {
		t.Fatalf("NetworkFlows: %v", err)
	}
	if len(flows) != 1 || flows[0].DstIP != "140.82.121.4" || flows[0].DstPort != 443 || flows[0].BytesIn != 5300 {
		t.Fatalf("wrong flows: %+v", flows)
	}
}

// TestFirecrackerNetworkFlowsNoRecorder confirms an instance with no recorder
// (control-only spoke) reports no flows rather than panicking.
func TestFirecrackerNetworkFlowsNoRecorder(t *testing.T) {
	inst := &fcInstance{prov: &Provisioner{}, meta: boxMeta{BoxID: "box-a"}}
	flows, err := inst.NetworkFlows(context.Background())
	if err != nil || flows != nil {
		t.Fatalf("want no flows/err, got %v / %v", flows, err)
	}
}
