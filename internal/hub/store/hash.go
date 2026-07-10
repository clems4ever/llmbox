package store

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashToken returns the hex SHA-256 of a bearer token, the form the store keys
// secret-bearing values by so a stolen database file yields no usable
// credential. It is the single hashing primitive for the state file's
// server-minted secrets — box activation tokens, identity-session cookie ids,
// and OIDC state — mirroring how api keys and spoke join tokens are already
// stored (see internal/hub/apikey and internal/shared/cluster).
//
// The tokens are 256 bits of system randomness, so a plain cryptographic hash is
// the right primitive: reversing or brute-forcing the pre-image is infeasible,
// and unlike a password KDF it adds no per-lookup latency to the hot cookie path.
//
// @arg token The plaintext token to hash.
// @return string The hex-encoded SHA-256 of the token.
//
// @testcase TestHashTokenIsDeterministicHex hashes a token to stable lowercase hex.
// @testcase TestHashTokenDistinguishesInputs maps different tokens to different hashes.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
