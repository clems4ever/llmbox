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

// TestParseResourceServers checks "id=url" pairs are parsed and malformed or
// empty entries are skipped.
func TestParseResourceServers(t *testing.T) {
	got := parseResourceServers("github=http://gh:9091, gitlab=http://gl:9092 ,,bad,=nope,empty=")
	if len(got) != 2 {
		t.Fatalf("want 2 resource servers, got %d: %+v", len(got), got)
	}
	if got[0].ID != "github" || got[0].BaseURL != "http://gh:9091" {
		t.Errorf("rs[0] = %+v", got[0])
	}
	if got[1].ID != "gitlab" || got[1].BaseURL != "http://gl:9092" {
		t.Errorf("rs[1] = %+v", got[1])
	}
	if parseResourceServers("") != nil {
		t.Error("empty spec should yield nil")
	}
}
