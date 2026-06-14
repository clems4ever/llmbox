package main

import "testing"

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
