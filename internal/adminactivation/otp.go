package adminactivation

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"math/big"
	"strings"
)

const otpDomain = "autivashop-admin-activation-code-v1"

// GenerateCode returns a uniformly distributed six-digit numeric code.
// It uses crypto/rand and never falls back to math/rand.
func GenerateCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", fmt.Errorf("generate activation code: %w", err)
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// ValidCodeFormat accepts exactly six ASCII digits, with no whitespace.
func ValidCodeFormat(code string) bool {
	if len(code) != 6 {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < '0' || code[i] > '9' {
			return false
		}
	}
	return true
}

// HashCode computes a keyed HMAC-SHA256 over the authorized email and code.
// The email binding prevents a code issued to one address from being used for
// another, and the key prevents offline guessing from a database leak.
func HashCode(key []byte, email, code string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(otpDomain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(strings.ToLower(strings.TrimSpace(email))))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(code))
	return mac.Sum(nil)
}

// CodeMatches compares the keyed hash in constant time.
func CodeMatches(key []byte, email, code string, stored []byte) bool {
	if len(stored) != sha256.Size || !ValidCodeFormat(code) {
		return false
	}
	return subtle.ConstantTimeCompare(HashCode(key, email, code), stored) == 1
}
