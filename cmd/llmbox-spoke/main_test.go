package main

import (
	"testing"

	"github.com/clems4ever/llmbox/internal/spoke"
)

// TestMainBuildsRootCmd checks main builds the spoke command via spoke.NewRootCmd
// with this binary's name and version.
func TestMainBuildsRootCmd(t *testing.T) {
	cmd := spoke.NewRootCmd(name, version)
	if cmd.Use != name {
		t.Errorf("Use = %q, want %q", cmd.Use, name)
	}
	if cmd.Version != version {
		t.Errorf("Version = %q, want %q", cmd.Version, version)
	}
}
