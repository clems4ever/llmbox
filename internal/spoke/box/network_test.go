package box_test

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/internal/spoke/box"
)

// auditInst is a box.Instance that also implements box.NetworkAuditor, so the
// Manager's optional-capability path can be exercised.
type auditInst struct {
	meta  sandbox.Box
	flows []sandbox.NetworkFlow
	err   error
}

func (a *auditInst) Meta() sandbox.Box                         { return a.meta }
func (a *auditInst) Control(context.Context) (net.Conn, error) { return nil, errors.New("no control") }
func (a *auditInst) MarkReady(context.Context) error           { return nil }
func (a *auditInst) Pause(context.Context) error               { return nil }
func (a *auditInst) Resume(context.Context) error              { return nil }
func (a *auditInst) Destroy(context.Context) error             { return nil }
func (a *auditInst) NetworkFlows(context.Context) ([]sandbox.NetworkFlow, error) {
	return a.flows, a.err
}

func TestBoxManagerNetworkFlows(t *testing.T) {
	want := []sandbox.NetworkFlow{{Proto: "tcp", DstIP: "1.1.1.1", DstPort: 443, BytesOut: 10, BytesIn: 20}}
	m := box.NewManager(&stubProv{findInst: &auditInst{flows: want}}, box.Config{})
	got, err := m.NetworkFlows(context.Background(), "box-a")
	if err != nil {
		t.Fatalf("NetworkFlows: %v", err)
	}
	if len(got) != 1 || got[0].DstIP != "1.1.1.1" || got[0].BytesIn != 20 {
		t.Fatalf("wrong flows: %+v", got)
	}
}

// TestBoxManagerNetworkFlowsUnaudited verifies a backend that does not implement
// the auditor capability yields no flows rather than an error.
func TestBoxManagerNetworkFlowsUnaudited(t *testing.T) {
	m := box.NewManager(&stubProv{findInst: &stubInst{}}, box.Config{})
	got, err := m.NetworkFlows(context.Background(), "box-a")
	if err != nil {
		t.Fatalf("NetworkFlows should not error for an unaudited backend: %v", err)
	}
	if got != nil {
		t.Fatalf("want no flows for unaudited backend, got %+v", got)
	}
}

// TestBoxManagerNetworkFlowsNotFound surfaces the resolve error when no box
// matches.
func TestBoxManagerNetworkFlowsNotFound(t *testing.T) {
	m := box.NewManager(&stubProv{findErr: sandbox.ErrBoxNotFound}, box.Config{})
	if _, err := m.NetworkFlows(context.Background(), "missing"); !errors.Is(err, sandbox.ErrBoxNotFound) {
		t.Fatalf("want ErrBoxNotFound, got %v", err)
	}
}
