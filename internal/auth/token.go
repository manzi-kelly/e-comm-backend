package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

const (
	sessionTokenBytes = 32
	csrfTokenBytes    = 32
)

// GenerateToken returns a cryptographically random, URL-safe opaque token.
func GenerateToken(n int) (string, error) {
	if n <= 0 {
		n = sessionTokenBytes
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns a SHA-256 digest of a high-entropy token for storage and lookup.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// HashTokenHex returns the hex encoding of HashToken for logging-safe comparisons in tests/debug only.
func HashTokenHex(token string) string {
	return hex.EncodeToString(HashToken(token))
}

// TokensEqual performs constant-time comparison of token hashes.
func TokensEqual(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}
