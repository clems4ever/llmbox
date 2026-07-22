package docker

import (
	"context"
	"testing"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/box"
	"github.com/clems4ever/llmbox/internal/spoke/netaudit"
)

// TestProvisionRegistersBoxIP verifies Provision registers the container's box-
// network IP with the recorder, so a later conntrack event from that address is
// attributed to the box and surfaced by the instance's NetworkFlows.
func TestProvisionRegistersBoxIP(t *testing.T) {
	f := &fakeDocker{inspectIP: "172.30.0.2"}
	p := newTestProvisioner(t, f)
	p.recorder = netaudit.NewRecorder(0)

	inst, err := p.Provision(context.Background(), sandbox.CreateOptions{BoxID: "my-box"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// A conntrack event from the registered IP must land in this box's flows.
	p.recorder.Record(netaudit.Event{Proto: "tcp", SrcIP: "172.30.0.2", SrcPort: 5000, DstIP: "1.1.1.1", DstPort: 443, State: "ESTABLISHED", BytesOut: 100, BytesIn: 200})

	auditor, ok := inst.(box.NetworkAuditor)
	if !ok {
		t.Fatal("docker instance should implement box.NetworkAuditor")
	}
	flows, err := auditor.NetworkFlows(context.Background())
	if err != nil {
		t.Fatalf("NetworkFlows: %v", err)
	}
	if len(flows) != 1 || flows[0].DstIP != "1.1.1.1" || flows[0].BytesIn != 200 {
		t.Fatalf("wrong flows: %+v", flows)
	}
}

// TestDockerNetworkFlowsWithoutRecorder confirms a provisioner without a recorder
// (the default in most tests) reports no flows rather than panicking.
func TestDockerNetworkFlowsWithoutRecorder(t *testing.T) {
	f := &fakeDocker{}
	p := newTestProvisioner(t, f)
	inst, err := p.Provision(context.Background(), sandbox.CreateOptions{BoxID: "b"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	flows, err := inst.(box.NetworkAuditor).NetworkFlows(context.Background())
	if err != nil || flows != nil {
		t.Fatalf("want no flows/err, got %v / %v", flows, err)
	}
}

func TestBoxNetworkIPPrefersBoxNetwork(t *testing.T) {
	f := &fakeDocker{inspectIP: "10.0.5.7"}
	info, _ := f.ContainerInspect(context.Background(), "0123456789abcdeffull")
	if ip := boxNetworkIP(info, boxNetworkName("0123456789abcdeffull")); ip != "10.0.5.7" {
		t.Fatalf("want the box-network IP, got %q", ip)
	}
	// No networks -> empty.
	empty, _ := (&fakeDocker{}).ContainerInspect(context.Background(), "0123456789abcdeffull")
	if ip := boxNetworkIP(empty, "whatever"); ip != "" {
		t.Fatalf("want empty IP when no networks, got %q", ip)
	}
}
