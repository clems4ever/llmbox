package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/cluster"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
	"github.com/clems4ever/llmbox/testutils"
)

// TestListBoxesCarriesSessionURLs checks the backend's box listing merges each
// box's live session: the activation URL while pending, the session URL once
// ready.
func TestListBoxesCarriesSessionURLs(t *testing.T) {
	f := &testutils.FakeMgr{CreateID: "abcdef0123456789", CreateURL: "u", SubmitURL: "https://claude.ai/code/s/1"}
	s := newTestServer(f)
	b := s.boxBackend()

	sess, err := s.createBox(context.Background(), sandbox.CreateOptions{BoxID: "web-box"})
	if err != nil {
		t.Fatalf("createBox: %v", err)
	}

	views, err := b.ListBoxes(context.Background())
	if err != nil || len(views) != 1 {
		t.Fatalf("ListBoxes = %+v (%v), want one box", views, err)
	}
	if views[0].AuthURL != s.AuthPageURL(sess.Token) || views[0].SessionURL != "" {
		t.Errorf("pending view = %+v, want auth URL %q and no session URL", views[0], s.AuthPageURL(sess.Token))
	}

	if err := s.submitCode(context.Background(), sess.Token, "CODE"); err != nil {
		t.Fatalf("submitCode: %v", err)
	}
	views, err = b.ListBoxes(context.Background())
	if err != nil || len(views) != 1 {
		t.Fatalf("ListBoxes (ready) = %+v (%v)", views, err)
	}
	if views[0].SessionURL != "https://claude.ai/code/s/1" || views[0].AuthURL != "" {
		t.Errorf("ready view = %+v, want session URL and no auth URL", views[0])
	}
}

// TestBackendCreateSpoke checks the backend mints a join token and returns the
// per-backend start command, defaulting the backend to docker.
func TestBackendCreateSpoke(t *testing.T) {
	s, _, st := newAdminServer(t)
	b := s.boxBackend()

	sp, err := b.CreateSpoke(context.Background(), "edge", "", 0)
	if err != nil {
		t.Fatalf("CreateSpoke: %v", err)
	}
	if sp.Name != "edge" || sp.Token == "" {
		t.Errorf("enrollment = %+v", sp)
	}
	if !strings.Contains(sp.Command, "llmbox-spoke docker --hub") || !strings.Contains(sp.Command, sp.Token) {
		t.Errorf("command = %q, want a docker start command carrying the token", sp.Command)
	}
	if tokens, _ := st.ListJoinTokens(); len(tokens) != 1 || tokens[0].Name != "edge" {
		t.Errorf("stored tokens = %+v, want one for edge", tokens)
	}

	fc, err := b.CreateSpoke(context.Background(), "fc-1", "firecracker", time.Hour)
	if err != nil {
		t.Fatalf("CreateSpoke firecracker: %v", err)
	}
	if !strings.Contains(fc.Command, "llmbox-spoke firecracker --hub") {
		t.Errorf("firecracker command = %q", fc.Command)
	}
}

// TestBackendCreateSpokeRejectsBackend checks an unknown backend name is
// refused (and mints no token).
func TestBackendCreateSpokeRejectsBackend(t *testing.T) {
	s, _, st := newAdminServer(t)
	if _, err := s.boxBackend().CreateSpoke(context.Background(), "edge", "podman", 0); err == nil {
		t.Error("CreateSpoke with unknown backend = nil, want error")
	}
	if tokens, _ := st.ListJoinTokens(); len(tokens) != 0 {
		t.Errorf("a token was minted despite the rejected backend: %+v", tokens)
	}
}

// TestBackendDropSpoke checks the backend drops a spoke's enrollment record.
func TestBackendDropSpoke(t *testing.T) {
	s, _, st := newAdminServer(t)
	if err := st.PutSpoke("edge", cluster.SpokeRecord{Name: "edge", EnrolledAt: time.Now()}); err != nil {
		t.Fatalf("PutSpoke: %v", err)
	}
	if err := s.boxBackend().DropSpoke(context.Background(), "edge"); err != nil {
		t.Fatalf("DropSpoke: %v", err)
	}
	if _, found, _ := st.GetSpoke("edge"); found {
		t.Error("spoke record survived the drop")
	}
}

// TestBackendSetDefaultSpoke checks the backend sets the default spoke for an
// enrolled spoke and rejects an unenrolled one.
func TestBackendSetDefaultSpoke(t *testing.T) {
	s, _, st := newAdminServer(t)
	if err := st.PutSpoke("edge", cluster.SpokeRecord{Name: "edge", EnrolledAt: time.Now()}); err != nil {
		t.Fatalf("PutSpoke: %v", err)
	}
	if err := s.boxBackend().SetDefaultSpoke(context.Background(), "edge"); err != nil {
		t.Fatalf("SetDefaultSpoke: %v", err)
	}
	if def, _ := s.DefaultSpoke(); def != "edge" {
		t.Errorf("default = %q, want edge", def)
	}
	if err := s.boxBackend().SetDefaultSpoke(context.Background(), "ghost"); err == nil {
		t.Error("SetDefaultSpoke(ghost) = nil, want error for an unenrolled spoke")
	}
}

// TestBackendJoinTokens checks the backend lists outstanding join tokens and
// revokes one by ID.
func TestBackendJoinTokens(t *testing.T) {
	s, _, st := newAdminServer(t)
	b := s.boxBackend()
	if _, err := cluster.CreateJoinToken(st, "edge", time.Hour, time.Now()); err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}

	tokens, err := b.ListJoinTokens(context.Background())
	if err != nil || len(tokens) != 1 || tokens[0].Name != "edge" || tokens[0].ID == "" {
		t.Fatalf("ListJoinTokens = %+v (%v), want one for edge", tokens, err)
	}
	if err := b.RevokeJoinToken(context.Background(), tokens[0].ID); err != nil {
		t.Fatalf("RevokeJoinToken: %v", err)
	}
	if rest, _ := b.ListJoinTokens(context.Background()); len(rest) != 0 {
		t.Errorf("token survived revocation: %+v", rest)
	}
}

// TestCreateProxyRecordsPrincipal checks the backend stamps the request's
// authenticated principal (from the API auth middleware) as the proxy creator.
func TestCreateProxyRecordsPrincipal(t *testing.T) {
	s, _ := newProxyServer(t, &testutils.FakeMgr{CreateID: "abcdef0123456789"}, nil)
	registerBox(t, s, "web-box", "")

	ctx := context.WithValue(context.Background(), principalCtxKey{}, "admin@corp.com")
	if _, err := s.boxBackend().CreateProxy(ctx, "web-box", 8000, ""); err != nil {
		t.Fatalf("CreateProxy: %v", err)
	}
	proxies, err := s.listProxies("")
	if err != nil || len(proxies) != 1 {
		t.Fatalf("listProxies = %+v (%v)", proxies, err)
	}
	if proxies[0].CreatedBy != "admin@corp.com" {
		t.Errorf("CreatedBy = %q, want the principal", proxies[0].CreatedBy)
	}
}
