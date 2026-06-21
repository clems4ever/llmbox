package cluster

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Store persists cluster enrollment state: one-time join tokens and the
// per-spoke bearer credentials minted from them. Secrets are only ever stored
// hashed (the plaintext join token is shown to the operator once; the plaintext
// credential is held only by the spoke). All methods must be safe for concurrent
// use. The bolt-backed implementation lives in the server package.
type Store interface {
	// PutJoinToken stores a join token record keyed by the hash of its secret.
	PutJoinToken(hash string, rec JoinTokenRecord) error
	// TakeJoinToken atomically reads and removes the record for a token hash
	// (one-time use); found is false when no token matches.
	TakeJoinToken(hash string) (rec JoinTokenRecord, found bool, err error)
	// ListJoinTokens returns every outstanding join token (its hash as an opaque
	// ID, its spoke name, and its expiry — never the secret, which is not stored).
	ListJoinTokens() ([]JoinTokenInfo, error)
	// DeleteJoinToken removes a join token by its hash ID; deleting a missing one
	// is a no-op.
	DeleteJoinToken(hash string) error
	// PutSpoke stores (creating or replacing) an enrolled spoke keyed by name.
	PutSpoke(name string, rec SpokeRecord) error
	// GetSpoke returns the spoke record for name; found is false when none matches.
	GetSpoke(name string) (rec SpokeRecord, found bool, err error)
	// ListSpokes returns every enrolled spoke.
	ListSpokes() ([]SpokeRecord, error)
	// DeleteSpoke removes an enrolled spoke; deleting a missing name is a no-op.
	DeleteSpoke(name string) error
}

// JoinTokenRecord is the stored form of a one-time join token: the spoke name
// baked into it and when it expires. The secret itself is not stored (only its
// hash, which is the key).
type JoinTokenRecord struct {
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expires_at"`
}

// JoinTokenInfo describes an outstanding join token for listing/revocation. ID
// is the token's hash (an opaque handle the operator can revoke by); the secret
// is never recoverable.
type JoinTokenInfo struct {
	ID        string
	Name      string
	ExpiresAt time.Time
}

// SpokeRecord is an enrolled spoke: its name, the hash of its bearer credential,
// and when it enrolled.
type SpokeRecord struct {
	Name           string    `json:"name"`
	CredentialHash string    `json:"credential_hash"`
	EnrolledAt     time.Time `json:"enrolled_at"`
}

// secretBytes is the entropy of a generated join token or credential.
const secretBytes = 32

// errEnrollRejected is returned when an enrollment request cannot be authorized
// (bad/expired join token, unknown spoke, or wrong credential). It is
// deliberately vague so the wire error does not distinguish the failure modes.
var errEnrollRejected = errors.New("enrollment rejected")

// newSecret returns a 256-bit unguessable hex secret used as a join token or a
// per-spoke credential.
//
// @return string A 64-character hex-encoded random secret.
// @error error if the system random source fails.
//
// @testcase TestCreateJoinTokenStoresHash generates a token via newSecret.
func newSecret() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// hashSecret returns the hex SHA-256 of a secret, the form stored and compared
// (so a leaked store reveals no usable secret).
//
// @arg secret The plaintext secret.
// @return string The hex-encoded SHA-256 of the secret.
//
// @testcase TestCreateJoinTokenStoresHash stores the hash of the generated token.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// sameHash reports whether a plaintext secret matches a stored hash, in constant
// time so a comparison can't be timed.
//
// @arg secret The presented plaintext secret.
// @arg hash The stored hex hash to compare against.
// @return bool True when the secret hashes to the stored value.
//
// @testcase TestReconnectChecksCredential accepts the right credential and rejects a wrong one.
func sameHash(secret, hash string) bool {
	return subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(hash)) == 1
}

// CreateJoinToken mints a one-time join token for a spoke name, stores its hash
// with the given expiry, and returns the plaintext token to show the operator
// once. ttl<=0 is rejected so a token always expires.
//
// @arg store The cluster store to persist the token in.
// @arg name The spoke name baked into the token; required.
// @arg ttl How long the token stays valid; must be positive.
// @arg now The current time (for the expiry).
// @return string The plaintext join token (shown once, never recoverable).
// @error error if the name is empty, ttl is non-positive, the secret cannot be generated, or the store write fails.
//
// @testcase TestCreateJoinTokenStoresHash stores the token hash and returns a usable secret.
// @testcase TestCreateJoinTokenRejectsEmptyName rejects an empty spoke name.
// @testcase TestCreateJoinTokenRejectsTTL rejects a non-positive ttl.
func CreateJoinToken(store Store, name string, ttl time.Duration, now time.Time) (string, error) {
	if name == "" {
		return "", errors.New("spoke name is required")
	}
	if ttl <= 0 {
		return "", errors.New("join token ttl must be positive")
	}
	secret, err := newSecret()
	if err != nil {
		return "", fmt.Errorf("generating join token: %w", err)
	}
	rec := JoinTokenRecord{Name: name, ExpiresAt: now.Add(ttl)}
	if err := store.PutJoinToken(hashSecret(secret), rec); err != nil {
		return "", fmt.Errorf("storing join token: %w", err)
	}
	return secret, nil
}

// authenticateEnroll authorizes an enrollment request and returns the spoke's
// name plus, for first-time enrollment, the freshly minted plaintext credential
// to hand back to the spoke (empty on reconnect). It consumes the join token
// (one-time) on the join path, and verifies the saved credential on the
// reconnect path.
//
// @arg store The cluster store.
// @arg req The spoke's enrollment request.
// @arg now The current time (to check token expiry).
// @return name The authorized spoke's name.
// @return credential The new plaintext credential on first enrollment, empty on reconnect.
// @error error errEnrollRejected when the token/credential is invalid or expired, or a store error.
//
// @testcase TestEnrollWithJoinTokenMintsCredential consumes a join token and mints a credential.
// @testcase TestEnrollRejectsExpiredToken rejects (and consumes) an expired token.
// @testcase TestEnrollRejectsUnknownToken rejects an unrecognized join token.
// @testcase TestEnrollReusedTokenRejected rejects a join token used a second time.
// @testcase TestReconnectChecksCredential accepts a valid reconnect and rejects a bad credential.
func authenticateEnroll(store Store, req enrollReq, now time.Time) (name, credential string, err error) {
	if req.JoinToken != "" {
		return enrollWithJoinToken(store, req.JoinToken, now)
	}
	if err := reconnect(store, req); err != nil {
		return "", "", err
	}
	return req.Name, "", nil
}

// enrollWithJoinToken consumes a join token and mints a per-spoke credential.
//
// @arg store The cluster store.
// @arg token The plaintext join token presented by the spoke.
// @arg now The current time (to check expiry).
// @return name The spoke name baked into the token.
// @return credential The minted plaintext credential.
// @error error errEnrollRejected if the token is unknown or expired, or a store error.
//
// @testcase TestEnrollWithJoinTokenMintsCredential covers the happy path.
// @testcase TestEnrollRejectsExpiredToken covers the expiry path.
func enrollWithJoinToken(store Store, token string, now time.Time) (name, credential string, err error) {
	rec, found, err := store.TakeJoinToken(hashSecret(token))
	if err != nil {
		return "", "", err
	}
	if !found {
		// Either the token was never minted in this store, or it was already
		// consumed by a prior enrollment (join tokens are one-time use).
		return "", "", fmt.Errorf("%w: join token unknown or already used", errEnrollRejected)
	}
	if now.After(rec.ExpiresAt) {
		return "", "", fmt.Errorf("%w: join token for spoke %q expired at %s", errEnrollRejected, rec.Name, rec.ExpiresAt.Format(time.RFC3339))
	}
	credential, err = newSecret()
	if err != nil {
		return "", "", fmt.Errorf("generating spoke credential: %w", err)
	}
	spoke := SpokeRecord{Name: rec.Name, CredentialHash: hashSecret(credential), EnrolledAt: now}
	if err := store.PutSpoke(rec.Name, spoke); err != nil {
		return "", "", fmt.Errorf("storing spoke: %w", err)
	}
	return rec.Name, credential, nil
}

// reconnect verifies a spoke reconnecting with its saved name and credential.
//
// @arg store The cluster store.
// @arg req The enrollment request carrying Name and Credential.
// @error error errEnrollRejected if the spoke is unknown or the credential is wrong, or a store error.
//
// @testcase TestReconnectChecksCredential accepts the right credential and rejects a wrong one.
func reconnect(store Store, req enrollReq) error {
	if req.Name == "" || req.Credential == "" {
		return fmt.Errorf("%w: reconnect missing name or credential", errEnrollRejected)
	}
	rec, found, err := store.GetSpoke(req.Name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: reconnect for unknown spoke %q (not enrolled in this store)", errEnrollRejected, req.Name)
	}
	if !sameHash(req.Credential, rec.CredentialHash) {
		return fmt.Errorf("%w: credential mismatch for spoke %q", errEnrollRejected, req.Name)
	}
	return nil
}
