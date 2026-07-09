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

	if err := st.PutBox(store.Box{Token: "t"}); err != nil {
		t.Errorf("PutBox: %v", err)
	}
	if err := st.DeleteBox("t"); err != nil {
		t.Errorf("DeleteBox: %v", err)
	}
	if got, err := st.ListBoxes(); err != nil || got != nil {
		t.Errorf("ListBoxes = %v, %v; want nil, nil", got, err)
	}
	if err := st.PutOIDCFlow("s", store.OIDCFlow{}); err != nil {
		t.Errorf("PutOIDCFlow: %v", err)
	}
	if _, ok, err := st.TakeOIDCFlow("s"); ok || err != nil {
		t.Errorf("TakeOIDCFlow = %v, %v; want false, nil", ok, err)
	}
	if err := st.PutIdentitySession("id", store.IdentitySession{}); err != nil {
		t.Errorf("PutIdentitySession: %v", err)
	}
	if _, ok, err := st.GetIdentitySession("id"); ok || err != nil {
		t.Errorf("GetIdentitySession = %v, %v; want false, nil", ok, err)
	}
	if err := st.DeleteIdentitySession("id"); err != nil {
		t.Errorf("DeleteIdentitySession: %v", err)
	}
	if err := st.PurgeExpiredIdentities(time.Unix(0, 0)); err != nil {
		t.Errorf("PurgeExpiredIdentities: %v", err)
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
