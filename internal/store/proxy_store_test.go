package store

import (
	"path/filepath"
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
