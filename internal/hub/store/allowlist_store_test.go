package store

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestAllowlistStoreRoundTrip checks groups persist with their domains, list
// ordered by name, miss cleanly for an unknown id, and delete.
func TestAllowlistStoreRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	now := time.Unix(1700000000, 0).UTC()
	core := AllowlistGroup{
		ID: "core-ai", Name: "core-ai", Description: "LLM APIs", TTLSeconds: 30,
		IsGlobal: true, Domains: []string{"api.openai.com", "api.anthropic.com"},
		CreatedAt: now, UpdatedAt: now,
	}
	gh := AllowlistGroup{
		ID: "github", Name: "github", Description: "git", TTLSeconds: 60,
		Domains: []string{"github.com"}, CreatedAt: now, UpdatedAt: now,
	}
	for _, g := range []AllowlistGroup{gh, core} {
		if err := st.SaveAllowlistGroup(g); err != nil {
			t.Fatalf("SaveAllowlistGroup(%s): %v", g.ID, err)
		}
	}

	got, ok, err := st.GetAllowlistGroup("core-ai")
	if err != nil || !ok {
		t.Fatalf("GetAllowlistGroup: ok=%v err=%v", ok, err)
	}
	// Domains are returned sorted, so compare against the sorted expectation.
	want := core
	want.Domains = []string{"api.anthropic.com", "api.openai.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetAllowlistGroup = %+v, want %+v", got, want)
	}

	if _, ok, _ := st.GetAllowlistGroup("nope"); ok {
		t.Errorf("GetAllowlistGroup(nope) = ok, want miss")
	}

	list, err := st.ListAllowlistGroups()
	if err != nil {
		t.Fatalf("ListAllowlistGroups: %v", err)
	}
	if len(list) != 2 || list[0].ID != "core-ai" || list[1].ID != "github" {
		t.Fatalf("ListAllowlistGroups order = %v, want [core-ai github]", ids(list))
	}

	if err := st.DeleteAllowlistGroup("github"); err != nil {
		t.Fatalf("DeleteAllowlistGroup: %v", err)
	}
	if _, ok, _ := st.GetAllowlistGroup("github"); ok {
		t.Errorf("group still present after delete")
	}
}

// TestAllowlistStoreReplacesDomains checks a re-save swaps the domain set wholesale
// rather than accumulating stale domains.
func TestAllowlistStoreReplacesDomains(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	now := time.Unix(1700000000, 0).UTC()
	g := AllowlistGroup{ID: "g", Name: "g", TTLSeconds: 30, Domains: []string{"a.com", "b.com"}, CreatedAt: now, UpdatedAt: now}
	if err := st.SaveAllowlistGroup(g); err != nil {
		t.Fatalf("SaveAllowlistGroup: %v", err)
	}
	g.Domains = []string{"c.com"}
	if err := st.SaveAllowlistGroup(g); err != nil {
		t.Fatalf("re-SaveAllowlistGroup: %v", err)
	}
	got, _, err := st.GetAllowlistGroup("g")
	if err != nil {
		t.Fatalf("GetAllowlistGroup: %v", err)
	}
	if !reflect.DeepEqual(got.Domains, []string{"c.com"}) {
		t.Errorf("domains = %v, want [c.com]", got.Domains)
	}
}

// TestAllowlistBoxGroupsRoundTrip checks box assignments persist, list, replace,
// and are cascaded away when their group is deleted.
func TestAllowlistBoxGroupsRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	now := time.Unix(1700000000, 0).UTC()
	for _, id := range []string{"github", "pypi"} {
		if err := st.SaveAllowlistGroup(AllowlistGroup{ID: id, Name: id, TTLSeconds: 30, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("SaveAllowlistGroup(%s): %v", id, err)
		}
	}

	if err := st.SetBoxGroups("web", []string{"pypi", "github"}); err != nil {
		t.Fatalf("SetBoxGroups: %v", err)
	}
	got, err := st.GetBoxGroups("web")
	if err != nil {
		t.Fatalf("GetBoxGroups: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"github", "pypi"}) {
		t.Errorf("GetBoxGroups = %v, want [github pypi]", got)
	}

	all, err := st.ListBoxGroups()
	if err != nil {
		t.Fatalf("ListBoxGroups: %v", err)
	}
	if !reflect.DeepEqual(all, map[string][]string{"web": {"github", "pypi"}}) {
		t.Errorf("ListBoxGroups = %v", all)
	}

	// Replacing swaps the set wholesale.
	if err := st.SetBoxGroups("web", []string{"github"}); err != nil {
		t.Fatalf("SetBoxGroups replace: %v", err)
	}
	if got, _ := st.GetBoxGroups("web"); !reflect.DeepEqual(got, []string{"github"}) {
		t.Errorf("after replace GetBoxGroups = %v, want [github]", got)
	}

	// Deleting a group removes it from every box assignment.
	if err := st.DeleteAllowlistGroup("github"); err != nil {
		t.Fatalf("DeleteAllowlistGroup: %v", err)
	}
	if got, _ := st.GetBoxGroups("web"); len(got) != 0 {
		t.Errorf("after group delete GetBoxGroups = %v, want empty", got)
	}
}

// ids extracts group IDs for a readable assertion message.
func ids(groups []AllowlistGroup) []string {
	out := make([]string, len(groups))
	for i, g := range groups {
		out[i] = g.ID
	}
	return out
}
