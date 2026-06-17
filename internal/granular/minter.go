// Package granular wires llmbox to a granular authorization server. It mints a
// fresh subject token per box (so each agent has its own bearer identity for
// requesting grants) and revokes that subject when the box goes away. The token
// is injected into the box at SubjectPath, where the granular CLI reads it.
package granular

import (
	"context"
	"fmt"

	"github.com/clems4ever/granular/client"
)

// DefaultSubjectPath is where the subject token is written inside a box. It
// matches the home of the claude image's `node` user (~/.granular/subject_token),
// which the granular CLI reads as its token_file.
const DefaultSubjectPath = "/home/node/.granular/subject_token"

// Config configures a Minter. ASURL and AdminToken are required; an empty
// AdminToken means granular integration is disabled and New returns nil.
// SubjectPath defaults to DefaultSubjectPath when empty.
type Config struct {
	// ASURL is the base URL of the granular authorization server.
	ASURL string
	// AdminToken authenticates subject administration (mint/revoke) calls.
	AdminToken string
	// SubjectPath is the in-box path the minted token is written to.
	SubjectPath string
}

// Minter mints and revokes granular subject tokens using an admin credential.
type Minter struct {
	client      *client.Client
	subjectPath string
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
	path := cfg.SubjectPath
	if path == "" {
		path = DefaultSubjectPath
	}
	return &Minter{
		client:      client.New(client.Config{ASURL: cfg.ASURL, Token: cfg.AdminToken}),
		subjectPath: path,
	}
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
