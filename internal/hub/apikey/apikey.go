// Package apikey mints and verifies the API keys that authenticate callers of
// the llmbox box-control API. A key is an unguessable bearer secret shown once
// at creation; only its SHA-256 hash is persisted (in the hub's store), so
// neither the database nor a listing can recover a usable credential. The
// package also provides the `llmbox-server apikey` command tree (add/list/
// delete) that manages keys directly against the hub's state file.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/clems4ever/llmbox/internal/hub/store"
)

// secretPrefix marks every generated API key so a leaked one is recognizable
// (by humans and secret scanners) as an llmbox key.
const secretPrefix = "lbx_"

// secretBytes is the entropy of a generated API key secret.
const secretBytes = 32

// Create mints a new API key named name, valid for ttl, persists its hash in st,
// and returns the plaintext secret to show the operator once. ttl<=0 is rejected
// so a key always expires.
//
// @arg st The API key store the hash is persisted in.
// @arg name The operator-chosen label for the key; required.
// @arg ttl How long the key stays valid; must be positive.
// @arg now The current time (for the created/expiry stamps).
// @return string The plaintext API key (shown once, never recoverable).
// @error error if the name is empty, ttl is non-positive, the secret cannot be generated, or the store write fails.
//
// @testcase TestCreateStoresHashAndAuthenticates mints a key whose hash lands in the store.
// @testcase TestCreateRejectsEmptyName rejects an empty key name.
// @testcase TestCreateRejectsTTL rejects a non-positive ttl.
func Create(st store.APIKeyStore, name string, ttl time.Duration, now time.Time) (string, error) {
	if name == "" {
		return "", errors.New("api key name is required")
	}
	if ttl <= 0 {
		return "", errors.New("api key ttl must be positive")
	}
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating api key: %w", err)
	}
	secret := secretPrefix + hex.EncodeToString(b)
	rec := store.APIKeyRecord{Name: name, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	if err := st.PutAPIKey(HashSecret(secret), rec); err != nil {
		return "", fmt.Errorf("storing api key: %w", err)
	}
	return secret, nil
}

// Authenticate verifies a presented API key secret against st: it hashes the
// secret, looks the hash up, and checks the key has not expired. ok is false for
// an unknown or expired key (indistinguishable to the caller, so a probe learns
// nothing); err reports only store failures.
//
// @arg st The API key store to verify against.
// @arg secret The plaintext API key presented by the caller.
// @arg now The current time (to check expiry).
// @return store.APIKeyRecord The matched key's record when ok.
// @return bool True when the key is known and unexpired.
// @error error if the store lookup fails.
//
// @testcase TestCreateStoresHashAndAuthenticates authenticates a freshly minted key.
// @testcase TestAuthenticateRejectsUnknown rejects a secret with no stored hash.
// @testcase TestAuthenticateRejectsExpired rejects a key past its expiry.
func Authenticate(st store.APIKeyStore, secret string, now time.Time) (store.APIKeyRecord, bool, error) {
	if secret == "" {
		return store.APIKeyRecord{}, false, nil
	}
	rec, ok, err := st.GetAPIKey(HashSecret(secret))
	if err != nil {
		return store.APIKeyRecord{}, false, err
	}
	if !ok || now.After(rec.ExpiresAt) {
		return store.APIKeyRecord{}, false, nil
	}
	return rec, true, nil
}

// HashSecret returns the hex SHA-256 of an API key secret — the form keys are
// stored and looked up by, so a usable credential never touches the database.
//
// @arg secret The plaintext API key secret.
// @return string The hex-encoded SHA-256 of secret.
//
// @testcase TestCreateStoresHashAndAuthenticates looks a stored key up by this hash.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
