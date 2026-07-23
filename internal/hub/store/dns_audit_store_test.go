package store

import (
	"path/filepath"
	"testing"
	"time"
)

// TestDNSAuditStoreRoundTrip checks lookups aggregate by (box, domain, verdict),
// filter by box and verdict, order by recency, and delete per box.
func TestDNSAuditStoreRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	base := time.Unix(1700000000, 0).UTC()
	// Two lookups of the same triple aggregate into hits=2 with the later last-seen.
	if err := st.RecordDNSLookup("web", "github.com", "allowed", base); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := st.RecordDNSLookup("web", "github.com", "allowed", base.Add(time.Minute)); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := st.RecordDNSLookup("web", "evil.com", "blocked", base.Add(2*time.Minute)); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := st.RecordDNSLookup("data", "pypi.org", "allowed", base.Add(3*time.Minute)); err != nil {
		t.Fatalf("record: %v", err)
	}

	all, err := st.ListDNSAudit(DNSAuditFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("rows = %d, want 3", len(all))
	}
	// Newest last-seen first: data/pypi.org.
	if all[0].BoxID != "data" || all[0].Domain != "pypi.org" {
		t.Errorf("first row = %+v, want data/pypi.org", all[0])
	}

	// Filter by box.
	web, _ := st.ListDNSAudit(DNSAuditFilter{BoxID: "web"})
	if len(web) != 2 {
		t.Fatalf("web rows = %d, want 2", len(web))
	}
	// Filter by verdict.
	blocked, _ := st.ListDNSAudit(DNSAuditFilter{Verdict: "blocked"})
	if len(blocked) != 1 || blocked[0].Domain != "evil.com" || blocked[0].Hits != 1 {
		t.Fatalf("blocked = %+v", blocked)
	}
	// The aggregated github.com row has hits=2.
	gh, _ := st.ListDNSAudit(DNSAuditFilter{BoxID: "web", Verdict: "allowed"})
	if len(gh) != 1 || gh[0].Hits != 2 {
		t.Fatalf("github row = %+v, want hits=2", gh)
	}

	// Delete per box.
	if err := st.DeleteDNSAuditForBox("web"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if rows, _ := st.ListDNSAudit(DNSAuditFilter{}); len(rows) != 1 {
		t.Fatalf("after delete rows = %d, want 1", len(rows))
	}
}
