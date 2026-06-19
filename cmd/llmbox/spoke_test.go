package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/cluster"
	"github.com/clems4ever/llmbox/internal/config"
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

// TestRunSpokeRequiresTokenOrCreds checks runSpoke errors when neither a join
// token nor saved credentials are available for enrollment.
func TestRunSpokeRequiresTokenOrCreds(t *testing.T) {
	cfg := config.Default()
	statePath := filepath.Join(t.TempDir(), "none.json")
	err := runSpoke(context.Background(), cfg, "wss://hub/spoke/connect", "", statePath)
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("runSpoke err = %v, want a token-required error", err)
	}
}
