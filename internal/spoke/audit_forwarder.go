package spoke

import (
	"context"
	"strings"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/cluster"
	"github.com/clems4ever/llmbox/internal/spoke/dnsd"
)

// dnsAuditForwarder is a dnsd.AuditSink that ships each DNS lookup to the hub for
// the audit trail, over the spoke's live cluster connection. It buffers events
// and forwards them from a single background goroutine, so Record never blocks
// the DNS query path and a hub hiccup drops audit rows (best-effort) rather than
// slowing resolution. The buffer is bounded; when it is full, new events are
// dropped rather than backing up.
type dnsAuditForwarder struct {
	caller *cluster.HubCaller
	events chan dnsd.Event
}

// newDNSAuditForwarder starts a forwarder shipping to caller until ctx is done.
//
// @arg ctx Stops the forwarder when the spoke shuts down.
// @arg caller The hub caller to forward audit events over.
// @return *dnsAuditForwarder A running forwarder.
//
// @testcase TestDNSAuditForwarderForwards forwards a recorded event to the caller.
func newDNSAuditForwarder(ctx context.Context, caller *cluster.HubCaller) *dnsAuditForwarder {
	f := &dnsAuditForwarder{caller: caller, events: make(chan dnsd.Event, 512)}
	go f.run(ctx)
	return f
}

// Record enqueues an event, dropping it if the buffer is full so the query path
// is never blocked by audit forwarding.
//
// @arg ev The lookup event.
func (f *dnsAuditForwarder) Record(ev dnsd.Event) {
	select {
	case f.events <- ev:
	default: // buffer full: drop rather than block DNS resolution
	}
}

// run drains the buffer, forwarding each event to the hub, until ctx is done.
//
// @arg ctx Stops the loop.
func (f *dnsAuditForwarder) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-f.events:
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			// Best-effort: a disconnected hub or a rejected call just drops the row.
			_ = f.caller.RecordDNSAudit(cctx, ev.BoxID, auditDomain(ev.QName), string(ev.Verdict), ev.Time)
			cancel()
		}
	}
}

// auditDomain normalises a query name for the audit trail: lowercased, without
// the trailing root dot, so the UI shows "github.com" not "github.com.".
//
// @arg qname The raw query name.
// @return string The display domain.
func auditDomain(qname string) string {
	return strings.TrimSuffix(strings.ToLower(qname), ".")
}
