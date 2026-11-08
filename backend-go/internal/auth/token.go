// Package auth issues and verifies bearer credentials. Tokens are 32 random
// bytes (crypto/rand) encoded base64url; only their SHA-256 is stored, so a
// database leak does not reveal usable tokens. A player's identity always
// comes from the token, never from a username field (C2).
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
)

const tokenBytes = 32

// ErrMalformed means the presented credential cannot be a valid token.
var ErrMalformed = errors.New("malformed token")

// NewToken returns a fresh token and its storage hash.
func NewToken() (token string, hash []byte, err error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, Hash(token), nil
}

// Hash returns the SHA-256 used to look tokens up. A keyed hash is not needed:
// tokens carry 256 bits of entropy, so preimage attacks are infeasible.
func Hash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Validate checks the token shape before any database lookup.
func Validate(token string) error {
	if len(token) != base64.RawURLEncoding.EncodedLen(tokenBytes) {
		return ErrMalformed
	}
	if _, err := base64.RawURLEncoding.DecodeString(token); err != nil {
		return ErrMalformed
	}
	return nil
}

// BearerFromRequest extracts "Authorization: Bearer <token>".
func BearerFromRequest(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

// HashKey hashes an idempotency key: keys are private and never stored raw.
func HashKey(key string) []byte { return Hash("idem:" + key) }
