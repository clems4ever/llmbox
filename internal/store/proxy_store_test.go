package store

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestProxyStoreRoundTrip checks proxies persist by slug, list, miss cleanly for
// an unknown slug, and delete.
func TestProxyStoreRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	rec := ProxyRecord{
		Slug:        "abc123def456",
		BoxID:       "web-box",
		ContainerID: "abcdef0123456789",
		Port:        8000,
		Spoke:       "local",
		CreatedAt:   time.Unix(1700000000, 0).UTC(),
		CreatedBy:   "dev@example.com",
		Description: "preview server",
	}
	if err := st.SaveProxy(rec); err != nil {
		t.Fatalf("SaveProxy: %v", err)
	}

	got, ok, err := st.GetProxy(rec.Slug)
	if err != nil || !ok {
		t.Fatalf("GetProxy: ok=%v err=%v", ok, err)
	}
	if got != rec {
		t.Errorf("GetProxy = %+v, want %+v", got, rec)
	}

	if _, ok, _ := st.GetProxy("nope"); ok {
		t.Error("GetProxy found an unknown slug")
	}

	list, err := st.ListProxies()
	if err != nil {
		t.Fatalf("ListProxies: %v", err)
	}
	if len(list) != 1 || list[0] != rec {
		t.Errorf("ListProxies = %+v, want [%+v]", list, rec)
	}

	if err := st.DeleteProxy(rec.Slug); err != nil {
		t.Fatalf("DeleteProxy: %v", err)
	}
	if _, ok, _ := st.GetProxy(rec.Slug); ok {
		t.Error("proxy still present after delete")
	}
	// Deleting a missing slug is a no-op.
	if err := st.DeleteProxy("nope"); err != nil {
		t.Errorf("DeleteProxy(missing): %v", err)
	}
}

// TestProxyRecordDescriptionOmitEmpty checks the Description field is omitted from
// the on-disk JSON when empty, and that a record written before the field existed
// (its JSON lacks the "description" key) decodes with an empty Description, keeping
// old records backward compatible.
func TestProxyRecordDescriptionOmitEmpty(t *testing.T) {
	// An empty Description must not appear in the marshalled JSON.
	data, err := json.Marshal(ProxyRecord{Slug: "s", BoxID: "b", Port: 1})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got := string(data); strings.Contains(got, "description") {
		t.Errorf("empty description was serialized: %s", got)
	}

	// A legacy record (no description key) decodes with an empty Description.
	var legacy ProxyRecord
	const legacyJSON = `{"slug":"s","box_id":"b","port":8000,"created_at":"2023-11-14T22:13:20Z"}`
	if err := json.Unmarshal([]byte(legacyJSON), &legacy); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if legacy.Description != "" {
		t.Errorf("legacy record Description = %q, want empty", legacy.Description)
	}

	// A record with a description round-trips through JSON.
	var withDesc ProxyRecord
	if err := json.Unmarshal([]byte(`{"slug":"s","description":"note"}`), &withDesc); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if withDesc.Description != "note" {
		t.Errorf("Description = %q, want %q", withDesc.Description, "note")
	}
}
