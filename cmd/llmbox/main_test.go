package main

import (
	"bytes"
	"os"
	"testing"
)

// TestNewRootCmd checks the root command is wired up with the expected name,
// version, and the "version" subcommand which prints the build version.
func TestNewRootCmd(t *testing.T) {
	cmd := newRootCmd()
	if cmd.Use != name {
		t.Errorf("root Use = %q, want %q", cmd.Use, name)
	}
	if cmd.Version != version {
		t.Errorf("root Version = %q, want %q", cmd.Version, version)
	}

	var found bool
	for _, c := range cmd.Commands() {
		if c.Name() == "version" {
			found = true
			var buf bytes.Buffer
			c.SetOut(&buf)
			c.Run(c, nil)
			if got := buf.String(); got != name+" "+version+"\n" {
				t.Errorf("version output = %q, want %q", got, name+" "+version+"\n")
			}
		}
	}
	if !found {
		t.Error("version subcommand not registered")
	}
}

// TestEnvHelpers checks envOr and envInt fall back to defaults and parse values.
func TestEnvHelpers(t *testing.T) {
	t.Setenv("LLMBOX_TEST_STR", "set")
	if got := envOr("LLMBOX_TEST_STR", "def"); got != "set" {
		t.Errorf("envOr set = %q, want set", got)
	}
	if got := envOr("LLMBOX_TEST_MISSING", "def"); got != "def" {
		t.Errorf("envOr missing = %q, want def", got)
	}

	t.Setenv("LLMBOX_TEST_INT", "42")
	if got := envInt("LLMBOX_TEST_INT", 7); got != 42 {
		t.Errorf("envInt set = %d, want 42", got)
	}
	t.Setenv("LLMBOX_TEST_INT", "notanumber")
	if got := envInt("LLMBOX_TEST_INT", 7); got != 7 {
		t.Errorf("envInt invalid = %d, want default 7", got)
	}
	if got := envInt("LLMBOX_TEST_MISSING", 7); got != 7 {
		t.Errorf("envInt missing = %d, want default 7", got)
	}
}

// TestSplitLists checks splitPathList and splitCommaList split, trim, and drop
// empty entries, and yield nil for an empty spec.
func TestSplitLists(t *testing.T) {
	sep := string(os.PathListSeparator)
	got := splitPathList(" /opt/hook " + sep + sep + " /usr/bin/other ")
	if len(got) != 2 || got[0] != "/opt/hook" || got[1] != "/usr/bin/other" {
		t.Errorf("splitPathList = %v, want [/opt/hook /usr/bin/other]", got)
	}
	if splitPathList("") != nil {
		t.Error("empty path-list should yield nil")
	}

	peers := splitCommaList("granular-github, granular-as ,,")
	if len(peers) != 2 || peers[0] != "granular-github" || peers[1] != "granular-as" {
		t.Errorf("splitCommaList = %v, want [granular-github granular-as]", peers)
	}
	if splitCommaList("") != nil {
		t.Error("empty comma-list should yield nil")
	}
}
