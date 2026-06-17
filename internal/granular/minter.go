// Package granular wires llmbox to a granular authorization server. It mints a
// fresh subject token per box (so each agent has its own bearer identity for
// requesting grants) and revokes that subject when the box goes away. The token
// is injected into the box at SubjectPath, where the granular CLI reads it.
package granular

import (
	"context"
	"fmt"
	"path"

	"github.com/clems4ever/granular/client"
)

// DefaultSubjectPath is where the subject token is written inside a box. It
// matches the home of the claude image's `node` user (~/.granular/subject_token),
// which the granular CLI reads as its token_file.
const DefaultSubjectPath = "/home/node/.granular/subject_token"

// ResourceServer is a granular resource server an in-box agent can reach: a
// stable id (e.g. "github") and the base URL it is served at.
type ResourceServer struct {
	// ID is the resource server id, matching the per-RS CLI (e.g. "github").
	ID string
	// BaseURL is the resource server's HTTP base URL, reachable from the box.
	BaseURL string
}

// ConfigFile is one file to write into a box: an absolute path and its bytes.
type ConfigFile struct {
	Path    string
	Content []byte
}

// Config configures a Minter. ASURL and AdminToken are required; an empty
// AdminToken means granular integration is disabled and New returns nil.
// SubjectPath defaults to DefaultSubjectPath when empty. ResourceServers, when
// set, are written into each box as per-RS CLI config files so an agent need not
// pass the resource server URL on every call.
type Config struct {
	// ASURL is the base URL of the granular authorization server.
	ASURL string
	// AdminToken authenticates subject administration (mint/revoke) calls.
	AdminToken string
	// SubjectPath is the in-box path the minted token is written to.
	SubjectPath string
	// ResourceServers are the resource servers an in-box agent can reach.
	ResourceServers []ResourceServer
}

// Minter mints and revokes granular subject tokens using an admin credential.
type Minter struct {
	client          *client.Client
	subjectPath     string
	resourceServers []ResourceServer
}

// New builds a Minter from cfg, or returns nil when granular is not configured
// (cfg.ASURL or cfg.AdminToken empty). A nil *Minter is a no-op that callers can
// hold without nil-checking every call site, so integration stays opt-in.
//
// @arg cfg The granular AS URL, admin token, and optional subject path.
// @return *Minter A ready Minter, or nil when granular is not configured.
//
// @testcase TestNewDisabledWithoutConfig returns nil when the AS URL or admin token is empty.
// @testcase TestMintCallsAuthServer builds a Minter that talks to a stub AS.
func New(cfg Config) *Minter {
	if cfg.ASURL == "" || cfg.AdminToken == "" {
		return nil
	}
	subjectPath := cfg.SubjectPath
	if subjectPath == "" {
		subjectPath = DefaultSubjectPath
	}
	return &Minter{
		client:          client.New(client.Config{ASURL: cfg.ASURL, Token: cfg.AdminToken}),
		subjectPath:     subjectPath,
		resourceServers: cfg.ResourceServers,
	}
}

// ConfigFiles returns the per-resource-server CLI config files to inject into a
// box: one `<id>.yaml` (holding only `base_url`) per configured resource server,
// written next to the subject token (so ~/.granular/<id>.yaml resolves for the
// box user). It is safe to call on a nil Minter, returning no files.
//
// @return []ConfigFile One config file per configured resource server.
//
// @testcase TestConfigFilesRenderBaseURL renders one base_url file per resource server.
// @testcase TestConfigFilesNilIsEmpty returns no files for a nil Minter.
func (m *Minter) ConfigFiles() []ConfigFile {
	if m == nil || len(m.resourceServers) == 0 {
		return nil
	}
	dir := path.Dir(m.subjectPath)
	files := make([]ConfigFile, 0, len(m.resourceServers))
	for _, rs := range m.resourceServers {
		files = append(files, ConfigFile{
			Path:    path.Join(dir, rs.ID+".yaml"),
			Content: []byte(fmt.Sprintf("base_url: %q\n", rs.BaseURL)),
		})
	}
	return files
}

// SubjectPath returns the in-box path the minted token is written to. It is safe
// to call on a nil Minter, where it returns the default path.
//
// @return string The in-box subject token path.
//
// @testcase TestSubjectPathDefault returns the default path for a nil Minter.
func (m *Minter) SubjectPath() string {
	if m == nil {
		return DefaultSubjectPath
	}
	return m.subjectPath
}

// Mint creates a new granular subject and returns its token. It is safe to call
// on a nil Minter, where it returns an empty token and no error so disabled
// integration is a silent no-op.
//
// @arg ctx Context for the AS call.
// @return string The new subject token, or "" when the Minter is nil.
// @error error if the authorization server cannot mint a subject.
//
// @testcase TestMintNilIsNoop returns an empty token for a nil Minter.
// @testcase TestMintCallsAuthServer returns the token minted by the AS.
func (m *Minter) Mint(ctx context.Context) (string, error) {
	if m == nil {
		return "", nil
	}
	tok, err := m.client.CreateSubject(ctx)
	if err != nil {
		return "", fmt.Errorf("minting granular subject: %w", err)
	}
	return tok, nil
}

// Revoke destroys the granular subject named by token, invalidating its grants.
// It is safe to call on a nil Minter or with an empty token, both of which are
// no-ops, so torn-down boxes can revoke unconditionally.
//
// @arg ctx Context for the AS call.
// @arg token The subject token to revoke; "" is a no-op.
// @error error if the authorization server cannot destroy the subject.
//
// @testcase TestRevokeNilIsNoop does nothing for a nil Minter or empty token.
// @testcase TestRevokeCallsAuthServer destroys the subject via the AS.
func (m *Minter) Revoke(ctx context.Context, token string) error {
	if m == nil || token == "" {
		return nil
	}
	if _, err := m.client.DestroySubject(ctx, token); err != nil {
		return fmt.Errorf("revoking granular subject: %w", err)
	}
	return nil
}
