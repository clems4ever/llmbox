package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/api"
	"github.com/clems4ever/llmbox/internal/shared/cluster"
	"github.com/clems4ever/llmbox/testutils"
)

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

// TestBackendJoinTokens checks the backend lists outstanding join tokens —
// each with its recorded backend and a placeholder enrollment command (never
// the secret; a token stored without a backend lists as docker) — and revokes
// one by ID.
func TestBackendJoinTokens(t *testing.T) {
	s, _, st := newAdminServer(t)
	b := s.boxBackend()
	if _, err := cluster.CreateJoinToken(st, "edge", "firecracker", time.Hour, time.Now()); err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}

	tokens, err := b.ListJoinTokens(context.Background())
	if err != nil || len(tokens) != 1 || tokens[0].Name != "edge" || tokens[0].ID == "" {
		t.Fatalf("ListJoinTokens = %+v (%v), want one for edge", tokens, err)
	}
	if tokens[0].Backend != "firecracker" {
		t.Errorf("backend = %q, want firecracker", tokens[0].Backend)
	}
	if !strings.Contains(tokens[0].Command, "llmbox-spoke firecracker --hub") ||
		!strings.Contains(tokens[0].Command, "--token "+api.TokenPlaceholder) {
		t.Errorf("command = %q, want a firecracker command carrying the token placeholder", tokens[0].Command)
	}

	// A token stored before the backend was recorded (empty backend) lists with
	// the docker default, matching what CreateSpoke defaulted to back then.
	if err := st.PutJoinToken("legacyhash", cluster.JoinTokenRecord{Name: "old-edge", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("PutJoinToken: %v", err)
	}
	all, err := b.ListJoinTokens(context.Background())
	if err != nil || len(all) != 2 {
		t.Fatalf("ListJoinTokens with legacy = %+v (%v), want 2", all, err)
	}
	for _, tok := range all {
		if tok.Name == "old-edge" && (tok.Backend != "docker" || !strings.Contains(tok.Command, "llmbox-spoke docker --hub")) {
			t.Errorf("legacy token = %+v, want the docker default", tok)
		}
	}
	if err := st.DeleteJoinToken("legacyhash"); err != nil {
		t.Fatalf("DeleteJoinToken: %v", err)
	}

	if err := b.RevokeJoinToken(context.Background(), tokens[0].ID); err != nil {
		t.Fatalf("RevokeJoinToken: %v", err)
	}
	if rest, _ := b.ListJoinTokens(context.Background()); len(rest) != 0 {
		t.Errorf("token survived revocation: %+v", rest)
	}
}

// TestBackendRegenerateJoinToken checks regenerating a join token swaps it for
// a fresh one preserving the spoke name and recorded backend (the old ID is
// gone, the new command carries the new token), and that an unknown ID errors.
func TestBackendRegenerateJoinToken(t *testing.T) {
	s, _, _ := newAdminServer(t)
	b := s.boxBackend()

	old, err := b.CreateSpoke(context.Background(), "edge", "firecracker", time.Hour)
	if err != nil {
		t.Fatalf("CreateSpoke: %v", err)
	}
	tokens, _ := b.ListJoinTokens(context.Background())
	if len(tokens) != 1 {
		t.Fatalf("tokens = %+v, want 1", tokens)
	}
	oldID := tokens[0].ID

	sp, err := b.RegenerateJoinToken(context.Background(), oldID)
	if err != nil {
		t.Fatalf("RegenerateJoinToken: %v", err)
	}
	if sp.Name != "edge" || sp.Token == "" || sp.Token == old.Token {
		t.Errorf("regenerated enrollment = %+v, want a fresh token for edge", sp)
	}
	if !strings.Contains(sp.Command, "llmbox-spoke firecracker --hub") || !strings.Contains(sp.Command, "--token "+sp.Token) {
		t.Errorf("command = %q, want a firecracker command carrying the new token", sp.Command)
	}

	// The old token is gone; exactly one (the new one) remains, still firecracker.
	tokens, _ = b.ListJoinTokens(context.Background())
	if len(tokens) != 1 || tokens[0].ID == oldID || tokens[0].Name != "edge" || tokens[0].Backend != "firecracker" {
		t.Errorf("tokens after regenerate = %+v, want one fresh firecracker token for edge", tokens)
	}

	// The old token cannot be re-regenerated, and unknown IDs error.
	if _, err := b.RegenerateJoinToken(context.Background(), oldID); err == nil {
		t.Error("regenerating a consumed ID should error")
	}
	if _, err := b.RegenerateJoinToken(context.Background(), ""); err == nil {
		t.Error("regenerating an empty ID should error")
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
	if proxies[0].Owner != "admin@corp.com" {
		t.Errorf("CreatedBy = %q, want the principal", proxies[0].Owner)
	}
}
