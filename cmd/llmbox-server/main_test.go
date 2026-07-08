package main

import (
	"bytes"
	"testing"
)

// TestNewRootCmd checks the root command is wired up with the expected name,
// version, the --config flag, and the "version" subcommand which prints the
// build version.
func TestNewRootCmd(t *testing.T) {
	cmd := newRootCmd()
	if cmd.Use != name {
		t.Errorf("root Use = %q, want %q", cmd.Use, name)
	}
	if cmd.Version != version {
		t.Errorf("root Version = %q, want %q", cmd.Version, version)
	}
	if cmd.Flags().Lookup("config") == nil {
		t.Error("root command missing --config flag")
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
