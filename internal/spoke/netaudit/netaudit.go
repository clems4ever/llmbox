// Package netaudit records per-box outbound network flow metadata on a spoke,
// built from the host kernel's connection-tracking table (conntrack) rather than
// from packet payloads. It exists because llmbox routes box egress with kernel
// primitives (a per-box TAP + NAT for Firecracker, a per-box bridge for Docker),
// so the datapath itself never passes through llmbox — but conntrack exposes every
// flow as metadata the spoke can attribute back to a box by its source address.
//
// The design is deliberately observe-only and metadata-only: it answers "which
// box talked to what, and how much data moved" for an audit view, and never
// carries or inspects payloads. A Recorder holds a small, bounded ring of flows
// per box (evicting the least-recently-seen), so it is safe to run for the life of
// the spoke without unbounded growth, and cannot itself become a data-exfiltration
// surface.
//
// A Recorder is the pure, backend-neutral core: a Source (see conntrack.go) feeds
// it Events and a backend registers each box's egress IP, so the same recorder
// serves Firecracker and Docker boxes alike.
package netaudit

import (
	"sort"
	"sync"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// DefaultPerBoxFlows is the number of distinct flows retained per box. It bounds
// memory and keeps the audit view to a recent, useful window; the least-recently-
// seen flow is evicted once the cap is reached.
const DefaultPerBoxFlows = 256

// Event is one observation of a box-originated flow, as decoded from a conntrack
// event. It is the original-direction tuple (the box is the source), plus the
// accounting counters conntrack reports for each direction. BytesOut/BytesIn are
// cumulative for the connection, so a later event for the same flow supersedes an
// earlier one rather than adding to it.
type Event struct {
	// Proto is the L4 protocol: "tcp", "udp", or "icmp".
	Proto string
	// SrcIP is the box-side (original-direction) source address; it is what the
	// flow is attributed to a box by.
	SrcIP string
	// SrcPort is the box-side source port (0 for icmp).
	SrcPort int
	// DstIP is the destination the box connected out to.
	DstIP string
	// DstPort is the destination port (0 for icmp).
	DstPort int
	// State is the conntrack TCP state (e.g. ESTABLISHED, CLOSE), or empty.
	State string
	// BytesOut/BytesIn are cumulative bytes in the original/reply directions. They
	// are reported by conntrack only when flow accounting is enabled (and are most
	// complete on the DESTROY event); zero when not yet known.
	BytesOut uint64
	BytesIn  uint64
	// Closed marks a terminal (DESTROY) event, so the recorder can keep the final
	// counters even though the connection is gone.
	Closed bool
}

// flowKey identifies one connection within a box: the 4-tuple minus the box IP
// (which is implied by the box the table belongs to). Aggregating on it means the
// NEW/UPDATE/DESTROY events of a single connection collapse to one flow.
type flowKey struct {
	proto   string
	srcPort int
	dstIP   string
	dstPort int
}

// flowTable is one box's bounded set of flows, keyed for aggregation and capped.
type flowTable struct {
	cap   int
	flows map[flowKey]*sandbox.NetworkFlow
}

// Recorder attributes conntrack events to boxes by source IP and keeps a bounded,
// aggregated ring of flows per box. It is safe for concurrent use: a Source's
// collector goroutine calls Record while the control path calls Flows.
type Recorder struct {
	mu      sync.Mutex
	perBox  int
	byIP    map[string]string // egress IP -> box ID
	tables  map[string]*flowTable
	nowFunc func() time.Time
}

// NewRecorder returns a Recorder retaining up to perBox flows per box (<=0 uses
// DefaultPerBoxFlows).
//
// @arg perBox The number of flows retained per box, or <=0 for the default.
// @return *Recorder A ready recorder with no boxes registered.
//
// @testcase TestRecorderEvictsLeastRecent caps and evicts per the configured size.
func NewRecorder(perBox int) *Recorder {
	if perBox <= 0 {
		perBox = DefaultPerBoxFlows
	}
	return &Recorder{
		perBox:  perBox,
		byIP:    map[string]string{},
		tables:  map[string]*flowTable{},
		nowFunc: time.Now,
	}
}

// Register maps a box's egress IP to its box ID so later events from that address
// are attributed to it. Registering is idempotent and may be called again to point
// an IP at a different box (a reused pool slot), which starts that box's flows
// fresh.
//
// @arg boxID The box the IP belongs to.
// @arg ip The box's egress (original-direction) source IP.
//
// @testcase TestRecorderAttributesBySrcIP attributes events to the registered box.
func (r *Recorder) Register(boxID, ip string) {
	if boxID == "" || ip == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// If the IP was pointing at a different box (a recycled slot), drop that box's
	// stale flows so they are not misattributed to the newcomer.
	if prev, ok := r.byIP[ip]; ok && prev != boxID {
		delete(r.tables, prev)
	}
	r.byIP[ip] = boxID
	if _, ok := r.tables[boxID]; !ok {
		r.tables[boxID] = &flowTable{cap: r.perBox, flows: map[flowKey]*sandbox.NetworkFlow{}}
	}
}

// Unregister forgets a box: its IP mapping and recorded flows are dropped. It is
// called when a box is destroyed or its pool slot is freed.
//
// @arg boxID The box to forget.
//
// @testcase TestRecorderUnregisterDropsFlows drops a box's flows and mapping.
func (r *Recorder) Unregister(boxID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ip, id := range r.byIP {
		if id == boxID {
			delete(r.byIP, ip)
		}
	}
	delete(r.tables, boxID)
}

// Record attributes one event to a box by its source IP and folds it into that
// box's flow table. An event whose source IP is not registered to a box is
// dropped, so host and cross-box traffic never lands in a box's audit view.
//
// @arg ev The decoded conntrack event.
//
// @testcase TestRecorderAggregatesConnection collapses a connection's events into one flow.
// @testcase TestRecorderDropsUnknownSource ignores events from unregistered IPs.
func (r *Recorder) Record(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	boxID, ok := r.byIP[ev.SrcIP]
	if !ok {
		return
	}
	tbl := r.tables[boxID]
	if tbl == nil {
		tbl = &flowTable{cap: r.perBox, flows: map[flowKey]*sandbox.NetworkFlow{}}
		r.tables[boxID] = tbl
	}
	now := r.nowFunc().Unix()
	key := flowKey{proto: ev.Proto, srcPort: ev.SrcPort, dstIP: ev.DstIP, dstPort: ev.DstPort}
	f, ok := tbl.flows[key]
	if !ok {
		if len(tbl.flows) >= tbl.cap {
			tbl.evictOldest()
		}
		f = &sandbox.NetworkFlow{
			Proto:     ev.Proto,
			DstIP:     ev.DstIP,
			DstPort:   ev.DstPort,
			SrcPort:   ev.SrcPort,
			FirstSeen: now,
		}
		tbl.flows[key] = f
	}
	f.LastSeen = now
	if ev.State != "" {
		f.State = ev.State
	}
	// Counters are cumulative for the connection, so keep the largest seen — a
	// DESTROY carries the final totals, and out-of-order or partial updates never
	// make a flow's byte count go backwards.
	if ev.BytesOut > f.BytesOut {
		f.BytesOut = ev.BytesOut
	}
	if ev.BytesIn > f.BytesIn {
		f.BytesIn = ev.BytesIn
	}
	if ev.Closed && ev.State == "" {
		f.State = "CLOSE"
	}
}

// Flows returns a snapshot of the box's recorded flows, most-recently-seen first.
// It returns nil for an unknown box, so a caller cannot tell an unregistered box
// from one with no traffic yet — both simply have no flows.
//
// @arg boxID The box whose flows to return.
// @return []sandbox.NetworkFlow The box's flows, newest first (nil if none).
//
// @testcase TestRecorderFlowsSortedByLastSeen returns flows newest first.
func (r *Recorder) Flows(boxID string) []sandbox.NetworkFlow {
	r.mu.Lock()
	defer r.mu.Unlock()
	tbl := r.tables[boxID]
	if tbl == nil || len(tbl.flows) == 0 {
		return nil
	}
	out := make([]sandbox.NetworkFlow, 0, len(tbl.flows))
	for _, f := range tbl.flows {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastSeen != out[j].LastSeen {
			return out[i].LastSeen > out[j].LastSeen
		}
		// Stable tie-break so equal timestamps (common in fast tests) are deterministic.
		if out[i].DstIP != out[j].DstIP {
			return out[i].DstIP < out[j].DstIP
		}
		return out[i].SrcPort < out[j].SrcPort
	})
	return out
}

// evictOldest removes the least-recently-seen flow to make room under the cap. The
// caller holds the recorder lock.
func (t *flowTable) evictOldest() {
	var oldestKey flowKey
	var oldest int64
	first := true
	for k, f := range t.flows {
		if first || f.LastSeen < oldest {
			oldestKey, oldest, first = k, f.LastSeen, false
		}
	}
	if !first {
		delete(t.flows, oldestKey)
	}
}
