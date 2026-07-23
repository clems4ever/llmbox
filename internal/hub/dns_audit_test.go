package hub

import (
	"net/http"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/hub/store"
)

// TestDNSAuditListAndAddToGroup checks listing audit rows (with a verdict filter)
// and adding a blocked domain to a brand-new group.
func TestDNSAuditListAndAddToGroup(t *testing.T) {
	s, _, st := newAdminServer(t)
	c := signIn(t, st, true, false)

	now := time.Unix(1700000000, 0).UTC()
	mustRecord(t, st, "web", "registry.npmjs.org", "blocked", now)
	mustRecord(t, st, "web", "github.com", "allowed", now.Add(time.Minute))

	var listed struct {
		Entries []dnsAuditEntryView `json:"entries"`
	}
	if code := postAllowlist(t, s, c, "/api/v1/list-dns-audit", map[string]any{"verdict": "blocked"}, &listed); code != http.StatusOK {
		t.Fatalf("list-dns-audit = %d", code)
	}
	if len(listed.Entries) != 1 || listed.Entries[0].Domain != "registry.npmjs.org" {
		t.Fatalf("blocked entries = %+v", listed.Entries)
	}

	// Add the blocked domain to a new group.
	var added struct {
		Group allowlistGroupView `json:"group"`
	}
	if code := postAllowlist(t, s, c, "/api/v1/add-domain-to-group", map[string]any{
		"domain": "registry.npmjs.org", "new_group_name": "node-pkgs",
	}, &added); code != http.StatusOK {
		t.Fatalf("add-domain-to-group = %d", code)
	}
	if added.Group.ID != "node-pkgs" || len(added.Group.Domains) != 1 {
		t.Fatalf("created group = %+v", added.Group)
	}
	if g, ok, _ := st.GetAllowlistGroup("node-pkgs"); !ok || g.Domains[0] != "registry.npmjs.org" {
		t.Fatalf("stored group = %+v ok=%v", g, ok)
	}
}

// TestAddDomainToExistingGroup checks a domain is merged into an existing group.
func TestAddDomainToExistingGroup(t *testing.T) {
	s, _, st := newAdminServer(t)
	c := signIn(t, st, true, false)
	mustSave(t, st, store.AllowlistGroup{ID: "gh", Name: "gh", TTLSeconds: 30, Domains: []string{"github.com"}})

	if code := postAllowlist(t, s, c, "/api/v1/add-domain-to-group", map[string]any{
		"domain": "api.github.com", "group_id": "gh",
	}, nil); code != http.StatusOK {
		t.Fatalf("add-domain-to-group = %d", code)
	}
	g, _, _ := st.GetAllowlistGroup("gh")
	if len(g.Domains) != 2 {
		t.Fatalf("merged domains = %v, want 2", g.Domains)
	}
}

// mustRecord records a DNS lookup directly, failing the test on error.
func mustRecord(t *testing.T, st Store, box, domain, verdict string, at time.Time) {
	t.Helper()
	if err := st.RecordDNSLookup(box, domain, verdict, at); err != nil {
		t.Fatalf("RecordDNSLookup: %v", err)
	}
}
