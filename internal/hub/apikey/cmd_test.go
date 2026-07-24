package apikey

import (
	"bytes"
	"os"
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
// and that every subcommand exposes both --state-file and --config so the store
// can be named directly or resolved from the hub's config.
func TestNewCmd(t *testing.T) {
	cmd := NewCmd()
	if cmd.Use != "apikey" {
		t.Errorf("Use = %q, want %q", cmd.Use, "apikey")
	}
	for _, sub := range []string{"add", "list", "delete"} {
		subcmd(t, cmd, sub)
	}

	add := subcmd(t, cmd, "add")
	for _, f := range []string{"name", "ttl", "state-file", "config"} {
		if add.Flags().Lookup(f) == nil {
			t.Errorf("apikey add missing --%s flag", f)
		}
	}

	del := subcmd(t, cmd, "delete")
	for _, f := range []string{"id", "name", "state-file", "config"} {
		if del.Flags().Lookup(f) == nil {
			t.Errorf("apikey delete missing --%s flag", f)
		}
	}

	list := subcmd(t, cmd, "list")
	for _, f := range []string{"state-file", "config"} {
		if list.Flags().Lookup(f) == nil {
			t.Errorf("apikey list missing --%s flag", f)
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
	// The secret must be printed exactly once: a second copy in the curl example
	// makes `grep -oE 'lbx_...'` capture a two-line value with an embedded newline
	// that the server then rejects as a malformed header.
	if n := strings.Count(s, "lbx_"); n != 1 {
		t.Errorf("secret printed %d times, want exactly once: %q", n, s)
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

// TestAddAPIKeyResolvesConfigStateFile checks the wired-up `apikey add --config`
// mints the key into the state_file named by the hub's config (not the built-in
// default) and reports that path on stderr — the fix for keys landing in the
// wrong store when the hub customized state_file.
func TestAddAPIKeyResolvesConfigStateFile(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "hub-sessions.db")
	cfgPath := filepath.Join(dir, "llmbox.yaml")
	if err := os.WriteFile(cfgPath, []byte("state_file: "+stateFile+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := NewCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs([]string{"add", "--name", "recovery", "--config", cfgPath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("apikey add --config: %v", err)
	}

	// The notice on stderr names the resolved store; no default warning fires.
	if !strings.Contains(errb.String(), stateFile) {
		t.Errorf("stderr %q should name the config's state file", errb.String())
	}
	if strings.Contains(errb.String(), "warning") {
		t.Errorf("stderr warned despite a resolved config: %q", errb.String())
	}

	// The key really landed in the config's state file, not the default.
	st, err := storepkg.Open(stateFile)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	keys, err := st.ListAPIKeys()
	if err != nil || len(keys) != 1 || keys[0].Name != "recovery" {
		t.Fatalf("stored keys = %v (%v), want one named recovery", keys, err)
	}
}

// TestAddAPIKeyWarnsOnDefaultStateFile checks that with neither --state-file nor
// --config given (and no config on disk), the command warns it is using the
// built-in default and shows how to override it. It runs from a temp dir so the
// relative default store and the absent default config both resolve there rather
// than in the repo tree.
func TestAddAPIKeyWarnsOnDefaultStateFile(t *testing.T) {
	t.Chdir(t.TempDir())

	cmd := NewCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs([]string{"add", "--name", "recovery"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("apikey add: %v", err)
	}
	notice := errb.String()
	if !strings.Contains(notice, "warning") {
		t.Errorf("stderr %q should warn about the default state file", notice)
	}
	for _, want := range []string{"--state-file", "--config"} {
		if !strings.Contains(notice, want) {
			t.Errorf("stderr %q should point at %s", notice, want)
		}
	}
}
