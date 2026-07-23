package hub

import (
	"net/http"
	"time"

	"github.com/clems4ever/llmbox/internal/hub/store"
)

// registerDNSAuditRoutes mounts the DNS-audit routes: listing the lookups boxes
// made under network isolation, and the one-click "add a blocked domain to a
// group" action. Both are gated like the rest of the box-control API.
//
// @arg mux The mux to register the routes on.
//
// @testcase TestDNSAuditListAndAddToGroup lists audit rows and adds a domain to a group.
func (s *Server) registerDNSAuditRoutes(mux *http.ServeMux) {
	gate := func(h http.HandlerFunc) http.Handler { return s.requireAPIAuth(h) }
	mux.Handle("POST /api/v1/list-dns-audit", gate(s.handleListDNSAudit))
	mux.Handle("POST /api/v1/add-domain-to-group", gate(s.handleAddDomainToGroup))
}

// dnsAuditEntryView is the wire form of one audit row.
type dnsAuditEntryView struct {
	BoxID    string `json:"box_id"`
	Domain   string `json:"domain"`
	Verdict  string `json:"verdict"`
	Hits     int64  `json:"hits"`
	LastSeen int64  `json:"last_seen"` // unix seconds, for the UI's relative time
}

// handleListDNSAudit answers POST /api/v1/list-dns-audit with the aggregated
// lookups matching the (optional) box / verdict / domain filter.
//
// @arg w The response writer the JSON rows are written to.
// @arg r The request carrying the filter.
//
// @testcase TestDNSAuditListAndAddToGroup lists and filters audit rows.
func (s *Server) handleListDNSAudit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BoxID   string `json:"box_id"`
		Verdict string `json:"verdict"`
		Domain  string `json:"domain"`
		Limit   int    `json:"limit"`
	}
	if !decodeAllowlistBody(w, r, &req) {
		return
	}
	entries, err := s.store.ListDNSAudit(store.DNSAuditFilter{
		BoxID: req.BoxID, Verdict: req.Verdict, Domain: req.Domain, Limit: req.Limit,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "listing dns audit: "+err.Error())
		return
	}
	views := make([]dnsAuditEntryView, 0, len(entries))
	for _, e := range entries {
		views = append(views, dnsAuditEntryView{
			BoxID: e.BoxID, Domain: e.Domain, Verdict: e.Verdict, Hits: e.Hits,
			LastSeen: e.LastSeen.Unix(),
		})
	}
	writeAllowlistJSON(w, map[string]any{"entries": views})
}

// handleAddDomainToGroup answers POST /api/v1/add-domain-to-group: it adds a
// domain (typically one that was just blocked in the audit view) to an existing
// group, or creates a new group holding it, then re-pushes policy so the change
// takes effect. This is the fast loop from the audit view to an allow.
//
// @arg w The response writer the updated/created group is written to.
// @arg r The request carrying the domain and the target group id or new name.
//
// @testcase TestDNSAuditListAndAddToGroup adds a blocked domain to a new group.
// @testcase TestAddDomainToExistingGroup merges a domain into an existing group.
func (s *Server) handleAddDomainToGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Domain       string `json:"domain"`
		GroupID      string `json:"group_id"`
		NewGroupName string `json:"new_group_name"`
	}
	if !decodeAllowlistBody(w, r, &req) {
		return
	}
	domains, err := normalizeDomains([]string{req.Domain})
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(domains) == 0 {
		writeJSONError(w, http.StatusBadRequest, "domain is required")
		return
	}
	domain := domains[0]
	now := time.Now().UTC()

	var g store.AllowlistGroup
	if req.GroupID != "" {
		existing, ok, err := s.store.GetAllowlistGroup(req.GroupID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "loading group: "+err.Error())
			return
		}
		if !ok {
			writeJSONError(w, http.StatusNotFound, "no group with id "+req.GroupID)
			return
		}
		g = existing
		g.Domains = unionSorted(existing.Domains, []string{domain})
		g.UpdatedAt = now
	} else {
		name := req.NewGroupName
		if name == "" {
			writeJSONError(w, http.StatusBadRequest, "group_id or new_group_name is required")
			return
		}
		id := slugify(name)
		if id == "" {
			writeJSONError(w, http.StatusBadRequest, "new_group_name must contain a letter or digit")
			return
		}
		if _, exists, err := s.store.GetAllowlistGroup(id); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "checking group: "+err.Error())
			return
		} else if exists {
			writeJSONError(w, http.StatusConflict, "a group named "+name+" already exists")
			return
		}
		g = store.AllowlistGroup{
			ID: id, Name: name, TTLSeconds: defaultAllowlistTTLSeconds,
			Domains: []string{domain}, CreatedAt: now, UpdatedAt: now,
		}
	}
	if err := s.store.SaveAllowlistGroup(g); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "saving group: "+err.Error())
		return
	}
	s.pushAllPolicies(r.Context())
	counts, _ := s.groupBoxCounts()
	writeAllowlistJSON(w, map[string]any{"group": groupView(g, counts[g.ID])})
}
