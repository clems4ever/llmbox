package hub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/clems4ever/llmbox/internal/hub/store"
)

// postAllowlist drives one POST with a JSON body to path as the signed-in admin,
// decoding the JSON response into out (when non-nil). It returns the status code.
func postAllowlist(t *testing.T, s *Server, c *http.Cookie, path string, body, out any) int {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(c)
	req.Header.Set(csrfHeader, "CSRF")
	rec := httptest.NewRecorder()
	s.APIHandler().ServeHTTP(rec, req)
	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("decode %s response %q: %v", path, rec.Body.String(), err)
		}
	}
	return rec.Code
}

// TestAllowlistGroupsCRUD drives a group through create, list, update, and delete
// over the HTTP API.
func TestAllowlistGroupsCRUD(t *testing.T) {
	s, _, st := newAdminServer(t)
	c := signIn(t, st, true, false)

	// Create (empty id -> derived slug).
	var created struct {
		Group allowlistGroupView `json:"group"`
	}
	code := postAllowlist(t, s, c, "/api/v1/save-allowlist-group", map[string]any{
		"name": "Core AI", "description": "LLM APIs", "is_global": true,
		"domains": []string{"api.openai.com", "API.OPENAI.COM", "api.anthropic.com"},
	}, &created)
	if code != http.StatusOK {
		t.Fatalf("create = %d", code)
	}
	if created.Group.ID != "core-ai" {
		t.Errorf("derived id = %q, want core-ai", created.Group.ID)
	}
	if created.Group.TTLSeconds != defaultAllowlistTTLSeconds {
		t.Errorf("default ttl = %d, want %d", created.Group.TTLSeconds, defaultAllowlistTTLSeconds)
	}
	// Domains are lowercased and deduplicated.
	if !reflect.DeepEqual(created.Group.Domains, []string{"api.anthropic.com", "api.openai.com"}) {
		t.Errorf("domains = %v", created.Group.Domains)
	}

	// List.
	var listed struct {
		Groups []allowlistGroupView `json:"groups"`
	}
	if code := postAllowlist(t, s, c, "/api/v1/list-allowlist-groups", struct{}{}, &listed); code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	if len(listed.Groups) != 1 || !listed.Groups[0].IsGlobal {
		t.Fatalf("list = %+v", listed.Groups)
	}

	// Update (id present) — flip global off and change TTL.
	code = postAllowlist(t, s, c, "/api/v1/save-allowlist-group", map[string]any{
		"id": "core-ai", "name": "Core AI", "ttl_seconds": 15, "is_global": false,
		"domains": []string{"api.openai.com"},
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("update = %d", code)
	}
	got, ok, err := st.GetAllowlistGroup("core-ai")
	if err != nil || !ok {
		t.Fatalf("GetAllowlistGroup: ok=%v err=%v", ok, err)
	}
	if got.IsGlobal || got.TTLSeconds != 15 || len(got.Domains) != 1 {
		t.Errorf("after update = %+v", got)
	}

	// Delete.
	if code := postAllowlist(t, s, c, "/api/v1/delete-allowlist-group", map[string]any{"id": "core-ai"}, nil); code != http.StatusOK {
		t.Fatalf("delete = %d", code)
	}
	if _, ok, _ := st.GetAllowlistGroup("core-ai"); ok {
		t.Errorf("group still present after delete")
	}
}

// TestAllowlistSaveRejectsBadInput checks an empty name, a malformed domain, and a
// duplicate name are all refused.
func TestAllowlistSaveRejectsBadInput(t *testing.T) {
	s, _, st := newAdminServer(t)
	c := signIn(t, st, true, false)

	if code := postAllowlist(t, s, c, "/api/v1/save-allowlist-group", map[string]any{"name": "  "}, nil); code != http.StatusBadRequest {
		t.Errorf("empty name = %d, want 400", code)
	}
	if code := postAllowlist(t, s, c, "/api/v1/save-allowlist-group", map[string]any{
		"name": "bad", "domains": []string{"http://nope.com/path"},
	}, nil); code != http.StatusBadRequest {
		t.Errorf("bad domain = %d, want 400", code)
	}
	// Create then attempt a duplicate name (same derived id).
	if code := postAllowlist(t, s, c, "/api/v1/save-allowlist-group", map[string]any{"name": "dup"}, nil); code != http.StatusOK {
		t.Fatalf("first create = %d", code)
	}
	if code := postAllowlist(t, s, c, "/api/v1/save-allowlist-group", map[string]any{"name": "Dup"}, nil); code != http.StatusConflict {
		t.Errorf("duplicate name = %d, want 409", code)
	}
}

// TestBoxAllowlistAssignAndCompute checks assigning groups to a box and reading
// back the effective (global ∪ assigned) allowlist.
func TestBoxAllowlistAssignAndCompute(t *testing.T) {
	s, _, st := newAdminServer(t)
	c := signIn(t, st, true, false)

	mustSave(t, st, store.AllowlistGroup{ID: "core", Name: "core", IsGlobal: true, Domains: []string{"api.anthropic.com"}, TTLSeconds: 30})
	mustSave(t, st, store.AllowlistGroup{ID: "gh", Name: "gh", Domains: []string{"github.com"}, TTLSeconds: 30})
	mustSave(t, st, store.AllowlistGroup{ID: "pypi", Name: "pypi", Domains: []string{"pypi.org"}, TTLSeconds: 30})

	if code := postAllowlist(t, s, c, "/api/v1/set-box-groups", map[string]any{
		"box_id": "web", "group_ids": []string{"gh"},
	}, nil); code != http.StatusOK {
		t.Fatalf("set-box-groups = %d", code)
	}

	var res struct {
		GroupIDs         []string `json:"group_ids"`
		EffectiveGroups  []string `json:"effective_groups"`
		EffectiveDomains []string `json:"effective_domains"`
	}
	if code := postAllowlist(t, s, c, "/api/v1/get-box-allowlist", map[string]any{"box_id": "web"}, &res); code != http.StatusOK {
		t.Fatalf("get-box-allowlist = %d", code)
	}
	// The box reaches its assigned "gh" plus the global "core" — but not "pypi".
	if !reflect.DeepEqual(res.EffectiveDomains, []string{"api.anthropic.com", "github.com"}) {
		t.Errorf("effective domains = %v", res.EffectiveDomains)
	}
	if !reflect.DeepEqual(res.EffectiveGroups, []string{"core", "gh"}) {
		t.Errorf("effective groups = %v", res.EffectiveGroups)
	}
}

// TestSetBoxGroupsRejectsUnknown checks an unknown group id is refused.
func TestSetBoxGroupsRejectsUnknown(t *testing.T) {
	s, _, st := newAdminServer(t)
	c := signIn(t, st, true, false)
	if code := postAllowlist(t, s, c, "/api/v1/set-box-groups", map[string]any{
		"box_id": "web", "group_ids": []string{"ghost"},
	}, nil); code != http.StatusBadRequest {
		t.Errorf("unknown group = %d, want 400", code)
	}
}

// TestAllowlistExportImport checks a bundle round-trips: export, delete, re-import.
func TestAllowlistExportImport(t *testing.T) {
	s, _, st := newAdminServer(t)
	c := signIn(t, st, true, false)
	mustSave(t, st, store.AllowlistGroup{ID: "gh", Name: "gh", Description: "git", Domains: []string{"github.com"}, TTLSeconds: 45})

	var bundle allowlistBundle
	if code := postAllowlist(t, s, c, "/api/v1/export-allowlist-groups", struct{}{}, &bundle); code != http.StatusOK {
		t.Fatalf("export = %d", code)
	}
	if bundle.Version != allowlistBundleVersion || len(bundle.Groups) != 1 || bundle.Groups[0].Name != "gh" {
		t.Fatalf("bundle = %+v", bundle)
	}

	if err := st.DeleteAllowlistGroup("gh"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var imp struct {
		Imported int `json:"imported"`
	}
	if code := postAllowlist(t, s, c, "/api/v1/import-allowlist-groups", map[string]any{"bundle": bundle}, &imp); code != http.StatusOK {
		t.Fatalf("import = %d", code)
	}
	if imp.Imported != 1 {
		t.Errorf("imported = %d, want 1", imp.Imported)
	}
	got, ok, _ := st.GetAllowlistGroup("gh")
	if !ok || got.TTLSeconds != 45 {
		t.Errorf("re-imported group = %+v ok=%v", got, ok)
	}
}

// TestAllowlistImportMerge checks the default merge mode unions domains into an
// existing group rather than overwriting it.
func TestAllowlistImportMerge(t *testing.T) {
	s, _, st := newAdminServer(t)
	c := signIn(t, st, true, false)
	mustSave(t, st, store.AllowlistGroup{ID: "gh", Name: "gh", Domains: []string{"github.com"}, TTLSeconds: 30})

	bundle := allowlistBundle{Version: allowlistBundleVersion, Groups: []allowlistBundleItem{
		{Name: "gh", Domains: []string{"api.github.com"}},
	}}
	if code := postAllowlist(t, s, c, "/api/v1/import-allowlist-groups", map[string]any{"bundle": bundle, "mode": "merge"}, nil); code != http.StatusOK {
		t.Fatalf("import merge = %d", code)
	}
	got, _, _ := st.GetAllowlistGroup("gh")
	if !reflect.DeepEqual(got.Domains, []string{"api.github.com", "github.com"}) {
		t.Errorf("merged domains = %v", got.Domains)
	}
}

// TestEffectiveAllowlist checks the pure union of global + assigned groups.
func TestEffectiveAllowlist(t *testing.T) {
	groups := []store.AllowlistGroup{
		{ID: "core", Name: "core", IsGlobal: true, Domains: []string{"a.com", "b.com"}},
		{ID: "gh", Name: "gh", Domains: []string{"b.com", "c.com"}},
		{ID: "pypi", Name: "pypi", Domains: []string{"d.com"}},
	}
	domains, names := effectiveAllowlist(groups, []string{"gh"})
	if !reflect.DeepEqual(domains, []string{"a.com", "b.com", "c.com"}) {
		t.Errorf("domains = %v", domains)
	}
	if !reflect.DeepEqual(names, []string{"core", "gh"}) {
		t.Errorf("names = %v", names)
	}
}

// TestNormalizeDomains checks cleaning, dedup, and rejection of malformed hosts.
func TestNormalizeDomains(t *testing.T) {
	got, err := normalizeDomains([]string{" GitHub.com ", "github.com", "*.github.com", ""})
	if err != nil {
		t.Fatalf("normalizeDomains: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"*.github.com", "github.com"}) {
		t.Errorf("normalized = %v", got)
	}
	for _, bad := range []string{"no-dot", "http://x.com", "a b.com", "*.*.com", "-x.com"} {
		if _, err := normalizeDomains([]string{bad}); err == nil {
			t.Errorf("normalizeDomains(%q) accepted, want error", bad)
		}
	}
}

// TestSlugify checks slug derivation.
func TestSlugify(t *testing.T) {
	cases := map[string]string{"Core AI": "core-ai", "  py_pkgs!! ": "py-pkgs", "GitHub.com": "github-com", "***": ""}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

// mustSave persists a group directly, failing the test on error.
func mustSave(t *testing.T, st Store, g store.AllowlistGroup) {
	t.Helper()
	if err := st.SaveAllowlistGroup(g); err != nil {
		t.Fatalf("SaveAllowlistGroup(%s): %v", g.ID, err)
	}
}
