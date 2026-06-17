package granular

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testMinter builds a Minter with two resource servers for config-file tests.
func testMinter() *Minter {
	return New(Config{
		ASURL:       "http://as",
		AdminToken:  "admin",
		SubjectPath: "/home/node/.granular/subject_token",
		ResourceServers: []ResourceServer{
			{ID: "github", BaseURL: "http://gh:9091"},
			{ID: "gitlab", BaseURL: "http://gl:9092"},
		},
	})
}

// TestConfigFilesRenderBaseURL checks each resource server yields a <id>.yaml
// with its base_url and the shared as_url, next to the subject token.
func TestConfigFilesRenderBaseURL(t *testing.T) {
	files := testMinter().ConfigFiles()
	// Two per-RS files + the client.yaml.
	if len(files) != 3 {
		t.Fatalf("want 3 config files, got %d", len(files))
	}
	if files[0].Path != "/home/node/.granular/github.yaml" {
		t.Errorf("path = %q, want .../github.yaml", files[0].Path)
	}
	if string(files[0].Content) != "base_url: \"http://gh:9091\"\nas_url: \"http://as\"\n" {
		t.Errorf("content = %q", files[0].Content)
	}
}

// TestConfigFilesRenderClientConfig checks the granular-client config (client.yaml)
// carries the AS URL, token file, and resource servers.
func TestConfigFilesRenderClientConfig(t *testing.T) {
	files := testMinter().ConfigFiles()
	client := files[len(files)-1]
	if client.Path != "/home/node/.granular/client.yaml" {
		t.Fatalf("path = %q, want .../client.yaml", client.Path)
	}
	want := "as_url: \"http://as\"\n" +
		"token_file: \"/home/node/.granular/subject_token\"\n" +
		"resource_servers:\n" +
		"  - id: \"github\"\n    base_url: \"http://gh:9091\"\n" +
		"  - id: \"gitlab\"\n    base_url: \"http://gl:9092\"\n"
	if string(client.Content) != want {
		t.Errorf("client config =\n%q\nwant\n%q", client.Content, want)
	}
}

// TestConfigFilesNilIsEmpty checks a nil Minter (or no resource servers) yields
// no config files.
func TestConfigFilesNilIsEmpty(t *testing.T) {
	var nilMinter *Minter
	if files := nilMinter.ConfigFiles(); files != nil {
		t.Errorf("nil ConfigFiles = %v, want nil", files)
	}
	m := New(Config{ASURL: "http://as", AdminToken: "admin"})
	if files := m.ConfigFiles(); files != nil {
		t.Errorf("no-RS ConfigFiles = %v, want nil", files)
	}
}

// TestNewDisabledWithoutConfig checks New returns nil when the AS URL or admin
// token is missing, leaving the integration disabled.
func TestNewDisabledWithoutConfig(t *testing.T) {
	if m := New(Config{ASURL: "", AdminToken: "tok"}); m != nil {
		t.Error("want nil Minter when AS URL is empty")
	}
	if m := New(Config{ASURL: "http://as", AdminToken: ""}); m != nil {
		t.Error("want nil Minter when admin token is empty")
	}
	if m := New(Config{ASURL: "http://as", AdminToken: "tok"}); m == nil {
		t.Error("want non-nil Minter when both are set")
	}
}

// TestSubjectPathDefault checks SubjectPath falls back to the default for a nil
// Minter and an unset path, and honours a configured path.
func TestSubjectPathDefault(t *testing.T) {
	var nilMinter *Minter
	if got := nilMinter.SubjectPath(); got != DefaultSubjectPath {
		t.Errorf("nil SubjectPath = %q, want %q", got, DefaultSubjectPath)
	}
	if got := New(Config{ASURL: "http://as", AdminToken: "t"}).SubjectPath(); got != DefaultSubjectPath {
		t.Errorf("default SubjectPath = %q, want %q", got, DefaultSubjectPath)
	}
	custom := "/etc/granular/token"
	if got := New(Config{ASURL: "http://as", AdminToken: "t", SubjectPath: custom}).SubjectPath(); got != custom {
		t.Errorf("custom SubjectPath = %q, want %q", got, custom)
	}
}

// TestMintNilIsNoop checks Mint on a nil Minter returns an empty token and no error.
func TestMintNilIsNoop(t *testing.T) {
	var nilMinter *Minter
	tok, err := nilMinter.Mint(context.Background())
	if err != nil || tok != "" {
		t.Errorf("nil Mint = (%q, %v), want (\"\", nil)", tok, err)
	}
}

// TestRevokeNilIsNoop checks Revoke is a no-op for a nil Minter or an empty token.
func TestRevokeNilIsNoop(t *testing.T) {
	var nilMinter *Minter
	if err := nilMinter.Revoke(context.Background(), "tok"); err != nil {
		t.Errorf("nil Revoke error: %v", err)
	}
	if err := New(Config{ASURL: "http://as", AdminToken: "t"}).Revoke(context.Background(), ""); err != nil {
		t.Errorf("empty-token Revoke error: %v", err)
	}
}

// TestMintCallsAuthServer checks Mint PUTs to the AS with the admin token and
// returns the minted subject token.
func TestMintCallsAuthServer(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotMethod, gotPath = r.Header.Get("Authorization"), r.Method, r.URL.Path
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "subj-123"})
	}))
	defer srv.Close()

	m := New(Config{ASURL: srv.URL, AdminToken: "admin-tok"})
	tok, err := m.Mint(context.Background())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if tok != "subj-123" {
		t.Errorf("token = %q, want subj-123", tok)
	}
	if gotMethod != http.MethodPut || gotPath != "/api/subject" {
		t.Errorf("request = %s %s, want PUT /api/subject", gotMethod, gotPath)
	}
	if !strings.Contains(gotAuth, "admin-tok") {
		t.Errorf("admin token not sent: %q", gotAuth)
	}
}

// TestRevokeCallsAuthServer checks Revoke DELETEs the subject on the AS.
func TestRevokeCallsAuthServer(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]int{"destroyed": 2})
	}))
	defer srv.Close()

	m := New(Config{ASURL: srv.URL, AdminToken: "admin-tok"})
	if err := m.Revoke(context.Background(), "subj-123"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/subject/subj-123" {
		t.Errorf("request = %s %s, want DELETE /api/subject/subj-123", gotMethod, gotPath)
	}
}
