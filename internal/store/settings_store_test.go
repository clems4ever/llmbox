package store

import (
	"path/filepath"
	"testing"
)

// TestSettingsStoreRoundTrip checks settings persist by key, read back, miss
// cleanly for an unset key, and overwrite on a repeat put.
func TestSettingsStoreRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if _, ok, err := st.GetSetting("default_spoke"); err != nil || ok {
		t.Fatalf("unset key: ok=%v err=%v, want ok=false", ok, err)
	}

	if err := st.PutSetting("default_spoke", "edge-1"); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}
	if v, ok, err := st.GetSetting("default_spoke"); err != nil || !ok || v != "edge-1" {
		t.Fatalf("GetSetting = %q, ok=%v, err=%v; want \"edge-1\"", v, ok, err)
	}

	// A repeat put overwrites in place.
	if err := st.PutSetting("default_spoke", "edge-2"); err != nil {
		t.Fatalf("PutSetting overwrite: %v", err)
	}
	if v, _, _ := st.GetSetting("default_spoke"); v != "edge-2" {
		t.Errorf("after overwrite = %q, want \"edge-2\"", v)
	}

	// Clearing to empty is a valid stored value (distinct from unset).
	if err := st.PutSetting("default_spoke", ""); err != nil {
		t.Fatalf("PutSetting empty: %v", err)
	}
	if v, ok, _ := st.GetSetting("default_spoke"); !ok || v != "" {
		t.Errorf("after clear = %q, ok=%v; want empty value present", v, ok)
	}
}
