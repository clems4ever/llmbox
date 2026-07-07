package testutils

import (
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/hub/store"
	"github.com/clems4ever/llmbox/internal/shared/cluster"
)

// TestNoopStore checks NoopStore satisfies store.Store and that every method is
// inert: writes are dropped and reads find nothing without error.
func TestNoopStore(t *testing.T) {
	var st store.Store = NoopStore{}

	if err := st.Save(store.PersistedSession{Token: "t"}); err != nil {
		t.Errorf("Save: %v", err)
	}
	if err := st.Delete("t"); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if got, err := st.LoadAll(); err != nil || got != nil {
		t.Errorf("LoadAll = %v, %v; want nil, nil", got, err)
	}
	if err := st.SaveLoginFlow("s", store.LoginFlow{}); err != nil {
		t.Errorf("SaveLoginFlow: %v", err)
	}
	if _, ok, err := st.TakeLoginFlow("s"); ok || err != nil {
		t.Errorf("TakeLoginFlow = %v, %v; want false, nil", ok, err)
	}
	if err := st.SaveLoginSession("id", store.LoginSession{}); err != nil {
		t.Errorf("SaveLoginSession: %v", err)
	}
	if _, ok, err := st.LoginSession("id"); ok || err != nil {
		t.Errorf("LoginSession = %v, %v; want false, nil", ok, err)
	}
	if err := st.DeleteLoginSession("id"); err != nil {
		t.Errorf("DeleteLoginSession: %v", err)
	}
	if err := st.PurgeExpiredLogins(time.Unix(0, 0)); err != nil {
		t.Errorf("PurgeExpiredLogins: %v", err)
	}

	if err := st.PutJoinToken("h", cluster.JoinTokenRecord{}); err != nil {
		t.Errorf("PutJoinToken: %v", err)
	}
	if _, ok, err := st.TakeJoinToken("h"); ok || err != nil {
		t.Errorf("TakeJoinToken = %v, %v; want false, nil", ok, err)
	}
	if got, err := st.ListJoinTokens(); err != nil || got != nil {
		t.Errorf("ListJoinTokens = %v, %v; want nil, nil", got, err)
	}
	if err := st.DeleteJoinToken("h"); err != nil {
		t.Errorf("DeleteJoinToken: %v", err)
	}
	if err := st.PutSpoke("n", cluster.SpokeRecord{}); err != nil {
		t.Errorf("PutSpoke: %v", err)
	}
	if _, ok, err := st.GetSpoke("n"); ok || err != nil {
		t.Errorf("GetSpoke = %v, %v; want false, nil", ok, err)
	}
	if got, err := st.ListSpokes(); err != nil || got != nil {
		t.Errorf("ListSpokes = %v, %v; want nil, nil", got, err)
	}
	if err := st.DeleteSpoke("n"); err != nil {
		t.Errorf("DeleteSpoke: %v", err)
	}
	if err := st.PutSetting("k", "v"); err != nil {
		t.Errorf("PutSetting: %v", err)
	}
	if _, ok, err := st.GetSetting("k"); ok || err != nil {
		t.Errorf("GetSetting = %v, %v; want false, nil", ok, err)
	}
	if err := st.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
