package token

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	storepkg "github.com/clems4ever/llmbox/internal/hub/store"
	"github.com/clems4ever/llmbox/internal/shared/cluster"
)

// subcmd returns the named direct subcommand of cmd, failing the test if absent.
func subcmd(t *testing.T, cmd *cobra.Command, use string) *cobra.Command {
	t.Helper()
	for _, c := range cmd.Commands() {
		if c.Use == use {
			return c
		}
	}
	t.Fatalf("subcommand %q not found under %q", use, cmd.Use)
	return nil
}

// TestNewCmd checks the token command wires its create/list/revoke subcommands,
// and that every subcommand exposes both --state-file and --config so the store
// can be named directly or resolved from the hub's config.
func TestNewCmd(t *testing.T) {
	cmd := NewCmd()
	if cmd.Use != "token" {
		t.Errorf("Use = %q, want %q", cmd.Use, "token")
	}
	for _, sub := range []string{"create", "list", "revoke"} {
		subcmd(t, cmd, sub)
	}

	create := subcmd(t, cmd, "create")
	for _, f := range []string{"name", "ttl", "state-file", "config"} {
		if create.Flags().Lookup(f) == nil {
			t.Errorf("token create missing --%s flag", f)
		}
	}

	list := subcmd(t, cmd, "list")
	for _, f := range []string{"state-file", "config"} {
		if list.Flags().Lookup(f) == nil {
			t.Errorf("token list missing --%s flag", f)
		}
	}

	revoke := subcmd(t, cmd, "revoke")
	for _, f := range []string{"id", "name", "state-file", "config"} {
		if revoke.Flags().Lookup(f) == nil {
			t.Errorf("token revoke missing --%s flag", f)
		}
	}
}

// TestCreateJoinTokenCmdPrintsToken checks the token-create command mints a
// token for the named spoke and prints it with a usage hint.
func TestCreateJoinTokenCmdPrintsToken(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "hub.db")

	var out bytes.Buffer
	if err := createJoinToken(&out, stateFile, "edge", time.Hour); err != nil {
		t.Fatalf("createJoinToken: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, `spoke "edge"`) {
		t.Errorf("output missing spoke name: %q", s)
	}
	if !strings.Contains(s, "llmbox-spoke docker --hub") {
		t.Errorf("output missing usage hint: %q", s)
	}
}

// seedJoinToken mints a join token for name into the store at stateFile and
// returns its ID.
func seedJoinToken(t *testing.T, stateFile, name string) string {
	t.Helper()
	store, err := storepkg.Open(stateFile)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer func() { _ = store.Close() }()
	if _, err := cluster.CreateJoinToken(store, name, "docker", time.Hour, time.Now()); err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	infos, err := store.ListJoinTokens()
	if err != nil {
		t.Fatalf("ListJoinTokens: %v", err)
	}
	for _, i := range infos {
		if i.Name == name {
			return i.ID
		}
	}
	t.Fatalf("seeded token for %q not found", name)
	return ""
}

// countJoinTokens returns how many join tokens are stored at stateFile.
func countJoinTokens(t *testing.T, stateFile string) int {
	t.Helper()
	store, err := storepkg.Open(stateFile)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer func() { _ = store.Close() }()
	infos, err := store.ListJoinTokens()
	if err != nil {
		t.Fatalf("ListJoinTokens: %v", err)
	}
	return len(infos)
}

// TestListJoinTokensCmd lists outstanding tokens with their spoke and a short ID.
func TestListJoinTokensCmd(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "hub.db")
	id := seedJoinToken(t, stateFile, "edge")

	var out bytes.Buffer
	if err := listJoinTokens(&out, stateFile, time.Now()); err != nil {
		t.Fatalf("listJoinTokens: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "edge") || !strings.Contains(s, "SPOKE") {
		t.Errorf("listing missing spoke/header: %q", s)
	}
	if !strings.Contains(s, id[:joinTokenIDLen]) {
		t.Errorf("listing missing short ID %q: %q", id[:joinTokenIDLen], s)
	}
}

// TestListJoinTokensCmdEmpty reports when there are no tokens.
func TestListJoinTokensCmdEmpty(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "hub.db")
	var out bytes.Buffer
	if err := listJoinTokens(&out, stateFile, time.Now()); err != nil {
		t.Fatalf("listJoinTokens: %v", err)
	}
	if !strings.Contains(out.String(), "No outstanding join tokens") {
		t.Errorf("empty listing = %q", out.String())
	}
}

// TestRevokeJoinTokenByID revokes the single token matching an ID prefix.
func TestRevokeJoinTokenByID(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "hub.db")
	id := seedJoinToken(t, stateFile, "edge")

	var out bytes.Buffer
	if err := revokeJoinTokens(&out, stateFile, id[:10], ""); err != nil {
		t.Fatalf("revokeJoinTokens: %v", err)
	}
	if !strings.Contains(out.String(), "Revoked") {
		t.Errorf("revoke output = %q", out.String())
	}
	if n := countJoinTokens(t, stateFile); n != 0 {
		t.Errorf("token count after revoke = %d, want 0", n)
	}
}

// TestRevokeJoinTokenByName revokes every token for a spoke name.
func TestRevokeJoinTokenByName(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "hub.db")
	seedJoinToken(t, stateFile, "edge")
	seedJoinToken(t, stateFile, "edge")
	seedJoinToken(t, stateFile, "other")

	var out bytes.Buffer
	if err := revokeJoinTokens(&out, stateFile, "", "edge"); err != nil {
		t.Fatalf("revokeJoinTokens: %v", err)
	}
	if n := countJoinTokens(t, stateFile); n != 1 {
		t.Errorf("token count after revoking edge = %d, want 1 (other remains)", n)
	}
}

// TestRevokeJoinTokenNoMatch errors when nothing matches.
func TestRevokeJoinTokenNoMatch(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "hub.db")
	if err := revokeJoinTokens(&bytes.Buffer{}, stateFile, "deadbeef", ""); err == nil {
		t.Fatal("expected error when no token matches")
	}
}
