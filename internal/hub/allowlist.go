package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/clems4ever/llmbox/internal/hub/store"
)

// defaultAllowlistTTLSeconds is the resolved-IP pin window a group falls back to
// when it stores none: how long a DNS-resolved IP stays reachable after a lookup
// before it must be re-resolved. 30s bounds the exposure of an IP reallocated to
// an unrelated service while staying long enough for ordinary request bursts.
const defaultAllowlistTTLSeconds = 30

// allowlistGroupView is the wire form of an allowlist group returned to the UI:
// the stored group plus BoxCount, the number of boxes that explicitly assign it
// (a global group additionally applies to every box, which the UI renders as
// "all workspaces").
type allowlistGroupView struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	TTLSeconds  int       `json:"ttl_seconds"`
	IsGlobal    bool      `json:"is_global"`
	Domains     []string  `json:"domains"`
	BoxCount    int       `json:"box_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// registerAllowlistRoutes mounts the network-isolation allowlist config routes on
// mux, each behind the same API-auth gate as the box-control API (an API key or an
// admin login session). They are hub-local: a spoke never calls them, it receives
// the computed effective allowlist over the cluster transport.
//
// @arg mux The mux to register the allowlist routes on.
//
// @testcase TestAllowlistGroupsCRUD drives create/list/update/delete through these routes.
func (s *Server) registerAllowlistRoutes(mux *http.ServeMux) {
	gate := func(h http.HandlerFunc) http.Handler { return s.requireAPIAuth(h) }
	mux.Handle("POST /api/v1/list-allowlist-groups", gate(s.handleListAllowlistGroups))
	mux.Handle("POST /api/v1/save-allowlist-group", gate(s.handleSaveAllowlistGroup))
	mux.Handle("POST /api/v1/delete-allowlist-group", gate(s.handleDeleteAllowlistGroup))
	mux.Handle("POST /api/v1/get-box-allowlist", gate(s.handleGetBoxAllowlist))
	mux.Handle("POST /api/v1/set-box-groups", gate(s.handleSetBoxGroups))
	mux.Handle("POST /api/v1/export-allowlist-groups", gate(s.handleExportAllowlistGroups))
	mux.Handle("POST /api/v1/import-allowlist-groups", gate(s.handleImportAllowlistGroups))
}

// handleListAllowlistGroups answers POST /api/v1/list-allowlist-groups with every
// group and its explicit-assignment count.
//
// @arg w The response writer the JSON groups are written to.
// @arg r The request (no body).
//
// @testcase TestAllowlistGroupsCRUD lists groups after creating them.
func (s *Server) handleListAllowlistGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.store.ListAllowlistGroups()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "listing groups: "+err.Error())
		return
	}
	counts, err := s.groupBoxCounts()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "counting assignments: "+err.Error())
		return
	}
	views := make([]allowlistGroupView, 0, len(groups))
	for _, g := range groups {
		views = append(views, groupView(g, counts[g.ID]))
	}
	writeAllowlistJSON(w, map[string]any{"groups": views})
}

// handleSaveAllowlistGroup answers POST /api/v1/save-allowlist-group: it creates a
// group (empty id — an id is derived from the name) or updates an existing one
// (id present). The name must be non-empty and each domain valid.
//
// @arg w The response writer the saved group is written to.
// @arg r The request carrying the group fields.
//
// @testcase TestAllowlistGroupsCRUD creates then updates a group.
// @testcase TestAllowlistSaveRejectsBadInput rejects an empty name, a bad domain, and a duplicate name.
func (s *Server) handleSaveAllowlistGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		TTLSeconds  int      `json:"ttl_seconds"`
		IsGlobal    bool     `json:"is_global"`
		Domains     []string `json:"domains"`
	}
	if !decodeAllowlistBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "name is required")
		return
	}
	domains, err := normalizeDomains(req.Domains)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	ttl := req.TTLSeconds
	if ttl <= 0 {
		ttl = defaultAllowlistTTLSeconds
	}
	now := time.Now().UTC()

	g := store.AllowlistGroup{
		Name: name, Description: strings.TrimSpace(req.Description),
		TTLSeconds: ttl, IsGlobal: req.IsGlobal, Domains: domains, UpdatedAt: now,
	}
	if req.ID == "" {
		g.ID = slugify(name)
		if g.ID == "" {
			writeJSONError(w, http.StatusBadRequest, "name must contain at least one letter or digit")
			return
		}
		if _, exists, err := s.store.GetAllowlistGroup(g.ID); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "checking group: "+err.Error())
			return
		} else if exists {
			writeJSONError(w, http.StatusConflict, "a group named "+name+" already exists")
			return
		}
		g.CreatedAt = now
	} else {
		existing, ok, err := s.store.GetAllowlistGroup(req.ID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "loading group: "+err.Error())
			return
		}
		if !ok {
			writeJSONError(w, http.StatusNotFound, "no group with id "+req.ID)
			return
		}
		g.ID = req.ID
		g.CreatedAt = existing.CreatedAt
	}
	if err := s.store.SaveAllowlistGroup(g); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "saving group: "+err.Error())
		return
	}
	counts, _ := s.groupBoxCounts()
	writeAllowlistJSON(w, map[string]any{"group": groupView(g, counts[g.ID])})
}

// handleDeleteAllowlistGroup answers POST /api/v1/delete-allowlist-group.
//
// @arg w The response writer.
// @arg r The request carrying the group id.
//
// @testcase TestAllowlistGroupsCRUD deletes a group.
func (s *Server) handleDeleteAllowlistGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !decodeAllowlistBody(w, r, &req) {
		return
	}
	if req.ID == "" {
		writeJSONError(w, http.StatusBadRequest, "id is required")
		return
	}
	if err := s.store.DeleteAllowlistGroup(req.ID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "deleting group: "+err.Error())
		return
	}
	writeAllowlistJSON(w, struct{}{})
}

// handleGetBoxAllowlist answers POST /api/v1/get-box-allowlist: the groups a box
// reaches (its explicitly-assigned non-global groups plus every global group) and
// the flattened, deduplicated effective domain set.
//
// @arg w The response writer the box allowlist is written to.
// @arg r The request carrying the box id.
//
// @testcase TestBoxAllowlistAssignAndCompute assigns groups then reads the effective allowlist.
func (s *Server) handleGetBoxAllowlist(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BoxID string `json:"box_id"`
	}
	if !decodeAllowlistBody(w, r, &req) {
		return
	}
	if req.BoxID == "" {
		writeJSONError(w, http.StatusBadRequest, "box_id is required")
		return
	}
	groups, err := s.store.ListAllowlistGroups()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "listing groups: "+err.Error())
		return
	}
	assigned, err := s.store.GetBoxGroups(req.BoxID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "reading box groups: "+err.Error())
		return
	}
	domains, names := effectiveAllowlist(groups, assigned)
	writeAllowlistJSON(w, map[string]any{
		"box_id":            req.BoxID,
		"group_ids":         assigned,
		"effective_groups":  names,
		"effective_domains": domains,
	})
}

// handleSetBoxGroups answers POST /api/v1/set-box-groups, replacing the non-global
// groups assigned to a box. Unknown group ids are rejected so a typo can't
// silently assign nothing.
//
// @arg w The response writer.
// @arg r The request carrying the box id and group ids.
//
// @testcase TestBoxAllowlistAssignAndCompute sets a box's groups.
// @testcase TestSetBoxGroupsRejectsUnknown rejects an unknown group id.
func (s *Server) handleSetBoxGroups(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BoxID    string   `json:"box_id"`
		GroupIDs []string `json:"group_ids"`
	}
	if !decodeAllowlistBody(w, r, &req) {
		return
	}
	if req.BoxID == "" {
		writeJSONError(w, http.StatusBadRequest, "box_id is required")
		return
	}
	for _, id := range req.GroupIDs {
		if _, ok, err := s.store.GetAllowlistGroup(id); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "checking group: "+err.Error())
			return
		} else if !ok {
			writeJSONError(w, http.StatusBadRequest, "no group with id "+id)
			return
		}
	}
	if err := s.store.SetBoxGroups(req.BoxID, req.GroupIDs); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "setting box groups: "+err.Error())
		return
	}
	writeAllowlistJSON(w, struct{}{})
}

// allowlistBundle is the versioned, portable form of a set of groups for the
// export/import feature. It omits ids, assignments, and timestamps so a bundle is
// host-independent.
type allowlistBundle struct {
	Version int                   `json:"version"`
	Groups  []allowlistBundleItem `json:"groups"`
}

type allowlistBundleItem struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Domains     []string `json:"domains"`
	TTLSeconds  int      `json:"ttl_seconds,omitempty"`
	IsGlobal    bool     `json:"is_global,omitempty"`
}

const allowlistBundleVersion = 1

// handleExportAllowlistGroups answers POST /api/v1/export-allowlist-groups with a
// portable JSON bundle of every group (or only the ids requested).
//
// @arg w The response writer the bundle is written to.
// @arg r The request optionally carrying the ids to export.
//
// @testcase TestAllowlistExportImport exports groups then re-imports them.
func (s *Server) handleExportAllowlistGroups(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if !decodeAllowlistBody(w, r, &req) {
		return
	}
	groups, err := s.store.ListAllowlistGroups()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "listing groups: "+err.Error())
		return
	}
	want := map[string]bool{}
	for _, id := range req.IDs {
		want[id] = true
	}
	bundle := allowlistBundle{Version: allowlistBundleVersion}
	for _, g := range groups {
		if len(want) > 0 && !want[g.ID] {
			continue
		}
		bundle.Groups = append(bundle.Groups, allowlistBundleItem{
			Name: g.Name, Description: g.Description, Domains: g.Domains,
			TTLSeconds: g.TTLSeconds, IsGlobal: g.IsGlobal,
		})
	}
	writeAllowlistJSON(w, bundle)
}

// handleImportAllowlistGroups answers POST /api/v1/import-allowlist-groups: it adds
// the bundle's groups. On a name conflict, mode "merge" (the default) unions the
// domains into the existing group and mode "replace" overwrites it.
//
// @arg w The response writer the import count is written to.
// @arg r The request carrying the bundle and conflict mode.
//
// @testcase TestAllowlistExportImport re-imports an exported bundle.
// @testcase TestAllowlistImportMerge merges domains into an existing group.
func (s *Server) handleImportAllowlistGroups(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Bundle allowlistBundle `json:"bundle"`
		Mode   string          `json:"mode"`
	}
	if !decodeAllowlistBody(w, r, &req) {
		return
	}
	if req.Bundle.Version != allowlistBundleVersion {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("unsupported bundle version %d", req.Bundle.Version))
		return
	}
	now := time.Now().UTC()
	imported := 0
	for _, item := range req.Bundle.Groups {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			writeJSONError(w, http.StatusBadRequest, "bundle group is missing a name")
			return
		}
		domains, err := normalizeDomains(item.Domains)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, name+": "+err.Error())
			return
		}
		ttl := item.TTLSeconds
		if ttl <= 0 {
			ttl = defaultAllowlistTTLSeconds
		}
		id := slugify(name)
		existing, ok, err := s.store.GetAllowlistGroup(id)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "checking group: "+err.Error())
			return
		}
		g := store.AllowlistGroup{
			ID: id, Name: name, Description: strings.TrimSpace(item.Description),
			TTLSeconds: ttl, IsGlobal: item.IsGlobal, Domains: domains,
			CreatedAt: now, UpdatedAt: now,
		}
		if ok {
			g.CreatedAt = existing.CreatedAt
			if req.Mode != "replace" {
				g.Domains = unionSorted(existing.Domains, domains)
				g.Description = existing.Description
				g.TTLSeconds = existing.TTLSeconds
				g.IsGlobal = existing.IsGlobal
			}
		}
		if err := s.store.SaveAllowlistGroup(g); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "importing group: "+err.Error())
			return
		}
		imported++
	}
	writeAllowlistJSON(w, map[string]any{"imported": imported})
}

// groupBoxCounts returns, per group id, how many boxes explicitly assign it.
//
// @return map[string]int The explicit-assignment count per group id.
// @error error if reading the assignments fails.
func (s *Server) groupBoxCounts() (map[string]int, error) {
	assignments, err := s.store.ListBoxGroups()
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, gids := range assignments {
		for _, gid := range gids {
			counts[gid]++
		}
	}
	return counts, nil
}

// groupView projects a stored group plus its box count onto the wire form.
//
// @arg g The stored group.
// @arg boxCount The number of boxes that explicitly assign it.
// @return allowlistGroupView The wire form.
func groupView(g store.AllowlistGroup, boxCount int) allowlistGroupView {
	return allowlistGroupView{
		ID: g.ID, Name: g.Name, Description: g.Description, TTLSeconds: g.TTLSeconds,
		IsGlobal: g.IsGlobal, Domains: g.Domains, BoxCount: boxCount,
		CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt,
	}
}

// effectiveAllowlist flattens the groups reaching a box — every global group plus
// the box's explicitly-assigned groups — into a sorted, deduplicated domain set
// and the sorted set of contributing group names.
//
// @arg groups Every stored group.
// @arg assigned The box's explicitly-assigned group ids.
// @return []string The deduplicated, sorted effective domains.
// @return []string The sorted names of the contributing groups.
//
// @testcase TestEffectiveAllowlist unions global and assigned groups and dedupes domains.
func effectiveAllowlist(groups []store.AllowlistGroup, assigned []string) ([]string, []string) {
	assignedSet := map[string]bool{}
	for _, id := range assigned {
		assignedSet[id] = true
	}
	domainSet := map[string]bool{}
	var names []string
	for _, g := range groups {
		if !g.IsGlobal && !assignedSet[g.ID] {
			continue
		}
		names = append(names, g.Name)
		for _, d := range g.Domains {
			domainSet[d] = true
		}
	}
	domains := make([]string, 0, len(domainSet))
	for d := range domainSet {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	sort.Strings(names)
	return domains, names
}

// domainPattern matches a valid egress host: a dotted name, optionally with a
// single leading "*." wildcard label. Labels are letters/digits/hyphens.
var domainPattern = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}$`)

// normalizeDomains lowercases, trims, validates, and deduplicates a domain list,
// returning them sorted. A malformed or empty entry is an error so a group can
// never store an unenforceable pattern.
//
// @arg in The raw domains from the request.
// @return []string The cleaned, sorted, deduplicated domains.
// @error error if any entry is not a valid host or wildcard.
//
// @testcase TestNormalizeDomains cleans, dedupes, and rejects malformed domains.
func normalizeDomains(in []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, raw := range in {
		d := strings.ToLower(strings.TrimSpace(raw))
		if d == "" {
			continue
		}
		if !domainPattern.MatchString(d) {
			return nil, fmt.Errorf("invalid domain %q", raw)
		}
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	sort.Strings(out)
	return out, nil
}

// slugRe collapses any run of non-alphanumeric characters into a single hyphen.
var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// slugify derives a stable kebab-case id from a group name.
//
// @arg name The human-facing group name.
// @return string The slug id (empty when name has no alphanumerics).
//
// @testcase TestSlugify slugs names and drops leading/trailing separators.
func slugify(name string) string {
	s := slugRe.ReplaceAllString(strings.ToLower(name), "-")
	return strings.Trim(s, "-")
}

// unionSorted returns the sorted, deduplicated union of two domain slices.
//
// @arg a The first domain slice.
// @arg b The second domain slice.
// @return []string The sorted, deduplicated union.
func unionSorted(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string{}, a...), b...) {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// decodeAllowlistBody decodes the JSON request body into dst, writing a 400 and
// returning false when the body is not valid JSON. An empty body leaves dst at its
// zero value (for the no-argument routes).
//
// @arg w The response writer a decode error is written to.
// @arg r The request whose body is decoded.
// @arg dst The pointer the body is decoded into.
// @return bool True when decoding succeeded (or the body was empty), false otherwise.
func decodeAllowlistBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.ContentLength == 0 {
		return true
	}
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeJSONError(w, http.StatusBadRequest, "decoding request: "+err.Error())
		return false
	}
	return true
}

// writeAllowlistJSON encodes v as an uncacheable JSON response. The allowlist is
// live control-plane state no intermediary may cache.
//
// @arg w The response writer.
// @arg v The value to encode.
func writeAllowlistJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
