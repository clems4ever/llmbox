package apikey

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	storepkg "github.com/clems4ever/llmbox/internal/hub/store"
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

// TestNewCmd checks the apikey command wires its add/list/delete subcommands,
// and that add reads --state-file (the hub's state file), not --config.
func TestNewCmd(t *testing.T) {
	cmd := NewCmd()
	if cmd.Use != "apikey" {
		t.Errorf("Use = %q, want %q", cmd.Use, "apikey")
	}
	for _, sub := range []string{"add", "list", "delete"} {
		subcmd(t, cmd, sub)
	}

	add := subcmd(t, cmd, "add")
	for _, f := range []string{"name", "ttl", "state-file"} {
		if add.Flags().Lookup(f) == nil {
			t.Errorf("apikey add missing --%s flag", f)
		}
	}
	if add.Flags().Lookup("config") != nil {
		t.Error("apikey add should not have a --config flag")
	}

	del := subcmd(t, cmd, "delete")
	for _, f := range []string{"id", "name", "state-file"} {
		if del.Flags().Lookup(f) == nil {
			t.Errorf("apikey delete missing --%s flag", f)
		}
	}
}

// TestAddAPIKeyCmdPrintsKey checks apikey-add mints a key into the state file
// and prints the secret with a bearer usage hint.
func TestAddAPIKeyCmdPrintsKey(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "hub.db")

	var out bytes.Buffer
	if err := addAPIKey(&out, stateFile, "ci", time.Hour); err != nil {
		t.Fatalf("addAPIKey: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, `API key "ci"`) {
		t.Errorf("output missing key name: %q", s)
	}
	if !strings.Contains(s, "lbx_") {
		t.Errorf("output missing the secret: %q", s)
	}
	if !strings.Contains(s, "Authorization: Bearer") {
		t.Errorf("output missing usage hint: %q", s)
	}

	// The key really landed (hashed) in the state file.
	st, err := storepkg.Open(stateFile)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	keys, err := st.ListAPIKeys()
	if err != nil || len(keys) != 1 || keys[0].Name != "ci" {
		t.Fatalf("stored keys = %v (%v), want one named ci", keys, err)
	}
}

// seedKey mints an API key named name into the store at stateFile and returns
// its hash ID.
func seedKey(t *testing.T, stateFile, name string) string {
	t.Helper()
	st, err := storepkg.Open(stateFile)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	secret, err := Create(st, name, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return HashSecret(secret)
}

// TestListAPIKeysCmd checks apikey-list shows each key's short ID, name, and
// expiry, and reports an empty store.
func TestListAPIKeysCmd(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "hub.db")

	var empty bytes.Buffer
	if err := listAPIKeys(&empty, stateFile, time.Now()); err != nil {
		t.Fatalf("listAPIKeys (empty): %v", err)
	}
	if !strings.Contains(empty.String(), "No API keys.") {
		t.Errorf("empty listing = %q", empty.String())
	}

	id := seedKey(t, stateFile, "ci")

	var out bytes.Buffer
	if err := listAPIKeys(&out, stateFile, time.Now()); err != nil {
		t.Fatalf("listAPIKeys: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "ci") || !strings.Contains(s, shortID(id)) {
		t.Errorf("listing = %q, want name ci and short id %s", s, shortID(id))
	}
	if strings.Contains(s, "(expired)") {
		t.Errorf("fresh key flagged expired: %q", s)
	}

	// An expired key is flagged.
	var later bytes.Buffer
	if err := listAPIKeys(&later, stateFile, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatalf("listAPIKeys (later): %v", err)
	}
	if !strings.Contains(later.String(), "(expired)") {
		t.Errorf("expired key not flagged: %q", later.String())
	}
}

// TestDeleteAPIKeyByID checks apikey-delete removes the single key matching an
// ID prefix and errors on an ambiguous prefix.
func TestDeleteAPIKeyByID(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "hub.db")
	id := seedKey(t, stateFile, "ci")
	seedKey(t, stateFile, "other")

	// A prefix matching more than one key is refused. Seed two keys with chosen
	// hashes so the shared prefix is deterministic.
	ambig := filepath.Join(t.TempDir(), "ambig.db")
	st, err := storepkg.Open(ambig)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Now()
	for _, h := range []string{"aa1", "aa2"} {
		if err := st.PutAPIKey(h, storepkg.APIKeyRecord{Name: "k", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Fatalf("PutAPIKey: %v", err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := deleteAPIKeys(&bytes.Buffer{}, ambig, "aa", ""); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("ambiguous prefix = %v, want ambiguity error", err)
	}

	var out bytes.Buffer
	if err := deleteAPIKeys(&out, stateFile, id, ""); err != nil {
		t.Fatalf("deleteAPIKeys: %v", err)
	}
	if !strings.Contains(out.String(), "deleted api key") {
		t.Errorf("output = %q", out.String())
	}

	after, err := storepkg.Open(stateFile)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = after.Close() }()
	if _, found, _ := after.GetAPIKey(id); found {
		t.Error("deleted key still present")
	}
	if keys, _ := after.ListAPIKeys(); len(keys) != 1 {
		t.Errorf("keys after delete = %v, want the one other key", keys)
	}
}

// TestDeleteAPIKeyByName checks apikey-delete removes every key with a name.
func TestDeleteAPIKeyByName(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "hub.db")
	seedKey(t, stateFile, "ci")
	seedKey(t, stateFile, "ci")
	seedKey(t, stateFile, "keep")

	var out bytes.Buffer
	if err := deleteAPIKeys(&out, stateFile, "", "ci"); err != nil {
		t.Fatalf("deleteAPIKeys: %v", err)
	}

	st, err := storepkg.Open(stateFile)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	keys, err := st.ListAPIKeys()
	if err != nil || len(keys) != 1 || keys[0].Name != "keep" {
		t.Fatalf("keys after delete = %v (%v), want only keep", keys, err)
	}
}

// TestDeleteAPIKeyNoMatch checks apikey-delete errors when nothing matches.
func TestDeleteAPIKeyNoMatch(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "hub.db")
	seedKey(t, stateFile, "ci")
	if err := deleteAPIKeys(&bytes.Buffer{}, stateFile, "", "nope"); err == nil {
		t.Error("delete with no match = nil, want error")
	}
}
