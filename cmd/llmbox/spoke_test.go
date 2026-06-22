package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/cluster"
	"github.com/clems4ever/llmbox/internal/config"
	"github.com/clems4ever/llmbox/internal/server"
)

// TestNewSpokeCmd checks the spoke command wiring: its flags and the
// token/create subcommand tree.
func TestNewSpokeCmd(t *testing.T) {
	cmd := newSpokeCmd()
	if cmd.Use != "spoke" {
		t.Errorf("Use = %q, want spoke", cmd.Use)
	}
	for _, f := range []string{"hub", "token", "state", "config"} {
		if cmd.Flags().Lookup(f) == nil {
			t.Errorf("missing --%s flag", f)
		}
	}
	// token subcommand with a create child.
	var tokenCmd, createCmd bool
	for _, c := range cmd.Commands() {
		if c.Use == "token" {
			tokenCmd = true
			for _, cc := range c.Commands() {
				if cc.Use == "create" {
					createCmd = true
					for _, f := range []string{"name", "ttl"} {
						if cc.Flags().Lookup(f) == nil {
							t.Errorf("create missing --%s flag", f)
						}
					}
				}
			}
		}
	}
	if !tokenCmd || !createCmd {
		t.Errorf("token/create subcommands present: token=%v create=%v", tokenCmd, createCmd)
	}
}

// TestCreateJoinTokenCmdPrintsToken checks the token-create command mints a
// token for the named spoke and prints it with a usage hint.
func TestCreateJoinTokenCmdPrintsToken(t *testing.T) {
	cfg := config.Default()
	cfg.StateFile = filepath.Join(t.TempDir(), "hub.db")

	var out bytes.Buffer
	if err := createJoinToken(&out, cfg, "edge", time.Hour); err != nil {
		t.Fatalf("createJoinToken: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, `spoke "edge"`) {
		t.Errorf("output missing spoke name: %q", s)
	}
	if !strings.Contains(s, "llmbox spoke --hub") {
		t.Errorf("output missing usage hint: %q", s)
	}
}

// seedJoinToken mints a join token for name into cfg's store and returns its ID.
func seedJoinToken(t *testing.T, cfg *config.Config, name string) string {
	t.Helper()
	store, err := server.OpenStore(cfg.StateFile)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer func() { _ = store.Close() }()
	if _, err := cluster.CreateJoinToken(store, name, time.Hour, time.Now()); err != nil {
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

// countJoinTokens returns how many join tokens are stored in cfg's state file.
func countJoinTokens(t *testing.T, cfg *config.Config) int {
	t.Helper()
	store, err := server.OpenStore(cfg.StateFile)
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
	cfg := config.Default()
	cfg.StateFile = filepath.Join(t.TempDir(), "hub.db")
	id := seedJoinToken(t, cfg, "edge")

	var out bytes.Buffer
	if err := listJoinTokens(&out, cfg, time.Now()); err != nil {
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
	cfg := config.Default()
	cfg.StateFile = filepath.Join(t.TempDir(), "hub.db")
	var out bytes.Buffer
	if err := listJoinTokens(&out, cfg, time.Now()); err != nil {
		t.Fatalf("listJoinTokens: %v", err)
	}
	if !strings.Contains(out.String(), "No outstanding join tokens") {
		t.Errorf("empty listing = %q", out.String())
	}
}

// TestRevokeJoinTokenByID revokes the single token matching an ID prefix.
func TestRevokeJoinTokenByID(t *testing.T) {
	cfg := config.Default()
	cfg.StateFile = filepath.Join(t.TempDir(), "hub.db")
	id := seedJoinToken(t, cfg, "edge")

	var out bytes.Buffer
	if err := revokeJoinTokens(&out, cfg, id[:10], ""); err != nil {
		t.Fatalf("revokeJoinTokens: %v", err)
	}
	if !strings.Contains(out.String(), "Revoked") {
		t.Errorf("revoke output = %q", out.String())
	}
	if n := countJoinTokens(t, cfg); n != 0 {
		t.Errorf("token count after revoke = %d, want 0", n)
	}
}

// TestRevokeJoinTokenByName revokes every token for a spoke name.
func TestRevokeJoinTokenByName(t *testing.T) {
	cfg := config.Default()
	cfg.StateFile = filepath.Join(t.TempDir(), "hub.db")
	seedJoinToken(t, cfg, "edge")
	seedJoinToken(t, cfg, "edge")
	seedJoinToken(t, cfg, "other")

	var out bytes.Buffer
	if err := revokeJoinTokens(&out, cfg, "", "edge"); err != nil {
		t.Fatalf("revokeJoinTokens: %v", err)
	}
	if n := countJoinTokens(t, cfg); n != 1 {
		t.Errorf("token count after revoking edge = %d, want 1 (other remains)", n)
	}
}

// TestRevokeJoinTokenNoMatch errors when nothing matches.
func TestRevokeJoinTokenNoMatch(t *testing.T) {
	cfg := config.Default()
	cfg.StateFile = filepath.Join(t.TempDir(), "hub.db")
	if err := revokeJoinTokens(&bytes.Buffer{}, cfg, "deadbeef", ""); err == nil {
		t.Fatal("expected error when no token matches")
	}
}

// TestSpokeCredsRoundTrip checks saved spoke credentials round-trip through the
// state file and that a missing file reads back as nil.
func TestSpokeCredsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spoke.json")

	// Missing file is not an error; returns nil.
	if c, err := loadSpokeCreds(path); err != nil || c != nil {
		t.Fatalf("loadSpokeCreds(missing) = (%v,%v)", c, err)
	}

	want := cluster.Credentials{Name: "edge", Credential: "secret"}
	if err := saveSpokeCreds(path, want); err != nil {
		t.Fatalf("saveSpokeCreds: %v", err)
	}
	got, err := loadSpokeCreds(path)
	if err != nil || got == nil || *got != want {
		t.Fatalf("loadSpokeCreds = (%+v,%v), want %+v", got, err, want)
	}
}

// TestCheckStateWritable checks the pre-enrollment probe accepts a writable state
// directory and rejects a read-only one, so a non-writable state location fails
// fast instead of burning the one-time join token on an unsavable enrollment.
func TestCheckStateWritable(t *testing.T) {
	// Writable: a fresh temp dir, and a nested path whose parent does not exist yet.
	dir := t.TempDir()
	if err := checkStateWritable(filepath.Join(dir, "spoke.json")); err != nil {
		t.Fatalf("checkStateWritable(writable) = %v, want nil", err)
	}
	if err := checkStateWritable(filepath.Join(dir, "sub", "spoke.json")); err != nil {
		t.Fatalf("checkStateWritable(creatable subdir) = %v, want nil", err)
	}

	// Read-only: a 0500 directory the probe cannot create a file in. (Skipped when
	// the test runs as root, which bypasses the permission bits.)
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permissions")
	}
	ro := filepath.Join(dir, "readonly")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatalf("mkdir read-only: %v", err)
	}
	if err := checkStateWritable(filepath.Join(ro, "spoke.json")); err == nil {
		t.Fatal("checkStateWritable(read-only) = nil, want a not-writable error")
	}
}

// TestRunSpokeRequiresTokenOrCreds checks runSpoke errors when neither a join
// token nor saved credentials are available for enrollment.
func TestRunSpokeRequiresTokenOrCreds(t *testing.T) {
	cfg := config.Default()
	statePath := filepath.Join(t.TempDir(), "none.json")
	err := runSpoke(context.Background(), cfg, "wss://hub/spoke/connect", "", statePath, "")
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("runSpoke err = %v, want a token-required error", err)
	}
}

// TestRunSpokeRejectsBadGPUs checks runSpoke fails fast on a malformed --box-gpus
// spec, before attempting any enrollment.
func TestRunSpokeRejectsBadGPUs(t *testing.T) {
	cfg := config.Default()
	statePath := filepath.Join(t.TempDir(), "none.json")
	err := runSpoke(context.Background(), cfg, "wss://hub/spoke/connect", "tok", statePath, "0")
	if err == nil || !strings.Contains(err.Error(), "box-gpus") {
		t.Fatalf("runSpoke err = %v, want a box-gpus error", err)
	}
}
