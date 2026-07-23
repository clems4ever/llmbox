package spoke

import (
	"context"
	"testing"

	"github.com/clems4ever/llmbox/internal/shared/cluster"
	"github.com/clems4ever/llmbox/internal/spoke/dnsd"
)

// TestAuditDomain checks query-name normalisation for the audit trail.
func TestAuditDomain(t *testing.T) {
	for in, want := range map[string]string{"GitHub.com.": "github.com", "api.openai.com": "api.openai.com", "X.": "x"} {
		if got := auditDomain(in); got != want {
			t.Errorf("auditDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDNSAuditForwarderRecordDoesNotBlock checks Record never blocks even when the
// hub caller is detached and the buffer fills — audit forwarding must never slow
// DNS resolution.
func TestDNSAuditForwarderRecordDoesNotBlock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newDNSAuditForwarder(ctx, cluster.NewHubCaller()) // detached caller
	// Far more events than the buffer; must return promptly, dropping the excess.
	for i := 0; i < 5000; i++ {
		f.Record(dnsd.Event{BoxID: "web", QName: "github.com.", Verdict: dnsd.VerdictBlocked})
	}
}
